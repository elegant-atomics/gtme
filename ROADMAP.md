# ROADMAP.md

**Non-normative.** Nothing here is decided; nothing here is a commitment.
This is the parking lot for ideas named and explicitly deferred during
design, so they aren't lost and aren't accidentally built early. Promotion
out of this file requires an ADR in DECISIONS.md and, if spec-visible, an
approved SPEC.md diff. See PROCESS.md.

## `expand` role

DECISIONS.md ADR-008. A pipeline step that takes one record in and emits N
records out — possibly of a different `entity_type` — writing `relations`
along the way (e.g., "people at this company" fanning a company identity
out into person identities). It's genuinely a fourth shape beside
source/enrich/deliver ("sources receive no RECORDs" is violated on purpose
here), which is why v0 defers it rather than bending an existing role to
fit.

**Open question:** run-membership semantics when the entity type switches
mid-run. `run_records` keys on `(run_id, identity_id)` for the run's
original entities — what does it mean for a company-entity run to gain
person-entity members partway through? Does `expand` mint a sub-run, or
does `run_records` need a third dimension? Unresolved; needs a design pass
before `expand` gets a manifest shape.

## Pipes as a transport, not a syntax

DECISIONS.md ADR-005 killed pipe syntax as a v0 *authoring* surface (`gtm
source | gtm enrich | ...`) but preserved the seam on purpose: nothing in
the runner may couple steps to shared in-process memory in a way that
precludes running the same step executor over a stdio transport later.
`adapters.Session` is already the boundary a future transport would sit
behind (see the 2026-08-13 "built-in adapters run in-process over pipes"
implementation decision). If pipes return, they return as an alternate
transport for the same pipeline object YAML describes today — not as a
second authoring grammar to keep in sync with the first.

## `listen` verb

Named in the design session, not specified. The reverse flow: an adapter
that emits *events* (replies, opens, bounces, meetings booked) as a source,
so that reply-handling and meeting-booking become just another pipeline
rather than a special case. Shape unresolved — likely a `source` variant
whose records are events rather than person/company records, which has
implications for identity-key derivation (an event correlates to an
existing identity rather than minting one) that need their own design pass.

## REPL

Named in the design session as a future interactive surface, not otherwise
specified there. A natural extension of "two modes, one engine" (SPEC.md
§1): today's modes are pipe-mode-for-exploration (deleted, see ADR-005) and
YAML-for-frozen-workflows. An interactive shell — inspect the ledger,
iterate on a pipeline's `needs`/`provides` wiring, re-run a single step
against a small sample — is a plausible third mode once `gtm show` and
`gtm plan` exist as building blocks. Genuinely speculative; no ADR grounds
its shape.

## MCP as a control-plane doorway

Standing position (DECISIONS-SEED.md, not an ADR): MCP is a later doorway,
never the data plane. The CLI/NDJSON contract (SPEC.md §5) is what bulk
record movement needs — streaming, checkpointing, language-agnostic
adapters, cheap token economics for an agent driving a shell. MCP's
request/response tool-call shape fits a different job: an agent composing
and launching pipelines, monitoring runs, or doing the fuzzy per-record
research step, sitting *on top of* the CLI rather than replacing it. If
this gets built, it's a thin control-plane wrapper that shells out to `gtm`
— the wire protocol and ledger stay the single source of truth.
