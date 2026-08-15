# spec/ — the machine-checkable half of SPEC.md

SPEC.md is the single source of truth for observable behavior (ADR-010). This
directory holds the parts of it a machine can check, extracted verbatim, and
the Go test suite loads these files **directly** rather than re-encoding their
shapes in Go. When the code and the spec disagree, a test fails and names the
artifact.

| path                      | what it fixes                                                     | SPEC.md      |
|---------------------------|-------------------------------------------------------------------|--------------|
| `ledger.sql`              | The canonical ledger DDL, including the `current_fields` view      | §3, ADR-003  |
| `schemas/msg-*.json`      | Every wire-protocol message type, per direction                    | §5           |
| `schemas/manifest.schema.json` | An adapter's `manifest.json`                                  | §6           |
| `schemas/pipeline.schema.json` | `pipeline.yaml`, including `uses:`                            | §9, ADR-004  |
| `wire/*.ndjson`           | Golden transcripts recorded from the real adapters                 | §5, §11 M2   |
| `acceptance/*.yaml`       | The eight operator stories as structured given/when/then           | ADR-012      |

The tests that consume them live in `test/conformance/`.

## Editing rules

- **`ledger.sql` and `acceptance/*.yaml` are transcriptions.** They are copied
  from SPEC.md and must stay identical to it. Changing behavior means amending
  SPEC.md first (with human approval, per ADR-010), then re-transcribing.
- **The JSON Schemas describe real serialization**, derived from
  `internal/protocol.Message`, `internal/adapters.Manifest` and
  `internal/pipeline.Pipeline` — not from SPEC.md's prose examples, which are
  abbreviated. Where a Go struct tag makes the wire differ from the prose (a
  zero `amount_usd` dropped by `omitempty`, a source's keyless RECORD), the
  schema follows the wire and says so in a `description`.
- **`wire/*.ndjson` is recorded, not authored.** See `wire/README.md` for how
  `basic-run.ndjson` was captured and how to regenerate it.

## Standalone use

`ledger.sql` runs against a plain SQLite:

```sh
sqlite3 /tmp/canonical.db < spec/ledger.sql
```

The schemas are draft-07 and validate with any conforming validator; gtm uses
`santhosh-tekuri/jsonschema/v5`.
