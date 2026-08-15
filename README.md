# gtm

A CLI for GTM data pipelines. Unix-pipe ergonomics, an append-only SQLite ledger
as the data bus, schema contracts between steps, and AI transforms as first-class
steps.

Steps don't pass records to each other. They read a projection from the ledger
and write fields back — the logical record gets wider, the storage never does.
Every step declares `needs` and `provides` as JSON Schema, so a pipeline is
validated before it spends anything, and a re-run pays only for what it doesn't
already know.

## 60-second quickstart

No API keys required — this uses the CSV source and the bundled Python adapter.

```sh
make build && ./bin/gtm init          # creates ~/.gtm and the ledger

cat > people.csv <<'CSV'
email,full_name,company_domain,linkedin_url,title
jane.doe@acme.com,Jane Doe,acme.com,https://www.linkedin.com/in/jane-doe/,VP Marketing
bob@globex.io,Bob Stone,globex.io,linkedin.com/in/bob-stone,Head of Growth
CSV

cat > pipeline.yaml <<'YAML'
name: hello-gtm
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: score
    use: mock-enrich-py
    cache: 30d
YAML

export GTM_ADAPTER_PATH=$PWD/adapters   # where the bundled Python adapter lives

./bin/gtm plan pipeline.yaml            # validates contracts, spends nothing
./bin/gtm run  pipeline.yaml            # sources, enriches, writes the ledger
./bin/gtm run  pipeline.yaml            # again: 100% cache hits, $0 spent
./bin/gtm query "SELECT i.identity_key, fv.field, fv.value
                 FROM identities i JOIN field_values fv ON fv.identity_id = i.id"
./bin/gtm show bob@globex.io --provenance   # what the ledger knows about one record
./bin/gtm runs last                     # the receipt, rebuilt from the ledger
```

## Writing and running pipelines

`pipeline.yaml` is the only pipeline surface (`gtm run pipeline.yaml`) — see
[`examples/apollo-to-instantly.yaml`](examples/apollo-to-instantly.yaml) for a
full one, or run `gtm help --agent` for a machine-readable version of this
whole surface plus every installed adapter's contract, meant for an agent
authoring a pipeline without any other context.

```sh
gtm plan pipeline.yaml     # resolve, validate, print — no network, no spend
gtm run  pipeline.yaml
gtm run  pipeline.yaml --resume last    # finishes what a failed run started
gtm freeze last > frozen.yaml           # reconstructs the pipeline.yaml a run used
```

## Commands

| Command | What it does |
|---|---|
| `gtm init` | Create `~/.gtm` and the ledger |
| `gtm secret set KEY [VALUE]` | Store a credential in `~/.gtm/secrets` (0600, no echo) |
| `gtm plan pipeline.yaml` | Resolve adapters, check contracts and credentials, print the plan |
| `gtm run pipeline.yaml [--resume RUN_ID\|last]` | Execute; resume picks up where a failure stopped |
| `gtm query "SQL" [--save NAME] [--name NAME] [--list]` | Read-only SQL and saved segments |
| `gtm show <identity-key>\|--run RUN_ID\|last [--fields][--provenance]` | Inspect what the ledger knows |
| `gtm runs [RUN_ID\|last]` | List runs, or print one run's receipt |
| `gtm freeze [RUN_ID\|last]` | Reconstruct the `pipeline.yaml` that produced a run |
| `gtm help --agent` | Machine-readable CLI + adapter surface |

`stdout` is data (NDJSON/JSON: query rows, `gtm show`, `gtm help --agent`,
`gtm freeze`'s YAML); everything human-facing goes to `stderr`. Exit codes:
`0` ok, `2` validation/contract, `3` auth/credential, `4` rate-limited,
`5` network, `1` other.

## What makes a re-run cheap

- **Identity.** A person is one identity however they arrive — email, LinkedIn
  URL, or name + company domain. A stronger key upgrades the identity in place,
  and the old key keeps resolving to it.
- **Cache.** An enrich step skips a record when the ledger already holds fresh
  answers from that adapter, inside the step's window (`cache: 30d`, or the
  adapter's `freshness_days`). The receipt shows what that saved.
- **Idempotency.** A delivery is keyed (by default on the identity key,
  or on `idempotency: email`) so running twice cannot mail anyone twice.
- **Append-only.** Nothing is overwritten. The current value of a field is the
  highest-confidence value inside its freshness window; everything else stays as
  history, with the adapter and run that produced it.

## Adapters

Built in: `csv/source`, `apollo/search`, `ai/filter`, `ai/compose`,
`harvest/profile`, `instantly/add-to-campaign`.

External adapters are any executable that speaks the protocol. Drop a directory
containing `manifest.json` and an executable named `run` into
`~/.gtm/adapters/<name>/` (or anywhere on `GTM_ADAPTER_PATH`).
[`adapters/mock-enrich-py/`](adapters/mock-enrich-py/) is a ~90-line Python
example; the runner talks to it exactly as it talks to the built-ins.

```
runner → adapter   OPEN, RECORD…, END
adapter → runner   SCHEMA, RECORD, VERDICT, COST, STATE, LOG, END
```

`fields` on an inbound RECORD is exactly the projection of the adapter's `needs`
— nothing more. Output is validated against `provides` before it reaches the
ledger.

## AI steps

`ai/filter` returns a keep/drop verdict per record; `ai/compose` writes fields.
Both batch (`batch_size`, default 25 — one model call per batch), validate the
answer against a strict output contract, and retry once with the validation error
appended before failing the batch. The engine is the Anthropic API by default, or
the `claude` CLI with `engine: claude-code`. Token usage becomes COST rows, so
the receipt and `gtm runs` show what a run actually cost.

## Live providers

Apollo, HarvestAPI, Instantly and Anthropic are exercised offline against
fixtures in `make check`. Real calls are a deliberate manual gate:

```sh
make live           # read-only/small calls; each test skips without its key
make live-deliver   # adds ONE real lead to a real campaign (opt-in)
```

## Development

```sh
make check    # gofmt, go vet, unit + e2e tests (all offline)
make build    # ./bin/gtm
```

`SPEC.md` is the design; `DECISIONS.md` records every choice made where the spec
was silent, and why.
