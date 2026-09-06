# Start here

**For humans:** this page installs `gtme`, a single binary, and walks
one of four doors to a receipt — a table of what a campaign pipeline did,
what it cost, and what it would have sent. Nothing is sent and nothing is
spent until a door says so, in plain words, right before the command.

**For agents:** the paste line is *"Follow gtme.run/start.md"*. This file
is the whole instruction set; `gtme help --agent` is the machine-readable
surface when a step needs more. Read the rules at the bottom before the
first command. (Until the site is up, the same file is
`https://raw.githubusercontent.com/elegant-atomics/gtme/main/START.md`.)

## Install

macOS or Linux, arm64 or amd64. Pick one:

```sh
brew install elegant-atomics/tap/gtme     # a prebuilt, checksummed binary
```

```sh
# or: the release tarball — verify against checksums.txt, untar, put gtme on your PATH
# https://github.com/elegant-atomics/gtme/releases/latest
```

```sh
# or: from source (Go 1.24+); also installs the repo's example adapters
git clone https://github.com/elegant-atomics/gtme && cd gtme && ./install.sh
```

Then:

```sh
gtme version      # prints the version
gtme init         # creates ~/.gtme and the ledger; safe to repeat
```

Nothing here pipes a download into a shell, and nothing phones home.

## The four doors

| Door | Needs | Spends | Ends with |
|---|---|---|---|
| **1. Show me** | nothing | $0 | a receipt from fixtures, twice |
| **2. My CSV** | one model key | cents, on the model | your rows judged and written, then the cache receipt |
| **3. My stack** | vendor keys | vendor credits, gated | a dry-run receipt a human reads, then one armed run |
| **4. Add a vendor** | nothing | $0 | a new adapter that verifies and simulates |

Each door is one pipeline file you fetch, and every command below is
safe to re-run.

### Door 1 — Show me (no keys)

What happens: a whole outbound pipeline — vendor search, AI filter,
paid reveal, AI compose, CRM delivery — runs **offline**. The vendor
adapters serve their recorded fixtures, the AI steps answer synthetically
and say so in provenance, delivery is held with its merge variables
resolved into the receipt. No network, no keys, no spend, nothing
persisted.

```sh
mkdir -p gtme-start && cd gtme-start
curl -fsSLO https://raw.githubusercontent.com/elegant-atomics/gtme/main/examples/demo.yaml
gtme run demo.yaml --simulate
gtme run demo.yaml --simulate
```

The first receipt is the door's proof: a step table with `in`, `out`,
`cached`, `cost` and `avoided` columns, a `SIMULATED` banner, one
estimated charge on the reveal step, and the two held records with their
variables rendered. The two records both read "Jane Doe" because the
reveal fixture answers every lookup with the same sanitized person —
fixtures are canned responses, and the receipt says so.

The second receipt is identical to the first, on purpose: a simulated run
executes against a throwaway copy of the ledger and persists nothing, so
it can be repeated forever without side effects. The cache — the
`cached` and `avoided` columns filling in — appears the first time a run
persists, which is door 2.

Done when: two receipts printed, both marked `SIMULATED`, exit code 0.

### Door 2 — My CSV (one model key)

What happens: your CSV of people is read, an AI filter keeps the ones
that fit a prompt you wrote, an AI compose writes two intro lines for
each, and the result is written to a CSV beside the input. Nothing
leaves the machine except the model calls. The second run re-judges
nobody: the receipt shows what the cache saved and delivers nothing
twice.

You need a CSV with a header row, and an Anthropic API key. The human
enters the key; it is stored in `~/.gtme/secrets`, never in a pipeline
file, never in a shell history line.

```sh
gtme secret set ANTHROPIC_API_KEY     # prompts, no echo — the human types it
curl -fsSLO https://raw.githubusercontent.com/elegant-atomics/gtme/main/examples/my-csv.yaml
```

Edit `my-csv.yaml`: set `path:` to the CSV, and under `columns:` map
the canonical names (`full_name`, `email`, `title`, `company_domain`) to
your header names. Headers that already match auto-map; unmapped
headers are kept as `csv.<header>`. Then rewrite the two prompts for
your campaign.

```sh
gtme plan my-csv.yaml              # $0: checks the mapping and the contracts
gtme run  my-csv.yaml --simulate   # $0: the shape of the output, from fixtures
gtme run  my-csv.yaml              # spends on the model; writes out.csv
gtme run  my-csv.yaml              # again: cached, nothing delivered twice
```

`plan` names a header it cannot find and lists the ones it saw; fix
`columns:` and plan again. Start with a slice — the first twenty rows in
a second file — before the whole list; a run scoped small exercises the
whole chain at minimal cost.

Done when: `out.csv` holds one row per kept record with `first_line` and
`ps_line`, and the second receipt shows `cached` above zero on both AI
steps, a dollar amount in `avoided`, and `0` out on the deliver step.

### Door 3 — My stack (vendor keys)

What happens: the same pipeline as door 1, live — Apollo searches,
the filter judges, Apollo reveals only past the filter, the compose
writes, and an Instantly campaign receives. Every rung of the ladder
before the last spends nothing on delivery; the last is armed by a human.

The Instantly campaign named in `demo.yaml` (`with: { campaign: ... }`)
must exist; edit the name to one of yours. The dry run reads it and
reports whether it is fit to send to — active, with a sequence that
references every variable the step sends — before a single record moves.

```sh
gtme secret set APOLLO_API_KEY
gtme secret set ANTHROPIC_API_KEY
gtme secret set INSTANTLY_API_KEY
gtme plan demo.yaml                 # $0: contracts, credentials, cost estimate
gtme run  demo.yaml --dry-run       # spends on search, reveal and the model; delivers nothing
```

**Stop here.** The dry-run receipt lists every record that would be
delivered, with its variables resolved. A human reads it. Only a human
runs the next line, and only after saying so:

```sh
gtme run demo.yaml                  # armed: delivers; re-runs deliver nothing twice
gtme run demo.yaml                  # again: the cache receipt, zero re-delivery
```

`examples/apollo-to-instantly.yaml` is the same shape at campaign size,
with a LinkedIn enrichment in the middle; its header says which four
keys it wants.

Done when: a dry-run receipt was read by a human, one armed run
delivered, and the run after it shows `avoided` on the paid steps and
`0` out on delivery.

### Door 4 — Add a vendor (no keys)

What happens: you write an adapter for an API gtme does not ship — as
one YAML file, no code — verify it offline against a recorded response,
and simulate a pipeline through it. Most vendor APIs are CRUD over HTTP,
and for those this is the whole job.

```sh
gtme help --bindings > bindings.json     # the contract: schema, discovery path, a reference binding
mkdir -p ~/.gtme/adapters/<vendor>-<operation>
```

Write `~/.gtme/adapters/<vendor>-<operation>/binding.yaml` against the
schema, modelled on the reference — its `id` is `<vendor>/<operation>`.
Record one real, sanitized response per request the binding makes into
`fixtures/conformance.json` beside it. Then:

```sh
gtme adapters verify <vendor>/<operation>   # schema + fixtures, offline; prints the hosts and credentials it would use
gtme run my-pipeline.yaml --simulate        # a pipeline that says `use: <vendor>/<operation>`, served from the fixtures
```

The moment the integration needs conditionals, multi-call workflows, an
OAuth dance or computation, it is not a binding: `gtme help --agent`
documents the process-adapter protocol, and `CONTRIBUTING.md` in the
repo has the checklist for sharing either kind.

Done when: `verify` passes and a simulated receipt shows records coming
out of the new adapter.

## Rules for the agent

- **Never arm.** A command without `--simulate` or `--dry-run` on a
  pipeline whose deliver step reaches a live target (door 3) is run by
  the human, after reading the dry-run receipt. Door 2's target is a
  local CSV; its armed run spends on the model only, and the human has
  read the line above it that says so.
- **Never handle a key.** `gtme secret set KEY` prompts the human; do
  not paste a key on the command line, into a file, or into a chat.
- **Spend is announced before it happens.** Every command above says
  what it spends. `gtme plan` is always $0 and always the first move on
  a pipeline you edited.
- **Errors name their fix.** Read the message, do the named thing, run
  the same command again. `gtme help --agent` is the reference for
  anything the message does not settle.
- **Stop at the door's "done when."** Report the receipt to the human;
  the next door is theirs to open.

## Then

```sh
gtme show <email> --provenance     # every fact, who wrote it, when
gtme runs last                     # the receipt, reconstructed
gtme query "SELECT field, value FROM current_fields WHERE ..."
gtme freeze last --bundle DIR      # the run as a self-contained, portable folder
```

The README is the tour; `SPEC.md` is the canon; `ADAPTERS.md` lists what
ships. A campaign is a folder under version control — pipelines diff,
prompts are commits, and a colleague's campaign is a `git pull`.
