#!/usr/bin/env python3
"""Refreeze a pattern bundle from a run, offline.

A bundle is `gtme freeze --bundle` output (SPEC §8): the exact pipeline that
ran, every binding it referenced with its fixtures, the registry slice, and a
manifest of hashes. `gtme freeze` needs a persisted run, and a `--simulate`
run persists nothing — so this script mints the run the bundle is frozen
from, without spending or sending: a throwaway home and ledger, the pattern's
bindings served from their own conformance fixtures by a local server, AI
steps on the fixture engine, delivery held by `--dry-run`. Then it freezes,
lays the frozen files over the bundle directory (keeping README.md, the
input files, receipt.txt), and rewrites receipt.txt from a fresh
`gtme run . --simulate` of the result — the receipt a clean checkout sees.

    bundles/refreeze.py bundles/email-waterfall
    bundles/refreeze.py bundles/qualify-group-send/2-send --after bundles/qualify-group-send/1-qualify

`--after` names bundles to run armed first, in order, so a pipeline whose
source is a group frozen by an earlier bundle has that group in the
throwaway ledger. Those runs are offline by construction — they are the
chain's cheap halves (csv, sql, demo/enrich, AI on the fixture engine,
group handoffs) — and the script refuses anything else by never giving
them a network.

Python 3 standard library only, like the test fixture adapters. Needs a
built `gtme` on PATH or at ./bin/gtme.
"""

import argparse
import http.server
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import urllib.parse

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
KEEP = {"README.md", "receipt.txt"}  # beside the manifest, never frozen
FROZEN = {"manifest.json", "pipeline.yaml", "adapters", "registry"}


def gtme_bin():
    local = os.path.join(REPO, "bin", "gtme")
    if os.access(local, os.X_OK):
        return local
    found = shutil.which("gtme")
    if not found:
        sys.exit("refreeze: no gtme binary — run `make build` first")
    return found


class Fixtures(http.server.BaseHTTPRequestHandler):
    """Serves every installed binding's conformance fixtures with the engine's
    own matching rule (internal/binding/fixtures.go): a response whose
    `match` is a substring of "METHOD path" or of the full URL."""

    responses = []  # (match, status, body) in file order across bindings

    def _serve(self):
        key = self.command + " " + urllib.parse.urlsplit(self.path).path
        for match, status, body in self.responses:
            if match in key or match in self.path:
                raw = json.dumps(body).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)
                return
        raw = json.dumps({"error": "no fixture matches " + key}).encode()
        self.send_response(404)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = _serve

    def log_message(self, *_):
        pass


def start_fixture_server():
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Fixtures)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv, "http://127.0.0.1:%d" % srv.server_address[1]


def binding_dirs(src):
    root = os.path.join(src, "adapters")
    if not os.path.isdir(root):
        return []
    return sorted(
        os.path.join(root, d) for d in os.listdir(root)
        if os.path.isfile(os.path.join(root, d, "binding.yaml"))
    )


def install(home, src, base_url=None):
    """Copy the pattern's bindings into the throwaway home. With base_url set,
    point each binding's `base_url` default at the fixture server for the
    producing run; the originals go back before the freeze, so the bundle
    carries the binding exactly as authored."""
    creds = set()
    for d in binding_dirs(src):
        dest = os.path.join(home, ".gtme", "adapters", os.path.basename(d))
        shutil.rmtree(dest, ignore_errors=True)
        shutil.copytree(d, dest)
        doc = open(os.path.join(dest, "binding.yaml")).read()
        m = re.search(r"^credentials:\s*\[([^\]]*)\]", doc, re.M)
        if m:
            creds.update(c.strip() for c in m.group(1).split(",") if c.strip())
        if base_url:
            out, in_base = [], False
            for line in doc.splitlines():
                if re.match(r"^\s*base_url:\s*$", line):
                    in_base = True
                elif in_base and re.match(r'^\s*default:\s*"https?://', line):
                    line = re.sub(r'"https?://[^"]*"', '"%s"' % base_url, line)
                    in_base = False
                elif in_base and not line.startswith(" "):
                    in_base = False
                out.append(line)
            open(os.path.join(dest, "binding.yaml"), "w").write("\n".join(out) + "\n")
        fx = os.path.join(d, "fixtures", "conformance.json")
        if base_url and os.path.isfile(fx):
            for r in json.load(open(fx)).get("responses", []):
                Fixtures.responses.append((r["match"], r.get("status") or 200, r.get("body")))
    return creds


def env_for(home, creds):
    auto = os.path.join(home, "auto.json")
    with open(auto, "w") as f:
        f.write('["$auto"]\n')
    env = {
        "HOME": home,
        "PATH": os.environ.get("PATH", ""),
        "GTME_AI_ENGINE": "fixture",
        "GTME_AI_FIXTURE": auto,
        # Nothing here may reach a vendor. Go's HTTP client honours these and
        # never proxies loopback, so the fixture server stays reachable while
        # every other host fails fast (a built-in deliver adapter's dry-run
        # preflight, say, which then reports itself inconclusive and holds).
        "HTTP_PROXY": "http://127.0.0.1:9",
        "HTTPS_PROXY": "http://127.0.0.1:9",
        "NO_PROXY": "127.0.0.1,localhost",
    }
    for c in creds:
        env[c] = "refreeze-placeholder"
    return env


def run(gtme, env, cwd, *args, check=True):
    p = subprocess.run([gtme, *args], cwd=cwd, env=env, capture_output=True, text=True)
    if check and p.returncode != 0:
        sys.stderr.write(p.stderr)
        sys.exit("refreeze: gtme %s exited %d" % (" ".join(args), p.returncode))
    return p


def prepare_home(gtme, tmp, name, after):
    home = os.path.join(tmp, name)
    os.makedirs(home)
    env = env_for(home, set())
    run(gtme, env, home, "init")
    # The chain's earlier bundles run armed against the throwaway ledger so
    # their groups exist — offline steps only, by the bundles' construction.
    for b in after:
        b = os.path.abspath(b)
        p = run(gtme, env, b, "run", ".")
        sys.stderr.write("refreeze: ran %s armed (throwaway ledger)\n" % os.path.relpath(b, REPO))
    return home, env


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("src", help="pattern directory: pipeline.yaml, adapters/, input files")
    ap.add_argument("--out", help="bundle directory to write (default: src)")
    ap.add_argument("--after", nargs="*", default=[], help="bundles to run armed first, in order")
    args = ap.parse_args()

    gtme = gtme_bin()
    src = os.path.abspath(args.src)
    out = os.path.abspath(args.out or args.src)
    if not os.path.isfile(os.path.join(src, "pipeline.yaml")):
        sys.exit("refreeze: %s has no pipeline.yaml" % src)

    tmp = tempfile.mkdtemp(prefix="gtme-refreeze-")
    try:
        # 1. The producing run: bindings on the fixture server, delivery held.
        srv, base = start_fixture_server()
        home, env = prepare_home(gtme, tmp, "produce", args.after)
        env.update({k: "refreeze-placeholder" for k in install(home, src, base)})
        # A built-in adapter's credential (the Go instantly/add-to-campaign,
        # say) is not in any bundled binding: ask the plan which ones are
        # missing and stand placeholders in. Nothing can reach a vendor with
        # them — see env_for — so a placeholder is only ever a plan-time key.
        plan = run(gtme, env, src, "plan", "pipeline.yaml", check=False)
        for name in set(re.findall(r"missing credential (\w+)", plan.stderr)):
            env[name] = "refreeze-placeholder"
        p = run(gtme, env, src, "run", "pipeline.yaml", "--dry-run")
        srv.shutdown()

        # 2. Originals back, then freeze: the bundle carries the bindings as
        #    authored, not as pointed at the server.
        install(home, src)
        frozen = os.path.join(tmp, "frozen")
        run(gtme, env, src, "freeze", "last", "--bundle", frozen)

        # 3. Lay the frozen files over the bundle directory.
        os.makedirs(out, exist_ok=True)
        for name in FROZEN:
            target = os.path.join(out, name)
            if os.path.isdir(target):
                shutil.rmtree(target)
            elif os.path.exists(target):
                os.remove(target)
        for name in os.listdir(frozen):
            s, d = os.path.join(frozen, name), os.path.join(out, name)
            shutil.copytree(s, d) if os.path.isdir(s) else shutil.copy2(s, d)
        if src != out:
            for name in os.listdir(src):
                if name in FROZEN or name in KEEP or name.startswith("."):
                    continue
                s, d = os.path.join(src, name), os.path.join(out, name)
                if os.path.isfile(s):
                    shutil.copy2(s, d)

        # 4. receipt.txt: what a clean checkout sees. A fresh home, the chain
        #    replayed, then the bundle itself, simulated.
        home2, env2 = prepare_home(gtme, tmp, "receipt", args.after)
        sim = run(gtme, env2, out, "run", ".", "--simulate")
        lines = [l for l in sim.stderr.splitlines() if not l.startswith("gtme home:")]
        with open(os.path.join(out, "receipt.txt"), "w") as f:
            f.write("$ gtme run . --simulate\n" + "\n".join(lines) + "\n")
        sys.stderr.write("refreeze: wrote %s\n" % os.path.relpath(out, REPO))
        sys.stdout.write("\n".join(lines) + "\n")
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    main()
