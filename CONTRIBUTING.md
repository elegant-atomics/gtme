# Contributing to gtme

The contribution surface of this project is **adapters**. The runner is
deliberately small and slow-moving; the adapter catalog is where gtme
grows, and it's designed so that contributing one is a small, reviewable,
mostly-declarative change. This document is organized around that.

Ground rule for everything else: **SPEC.md is canon.** The binary is built
from it; code that diverges from a DECIDED section is a bug even when it
works. If your change is observable — CLI surface, wire protocol, schemas,
ledger DDL — read [PROCESS.md](PROCESS.md) first: spec-visible changes are
proposed as spec diffs and decided before they're built, and the reasoning
lands in [DECISIONS.md](DECISIONS.md).

## Contributing a binding (the main path)

Most vendor APIs are CRUD over HTTP, and for those an adapter is a
**binding**: one YAML document, interpreted by the generic engine, that
cannot execute code — which is exactly what makes a third-party
contribution reviewable by reading it.

**1. Check the shape first.** A binding gets you: one auth scheme, a
templated request, pagination (page/cursor/offset), dotted-path extraction
with registry-rule transforms, error→verdict mapping, idempotency
declaration, and cost. The moment your integration needs conditionals,
multi-call workflows, OAuth dances, request signing, or computation, it is
not a binding — see "Process adapters" below. This is a hard rule (the
graduation rule, SPEC §10a): no expression language will ever grow inside
binding YAML, so don't ask for one — graduate instead.

**2. Author it.** Copy the shape of a shipped binding —
[`spec/bindings/apollo-search/`](spec/bindings/apollo-search/) is the
fullest example (auth, body templating, pagination, sentinel values,
LinkedIn shape-routing) — and validate against
[`spec/binding-schema.json`](spec/binding-schema.json). `gtme help
--bindings` prints that schema, the discovery path and the smallest
shipped binding as one document — the same contract, for an agent
authoring one. Layout:

```
spec/bindings/<vendor>-<operation>/
  binding.yaml
  fixtures/conformance.json
```

**3. Use canonical field names.** Extraction targets must be canonical
fields (`spec/fields/*.json`) or vendor-namespaced
(`<vendor>.<field>`). When in doubt, namespace — promotion to canonical is
cheap (it happens when a second adapter provides the same fact; one ADR
line), demotion is breaking. Values must leave your binding normalized;
that's what the `transform:` rules are for, and the conformance kit checks
it.

**4. Ship fixtures.** `fixtures/conformance.json` holds real (sanitized!)
API responses:

```json
{
  "responses": [
    { "match": "GET /v2/things", "status": 200, "body": { "...": "a real response" } }
  ]
}
```

Fixtures do double duty: they are the adapter's conformance test *and*
what `--simulate` serves, so keeping them faithful is what keeps offline
validation honest. **Sanitize them** — no live keys, no real prospect PII;
use example.com-style identities.

**5. Prove it.** The conformance kit picks up everything under
`spec/bindings/` automatically:

```sh
make check                                  # includes binding conformance
gtme run your-pipeline.yaml --simulate      # your fixtures, end to end
```

Add expectations for your binding to `test/conformance/binding_test.go`
(records out, costs, the tricky mappings), mirroring the existing cases.

**PR checklist for a binding:**

- [ ] Conforms to `spec/binding-schema.json`; `make check` green
- [ ] Extraction targets are canonical or vendor-namespaced
- [ ] Fixtures are real-shaped, sanitized, and cover the tricky cases
      (empty pages, sentinel values, the pagination terminator)
- [ ] Conformance expectations added
- [ ] Header comment says what the API is, auth env var, and docs URL
- [ ] Cost declaration is honest (`per: record|request`, or absent)

You don't need a PR to *use* a binding, by the way — drop
`binding.yaml` (+ fixtures) into `~/.gtme/adapters/<name>/` and it
resolves like any built-in. PRs are for bindings worth sharing.

## Process adapters (tier 2)

When the graduation rule fires, an adapter is an executable speaking
NDJSON on stdin/stdout — any language. The protocol is SPEC §5 (schemas in
`spec/schemas/msg-*.json`, golden transcripts in `spec/wire/`);
[`adapters/mock-enrich-py/`](adapters/mock-enrich-py/) is a complete
working example in ~40 lines of Python. Ship a `manifest.json` (SPEC §6)
beside the executable, keep the HTTP surface in one clearly-marked file so
field mappings are trivially correctable, put every HTTP call behind a
stubbable seam, and ship fixtures — the same conformance bar as bindings.

## Everything that isn't an adapter

- **Bug in behavior?** Check SPEC.md first. If the code disagrees with the
  spec, that's the bug — say so in the issue. If the *spec* seems wrong or
  internally inconsistent, that's a spec discussion, not a quiet patch.
- **New capability?** Open an issue proposing the ADR before writing code.
  [ROADMAP.md](ROADMAP.md) lists what's already been considered and
  deliberately deferred — read it first; several "missing features" are
  parked on purpose, with reasoning.
- **Dependencies:** the allowed list is SPEC §2. Anything beyond it needs
  a recorded decision before use.

## Development

Go ≥ 1.22. `make build` produces `bin/gtme`; `make check` (fmt, vet, all
tests — unit, conformance, e2e) must be green, and runs fully offline: no
test may touch the network or require a key. Layout:

```
cmd/gtme/            entrypoint
internal/            runner, planner, ledger, binding engine, adapters, cli
spec/                machine-checkable canon: schemas, fields/, bindings/, ledger.sql, wire/
adapters/            external process-adapter examples
test/                conformance + e2e (drives the built binary, fixtures only)
```

By contributing you agree your contributions are licensed under
[Apache-2.0](LICENSE).
