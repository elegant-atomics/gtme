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

DECISIONS.md ADR-005 killed pipe syntax as a v0 *authoring* surface (`gtme
source | gtme enrich | ...`) but preserved the seam on purpose: nothing in
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
against a small sample — is a plausible third mode once `gtme show` and
`gtme plan` exist as building blocks. Genuinely speculative; no ADR grounds
its shape.

## Groups, option C — rules living on the group

DECISIONS.md ADR-021 (accepted; built as milestone M9, SPEC v0.7)
deliberately stops at "groups remember, pipelines decide": the group tables carry membership and history, and all
policy lives in plan-validatable pipeline YAML. The excluded half is
groups that *own* behavior, parked here so it isn't built early:

- **Intensional groups** — a group defined by a saved segment's SQL that
  evaluates itself (the "smart list"). Touches the same
  membership-refresh semantics as segment-sources below.
- **Group-owned rule bundles and lifecycle state machines** — frequency
  caps, stage transitions, auto-add/auto-remove rules. This is the
  workflow-engine line §0's closed-grammar principle refuses for now.
- **Typed groups** — a `type` becomes meaningful only if it implies a rule
  bundle; until then character stays derived from events and references.
- **Cross-type traversal policies** — "exclude people whose company is in
  `<group>`, via `works_at`": real, and the same relation-traversal
  territory as the `expand` role above; they should be designed together.

## SQL segments as pipeline sources

Named in SPEC §1's long-term list; sharpened by the groups discussion
(ADR-021): a segment is an intensional definition (a saved SQL statement
re-evaluated at read time), and with group events in the ledger, "audience
minus anyone touched in scope X, judged pass in Y" is one query. Making a
segment a *source* (pipeline input) needs: read-only evaluation feeding
identity keys, membership snapshotting semantics for the run, and a story
for fields the segment's SELECT carries along. Unresolved; needs a design
pass. ADR-023 (the universal adapter set) confirmed this stays parked: its
`query/source` slot was reconciled to ADR-021's group-as-source (the
decided, extensional half); a segment-as-source remains the intensional
half, still needing the design pass above. The
`gtme groups add --from-segment` snapshot affordance (ADR-021) covers the
common case in the meantime.

## Floor→ceiling growth loop

Standing position from ADR-023: receipts showing the same `http/*` target
recurring across runs are the tool's cue to suggest minting a named
binding — and, later, the demand signal that prioritizes what the
adapter-authoring codegen skill should target. Needs a design pass on
where the suggestion surfaces (receipt footer? `gtme plan` note?).

## Reader-provider binding for JS-heavy pages

ADR-024 scopes `http/enrich` to no-JS fetching, honestly. The hard
version routes to a reader-provider binding (Jina Reader / Firecrawl
class — URL→markdown as an API): the provider-shape absorbs the fight,
same as harvest. Pure binding YAML once the binding engine exists; needs
a provider pick and a cost model, nothing architectural.

## OpenAPI→binding codegen in the adapter-authoring skill

ADR-025: runtime OpenAPI interpretation is rejected; bind-time codegen is
the happy path — paste an OpenAPI URL, the model proposes a binding
(operation, mapping, idempotency, pagination), conformance tests gate it.
HarvestAPI (OpenAPI + llms.txt published) is the ideal first target. The
skill's requirements get written when the binding engine and its
conformance kit exist to generate against.

## LinkedIn outreach deliver binding

HarvestAPI exposes send-connection / send-message endpoints (standing
note, 2026-08-16 packet): a future LinkedIn outreach deliver BINDING
would make gtme multichannel with zero new architecture. Gated on the
binding engine, a deliverability/safety review, and the same armed-gate
discipline as email delivery.

## Payload re-extraction, fixture minting, simulate replay

ADR-030 creates the substrate (retained raw payloads as purgeable cache);
these are the verbs it deliberately defers: a re-extraction mode that
runs an improved binding's `extract:` over stored payloads (back-filling
new fields at zero vendor cost); minting conformance fixtures from real
stored payloads (systematizing what the campaign-zero shape-drift
episode did by hand); and `--simulate` replaying a pipeline against the
operator's own payload history instead of synthetic fixtures. Each needs
a small design pass on invocation shape (a verb? an engine flag?) once
the ADR-030 substrate exists.

## Adapter marketplace — the bindings security framing

The marketplace itself stays a §13 non-goal. When it comes, ADR-022's
security consequence is the load-bearing fact: bindings cannot execute
code, their blast radius is what the engine permits, and community
bindings are reviewable, diffable data — which is what makes hosting
third-party adapters safe at all. Recorded here so the marketplace
conversation starts from that framing rather than rediscovering it.

## Run-lifecycle notification hook

Named in the ADR-031 design conversation, deliberately not a step: a
"notify Slack/email when the run finishes" surface operates on run
metadata (the terminal receipt), not on records — it answers none of a
step's contract questions (per-record needs, idempotency key,
`on_missing`), which is the test ADR-031 applied. v0's answer is the
receipt on stderr plus the cron/webhook wrapper recipe (SPEC §8): pipe
`gtme run` output wherever you like. If demand shows up, it enters as a
top-level `notify:` run hook (or a receipt sink), never as a step role.
Aggregate record delivery (Google Sheet, CSV export) needs no hook at
all — it's a `batch: true` deliver adapter; if one invocation must see
every surviving record, the missing piece is at most a `batch_size: all`
idiom, not a new cardinality.

## MCP as a control-plane doorway

Standing position (DECISIONS-SEED.md, not an ADR): MCP is a later doorway,
never the data plane. The CLI/NDJSON contract (SPEC.md §5) is what bulk
record movement needs — streaming, checkpointing, language-agnostic
adapters, cheap token economics for an agent driving a shell. MCP's
request/response tool-call shape fits a different job: an agent composing
and launching pipelines, monitoring runs, or doing the fuzzy per-record
research step, sitting *on top of* the CLI rather than replacing it. If
this gets built, it's a thin control-plane wrapper that shells out to `gtme`
— the wire protocol and ledger stay the single source of truth.
