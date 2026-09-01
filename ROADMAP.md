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

## Asynchronous steps

Named 2026-08-28, not specified. A step that may return PENDING with a
token instead of records, so a run ends with the step in flight and a
later run (or `--resume`) hands the token back and collects. One
mechanism serves two needs: the Message Batches API — AI steps at 50% of
the per-token price, results keyed by `custom_id` in any order, which is
gtme's record shape — and `listen`-style provider polling. Additive to
the wire protocol (unknown message types are already ignored). Needs an
ADR on run-record state and receipt rendering for in-flight steps.

## Judgment cache — no paid call twice by default

Named 2026-08-29 while amending ADR-038. Enrich/verify steps already
cache-skip within a freshness window; AI steps never do, so a re-run
re-judges every record unless the author remembered `exclude:`. The
symmetric rule: a paid per-record call is never made twice for the same
(identity, judgment signature = adapter + model + prompt hash + provides
schema) within the window unless the step says `respend: true`. Needs: a
durable verdict to reuse (filter verdicts live only in `run_records`;
a step-event detail carrying the signature is enough — no table) and the
re-application of a reused verdict (pass advances, fail freezes);
composes get it almost free through the enrich cache once the prompt
hash joins the provenance string (ADR-026's `ai/compose @ <model>` — a
spec-visible format change). Sources stay excluded by design: a source's
spend is the query, and "search once, consume the group" already covers
it. Retires ADR-038's respend warning when it lands. Needs its own ADR.

**Promoted (2026-08-29, ADR-039 — accepted 2026-08-29; build queued as M16).** Keyed on a signature over
the question (adapter, model, prompt, output shape, uses) and a hash of
the record's projected inputs — no clock by default, `cache: Nd` to
bound, `respend: true` to opt out — recorded in the `done` event and the
provenance string; no table, no migration. The AI respend warning
retires; the paid-enrich one stays.

**Promoted (2026-08-28, ADR-038 — accepted 2026-08-29; build queued as M15).** The ADR answers the two
questions above without new state grammar: in flight is a `pending` step
event plus a `pending` run status, `--resume` is the collection verb, and
opting in is `deferred: true` on an AI step. Batches only; `listen`
polling reuses the mechanism later but keeps its own design pass (identity
correlation for events). Waiting of any kind stays out — no daemon.

## Deliver preflight

Named 2026-08-28, not specified. A deliver adapter MAY declare checks
against the live target that `plan` or `--dry-run` runs before arming:
campaign status is Active, step count matches, every merge field the
copy expects is actually referenced by the template, no A/B variants —
the class of failure where every request succeeds and nothing sends.
Manifest capability, per adapter; the Instantly adapter is the first
candidate.

**Promoted (2026-08-29, ADR-040 — accepted 2026-08-29; build queued as M17).** At `--dry-run` and arm
time, not `plan` (which stays zero-network): a short preflight session
per deliver step, a three-way answer (`ok` / `blocked` / `inconclusive`),
checks derived from the step's own `variables:`; Instantly first.

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

## Participants — humans and AI in the pipeline

A governing perspective (2026-09-01), named ahead of any single feature:
filters, reviews, and content creation should accept any participant — a
person at a terminal, a person reached through an agent conversation, an
API model, an agent session or workflow — under one contract: **a
participant's output is a ledger fact** (a verdict, a score, a field, a
group membership), written with provenance, never control flow. Three
roles cover the space:

- **filter** — closed verdict plus reason (`ai/filter` is the built
  instance);
- **create** — new content fields (`ai/compose`; a human writing or
  editing outgoing copy is the same role on a different engine);
- **review** — filter and create *fused*: one step emitting a closed
  verdict and, optionally, free text or revised content. Open-ended
  review is creation anchored to a referent, which exposes the one
  mechanical gap in the ledger today: provenance cannot record
  *was-a-review-of* (a referent link), which pure creation never needed.

The arm gate is deliberately absent from that list: dry-vs-armed is a
property of the run, not a step in the graph, precisely so it cannot be
composed away.

Positions this entry holds, each wanting an ADR before it is real:

- **One declaration, any engine.** A step's declared output schema
  (ADR-033) *is* its verdict vocabulary: the same enum constrains an API
  model's output, an agent session's judgment, and the choice menu a
  human is shown. Surfaces render from the declaration.
- **Pending/collect is the universal out-of-band surface.** ADR-038's
  shape — the run ends `pending` with the work described in ledger
  state, and a later plain run collects — generalizes past the batch
  API: an agent session that picks up the ledger, judges the pending
  records through the CLI, and leaves results for the next run to
  collect is the same mechanism, and a human answering through an agent
  conversation is too. One collection surface for every participant
  that is not synchronous.
- **Retire the shell-out.** The `claude-code` engine (§2) blocks the
  runner on a subprocess, which the pending/collect shape above makes
  unnecessary — a session picking up the ledger on its own trigger is
  strictly better aligned with "no waiting stays in the runner."
  Spec-visible; wants its own ADR.
- **Agent workflows are a candidate engine, not a new concept** —
  useful where judgment outgrows one prompt (review-then-verify passes,
  competing perspectives). The open question with teeth: ADR-039 caches
  a judgment on a signature over prompt, model, and inputs; what is the
  signature of a workflow?
- **Routing is a pattern, not a key.** A verdict fact, a per-branch
  `sql/filter`, and `group/deliver` already route N ways (the M14
  acceptance runs the two-way case). A `route:` key is the
  mechanism-shaped version; the signal for revisiting is operators
  repeatedly failing to compose the assembly, not the assembly merely
  existing.

The engine landscape this entry governs:

| Engine | Roles | Status | Timing shape |
|---|---|---|---|
| Human, in-band (terminal) | filter, review, create | named below ("Interactive review step"), unspecified | sync, per-record, blocking — needs defined non-interactive behavior |
| Human, out-of-band | arm (run mode, not a role) | built (dry-run → arm, ADR-019/031) | whole-run, nothing waits |
| Human, agent-mediated | filter, review, create | named, unspecified | async via pending/collect |
| API model step | filter, create, review | built (ADR-004/033; cache ADR-039; batch ADR-038) | sync, or deferred → collect |
| Agent session on the ledger | filter, create, review | position above; replaces the shell-out | async via pending/collect, own trigger |
| Agent workflow | filter, review, create; multi-pass | candidate engine, unnamed elsewhere | async, long-running |
| Cron / unattended | none — the forcing function | built | every row above must state its behavior here |

The "Interactive review step" entry below predates this one and is its
human-engine case; its open questions stand.

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

Named 2026-08-31, not specified. A per-record human review: the pipeline
stops a record at a review step, shows the operator a configured rendering of
it, and takes a decision from a configured response set. Design position
(leaning, not ADR'd): **the verdict is a fact, not a branch.** The reviewer's
answer is a recorded judgment — provenance, judgment cache, asked once per
scope — that downstream steps consume through the machinery that already
exists (`sql/filter` on the verdict field, group membership). Per-response
control flow inside the pipeline is declined: routing on responses is an
expression language by another name, the same mechanism shape "Groups,
option C" refused. "Rejected → nurture" is a second pipeline reading the
verdict, and that idiom is a pattern to document, not syntax to add.

The step is structurally `ai/filter` with a human behind the contract —
`engine: human` rather than a new step kind. It inherits declared output
schemas (ADR-033): the **response set is the declared output enum**, so the
pipeline author configures the verdict vocabulary the same way an AI step
declares its shape. The author also configures the per-record rendering —
which fields the reviewer sees and how the message is presented — in the
step's config, so the review surface is authored in the pipeline file like
everything else.

Collection reuses ADR-038's pending machinery: a run with unanswered reviews
ends `pending`, the receipt says what is awaited, and a later plain `gtme
run` collects. No TTY block as the primitive, no daemon, no new verb —
"waiting of any kind stays out" holds because the pending state is ledger
state, not a live process.

Open question: the collection *surface*. At least two are real — a terminal
prompt (gtme itself asks, record by record, when a human runs it) and an
agent-mediated review (the run is pending; a Claude Code session presents
each record to the human in conversation and records verdicts through the
CLI). Whether those are two engines behind one contract (the AI steps'
`api` / `claude-code` precedent) or two adapters is undecided; the contract —
configured rendering in, enum verdict out, judgment recorded — should not
differ between them.

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
