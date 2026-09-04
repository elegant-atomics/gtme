# ROADMAP.md

**Non-normative.** Nothing here is decided; nothing here is a commitment.
This is the parking lot for ideas named and explicitly deferred during
design, so they aren't lost and aren't accidentally built early. Promotion
out of this file requires an ADR in DECISIONS.md and, if spec-visible, an
approved SPEC.md diff. See PROCESS.md.

A section marked **BUILT** has been promoted and is no longer a plan: it
is kept as the trail from the name to the ADR that answered it, and to
hold whatever it named that is *still* open. Read those for history, not
for what is coming.

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

**Retired by composition (2026-08-28, ADR-037).** A runnable test built the
account shape from shipped atoms: three of its four cardinality moves ran
today (the cross-type gate as `sql/filter`, the fan-in as a cross-record
transform, the company judged on the aggregate), and the fourth — a
company fanning into its people — needed a config value drawn from the
ledger, not a role. Fan-out at a pipeline boundary never raises the open
run-membership question above, because membership is fresh per run. What
remains of `expand` is single-file ergonomics, and single-file would
*remove* a review gate (ADR-031 arms every deliver in a run at once), so
it is not a safety improvement. Kept here as a convenience item only;
the open question is retired, not solved — nothing needs it answered.

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

**Tested and held (2026-08-28, ADR-032).** The excluded half — group-owned
lifecycle state machines — was put under the strongest pressure available:
a real multi-stage system whose operational layer is built from exactly
the refused primitives (holds requiring an explicit release to authorise
spend, leases with TTLs, attempt counters with backoff, a third worker
outcome that returns a subject uncharged, ranked serve order). Read
compositionally, that whole layer collapses into ADR-032: the handoff to
the next stage is a *delivery*, so it inherits the review artifact, the
arming gate, idempotency, suppression, completeness policy, and touch
history already built for one. One step and one config key against six
keys, a table, a migration, and a shared invariant. **Consequence: holds,
releases, leases, attempt counters, and ranked serve order will not be
built.** This is now a tested position rather than a design instinct, and
the test generalizes: atoms compose and mechanisms do not — a candidate
that does one specific thing one specific way, combines with nothing, and
introduces a shared invariant is a mechanism, and mechanisms arrive
disguised as the obvious fix. The cross-type traversal item above turned
out not to need `expand` at all: it is a `sql/filter` today.

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

**Half delivered (2026-08-28, ADR-037).** A segment (or an inline query) may
now feed any *config value* — `domains: {segment: qualified-domains}` on a
source — resolved at plan time with rows shown, recorded per run. That
touches neither run membership nor identity minting, so it needs none of
the snapshotting semantics above; when stability matters, snapshot into a
group first. Segments as the run's *records themselves* still needs the
design pass and stays parked.

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

**Promoted as a registry (2026-08-29, ADR-042 — accepted 2026-08-30; build queued as M19).** Not a
marketplace: an index file and a fetch verb. Bindings are URL-addressed
and hash-pinned (`gtme adapters add github.com/…@ref`), nothing installs
unverified (`adapters verify` runs the fixtures offline first), the
registry repository holds the index and the verified set, community
entries point at their authors' repositories. The binary keeps the floor
and the reference twins only. The hosted marketplace — accounts,
payments, a service — is what §13 still excludes.

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

## Patterns as runnable bundles

If capability grows by documented assembly rather than by new grammar,
the pattern library becomes the real growth surface over time, and its
*form* is a design question. Prose pattern libraries rot silently. A
pattern shipped as a campaign bundle (ADR-029) would mean an agent does
not read about an assembly — it runs it, reads the receipt, and diffs its
own variant against a known-good one, free and deterministic under
`--simulate`; staleness becomes detectable, because a pattern that stops
simulating is out of date and CI can say so. Adds no surface. Needs a
design pass on where such bundles live, how they are indexed for an
agent, and whether they belong in-repo or in a companion catalog. The
limit worth recording alongside: patterns transfer structure, never
judgment — which fields a campaign should judge on and what to conclude
is empirical knowledge about a market, and the ledger (verdicts and
reasons persist) is how that gets earned, run by run.

The same catalog slot (2026-09-01) likely holds an **operator plugin** —
skills that guide the work rather than perform it: designing a pipeline
(the interactive what-should-we-build conversation), authoring bindings
(see "Bindings from OpenAPI specs"), and reading the ledger (a reporting
skill over `gtme query`). Distinct from "MCP as a control-plane doorway"
above — that is a call surface, this is guidance — though the overlap
wants resolving before either is built. One design perspective belongs
to whichever ships first: the schemas already describe every knob
(`config_schema` for adapters, declared output schemas for steps), so
interactive surfaces should be *generated from declarations* — a select
menu from an enum, a form from a config schema — never hand-built per
adapter or per step.

## Asynchronous steps — BUILT

Graduated: ADR-038 (accepted 2026-08-28, drafted from this entry; amended
in review 2026-08-29 for the last-step rule and collect-first). A step may
end a run in flight under a token, and a later `gtme run` (or `--resume`)
collects. Both needs it was named for are served: the Message Batches API
for AI steps (`internal/ai`, one request per record keyed by `custom_id`),
and the same mechanism now carries `human/*`/`agent/*` steps (ADR-049).

What it was also named for and is still open: `listen`-style provider
polling — see the `listen` verb entry above, which is the surface that
would use this mechanism for inbound events.

## Judgment cache — no paid call twice by default — BUILT

Graduated: ADR-039 (accepted 2026-08-29, drafted from this entry), built
in M16 (spec v0.19). A paid per-record judgment is not made twice for the
same (identity, judgment signature) within the window unless the step says
`respend: true`; the signature is the step's question — adapter, model,
prompt, output shape, `uses:`, and `of:` (ADR-048) — and the input hash
is the facts it reads, excluding the step's own outputs. Kept here only
as the trail from the name to the ADR.

## Deliver preflight — BUILT

Graduated: ADR-040 (accepted 2026-08-29, drafted from this entry), built
in M17 (spec v0.21). A deliver adapter MAY declare checks against the
live target that `plan` and `--dry-run` run before arming — the class of
failure where
every request succeeds and nothing sends. Kept here only as the trail
from the name to the ADR.

## Email waterfall as a pattern, not a provider

Named 2026-08-29. §13 keeps email waterfall *providers* and the
`waterfall:` syntax as non-goals, and neither is needed: N `http/enrich`
steps each providing `email` are a waterfall by construction, because a
step whose provides are already current cache-skips (§7) — the chain
falls through on its own. A verifier is one more `http/enrich` providing
`email_status` (a canonical field already) and a `sql/filter` on it.
Ships as a *pattern* — runnable, fixture-served, under "patterns as
runnable bundles" — not as vendor adapters; an operator's finder is their
own binding in `~/.gtme/adapters/`. The one reach gap: finders that are
asynchronous (submit, poll later) need bindings to emit PENDING
(ADR-038's mechanism, not yet extended to the binding engine).

## Seat-coordinated composition (the A-4 question)

Named 2026-08-29. Writing several people at one account as one story with
a different angle per seat wants the composer to see the siblings. The
mechanism-shaped answer is a batching key ("batch by company"); the
accepted answer for now is a `sql/transform` that writes each person the
committee's titles as a field, so every prompt carries the sibling facts
and the model writes one person at a time knowing who else is written
to. Revisit only if receipts show seat copy converging; a batching key is
then a small change to how the runner chunks, not a new step.

## Participants — humans and AI in the pipeline — BUILT

Graduated: ADR-048, ADR-049 and ADR-050 (accepted 2026-09-02), built in
M24 (spec v0.35). The governing position held: **a participant's output
is a ledger fact** — a verdict, a value, a label, written with provenance,
never control flow — whether the participant is a model, a person, or an
agent driving gtme. Three roles by what goes in and what comes out
(filter gates, compose writes, review labels and never gates); `human/*`
and `agent/*` as runner-owned adapters; `gtme answer` as the one write
path; `of:` as the referent; the `claude-code` shell-out and the
`engine:` key retired.

What the first draft got wrong, kept so it is not re-proposed: an
`engine: human` switch under an `ai/*` name (who answers is the adapter's
name, not a config key); "review" as a third kind of step (it is a role
defined by its output — labels about a value — and it never gates); a
`session/*` participant distinct from a person (an agent answering is
`agent/*`, same code); and applying ADR-038's "nothing waits in the
runner" to a person at a terminal (the rule exists for cron and batch
APIs; a terminal walk is an API with a little more latency).

**Still open, deliberately: agent workflows.** A multi-pass agent (review
then verify, several perspectives) answers under an `agent/*` step like
any agent; its name is provenance, and the judgment signature is the step
declaration alone (the cache is checked before anyone answers). Whether a
declared workflow identity should ever enter the cache key — so a changed
process re-judges without `respend:` — is the question, and it waits for
a real multi-pass agent to have been used.

## Object ontology — beyond person and company

§4 derives identity for exactly two entity types; the binding schema
deliberately keeps `entity_type` an open string, and plan/verify now
refuse what has no derivation (issue #27) — so the extension point is
real and the boundary is enforced. What a third type requires: a §4
key-derivation rule, a registry vocabulary file, and an answer for which
relations it carries. Some "objects" never need to be types — the
account shape ran entirely on relations over person and company.

The live candidates are the operational structures: groups, campaigns,
segments. Today a group is runner-owned state and a campaign is a name
in an adapter's config; making them addressable objects would let a
record's association with them be a *fact*. The distinction that governs
this entry: **association vs commitment**. Associating a record with an
object (this person belongs with the campaign a field value names) is a
relation write — per-record, data-driven, free, no gate, and the honest
answer to "route on an enum." Committing a record (delivery into a
group or campaign) keeps a static target per step, because the dry-run
receipt, plan validation, and the idempotency scope all depend on the
target being known before the run; a per-record computed delivery
target is the routing key in disguise and stays refused. A later
pipeline can always source *from* the association (`{query:}` over
relations) into its one gated target.

## Getting started paths

v0.1.0 ships darwin/linux binaries with checksums on a tag-triggered
workflow, and the README installs from a clone. Named but not built: a
one-line installer, a package on the common managers. The bar to hold
them to: a person or an agent goes from nothing to `gtme plan` on a
working example in one command and a few minutes, offline after the
download. Nothing here changes the tool; it changes how many people ever
reach `--simulate`.

## Bindings from OpenAPI specs

Mass adapter coverage by generation splits into three layers with very
different automation ceilings. **Mechanical:** a binding's request
template, auth shape, `config_schema`, and often pagination are
derivable straight from a vendor's OpenAPI document — a generator could
scaffold a vendor's surface in minutes. **Judgment:** what the spec
cannot say — which endpoints are even source/enrich/deliver-shaped, the
`entity_type`, the extraction mapping onto the canonical registry (the
thing that makes a binding compose), and an honest cost declaration,
since pricing is never in the spec. That layer is agent work guided by
the registry's own near-miss suggestions, not codegen. **Evidence:**
conformance fixtures are recorded real responses, and `adapters verify`
refuses a binding without them — the deliberate bottleneck, and it
stays one: a scaffolded binding becomes certifiable only when someone
with a credential records a real response. Validation round 3 showed an
agent authoring a working binding from `gtme help --bindings` alone; a
spec-driven path is a throughput multiplier on that demonstrated floor,
and probably takes the form of the adapter-builder skill named under
"Patterns as runnable bundles."

## Interactive review step

Named 2026-08-31; folded into the participants packet 2026-09-02 as the
`human/*` adapters (ADR-049). Its open question — terminal prompt versus
agent-mediated review — is answered there: both are `human/*` steps, the
terminal walk happens inside `gtme run` (or `gtme answer` interactively),
and an agent relaying a person's decisions answers with `gtme answer
--as`. What stays deferred: an in-place *editor* for outgoing copy (a
compose with `of:` accepts a revised value; a richer editing surface is a
UI, §13) and any consequence routing (declined, ADR-048).

## Registry-maintained cost declarations

DECISIONS.md ADR-046 records the basis of a spend (measured vs estimated)
but leaves the estimated numbers themselves wherever the binding author
typed them, which for community bindings is unverifiable (issue #29's
durable ask). The natural home is the registry index: a per-binding cost
declaration re-checked on the same cadence that re-records fixtures, so
an estimate traces to something maintained and a stale price becomes a CI
failure rather than a silent receipt error. Registry work, not runner
work; deferred from M23.

## Templated `cost_estimate_usd`

ADR-046 lets `cost.amount_usd` (the ledger figure) template from config;
`cost_estimate_usd` (the manifest figure `gtme plan` prints) has the same
plan-dependent-pricing problem and could take the same treatment. Less
urgent — a wrong estimate is less damaging than a wrong ledger row — and
deferred so M23 stays small. Whoever picks it up: the resolution point is
plan time, from the step's resolved config.
