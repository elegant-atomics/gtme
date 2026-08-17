# spec/wire — golden wire transcripts

Recorded exchanges between the runner and adapters, in the NDJSON format of
SPEC.md §5. They are the machine-checkable counterpart of that section's prose
examples (ADR-010): the Go test suite loads them and validates every message
against `spec/schemas/`, so a schema that drifts from what gtme actually puts on
the wire fails a test instead of quietly rotting.

## `basic-run.ndjson`

A two-step run: `csv/source` → `mock-enrich-py`. It was **recorded from the
real adapters**, not written by hand — the `csv/source` half by driving the
built-in through `adapters.Resolve`/`Session`, the `mock-enrich-py` half by
piping the runner's messages into `adapters/mock-enrich-py/run` and capturing
its stdout verbatim. Regenerating it means re-running those adapters, not
editing the file.

Source data: `internal/adapters/csvsource/fixtures/people.csv` (three people).

### Format

One JSON object per line. The wire line itself is the `msg` value; the three
sibling keys say where it came from, which is what lets a test pick the right
schema:

| key       | meaning                                                      |
|-----------|--------------------------------------------------------------|
| `stream`  | which step's session: `source` or `enrich`                    |
| `dir`     | `runner->adapter` or `adapter->runner`                        |
| `adapter` | the adapter id on the other end                               |
| `msg`     | the literal NDJSON line that crossed the pipe                 |

To replay the runner's half at an adapter, extract `msg` from every line whose
`dir` is `runner->adapter` for one `stream` and feed it to the adapter's stdin.

### Which schema each line must validate against

Selected by `msg.type` and `dir`:

| `dir`             | `msg.type` | schema                                  |
|-------------------|------------|-----------------------------------------|
| `runner->adapter` | `OPEN`     | `spec/schemas/msg-open.schema.json`      |
| `runner->adapter` | `RECORD`   | `spec/schemas/msg-record-in.schema.json` |
| `runner->adapter` | `END`      | `spec/schemas/msg-end.schema.json`       |
| `adapter->runner` | `SCHEMA`   | `spec/schemas/msg-schema.schema.json`    |
| `adapter->runner` | `RECORD`   | `spec/schemas/msg-record-out.schema.json`|
| `adapter->runner` | `VERDICT`  | `spec/schemas/msg-verdict.schema.json`   |
| `adapter->runner` | `COST`     | `spec/schemas/msg-cost.schema.json`      |
| `adapter->runner` | `STATE`    | `spec/schemas/msg-state.schema.json`     |
| `adapter->runner` | `LOG`      | `spec/schemas/msg-log.schema.json`       |
| `adapter->runner` | `END`      | `spec/schemas/msg-end.schema.json`       |

### What it demonstrates

- **OPEN carries the resolved step config.** `config` is the step's `with:`
  block, already validated against the adapter's `config_schema`.
- **A source receives no RECORDs.** Its input is `OPEN` then `END` — "there is
  no input coming" (SPEC.md §5).
- **SCHEMA is the adapter's first message**, in both a built-in and an external
  adapter.
- **A source's RECORDs have no `key`.** `csv/source` emits raw CSV fields; the
  runner canonicalizes them into an identity key (SPEC.md §4). Every other
  role's RECORDs carry `key`, in both directions. This is why
  `msg-record-out.schema.json` makes `key` optional and
  `msg-record-in.schema.json` requires it.
- **Projection is narrow.** The runner hands `mock-enrich-py` only `email` and
  `full_name` — the properties of its manifest `needs` — not the five fields
  the CSV produced (SPEC.md §7).
- **Identity canonicalization happens in the runner.** `Jane.Doe@Acme.com`
  becomes the key `jane.doe@acme.com` while the `email` field keeps its
  original casing.
- **COST is emitted per record even when it is zero**, exercising the cost path
  end to end. Note that this transcript's COST lines come from a Python adapter,
  which writes `"amount_usd": 0` explicitly; the Go `protocol.Message` struct
  tags `AmountUSD` `omitempty`, so a Go adapter emitting a zero cost omits the
  key entirely. Both are valid — see `msg-cost.schema.json`.
