# Pattern bundles

Five outbound patterns, each frozen by `gtme freeze --bundle` from a run
and committed here as a **campaign bundle** (SPEC §8): the exact
`pipeline.yaml` that ran, every binding it referenced with its conformance
fixtures, the registry slice, and a `manifest.json` of content hashes.
Every bundle simulates offline from this checkout with no keys, no network
and no spend, and CI proves it on every push (`test/e2e/bundles_test.go`
simulates all nine and diffs each `receipt.txt` against a fresh run).

| Pattern | Bundles | Shape | Spends when armed |
|---|---|---|---|
| [`qualify-group-send/`](qualify-group-send/) | `1-qualify`, `2-send` | source → enrich → judge ⇒ group; group → compose → deliver | one model key; the send is gated |
| [`email-waterfall/`](email-waterfall/) | one | finder A → finder B → verify → filter → CSV | your finders' credits, only where the earlier one found nothing |
| [`account-shape/`](account-shape/) | `1-qualify` … `4-outreach` | companies ⇒ group; people gated by their company ⇒ group; fan-in per account ⇒ group; bounded outreach | one model key; the send is gated |
| [`events-cron/`](events-cron/) | one | a CSV a receiver appends to → filter → compose → deliver, under cron | one model key; the send is gated |
| [`posts-to-engagers/`](posts-to-engagers/) | one | people → their posts → who reacted → judge ⇒ group | HarvestAPI credits, when you record the fixtures live |

## Run one

From this checkout:

```sh
cd bundles/email-waterfall
gtme run . --simulate          # $0: the receipt in receipt.txt, from fixtures
```

Without a checkout — the release archive holds the same folders:

```sh
curl -fsSL https://github.com/gtme-run/gtme/archive/refs/heads/main.tar.gz \
  | tar xz --strip-components=2 gtme-main/bundles/email-waterfall
cd email-waterfall && gtme run . --simulate
```

`gtme run` accepts a bundle directory wherever it accepts a pipeline file.
It verifies every hash first, resolves the bundle's own bindings ahead of
whatever the machine has installed, and — under `--simulate` — serves them
from the fixtures inside the bundle. Each pattern's README says what its
receipt shows and which rung comes next (`plan`, `--dry-run`, armed), and
each bundle's `receipt.txt` is the simulated receipt, committed.

The chains (`qualify-group-send`, `account-shape`) hand records between
pipelines through **groups**, and membership lives in your ledger, not in
the bundle. A later bundle whose source is a group refuses to plan until
the earlier one has run armed and made that group. Its README walks the
order.

## What is in a bundle, and what is not

```
email-waterfall/
  manifest.json          # format version, source run id, sha256 per frozen file
  pipeline.yaml          # the exact config that ran — comments do not survive a freeze; the README carries them
  adapters/<id>/         # every binding the pipeline references, at its frozen version, fixtures included
  registry/              # the field vocabulary the contracts speak
  people.csv             # the input, beside the pipeline — yours to replace
  README.md              # the pattern: what it does, what the receipt shows, the next rung
  receipt.txt            # `gtme run . --simulate` from a clean checkout
```

The manifest lists the frozen files only. The input CSV, the README and
the receipt sit beside it, unlisted, so you can swap in your own rows
under the same file name and the bundle still verifies: **the pipeline is
frozen; the data is yours.** Edit `pipeline.yaml` and `gtme run` refuses
the bundle — that is the manifest doing its job. To change a pattern, copy
`pipeline.yaml` out and run the copy as a plain pipeline, or refreeze.

Built-in adapters (`csv/*`, `sql/*`, `ai/*`, `demo/enrich`, `group/deliver`,
the Go `instantly/add-to-campaign`) ship inside the binary and are not
packed; the bundle needs the same binary either way. Credentials never
travel: `gtme secret set KEY` on the machine that runs armed.

## Stand-in vendors and hand-written fixtures

Two patterns carry bindings for vendors gtme does not ship. The
waterfall's finders and verifier are **fictional** (`finder-a/email`,
`finder-b/email`, `verifier/email-status`, on reserved `.example` hosts):
the shape is the common one and the slot is the point — your finder is
your own binding, dropped in under the same role. The traverse pattern's
`harvest/profile-posts` and `harvest/post-reactions` are shaped from
HarvestAPI's docs, and their fixtures are **hand-written to that shape,
not recorded** — each `fixtures/conformance.json` says so in its `note`.
Record a real, sanitized response over them (`gtme adapters verify` runs
the fixtures) before trusting the shape live.

## Refreezing

`refreeze.py` rebuilds a bundle from its directory, offline: a throwaway
home and ledger, the pattern's bindings served from their own fixtures by
a local server, AI steps on the fixture engine, delivery held by
`--dry-run`, every other host unreachable. It freezes, lays the frozen
files over the directory (keeping the README, inputs and receipt), and
rewrites `receipt.txt` from a fresh simulate.

```sh
make build
bundles/refreeze.py bundles/email-waterfall
bundles/refreeze.py bundles/qualify-group-send/2-send --after bundles/qualify-group-send/1-qualify
```

`--after` runs the chain's earlier bundles armed first so their groups
exist. Refreeze when a binding, an input or a prompt changes, or when the
bundle format does; `make check` tells you which receipt went stale.
