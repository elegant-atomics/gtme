// Package e2e drives the built gtm binary the way an operator would: no network,
// no real keys, fixture adapters only (SPEC §11).
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/trevorfox/gtm/internal/ledger"
)

var gtmBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gtm-e2e-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: temp dir:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	gtmBin = filepath.Join(dir, "gtm")
	build := exec.Command("go", "build", "-o", gtmBin, "./cmd/gtm")
	build.Dir = repoRoot()
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: building gtm:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("e2e: cannot locate the repo root")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// harness is one isolated gtm installation: its own home, ledger and workspace.
type harness struct {
	t      *testing.T
	home   string
	work   string
	ledger string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{
		t:      t,
		home:   filepath.Join(root, "home"),
		work:   filepath.Join(root, "work"),
		ledger: filepath.Join(root, "home", ".gtm", "ledger.db"),
	}
	for _, d := range []string{h.home, h.work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	h.mustRun("init")
	return h
}

// env is the process environment for a gtm invocation: an isolated HOME, the
// repo's external adapters on the search path, and nothing else.
func (h *harness) env() []string {
	return []string{
		"HOME=" + h.home,
		"PATH=" + os.Getenv("PATH"),
		"GTM_LEDGER=" + h.ledger,
		"GTM_ADAPTER_PATH=" + filepath.Join(repoRoot(), "adapters") + ":" +
			filepath.Join(repoRoot(), "test", "fixtures", "adapters"),
	}
}

type result struct {
	stdout string
	stderr string
	code   int
}

func (h *harness) run(args ...string) result {
	h.t.Helper()
	return h.runWithEnv(nil, "", args...)
}

// commandTimeout keeps a hung adapter or a deadlocked runner from stalling the
// whole suite; nothing here should take more than a second or two.
const commandTimeout = 60 * time.Second

// runWithEnv runs gtm with extra environment entries and optional stdin.
func (h *harness) runWithEnv(extraEnv []string, stdin string, args ...string) result {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, gtmBin, args...)
	cmd.Dir = h.work
	cmd.Env = append(h.env(), extraEnv...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()

	code := 0
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			code = ee.ExitCode()
		} else {
			h.t.Fatalf("running gtm %s: %v", strings.Join(args, " "), err)
		}
	}
	return result{stdout: out.String(), stderr: errb.String(), code: code}
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

func (h *harness) mustRun(args ...string) result {
	h.t.Helper()
	res := h.run(args...)
	if res.code != 0 {
		h.t.Fatalf("gtm %s failed with %d\nstderr:\n%s", strings.Join(args, " "), res.code, res.stderr)
	}
	return res
}

// write puts a file in the workspace and returns its path.
func (h *harness) write(name, content string) string {
	h.t.Helper()
	path := filepath.Join(h.work, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		h.t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// open returns a handle on the harness ledger for assertions.
func (h *harness) open() *ledger.Ledger {
	h.t.Helper()
	l, err := ledger.Open(context.Background(), h.ledger)
	if err != nil {
		h.t.Fatalf("opening ledger: %v", err)
	}
	h.t.Cleanup(func() { l.Close() })
	return l
}

// queryInt runs a scalar query against the ledger.
func (h *harness) queryInt(query string, args ...any) int {
	h.t.Helper()
	l := h.open()
	var n int
	if err := l.DB().QueryRow(query, args...).Scan(&n); err != nil {
		h.t.Fatalf("query %q: %v", query, err)
	}
	return n
}

// queryStrings runs a single-column query against the ledger.
func (h *harness) queryStrings(query string, args ...any) []string {
	h.t.Helper()
	l := h.open()
	rows, err := l.DB().Query(query, args...)
	if err != nil {
		h.t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			h.t.Fatalf("scan %q: %v", query, err)
		}
		out = append(out, s)
	}
	return out
}

func contains(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%s should contain %q, got:\n%s", what, needle, haystack)
	}
}

// nonEmptyLines splits NDJSON output into its non-blank lines.
func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// writeAdapter installs an external adapter into the harness home, the way an
// operator would drop one into ~/.gtm/adapters/<name>/.
func (h *harness) writeAdapter(name, manifest, script string) string {
	h.t.Helper()
	dir := filepath.Join(h.home, ".gtm", "adapters", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		h.t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run"), []byte(script), 0o755); err != nil {
		h.t.Fatalf("write run: %v", err)
	}
	return dir
}

const needsLinkedInManifest = `{
  "id": "needs-linkedin",
  "version": 1,
  "role": "enrich",
  "entity_type": "person",
  "needs": {"type":"object","required":["linkedin_url"],"properties":{"linkedin_url":{"type":"string"}}},
  "provides": {"type":"object","additionalProperties":false,"properties":{"headline":{"type":"string"}}}
}`

// echoAdapterScript answers every record with a fixed headline.
const echoAdapterScript = `#!/usr/bin/env python3
import json, sys
PROVIDES = {"type":"object","additionalProperties":False,"properties":{"headline":{"type":"string"}}}
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    if msg.get("type") == "OPEN":
        print(json.dumps({"type":"SCHEMA","provides":PROVIDES}), flush=True)
    elif msg.get("type") == "RECORD":
        print(json.dumps({"type":"RECORD","key":msg["key"],"fields":{"headline":"fixture"}}), flush=True)
    elif msg.get("type") == "END":
        break
print(json.dumps({"type":"END"}), flush=True)
`
