# DECISIONS.md

The project's decision log. Two kinds of entry, in one file, newest-in-each-section-last:

- **Architecture Decisions (ADR-NNN)** — Nygard format (Status / Context /
  Decision / Consequences) plus a required **Spec impact** field. These
  originate from design sessions with the human and are the authoritative
  record of *why* SPEC.md says what it says. Supersede, never delete: a
  later ADR that changes course adds a new entry and marks the old one
  Superseded, it does not edit or remove it.
- **Implementation Decisions** — small, spec-invisible choices made while
  building, recorded per SPEC.md §12 ("Decide-and-record when: something
  small is underspecified"). Dated entries, newest last. These do not get
  ADR numbers; they document internals, not contracts.

All new entries — of either kind — follow this convention. See PROCESS.md
for how an entry gets here (chat proposes, repo decides).

---

## Architecture Decisions

Seeded from the 2026-08-14 design session (`DECISIONS-SEED.md`, a session
artifact — see PROCESS.md's mechanics section — not committed). Numbering
preserved from the seed.

### ADR-001: Go runner, single static binary
**Status:** Accepted
**Context:** Distribution wedge is a 60-second install; contribution surface is adapters (any language, process-isolated), not the runner.
**Decision:** Go 1.22+, single binary. Deps limited to modernc.org/sqlite (pure-Go, no cgo), santhosh-tekuri/jsonschema/v5, gopkg.in/yaml.v3.
**Consequences:** Adapter contributors never touch the runner; runner contributors are few by design.
**Spec impact:** Already in spec §2. No change.

### ADR-002: The ledger is the bus
**Status:** Accepted
**Context:** n8n-style "every step forwards all fields" breaks at scale; steps need only projections.
**Decision:** Steps read projections from and write facts to SQLite; any stream carries only identity keys + control. Two-layer schema: identity layer (identities, field_values append-only, relations) is durable/cross-run cache; run layer (runs, run_records, step_events, costs, deliveries with UNIQUE target+idempotency) is per-execution.
**Consequences:** Cache-aware waterfalls, SQL segmentation over history+treatment, resume, receipts — all fall out of the schema.
**Spec impact:** Already in spec §3. No change.

### ADR-003: Projection ships as a SQL VIEW
**Status:** Accepted (audit fix; supersedes projection-in-Go-only)
**Context:** Query examples referenced a `current_fields` relation that didn't exist; projection logic lived only in `internal/ledger/project.go`, so query-land and runner-land could drift.
**Decision:** Define the current-value projection (highest-confidence within freshness window, newest wins ties) as a SQL view created with the schema. Runner and `gtm query` both use it. One definition.
**Spec impact:** AMEND — ledger section gains the view DDL; query examples corrected to use it.

### ADR-004: `uses:` — dynamic needs for AI steps
**Status:** Accepted (audit fix). **Partially superseded by ADR-019**, which
generalizes this from an AI-only mechanism to a general `needs: dynamic`
concept with a second instance (deliver's `variables:`) — the `uses:`
mechanics here are unchanged, only the framing broadens.
**Context:** AI step prompts reference arbitrary fields; static manifest `needs` can't know them, so plan-time validation silently skipped the riskiest steps.
**Decision:** AI adapters accept `uses: [field, ...]` in step config. Runner treats `uses` as the step's needs for projection and plan validation. A prompt referencing a field not in `uses` (or a `uses` field no upstream step provides) is a plan error.
**Spec impact:** AMEND — manifest/config section + plan semantics.

### ADR-005: Pipe syntax dropped from v0 entirely
**Status:** Accepted. **Supersedes** (in order): multi-process pipe chaining as M3 → inline `gtm x '<a>|<b>'` expression form → both.
**Context:** The v0 hypotheses to falsify are (a) ledger/runner semantics, (b) adapters writable against the protocol, (c) Claude can build and operate the system. Pipe syntax tests none of them — it is a presentation of the pipeline object — and it was the recurring source of spec inconsistencies.
**Decision:** v0 CLI surface is exactly: `init, secret, plan, run (--resume), query, show, runs, freeze, help --agent`. No `gtm x`, no multi-process pipes, no standalone source/filter/enrich/compose/deliver subcommands (they existed only for pipe mode — cull them). YAML is the only pipeline authoring surface. One constraint on implementation: nothing may couple steps to shared in-process memory in a way that precludes a future stdio transport; do not build the transport abstraction, just don't destroy the seam.
**Consequences:** M3 (pipe mode) is deleted; milestones renumber. Validation campaign debugs one execution path. Pipes return post-v0, if at all, as a transport over the same step executor (see ROADMAP.md).
**Spec impact:** AMEND — CLI surface section, §8 pipe-mode mechanics deleted, milestones restructured, non-goals gains: "pipe syntax deferred; all pipeline surfaces compile to the pipeline object."

### ADR-006: `gtm show` — standalone read-only projection inspector
**Status:** Accepted (mid-stream tap form dies with ADR-005; standalone form survives)
**Decision:** `gtm show <identity-key>` and `gtm show --run last` print full ledger projection with `--fields/--provenance/--limit`. Strictly read-only; never appears in freeze output.
**Spec impact:** AMEND — add verb to CLI surface (lands in what was M4).

### ADR-007: `gtm help --agent` — self-describing surface
**Status:** Accepted
**Context:** The LLM's real interface is whatever document about the CLI is in its context; that document must be generated, never hand-maintained.
**Decision:** `gtm help --agent` emits the full surface — verbs, flags, every installed adapter manifest (needs/provides), 3 canonical examples — as one compact (~1–2k token) machine-readable doc regenerated from the registry.
**Spec impact:** AMEND — add to CLI surface + acceptance criterion (doc must round-trip: an agent given only this doc can author a valid pipeline).

### ADR-008: `expand` role — named and deferred
**Status:** Accepted (deferred post-v0)
**Context:** Mid-pipe apollo/people-at-company violates "sources receive no RECORDs"; it's really record-in → N records (possibly different entity_type) out, writing relations.
**Decision:** Name the role `expand`, define nothing further in v0, park in ROADMAP.md with the open question: run-membership semantics when entity type switches mid-run.
**Spec impact:** ROADMAP entry only; spec non-goals mentions it by name.

### ADR-009: Webhooks/batch via spool + cron; no daemon
**Status:** Accepted
**Decision:** Event-driven operation = commodity receiver (Worker/Zapier/Action) appends payloads to a spool; scheduled `gtm run` drains it via a `webhook/source` adapter (near-clone of csv/source). Deliveries-table idempotency absorbs at-least-once redelivery structurally. Per-event low latency is explicitly out of scope for v0.
**Spec impact:** AMEND — add webhook/source to adapter list, document the pattern as a recipe, keep "no daemon" in non-goals with this as the stated answer.

### ADR-010: Spec-as-canon methodology
**Status:** Accepted
**Decision:** SPEC.md is the single source of truth for observable behavior: CLI surface, exit codes, JSON output schemas, error structure, ledger DDL, identity-key derivation, wire protocol, manifest schema, projection/freshness semantics, idempotency guarantees, behavioral invariants, acceptance criteria. Implementation (packages, concurrency, naming, internals) belongs to Claude Code, recorded in DECISIONS.md when non-obvious. Litmus: "would a second clean-room implementation need this to interoperate?" Spec-visible changes require a proposed spec diff + human approval BEFORE code; spec-invisible decisions are autonomous. Machine-checkable artifacts (JSON Schemas, ledger.sql incl. the ADR-003 view, golden wire transcripts, acceptance scripts) live in `spec/` and are loaded directly by the test suite. Code that diverges from spec is a bug even if it works.
**Spec impact:** Governs everything; encoded in CLAUDE.md.

### ADR-011: Documentation formats
**Status:** Accepted
**Decision:** ADRs in a single DECISIONS.md (numbered entries, Nygard format + a "Spec impact" field; supersede, never delete). RFC 2119 keywords in SPEC.md normative sections, declared once. Acceptance criteria in Given/When/Then sentence form (no Cucumber tooling). Keep-a-Changelog section at the bottom of SPEC.md. Principles stay freeform prose.
**Spec impact:** SPEC.md gains RFC 2119 declaration + Changelog section.

### ADR-012: Principles and stories placement
**Status:** Accepted
**Decision:** Principles = SPEC.md §0 preamble, explicitly non-normative (guides proposals, never overrides a MUST; ~1 page max; only principles that settled a real argument). The eight operator stories (launch, top-up, interrogate, iterate, segment, guard, recover, report) appear twice with different jobs: invariant + Given/When/Then acceptance criteria in SPEC.md (normative); enactment scripts in VALIDATION.md (non-normative, living).
**Spec impact:** AMEND — add §0 and story acceptance sections.

### ADR-014: Chat ↔ Claude Code contract
**Status:** Accepted
**Decision:** "Nothing is decided until it's in the repo." Chat = design organ (proposes; each session ends in a session packet: ADR entries + spec diffs + roadmap items). Repo = canon. Claude Code = implementer + conformance checker (builds from spec, proposes amendments, never silently diverges). Human = sole approver of spec-visible change. Encoded in PROCESS.md.
**Spec impact:** None (process).

### ADR-015: Instruction vs knowledge
**Status:** Accepted
**Decision:** Instruction = how to act (CLAUDE.md, skill files) — thin, procedural, under a page. Knowledge = what's true (SPEC.md, DECISIONS.md, principles, stories) — consulted, can be long. The spec is knowledge with normative force; the instruction pointing at it is one line.
**Spec impact:** Shapes CLAUDE.md authoring.

### ADR-016: Next milestone after reconciliation = the validation campaign
**Status:** Accepted. **Superseded in part by ADR-019**, which renames the
first campaign "campaign zero" and gives it a smaller shape (see ADR-019);
this entry's ~50-record Apollo funnel survives as a *second*, later
campaign once campaign zero has run clean.
**Context:** Code exists but nothing has run a real campaign; the untested-ness is the main risk, not documentation.
**Decision:** After spec reconciliation and divergence audit, the priority is VALIDATION.md's first campaign: ~50 real records, real Apollo pull, AI compose, deliver to a controlled Instantly campaign; enact the eight stories in miniature (kill/resume mid-run, Monday top-up proving dedupe + cache hits, interrogate one record, read the cost receipt).
**Spec impact:** VALIDATION.md created; spec untouched by outcomes until amendments are proposed.

---

## Session packet, 2026-08-15 (canonical vocabulary & edge contracts)

Second design-session packet (`DECISIONS-SEED-2.md`, a session artifact,
not committed — same as the first). Numbering continues from ADR-016.

**Scope note (2026-08-15):** on receiving this packet, the human scoped its
application: ADR-017/018/019 are recorded below as decided, and VALIDATION.md
was rewritten per ADR-019 (see "campaign zero" there). Their **Spec impact**
was deliberately **not applied** in that docs-only pass, and none of the
mechanics they describe (a field registry, `columns:`/`variables:` edge
mappings, `needs: dynamic`, the `on_missing` policy, the dry/armed gate)
existed in code — a separate, larger reconciliation-plus-build pass, scoped
explicitly as future work. *Update (2026-08-15, later the same day):* that
pass ran on the human's instruction. The Spec impact of all three ADRs was
applied to SPEC.md (changelog v0.4, milestone M7), together with ADR-020
below (Accepted after review) resolving the identity-normalization gap
flagged under ADR-017; the M7 mechanics were then built — `make check`
green, including offline e2e acceptance of the campaign-zero shape — and
VALIDATION.md's campaign zero is now marked runnable, still human-gated as
ever.

### ADR-017: Canonical field registry (the shared vocabulary)
**Status:** Accepted (spec impact deferred — see scope note above)
**Context:** `needs`/`provides` matching is string equality; it is only meaningful if adapters agree on field names. Singer standardized the protocol but never the vocabulary — every tap emitted its own shapes, so composability never materialized. gtme must not inherit that failure. The registry is also what makes AI-generated adapters trustworthy: codegen targets a closed vocabulary with conformance tests instead of inventing names.
**Decision:** A canonical field registry per entity type lives in `spec/fields/<entity_type>.json`. Each entry: name, type, format, normalization rule, value domain (enum where applicable), example. Scope rule — a field is canonical when it crosses an adapter boundary; three tiers:
1. **Identity fields** (mandatory): person = email, linkedin_slug, first_name, last_name; company = company_domain, company_name. These back identity-key derivation; their normalization rules (email lowercased, company_domain reduced to eTLD+1) are part of the registry and were previously implicit in the key-derivation spec — make them explicit and shared.
2. **Canonical core**: any field that (a) ≥2 adapters provide, (b) is waterfall/dedupe-relevant, or (c) is commonly consumed by compose/deliver steps. Seed by one-time curation of the overlap across major B2B providers (Apollo, Clearbit, PDL, ZoomInfo class) — expect ~40–60 fields. Canonical fields declare canonical VALUE domains too (e.g. `seniority` is a fixed enum; `employee_count` is an integer, never a range string) — without value normalization, waterfalls compare incomparables.
3. **Vendor namespace**: everything else as `<vendor>.<field>` (e.g. `apollo.intent_score`). Stored with provenance, queryable, usable in `uses:`. Namespaced fields in a pipeline's needs make vendor coupling visible; `plan` notes it.
Promotion: namespaced → core when a second adapter provides the same fact ("rule of two"). Additive changes = one ADR line, non-breaking. Renames = breaking, spec amendment + version bump.
Enforcement, three layers: (1) manifest validation — `needs`/`provides`/`uses` entries must exist in the registry or be namespaced; (2) runtime — RECORD output validated against `provides` including normalization/value domains; (3) an adapter conformance kit — golden vendor-payload fixtures in, expected canonical records out — which the adapter-authoring skill targets, making "generate adapter" a generate→test→fix loop with a machine-checkable finish.
**Consequences:** Adapters map vendor dialect → canonical at their own boundary; nobody downstream thinks about mappings. Waterfalls work without configuration. Registry starts small and grows by demand, never by design session.
**Spec impact:** AMEND (not yet applied) — new registry section; `spec/fields/` artifacts; manifest validation rules; conformance-kit requirement added to adapter authoring section; identity-key normalization rules cross-referenced to registry.

**Known gap to resolve when this is built (flagged 2026-08-15, not yet an
ADR):** person identity fields need at least one more normalization rule
than SPEC §4 currently has, and possibly two more key tiers.
- **`linkedin_slug` has two incompatible shapes in the wild.** A provider's
  `linkedin_url`-shaped field can be the public vanity URL
  (`linkedin.com/in/jane-doe`) or an internal/member-ID form (opaque,
  Sales-Navigator-style). §4's normalization (strip protocol/host/trailing
  slash/query, lowercase) silently produces two different strings for the
  same real person depending on which shape arrived — a dedup failure, not
  a caught error. Unlike a weak→strong key upgrade (§4's existing
  mechanism), this is two values *within the same tier* that are secretly
  the same identifier. The registry needs either a normalization rule that
  detects and resolves the internal form (plausibly requiring an
  enrichment call — Harvest or similar — before the field is trustworthy
  as key material), or a rule that refuses to key on an unresolved
  internal-form URL at all until something resolves it.
- **`github_username` and `twitter_handle` are plausible additional
  person identity tiers** — both globally unique, public, low-collision,
  likely ranking below `linkedin_slug` (LinkedIn stays primary for B2B)
  but above the name-hash fallback. No v0 adapter provides either field
  yet, so this is speculative until one does — not urgent, but worth
  reserving the tier ordering for now so it doesn't get designed around
  later.

### ADR-018: Mapping exists only at the edges
**Status:** Accepted (spec impact deferred — see scope note above)
**Context:** The minimal pipeline (csv/source → instantly/add-to-campaign) exposes two foreign vocabularies: CSV headers (user's world) and campaign merge fields (Instantly template author's world). Neither is gtme's; the interior is.
**Decision:** Exactly two mapping sites, both declarative, both in step config:
- Ingress: `csv/source` takes `columns:` mapping headers → canonical names. Headers already matching canonical names auto-map with zero config; near-misses are SUGGESTED in plan output, never silently guessed. A mapping that yields no identity-key path (person: no email) is a plan error. Normalization per the registry happens at ingress; invalid values are per-record verdicts, not crashes.
- Egress: `instantly/add-to-campaign` (and deliver adapters generally) take `variables:` mapping canonical/namespaced ledger fields → the target's arbitrary merge-field names.
No interior step may carry a mapping block. Code transforms are a named escape hatch for COMPUTED fields only (e.g. splitting full_name), not a mapping mechanism — declarative mappings are plan-validatable; code is opaque to plan.
**Consequences:** Pipelines stay portable; mapping burden sits with the two parties who own the foreign vocabularies; `plan` can prove edge-to-edge coherence before any row is read.
**Spec impact:** AMEND (not yet applied) — csv/source and deliver-adapter config schemas; plan semantics (auto-map + suggestion behavior; identity-path check).

### ADR-019: Dynamic needs generalized (supersedes ADR-004's AI-only framing)
**Status:** Accepted (spec impact deferred — see scope note above). **Partially
supersedes ADR-004**: the mechanics `uses:` established for AI steps are
unchanged; this generalizes the *concept* to a second instance (deliver's
`variables:`) rather than replacing anything ADR-004 built.
**Context:** ADR-004 gave AI steps `uses:` because prompts reference arbitrary fields. The deliver case revealed this is the general pattern: any step whose contract is defined by external user-authored content (a prompt, a campaign template) has needs unknowable to a static manifest.
**Decision:** A manifest may declare `needs: dynamic`, in which case the step's effective needs are derived from config: `uses:` for AI steps, the values of `variables:` for deliver steps. The runner projects exactly those fields; plan validates each against upstream provides; a referenced field nothing provides is a plan error. Mechanics of ADR-004 unchanged — this renames the concept from an AI special case to a general one with (currently) two instances. Additionally: per-record completeness at deliver time is a runtime contract with explicit policy `on_missing: skip | fail`, default **skip with verdict** — blank merge fields must never send. Skipped records appear in the receipt with reasons. Dry-run receipts for deliver steps render the RESOLVED variables per record (the approval artifact a human reviews before arming).
**Consequences:** One mechanism instead of two; the minimal CSV→send pipeline exercises the full contract spine (identity, registry, both edge mappings, dynamic needs, plan coherence, per-record verdicts, idempotent delivery, armed gate) with two adapters and zero enrichment spend — making it the correct first validation campaign shape.
**Spec impact:** AMEND (not yet applied) — manifest schema (`needs: dynamic`); plan semantics; deliver runtime policy + receipt format; VALIDATION.md gains the CSV→send pipeline as campaign zero, run dry → review resolved-variables receipt → arm at ~10 records into a controlled campaign → re-run same CSV to prove zero duplicate deliveries. **This VALIDATION.md change was applied** (2026-08-15) even though the manifest/plan/runtime mechanics it depends on were not — campaign zero is documented as blocked on that follow-up work, not as runnable today.

#### Registry seeding note (implementation guidance, not an ADR)
Seed `spec/fields/person.json` and `spec/fields/company.json` from the fields the v0 adapters actually touch plus the curated cross-provider overlap; do not exceed ~60 entries in the first pass. Every entry must have a normalization rule and (where comparability matters) a value domain. When in doubt, leave a field namespaced — promotion is cheap, demotion is breaking. Executed 2026-08-15 (37 entries: 31 person, 6 company). Per the human's direction during the apply pass, the v0 seed declares **no value domains** — ADR-017's `seniority`-enum example notwithstanding — because no field yet shows real cross-provider convergence; the `enum` mechanism remains in the registry schema for when one does. Exact canonical *types* (integer-never-range) apply from day one.

### ADR-020: Identity-tier amendments — internal-form LinkedIn URLs, reserved handle tiers
**Status:** Accepted (2026-08-15 — proposed by this reconciliation pass to
resolve the "known gap" flagged under ADR-017; human-approved same day
after two revisions: the shape split into explicit URL fields, and the
one-of needs corollary)
**Context:** ADR-017's known-gap note: a provider's `linkedin_url` field can
carry the public vanity URL or an internal/member-ID form (opaque member-id
slug, Sales-Navigator path). §4's strip-and-lowercase normalization
silently produces two different keys for the same real person — a dedup
failure *within* one tier, which the weak→strong upgrade mechanism cannot
catch. Separately, `github_username` and `twitter_handle` are plausible
additional person identity tiers worth reserving now so the ordering isn't
designed around later.
**Decision:** (1) The observable URL shapes are explicitly distinct
registry fields, so they can never collide under one name: `linkedin_url`
admits the public vanity URL only (its normalization rule rejects any
other shape as invalid), `linkedin_internal_url` holds an internal-form
profile URL, `linkedin_sales_nav_url` a Sales Navigator URL — each stored
as the URL it is, case preserved, never reinterpreted as an extracted
"member ID" (gtme distinguishes shapes; it does not claim to know
LinkedIn's identifier semantics). Adapters classify at their own boundary
(`sales/…` path → sales-nav; opaque `acwaa`/`acoaa`-prefixed token after
`in/`/`pub/`, or `profile`/`talent` paths → internal; otherwise public)
and emit the matching field. Neither non-public field is key material in
v0: v0 never merges identities, so keying on a non-public shape would
permanently fork a person who later arrives under the public form, whereas
falling through to a weaker tier converges via §4's existing upgrade path
once an enrichment resolves the profile and writes `linkedin_url`.
Resolution-by-enrichment (the other option the gap note named) is thus the
recovery path, not a v0 build item. (2) Person identity tiers become:
email > public LinkedIn slug > `gh:` github_username > `tw:` twitter_handle
> name-hash. The handle tiers are implemented in key derivation and listed
in the registry as `reserved: true` — no v0 adapter provides either field;
normalization is the registry's `handle` rule.
**Consequences:** An internal-form-only record keys on a weaker tier
(possibly name-hash) until something resolves the public profile — correct
but weaker dedupe, visible in the ledger rather than silently forked.
Adapters providing github/twitter handles later slot into a fixed ordering.
Splitting the shapes surfaced a needs-model gap: `harvest/profile` needs
*at least one* LinkedIn URL shape, which a flat `required` list cannot say
— hence one-of (`anyOf`) needs in the planner's contract walk (§7), with
harvest re-contracted to accept any shape and provide the resolved public
`linkedin_url` (the recovery path made concrete).
**Spec impact:** AMEND (applied and approved in the v0.4 pass) — §4 tier
list, shape-split rule, reserved-tier prose; §7 one-of needs; §10.4
harvest re-contract; registry entries in `spec/fields/person.json`.

---

## Session packet, 2026-08-16 (groups: the association primitive)

Produced by a design conversation during the incremental validation
campaign (campaign zero and its widenings — see VALIDATION.md's campaign
log). The trigger: campaign zero exposed that delivery dedupe scopes to
the adapter rather than any chosen scope, and that filter verdicts scope
to the run while their meaning is campaign-relative — both symptoms of a
missing model layer between durable facts and per-run bookkeeping.

### ADR-021: Groups — a named association between identities and a context
**Status:** Accepted (2026-08-16 — proposed and iterated in a design
conversation the same day: segments-redundancy, filter-orthogonality,
nondeterminism, and after-the-fact-grouping objections each tested and
absorbed; human-approved. Spec impact not yet applied — a separate
reconciliation-plus-build pass, like ADR-017/018/019's.)
**Naming note:** `groups` is verified safe unquoted as a SQLite table name
(the reserved word is `GROUP`; joins and `GROUP BY` against a `groups`
table coexist cleanly, tested on 3.43). MySQL-class engines reserve
`GROUPS`; irrelevant to v0's SQLite-only contract (§2) and an internal
concern for any future hosted store. The *concept* name was also weighed
against the `GROUP BY` homonym and kept: Unix/IAM groups —
policy-bearing asserted membership, exactly this feature's shape — have
coexisted with SQL aggregation in the same users' heads for decades, and
the alternatives each import a worse wrong meaning (cohort:
time-bucketed analytics; list: static, ESP-collides; audience:
re-narrows to delivery; set: harder SQL collision). One docs discipline
follows: never use bare "group" as a verb meaning aggregate; the word
belongs to the feature.
**Context:** The ledger models durable facts (identities/field_values) and
execution receipts (runs/run_records) crisply, but everything in between —
campaign membership, suppression lists, qualified pools, touch history —
is either smeared across config strings and external tools or accidentally
hardcoded: `deliveries` dedupes per adapter (not per chosen scope), and
filter verdicts persist per run while their meaning is campaign-relative
(so a top-up re-judges — and, the judge being an LLM, possibly re-*rolls*
— people the same campaign already decided on). Every tool in the
category has this association layer in fragments (lists, audiences,
campaign membership, suppression lists), each hardcoding one flavor with
one policy; the general case is one primitive.
**Decision:** A **group** is a named association between identities and a
context: `groups(id, name UNIQUE, note, created_at)` plus an append-only
`group_events(id, group_id, identity_id, event, detail, run_id,
created_at)` with exactly three event kinds — `added`/`removed`
(membership, provenance in detail) and `touched` (a delivery under this
group's banner). Current membership is a view over added/removed (the
ADR-003 append-then-derive pattern). Members are identities, so groups
hold people and companies alike. Groups carry **no type field and no
executable logic**: a group's character (campaign-like, DNC-list-like,
pool-like) is derived from its events and the pipelines that reference it
— a stored type would be an assertion no behavior backs.

Groups participate in pipelines at both ends, plus two gates — all
runner-owned semantics (adapters see only projections, never the ledger),
all plan-validated:

- **Terminus:** a pipeline may end in group membership instead of (or in
  addition to) an external deliver — records that complete the run are
  `added`. This is the recommended campaign decomposition: a *qualify*
  pipeline (source → enrich → filter ⇒ group) runs cheaply and often; the
  group is a durable, reviewable, hand-editable artifact; a separate
  *send* pipeline consumes it deliberately. The judgment-to-money gap
  gets a human-inspectable checkpoint, per the plan-gate principle.
- **Source:** a group can be a pipeline source — members projected from
  the ledger like any record. (The easy, extensional half of ROADMAP's
  "segments as sources"; a filter step remains role-agnostic about where
  its records came from.)
- `require: <group>` / `exclude: <group>` on any step — membership as a
  plan-checkable gate. Exclusion is also the **judgment-memory
  mechanism**: a qualify pipeline that excludes its own output groups
  (`exclude: [q3-qualified, q3-rejected]`) sends only never-judged
  records to the AI filter, so each identity is judged once per scope.
  This is a determinism device before a cost one — an LLM judge is
  stochastic run to run, and set membership freezes its first answer as a
  recorded decision; re-judging becomes a deliberate act (remove from the
  group, change the prompt), never an accident of re-running. Plain set
  arithmetic, inspectable with `gtm query`, replaces any judgment-cache
  mechanism. (A `remember:`-style cache consulting judgment events was
  considered and dropped as redundant sugar over exclude-and-add.)
- `record: <group>` on a deliver step — successful deliveries append
  `touched`; **defaults to the pipeline name**, so every pipeline is
  safely scoped by default and sharing a scope across pipelines is an
  explicit override.
- `suppress: {group: <g>, within: Nd}` on a deliver step — skip records
  with a `touched` in that group/window; skips are receipted with reasons
  (the `on_missing` pattern).

Filtering stays orthogonal: the filter *role* (AI-backed or not — any
adapter emitting VERDICTs qualifies: deterministic rules, verification
services, human review) gates which records continue through this run,
groups persist sets, and only the runner connects them. Single-pipeline
use with no groups at all remains fully supported.

Everything subtler is `gtm query`: segments-as-SQL extend over the group
tables automatically, and an extensional "frozen list" is simply a group
nobody updates. Two affordances follow: `gtm groups add <group>
--from-segment <name>` (or `--query "SQL"`) snapshots an intensional
definition into extensional membership, each `added` event carrying
"segment X evaluated at T" provenance; and because the run layer logs
every person's every run (run_records, step_events, deliveries,
field_values.run_id), any grouping — including a filter's *failers* — is
reconstructable after the fact, so `record:` stays single-valued and the
terminus captures completers only in v0.
**Consequences:** Groups are not redundant with segments: segments
*derive* sets from what the ledger already implies (and re-evaluate, so
membership drifts); groups *assert* sets — hand-picked members, imported
lists, frozen commitments — with membership provenance as an event trail,
plan-checkable references safe enough for the delivery path (arbitrary
segment SQL is not, per the ADR-018 declarative-vs-opaque line), and a
scope key that `touched` events need to exist at all. Segments answer
questions; groups record decisions about sets. "Campaign" needs no entity
and no spec vocabulary — it is a usage pattern (a group a qualify
pipeline fills, a send pipeline consumes, records touches into, and
suppresses against). Delivery suppression becomes chosen rather than
accidental; the current adapter-wide dedupe survives as the idempotency
floor (§8) with group suppression layered above it. `run_records` is
recognizable as a degenerate group (label forced to one execution) — not
refactored, just understood. The deliberately excluded half (option C) is
parked in ROADMAP.md: group-owned rules, intensional self-evaluating
groups, lifecycle state machines, cross-type traversal policies, typed
groups.
**Spec impact:** AMEND (not yet applied) — §3 ledger DDL (two tables + a
membership view), §9 YAML (group terminus and source, `require:`,
`exclude:`, `record:`, `suppress:`), §8 deliver semantics and receipt
lines, §7 plan checks (referenced groups exist; windows well-formed), a
`gtm groups` verb set (list with derived character, `add
--from-segment/--query`), and acceptance criteria. To be applied only
after human approval, as a reconciliation-plus-build pass like
ADR-017/018/019's.
## Implementation Decisions

Predates the ADR log above; recorded per SPEC.md §12. Newest last.

### 2026-08-12 — Module path

**Q:** What Go module path?
**Choice:** `github.com/trevorfox/gtm`.
**Why:** Matches the repo owner's GitHub account. Nothing outside `go.mod`,
imports, and the `make build` ldflags depends on it; rename with a single
`gofmt -r`-style sweep if the repo lands elsewhere.
**Spec impact:** None (spec-invisible, internal naming).

### 2026-08-12 — ULID generation without a new dependency

**Q:** SPEC §3 wants ULIDs; §2 pins a minimal dependency set that has no ULID
library.
**Choice:** A ~90-line `internal/ulid` (48-bit ms timestamp + 80 random bits,
Crockford base32, entropy incremented within a millisecond so IDs minted in a
tight loop still sort in creation order).
**Why:** Cheaper than a dependency for the one property we actually need —
lexicographically sortable unique ids. No parsing, no interop with other ULID
producers is required.
**Spec impact:** None (spec-invisible; satisfies the DECIDED requirement without adding a dependency).

### 2026-08-12 — Identity key aliases (additive table)

**Q:** SPEC §4 says a stronger key replaces a weaker one *in place*. What
happens to the vacated key? A record that later arrives carrying only the old
weak key would find nothing and create the duplicate the spec forbids — and the
cross-run cache (§1 bet 2) would miss on every re-source.
**Choice:** New additive table `identity_aliases(entity_type, identity_key,
identity_id)` (migration `0002`). Every key a record carries that is weaker than
the winner is aliased at create time, and the vacated key is aliased on upgrade.
Lookup prefers a live `identities.identity_key` and falls back to an alias.
`INSERT OR IGNORE` means an alias never re-points, so v0 still never merges two
existing identities — when the strong key is already taken by another identity,
the weaker one is left alone.
**Why:** No change to any DECIDED table or to the wire contract; it only makes
"do not create a duplicate" actually hold across runs. Identity merging remains
out of scope for v0.
**Spec impact:** None (additive table, spec-invisible per the ADR-010 litmus).

### 2026-08-12 — Name-hash key inputs

**Q:** §4 specifies `sha256(lower(full_name) + "|" + lower(company_domain))`.
Which incoming field names, and is the domain normalized?
**Choice:** Name comes from `full_name`, else `name`, else `first_name` +
`last_name`; whitespace is collapsed as well as lowercased. The domain half is
run through the same registrable-domain normalization used for company keys, so
`Acme.com`, `www.acme.com`, and `https://www.acme.com/careers` produce one key.
Company keys accept `domain`, `company_domain`, or `website`.
**Why:** Unnormalized inputs would silently fork one person into several
identities, which defeats the cache. Accepting the obvious column aliases keeps
`csv/source` (§10.1) flag-free, per the §1 "minimal interactive overhead" rule.
**Spec impact:** None (fills in an underspecified normalization rule; doesn't change the DECIDED formula).

### 2026-08-12 — Provenance sentinels for run-less writes

**Q:** `field_values.run_id` is nullable but `step_events.run_id`/`step_id` are
NOT NULL, yet §4 requires an `identity_upgraded` event even for imports that
have no run.
**Choice:** `run_id` is left NULL on field values written outside a run; step
events written outside a run use the sentinels `run_id='(none)'` and
`step_id='(import)'`.
**Why:** Keeps the DECIDED schema untouched while making run-less writes
representable and greppable.
**Spec impact:** None (schema untouched; sentinel values are internal).

### 2026-08-12 — Ledger timestamps and single writer

**Q:** §3 says RFC3339; SQLite string comparison is used for ordering.
**Choice:** All timestamps stored as `2006-01-02T15:04:05.000Z07:00` in UTC, so
lexical order equals chronological order. The `*sql.DB` pool is capped at one
connection.
**Why:** Millisecond precision keeps same-second tie-breaks meaningful; a single
writer avoids `SQLITE_BUSY` between pooled connections at v0 concurrency (the
worker pool defaults to 4 and only the runner writes).
**Spec impact:** None (timestamp format already implied by RFC3339 + DECIDED ordering requirement).

### 2026-08-13 — Built-in adapters run in-process over pipes

**Q:** §1 bet 5 says built-ins are "invoked through the same protocol boundary as
external ones". Does that mean the binary must re-exec itself per step?
**Choice:** No. A built-in runs as a goroutine wired to the runner by two
`io.Pipe`s and speaks the identical NDJSON protocol; external adapters get
`os/exec`. `adapters.Session` is the only thing the runner talks to, so neither
side knows which transport it got.
**Why:** The boundary that matters is the message contract, and this keeps it
exactly one implementation wide. Re-exec would also break `go test`, where
`os.Executable()` is the test binary. Consequence worth knowing: pipes are
unbuffered, so the runner streams input from a goroutine
(`Session.SendStream`) — writing all input before reading deadlocks against an
adapter that starts replying early. That bug was found and fixed in M2.
**Spec impact:** None. Reinforces ADR-005's transport seam: `adapters.Session` is exactly the abstraction a future stdio transport would sit behind.

### 2026-08-13 — Dependencies added beyond §2

**Q:** §2 pins a minimal dependency set; three additions came up.
**Choice:** `github.com/anthropics/anthropic-sdk-go` for the AI engine,
`golang.org/x/term` for the no-echo `gtm secret set` prompt, and
`golang.org/x/net/publicsuffix` (already implied by §4's eTLD+1 requirement).
**Why:** The Anthropic SDK is the vendor-supported client — hand-rolling the
Messages API would mean owning request shapes, retries and error classification
for no gain. `x/term` is the only sane way to read a secret without echoing it.
Everything else in §2 stands; there is no HTTP framework, no CLI framework, and
no ORM.
**Spec impact:** None (implementation dependency, not an observable-behavior change; recorded here per CLAUDE.md's "no dependencies beyond §2 without a Decision" rule).

### 2026-08-13 — AI model default, and how cost is attributed

**Q:** §2 decides model `claude-sonnet-4-6`.
**Choice:** Kept as the default, overridable per step (`model:` in `with`) or
globally (`GTM_AI_MODEL`). A small static price table in
`internal/ai/pricing.go` turns token usage into the COST messages the receipt
sums; an unknown model reports *unpriced* rather than guessing, and the receipt
prints `?`.
**Why:** The spec's model is real and appropriate for batch classification.
`claude-sonnet-5` has since shipped at the same list price and is the better
default when you want it — hence the override rather than a hard-coded id.
Refusing to invent a price for an unknown model keeps the receipt trustworthy.
**Spec impact:** None (the default stays as DECIDED; the override is additive).

### 2026-08-13 — Prompt-and-validate instead of structured outputs

**Q:** Should AI steps use the API's structured-output mode?
**Choice:** No. The adapters send a strict output contract in the system prompt,
validate the answer (JSON array, one element per record, keys that match the
batch, correct types), and on failure retry once with the validation error
appended — exactly the loop §2 specifies.
**Why:** It is what the spec describes, it works on every engine including the
`claude-code` CLI and the fixture engine, and structured outputs are not
available on the spec's default model anyway. The validator is also stricter than
a schema: it catches invented, dropped and duplicated identity keys.
**Spec impact:** None (implements the DECIDED retry loop as written).

### 2026-08-13 — A fixture AI engine, selected by environment

**Q:** M5 says "tests use a fake engine". How does the test choose it without
polluting the pipeline format?
**Choice:** `GTM_AI_ENGINE=fixture` plus `GTM_AI_FIXTURE=<script.json>`, a JSON
array of scripted responses. `engine:` in a pipeline still accepts only the two
engines §2 defines. The sentinel response `"$auto"` makes the engine synthesize a
schema-valid answer for whatever batch is in flight, so a test can exercise
batching without hard-coding identity keys. One script is shared by every AI step
in a process, so a run consumes it in order.
**Why:** Keeps the public config honest while making the retry path (garbage,
then valid) testable offline and deterministically.
**Spec impact:** None (test-only selection mechanism; `engine:` in the YAML is unchanged).

### 2026-08-13 — Wildcard `needs` means "project everything"

**Q:** The runner builds each projection strictly from the `needs` properties. An
AI step wants whatever is known, which no property list can express.
**Choice:** A `needs` schema that is open-ended and names no properties
(`additionalProperties: true`, no `properties`) marks the step as *needs-all*;
the runner projects every field the ledger holds for the record. `gtm plan`
prints "projects: (every field known about the record)".
**Why:** Without it, an AI filter would receive an empty object. Expressing it in
the schema keeps the rule in the manifest rather than in an adapter-specific
branch in the runner.
**Spec impact:** None. Superseded in spirit by ADR-004 for AI steps specifically — `uses:` is now the mechanism an AI step declares its dynamic needs with; this wildcard convention remains the general-purpose "needs everything" idiom for non-AI steps that want it.

### 2026-08-13 — Cache-skip also checks provenance

**Q:** §7 skips a record when "every field in the step's provides already has a
current value". An adapter whose provides schema has optional properties can
never satisfy that, so it would be re-called (and re-paid for) forever.
**Choice:** A record is skipped when all provides fields are fresh **or** when
that adapter (matched on `id@version`) wrote anything for the record inside the
freshness window. Events record which rule fired (`fresh_in_ledger` /
`already_answered_by_adapter`).
**Why:** It answers the question the cache exists to answer — have we already
paid this provider for this record — and a version bump still invalidates.
**Spec impact:** None (refines a DECIDED cache rule's edge case without changing the observable skip/don't-skip contract for the common case).

### 2026-08-13 — Optional credentials

**Q:** §6 makes a missing declared credential a plan-time error. An AI step that
runs on the `claude-code` engine needs no API key, so declaring
`ANTHROPIC_API_KEY` as required would fail plans that are fine.
**Choice:** New additive manifest field `credentials_optional`: injected when
present, reported by `gtm plan` as a warning when absent, never a plan error.
Required credentials behave exactly as §6 says.
**Why:** Keeps "fail before spending" for the cases where the key is genuinely
required, without inventing conditional-credential syntax.
**Spec impact:** None (additive manifest field; §6's required-credentials rule is unchanged).

### 2026-08-13 — A probed CSV schema is closed

**Q:** `csv/source` provides whatever its header says. Should the probed schema
stay open-ended?
**Choice:** No. The static manifest schema is open (the planner may have no
config to probe with), but a **probed** header is exact, so that schema is closed
(`additionalProperties: false`).
**Why:** It is what makes `gtm plan` catch "this pipeline needs `linkedin_url`
and your CSV has no such column" before a single record is processed, instead of
discovering it per record at run time.
**Spec impact:** None (an implementation-level tightening of an already-open manifest schema).

### 2026-08-13 — Delivery idempotency keys are canonicalized

**Q:** §8 keys a delivery on "the value of the field named by `idempotency`".
Taken literally, `Jane.Doe@Acme.com` and `jane.doe@acme.com` are two different
keys — and the same person gets mailed twice.
**Choice:** Trim always; when the value parses as an email, lowercase it through
the same `identity.NormalizeEmail` the ledger uses. Non-email values keep their
case, because an external id legitimately can be case-sensitive.
**Why:** Double-delivery is the exact failure this table exists to prevent, and
email is case-insensitive everywhere it matters. Found by a test that asserted on
the stored key.
**Spec impact:** None (closes a real gap in §8's idempotency rule without changing the field it keys on).

### 2026-08-13 — Pipe mode is stage-buffered, and adapters can be discovered from a path

**Q:** How much streaming does pipe mode really do, and how do tests reach
external adapters that are not installed in `~/.gtm/adapters`?
**Choice:** Each pipe stage reads its whole input before dispatching, then emits.
`GTM_ADAPTER_PATH` (colon-separated) is searched before `~/.gtm/adapters`.
**Why:** Batching (one adapter invocation per `batch_size` records) and per-step
cache accounting both need the full working set anyway, and buffering keeps the
run/pipe semantics identical — the acceptance test relies on `gtm freeze`
producing a pipeline that runs the same way. Per-record streaming is a v1
question. The search path is how the repo's own fixture adapters are found
without installing anything.
**Spec impact:** **PARTIALLY SUPERSEDED by ADR-005.** The stage-buffering half
documented pipe mode, which is deleted from v0 entirely (see AUDIT.md for the
dead-code removal). The `GTM_ADAPTER_PATH` discovery mechanism is unaffected —
it's used by the e2e test harness independent of pipe mode — and remains live.

### 2026-08-13 — Provider exit codes survive the runner

**Q:** §8 defines exit codes 3 auth, 4 rate-limited, 5 network. Adapters are the
things that meet providers.
**Choice:** `internal/httpx` classifies provider failures and the error carries an
`ExitCode()`; the runner wraps errors with `%w`, and `gtm run` / the pipe verbs
exit with that code. An external adapter that exits 2/3/4/5 has its code
preserved through `adapters.ExitError`.
**Why:** "Rate limited" and "your key is wrong" deserve different retry
behaviour from a caller or a cron wrapper, which is the whole point of the code
table.
**Spec impact:** None (implements the DECIDED exit-code table faithfully).

### 2026-08-13 — Saved segments live in the ledger

**Q:** §8 has `gtm query --save NAME` but does not say where the SQL goes.
**Choice:** Migration `0003_saved_queries.sql`, a `saved_queries` table.
**Why:** A segment is a statement about the ledger's contents; it belongs beside
the ledger, gets backed up with it, and needs no new file format. `gtm query` is
enforced read-only twice over: the statement must be a single SELECT/WITH/EXPLAIN,
and it runs on a connection opened `mode=ro`.
**Spec impact:** None (fills in the storage location `--save` left unspecified).

### 2026-08-15 — M7 internals: embedded registry, injected variables, fixture upgrades

**Q:** Where does the runtime read the field registry from, how does a deliver
adapter receive the step-level `variables:` mapping, and what happens to the
fixtures the old hard-coded contracts leaned on?
**Choice:** (1) `spec/fields/*.json` is embedded via a tiny `package spec`
(`spec/embed.go`) and parsed once per process (`internal/registry`, sync.Once);
a conformance test asserts embedded == on-disk so the binary can never
disagree with the artifacts it was built from. Rule ids resolve to the same
`internal/identity` functions §4 key derivation uses — one implementation per
rule, as §4a demands. (2) The runner injects `variables` into the OPEN
`config` map (now stated in SPEC §9): the adapter owns the egress mapping,
the runner owns projection and the `on_missing` completeness check, so
neither reimplements the other. (3) `mock/deliver` upgraded to the dynamic
contract (email floor + `variables`) so the campaign-zero shape is
e2e-tested offline end to end; `mock-enrich-py`'s fields became
`mock.score`/`mock.note` and `apollo_id` became `apollo.id` under §4a's
namespacing rule. AI compose output is trimmed at the adapter boundary —
canonical values must be fixed points of their rule, and models emit stray
whitespace.
**Why:** Everything observable landed in SPEC.md v0.4; these are the
internal seams that make it hold.
**Spec impact:** None beyond v0.4 (the OPEN-config sentence was added to §9
as part of that pass).
