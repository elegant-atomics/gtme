# gtme — GTM as code

**Built for engineers who do GTM — not the other way around.**

Outbound tooling assumes you want a UI, credits, and someone else's opinion
of your workflow. If you'd rather have a CLI, a ledger you can query, YAML
you can diff, and receipts for every dollar — this is that. Campaigns are
pipelines. Adapters are data. Judgment is versioned. Everything replays.

```yaml
name: q3-outbound
source:
  use: apollo/search              # a vendor adapter that is ~150 lines of YAML
  with: { query: "vp marketing, saas", limit: 200 }
steps:
  - id: fit
    use: ai/filter                # AI judgment behind the same contract as any step
    uses: [full_name, title, company_domain]
    with: { prompt: Keep people who own outbound tooling decisions. }
  - id: lines
    use: ai/compose
    when: fit.passed
    uses: [full_name, title, company_name]
    with: { prompt: Write first_line and ps_line for a short, honest intro. }
  - id: send                      # delivery is a step like any other — put it anywhere, use several
    use: instantly/add-to-campaign
    with: { campaign: "Q3 VP Marketing" }
    variables: { first_line: first_line, ps_line: ps_line }
    idempotency: email            # re-runs deliver nothing twice, ever
```

```
$ gtme run q3-outbound.yaml
...
step     adapter                   in   out  cached  cost     avoided
source   apollo/search             0    200  0       $0       -
fit      ai/filter                 200  74   0       $0.19    -
lines    ai/compose                74   74   0       $0.31    -
send     instantly/add-to-campaign 74   74   0       $0       -
total: $0.50 spent
```

Run it again Monday with fresh data: overlapping records cache-skip, the
receipt shows dollars *avoided*, and nobody gets delivered twice.

## What it is

- **A single static binary** (`gtme`) that executes campaign pipelines
  defined in YAML. No daemon, no hosted anything, no login.
- **An append-only SQLite ledger as the data bus.** Steps never pass
  records to each other — they read a projection of what's known and write
  facts back, with provenance, confidence, and freshness on every value.
  What your pipelines learn accumulates; what a run did is receipted.
- **Contracts everywhere.** Every step declares `needs` and `provides` as
  JSON Schema against a canonical field registry, so `gtme plan` proves a
  pipeline coherent — end to end — before a single record moves or a cent
  is spent. AI steps live behind the same contract as everything else.
- **Adapters as data.** Most vendor APIs are CRUD over HTTP, so most
  adapters here are **bindings**: declarative YAML interpreted by one
  generic engine — auth, pagination, extraction, error mapping,
  idempotency, cost, all frozen at authoring time. The moment an
  integration needs real logic, it graduates to a process adapter speaking
  NDJSON over stdin/stdout, in any language.

## Use cases

- **Outbound campaigns** — source → enrich → AI-judge → AI-compose →
  deliver, with the send gated behind a human-reviewable receipt.
- **Enrichment that stops re-billing you** — freshness-windowed caching
  means top-ups and re-runs pay only for what the ledger doesn't already
  know; the receipt prints what the cache saved.
- **Qualify/send decomposition** — a cheap qualify pipeline fills a
  *group* (a durable, hand-editable set); a separate send pipeline
  consumes it, records touches, and suppresses re-contacts by window.
  AI verdicts become recorded decisions: an identity is judged once per
  scope, not re-rolled on every run.
- **Research enrichment** — `http/enrich` fetches a page to markdown as a
  ledger fact (with mandatory freshness — web content rots), and AI steps
  judge the stored content. Fetch once, judge many.
- **Deterministic transforms in SQL** — `sql/transform` and `sql/filter` run
  declared, read-only queries over the ledger: derive fields, bucket
  titles, gate on "3+ known contacts at this company" — no AI spend for
  computable questions.
- **The universal floor** — CSV in, webhooks in, groups in; any-URL POST
  out, CSV out. Anything with an export or an import button is wireable
  today, even before a proper adapter exists.
- **Portable campaigns** — `gtme freeze --bundle` snapshots a run into a
  self-contained folder (pipeline + bindings + fixtures + hash manifest)
  that runs, simulates, and diffs anywhere.

## Architecture

```
                  pipeline.yaml  (the only authoring surface)
                        │
                    gtme plan ──── contracts, credentials, cost — $0
                        │
   ┌────────────────────┼─────────────────────────────┐
   │ runner             ▼                             │
   │   ┌─────────┐  ┌────────┐  ┌─────────┐  ┌─────────┐
   │   │ source  │→ │ enrich │→ │ ai/sql  │→ │ deliver │   steps see only
   │   └────┬────┘  └───┬────┘  └────┬────┘  └────┬────┘   projections,
   │        │  ▲        │  ▲         │  ▲         │  ▲      never the ledger
   ├────────┼──┼────────┼──┼─────────┼──┼─────────┼──┼──┐
   │        ▼  │        ▼  │         ▼  │         ▼  │  │
   │   ledger (append-only SQLite)                      │
   │     identities · field_values (facts + provenance) │
   │     runs · step_events · costs · deliveries        │
   │     groups · group_events (decisions about sets)   │
   │     payloads (raw responses — cache, purgeable)    │
   └────────────────────────────────────────────────────┘
```

The load-bearing choices:

- **The ledger is the bus.** Cache-aware waterfalls, resume after a crash,
  cost attribution, and "what do we know about this person" as a SQL query
  all fall out of one structural decision: facts live in the ledger, not
  in a stream between steps.
- **Two adapter tiers.** Bindings (YAML, interpreted, cannot execute code)
  for anything that sells an API; NDJSON processes for anything that must
  be fought for. Both speak the same wire protocol and pass the same
  conformance kit: golden vendor payloads in → canonical records out.
- **A closed grammar.** The pipeline surface is a small, enumerable set of
  keys — no expression language, no DSL to hallucinate. Expressivity comes
  from composing a small vocabulary against a canonical field registry.
- **The gate ladder:** `simulate → plan → dry-run → armed`. Simulation
  executes the *whole pipeline* from conformance fixtures — no network, no
  keys, deterministic. Plan proves contracts. Dry-run does everything but
  deliver, rendering exactly what *would* send. Arming is the same command
  without the flag.
- **Agent-operable by design.** `gtme help --agent` emits the full CLI and
  adapter surface as one machine-readable document, and `gtme help
  --bindings` the contract for authoring an adapter it does not ship;
  every error names its fix; every question about state has a deterministic answer path. An
  agent can author a pipeline and validate its structure *and behavior*
  before a human reviews one artifact and arms it.

## Why engineers who do GTM

**Speed to deploy.** A new vendor integration is a YAML file — the Apollo
source adapter shipped here is ~150 lines of declarative binding
([see it](spec/bindings/apollo-search/binding.yaml)), and the authoring
loop is generate → conformance-test → done, which is exactly the loop a
coding agent closes well. APIs that publish OpenAPI are bind-time targets,
not build projects. The universal floor means day one is never blocked on
an adapter existing.

**Rapid iteration.** Simulation makes behavior checkable offline before
credentials exist. The cache makes iteration cheap: change a prompt and
re-run — enrichments skip, only judgment re-spends. Filter verdicts and
their *reasons* land in the ledger, so tuning a prompt is a SQL query over
what the model actually decided, not vibes. Freeze any run back into the
exact YAML that produced it.

**Safety.** Delivery is the one destructive edge and it is gated three
ways: the dry-run receipt is a per-record approval artifact a human reads
before arming; idempotency keys make re-runs structurally unable to
double-send; suppression windows enforce contact policy across pipelines.
Everything upstream is replayable because the ledger is append-only.
Blank merge fields never send. Groups freeze AI judgment into recorded,
inspectable decisions instead of per-run dice rolls.

**Cost.** Every step's spend lands in the receipt, attributed per record
and per model. The cache prints what it *saved* you, not just what you
spent. Fetch-once economics: N AI steps across M runs reuse one paid
enrichment while it's fresh. Raw vendor responses are retained (bounded,
TTL'd, evictable) so improving an extraction back-fills from payloads
you already paid for — at zero vendor spend.

## Install

Prebuilt binaries for macOS and Linux are on the
[releases page](https://github.com/elegant-atomics/gtme/releases) — verify
against `checksums.txt`, untar, put `gtme` on your PATH, `gtme init`.
Building from source is `git clone` + `./install.sh` (Go 1.24+), which
also installs the repo's example external adapters so the README
quickstart works offline.

## The adapters

What ships today — see **[ADAPTERS.md](ADAPTERS.md)** for each one's
config and behavior, or `gtme help --agent` for the machine-generated,
always-current surface:

| | |
|---|---|
| **In** | `csv/source` · `webhook/source` · group-as-source · `apollo/search` *(binding)* |
| **Enrich** | `harvest/profile` · `http/enrich` (any URL → markdown or JSON) · `sql/transform` |
| **Judge** | `ai/filter` · `sql/filter` · `ai/review` |
| **Write** | `ai/compose` |
| **Ask** | `human/filter` · `human/compose` · `human/review` · `agent/*` (the same three, answered by an agent) |
| **Out** | `instantly/add-to-campaign` · `attio/assert` *(binding)* · `http/deliver` (any URL) · `csv/deliver` · `group/deliver` (the next stage) |

Adding your own doesn't require touching this repo: drop a `binding.yaml`
into `~/.gtme/adapters/<name>/` and the id resolves immediately.

### Steps a person answers

A `human/*` step puts judgment in the pipeline without pretending a model
made it. At a terminal `gtme run` walks the records and asks; anywhere
else — cron, CI, a pipe — every record waits in the ledger, and `gtme
answer` records the verdict later. Either way the answer lands as a fact
with the person's name in its provenance, and the run collects it exactly
as it collects a deferred batch.

```bash
gtme run review.yaml                      # ends "pending — 12 awaiting human/review"
gtme show --run last --pending            # read what is waiting
gtme answer last jane@acme.com --set grade=B --note "too generic"
gtme run review.yaml                      # collects the answers, continues
```

`agent/*` is the same three steps for an agent that answers with its own
judgment rather than a person's; it never prompts, and `--as` puts its
name in the ledger.

Five things worth knowing before you put one in a pipeline:

- **Under cron, a pipeline with a participant step waits.** A pending run
  is resumed, not re-sourced, so nothing new is picked up until someone
  answers. The pattern that avoids it is two pipelines: the reviewing one
  a person runs, ending in a `group:`, and the scheduled one sourcing from
  that group. `gtme plan` prints a note when a deliver step follows a
  person.
- **Ctrl-C mid-walk is safe.** The records you answered are settled, the
  rest stay pending, and the run ends pending rather than failed.
- **The latest answer before collection wins.** Answers are ledger state,
  idempotent per record; answering again replaces the earlier one.
- **An unchanged value is not asked twice.** A person's judgment is cached
  like a model's: rewrite the draft and it comes back, leave it alone and
  it does not. `cache: 0d` or `respend: true` ask again on purpose.
- **A review labels; it never gates.** `when: <review>.passed` fails plan.
  Use a filter when the answer should stop a record.

## A campaign is a folder

Everything that defines a campaign is text, so a campaign works the way
the rest of your engineering does: a directory under version control.

```
q3-outbound/
  qualify.yaml          # source → enrich → ai/filter ⇒ group (cheap, run often)
  send.yaml             # group → compose → deliver (deliberate, gated)
  contacts.csv          # input data, when the source is a file
```

Pipelines diff in code review. Prompt changes are commits. A colleague's
campaign is a `git pull`. And when a run is worth keeping,
`gtme freeze --bundle` snapshots it into a self-contained artifact:

```
bundle/
  manifest.json         # format version, source run id, sha256 per file
  pipeline.yaml         # the exact config that ran
  adapters/             # every referenced binding, at its frozen version,
    apollo-search/      #   with its fixtures — so the bundle simulates offline
  registry/             # the field vocabulary the contracts speak
```

`gtme run <bundle>` accepts the folder anywhere a pipeline path goes,
verifies every hash first, and resolves the bundle's own bindings ahead of
whatever the machine has installed. Contracts travel; your ledger,
credentials, and input data stay yours. The natural unit for a playbook, a
handoff, or a per-client repo.

## Get started

```sh
git clone https://github.com/elegant-atomics/gtme && cd gtme
./install.sh        # builds gtme, installs it to ~/.local/bin, runs gtme init
```

`install.sh` is deliberately boring: it compiles from the checkout and
copies one static binary into place — no curl-pipe-to-shell, no network
fetch. It warns you if `~/.local/bin` isn't on your PATH (add it to your
shell profile, or run `PREFIX=/usr/local ./install.sh` instead). Prefer
not to install? `make build` gives you `./bin/gtme` and every command
below works with that path.

**The zero-key demo** — run a whole campaign, offline, right now:

```sh
gtme run examples/demo.yaml --simulate
```

The Apollo source serves its conformance fixtures, the AI steps answer
synthetically (and are *marked* synthetic in provenance), delivery is held
with its variables resolved into the receipt — and one record visibly
fails the delivery floor, because its email is Apollo's locked-email
placeholder and gtme refuses to key an identity on garbage. Receipts are
honest here; that's the point.

**Then with real keys**, the same file climbs the ladder:

```sh
gtme secret set APOLLO_API_KEY        # prompts, no echo
gtme secret set ANTHROPIC_API_KEY
gtme plan examples/demo.yaml          # contracts + cost, still $0
gtme run  examples/demo.yaml --dry-run  # everything but delivery
gtme run  examples/demo.yaml          # armed
gtme run  examples/demo.yaml          # again: cache skips, zero re-delivery
```

Then interrogate what happened — no log spelunking:

```sh
gtme show jane.doe@acme.com --provenance   # every fact, who wrote it, when
gtme runs last                             # the receipt, reconstructed
gtme query "SELECT field, value FROM current_fields ..."
gtme groups                                # decisions about sets, with tallies
```

## Further exploration

- **[SPEC.md](SPEC.md)** — the canon. Every observable behavior, DECIDED
  sections, acceptance criteria. The binary is built *from* this document;
  code that diverges from it is a bug even when it works.
- **[DECISIONS.md](DECISIONS.md)** — the why: the ADR log, including the
  autopsy of why Singer died twice and how the field registry and binding
  tier answer both causes.
- **[VALIDATION.md](VALIDATION.md)** — the receipts: real campaigns, real
  API drift absorbed, real dollar amounts, findings logged honestly.
- **[ADAPTERS.md](ADAPTERS.md)** — every shipped adapter, what it does,
  and the config that matters.
- **[spec/bindings/](spec/bindings/)** — vendor adapters as reviewable,
  diffable YAML, with the conformance fixtures that double as simulation.
- **[PROCESS.md](PROCESS.md)** — how design conversations become spec, and
  why nothing is decided until it's in the repo.
- **[ROADMAP.md](ROADMAP.md)** — named, deliberately deferred: the expand
  role, payload re-extraction, an OpenAPI→binding codegen skill, and more.
- **[CONTRIBUTING.md](CONTRIBUTING.md)** — the contribution surface is
  adapters, and most adapters are one reviewable YAML file; here's the
  shape and the checklist.
- `gtme help --agent` — the whole surface, machine-readable, for your
  agent; `gtme help --bindings` — the binding contract (schema, discovery
  path, a reference binding) when it needs an adapter that is not here.

## License

[Apache-2.0](LICENSE)
