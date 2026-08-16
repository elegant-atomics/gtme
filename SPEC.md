# gtm — SPEC.md (v0)

A CLI for GTM data pipelines. An append-only SQLite ledger as the data bus,
schema contracts between steps, and AI transforms as first-class steps.
Think: "Singer spec for outbound" with an n8n-style accumulating record
model — but the accumulation lives in the database, not the stream.

This document is written to be executed autonomously by a coding agent.
Every architectural question raised during design has been decided.
Sections marked **DECIDED** are not open for reinterpretation.
Section 12 defines what to do when something genuinely underspecified appears.

The key words **MUST**, **MUST NOT**, **REQUIRED**, **SHALL**, **SHALL NOT**,
**SHOULD**, **SHOULD NOT**, **RECOMMENDED**, **MAY**, and **OPTIONAL** in this
document (in DECIDED sections, and in the acceptance criteria of §0a) are to
be interpreted as described in RFC 2119. They are used deliberately, not
sprinkled: a plain-prose sentence in a DECIDED section that contains none of
these words is still normative (it is DECIDED), it just had no need for a
strength qualifier. §0, §1, and ROADMAP.md are explicitly non-normative and
use ordinary prose throughout.

---

## 0. Design principles (context, non-normative)

These are not requirements — §2 onward is where requirements live. This
section exists so a proposal to change something can be checked against
*why* it is the way it is. A principle here is one that settled a real
argument during design, not a restatement of good taste. One page, by
design: if it grows, something in it has stopped being load-bearing.

1. **The ledger is the bus.** Steps never pass full records to each other;
   they read projections from the ledger and write facts back to it. This
   is not a style preference — cache-aware waterfalls, segmentation over
   history and treatment, resume, and receipts all fall out of this one
   structural choice (see §1 bet 1, §3).
2. **Expressive vocabulary, closed grammar.** The pipeline surface (§9) has
   almost no room to hallucinate structure — a small, enumerable set of
   keys, not an open-ended DSL. Expressivity comes from composing a small
   vocabulary, not from the vocabulary itself being large.
3. **Adapters stay thin so vendors write them.** The contribution surface
   is adapters, not the runner (§1 bet 5). A thin adapter is one an agent
   can generate correctly from an API doc; keeping adapters thin is what
   keeps that true.
4. **Errors are prompts.** Every failure — a plan error, a validation
   failure, an exit code — names its fix, not just its symptom. "compose
   needs `recent_posts`, nothing provides it" is the target shape; a bare
   stack trace is a bug in the error, not just the code.
5. **The human supervises intent and money, the agent operates.** The
   operator states intent (a pipeline) and approves spend (plan, then
   run); the system executes the mechanical parts. §12's stop conditions
   are this principle enforced.
6. **Safe-to-retry is what permits autonomy.** Idempotent delivery (§8),
   cache-skip (§7), and resumable runs (§11) exist so that re-running
   something is never a risk decision — which is what makes it acceptable
   for an agent to be the one re-running it.
7. **Plan is the turn-taking point.** `gtm plan` is where contract
   validation, missing-credential checks, and cost estimation happen —
   before any record moves and before any money is spent. It is the single
   place a human or an agent checks work before committing to it.
8. **Every question has a deterministic answer path.** "What do we know
   about this record" is `gtm show`. "What happened in this run" is the
   receipt or `gtm runs`. "Who's in this segment" is `gtm query`. No
   question about system state requires reading logs or guessing.
9. **Destructive edges are gated, everything else is replayable.** Sending
   money or email is a human-gated, real-world action (§12). Nearly
   everything else — a run, a step, a query — can be repeated without
   consequence, because the ledger is append-only and idempotency keys
   exist precisely where replay would otherwise double-act.
10. **Spec every agreement, spec no mechanism.** SPEC.md fixes observable
    behavior — what a second clean-room implementation would need to
    interoperate (DECISIONS.md ADR-010's litmus). It does not fix package
    layout, concurrency strategy, or naming; those are Claude Code's to
    decide and record in DECISIONS.md.

---

## 1. Philosophy (context, not tasks)

**Principle: maximum expressivity, minimal interactive overhead.**
The operator only ever states intent. No field-mapping flags, no credential
flags, no batching flags. Contracts, manifests, and the ledger carry
everything else.

Core bets:

1. **The ledger is the bus.** Steps do not pass full records to each other.
   They read projections from the ledger and write fields back. The logical
   record gets wider; the physical storage never does (append-only field rows).
2. **Cache and run are separate layers.** What we *know* about an identity is
   durable and cross-run. What *happened* in a run (membership, step states,
   cost) is per-run. Facts belong to identities; runs only touch identities.
3. **Contracts make AI steps safe.** Every step declares `needs` and
   `provides` as JSON Schema. The runner validates the plan before spending
   money, projects only declared fields into each step, and validates output
   before it reaches the ledger. Nondeterministic steps are contained by the
   same gate as deterministic ones.
4. **YAML is the one pipeline surface.** `gtm run pipeline.yaml` is how a
   pipeline executes, full stop — see DECISIONS.md ADR-005, which cut the
   pipe-mode authoring surface (`gtm source ... | gtm enrich ...`) from v0
   entirely: it tested none of v0's real hypotheses and was the recurring
   source of spec inconsistencies. `gtm freeze RUN_ID` still exists, doing
   a smaller but still useful job: it reconstructs the exact `pipeline.yaml`
   that produced a given run from the run's stored config snapshot — a
   reproducibility and audit tool, not a mode-conversion tool. See
   ROADMAP.md for pipes potentially returning post-v0 as a *transport*
   under this same YAML-defined pipeline object, not as a second grammar.
5. **Adapters are processes speaking NDJSON.** Language-agnostic. v0 ships
   built-in adapters compiled into the main binary, but they are invoked
   through the same protocol boundary as external ones, and one external
   adapter (in a different language) proves the boundary is real.

Long-term vision this v0 must not foreclose (but must NOT build):
multi-entity account-based plays via the relations table, SQL segments as
pipeline sources, waterfall combinators over interchangeable providers,
cost dashboards, shared ledgers. See ROADMAP.md for the
currently-named (still undecided) extensions: the `expand` role, pipes as a
transport, the `listen` verb, a REPL, and MCP as a control-plane doorway.

---

## 2. Stack — DECIDED

- **Language:** Go (>= 1.22). The binary MUST be a single static executable
  named `gtm`. Implementation MUST use the standard library plus, at most,
  these dependencies: `modernc.org/sqlite` (pure-Go SQLite, no cgo),
  `santhosh-tekuri/jsonschema/v5` (JSON Schema validation), `gopkg.in/
  yaml.v3`. Any additional dependency MUST be recorded as a Decision in
  DECISIONS.md before use (per §12; see the existing dependency-addition
  entries for the ones already justified: the Anthropic SDK, `x/term`,
  `x/net/publicsuffix`).
- **Ledger:** SQLite, single file, default `~/.gtm/ledger.db`, overridable
  via `GTM_LEDGER` env var. MUST run in WAL mode.
- **Wire format:** NDJSON (newline-delimited JSON) on stdin/stdout.
- **External adapter proof:** one adapter MUST be written in Python
  (`adapters/mock-enrich-py/`), a plain script speaking the protocol.
- **AI engine:** Anthropic Messages API (model `claude-sonnet-4-6`,
  key from `ANTHROPIC_API_KEY`) is the default engine. A second engine,
  `claude-code`, SHALL shell out to `claude -p --output-format json` when
  the binary exists on PATH. Engine is selected per AI step config
  (`engine: api | claude-code`), default `api`. Both MUST produce output
  validated against the step's `provides` schema; on validation failure,
  the runner MUST retry once with the validation error appended to the
  prompt, then fail the batch.
- **No DuckDB.** Query features MUST use plain SQLite SQL.

---

## 3. Ledger schema — DECIDED

Append-only for facts. Migrations via numbered SQL files embedded in the
binary (`internal/ledger/migrations/000N_*.sql`), applied at open.

```sql
-- Layer 1: identity (durable, cross-run, the cache)

CREATE TABLE identities (
  id           TEXT PRIMARY KEY,          -- ULID
  entity_type  TEXT NOT NULL,             -- 'person' | 'company' (extensible)
  identity_key TEXT NOT NULL,             -- canonical key, see §4
  created_at   TEXT NOT NULL,             -- RFC3339
  UNIQUE(entity_type, identity_key)
);

CREATE TABLE field_values (
  id          TEXT PRIMARY KEY,           -- ULID
  identity_id TEXT NOT NULL REFERENCES identities(id),
  field       TEXT NOT NULL,              -- e.g. 'email', 'linkedin_url'
  value       TEXT NOT NULL,              -- JSON-encoded value
  source      TEXT NOT NULL,              -- adapter id, e.g. 'harvest/profile@1'
  confidence  REAL NOT NULL DEFAULT 1.0,  -- 0.0–1.0
  run_id      TEXT,                       -- provenance; nullable for imports
  created_at  TEXT NOT NULL
);
CREATE INDEX ix_fv_lookup ON field_values(identity_id, field, created_at DESC);

CREATE TABLE relations (
  from_id    TEXT NOT NULL REFERENCES identities(id),
  relation   TEXT NOT NULL,               -- e.g. 'works_at'
  to_id      TEXT NOT NULL REFERENCES identities(id),
  created_at TEXT NOT NULL,
  PRIMARY KEY (from_id, relation, to_id)
);

-- Layer 2: runs (per-execution working set)

CREATE TABLE runs (
  id          TEXT PRIMARY KEY,           -- ULID
  pipeline    TEXT NOT NULL,              -- name or '(adhoc)'
  config_json TEXT NOT NULL,              -- resolved pipeline config snapshot
  started_at  TEXT NOT NULL,
  finished_at TEXT,
  status      TEXT NOT NULL DEFAULT 'running'  -- running|done|failed
);

CREATE TABLE run_records (
  run_id      TEXT NOT NULL REFERENCES runs(id),
  identity_id TEXT NOT NULL REFERENCES identities(id),
  state       TEXT NOT NULL,              -- last completed step id, or 'sourced'
  verdicts    TEXT NOT NULL DEFAULT '{}', -- JSON: {step_id: pass|fail}
  PRIMARY KEY (run_id, identity_id)
);

CREATE TABLE step_events (
  id          TEXT PRIMARY KEY,           -- ULID
  run_id      TEXT NOT NULL,
  step_id     TEXT NOT NULL,
  identity_id TEXT,                       -- null for step-level events
  event       TEXT NOT NULL,              -- claimed|done|failed|skipped_cache
  detail      TEXT,                       -- JSON
  created_at  TEXT NOT NULL
);

CREATE TABLE costs (
  id          TEXT PRIMARY KEY,
  run_id      TEXT NOT NULL,
  step_id     TEXT NOT NULL,
  identity_id TEXT,
  provider    TEXT NOT NULL,
  amount_usd  REAL NOT NULL,              -- 0 allowed (unknown/free)
  detail      TEXT,                       -- JSON (credits, tokens, etc.)
  created_at  TEXT NOT NULL
);

CREATE TABLE deliveries (
  id             TEXT PRIMARY KEY,
  identity_id    TEXT NOT NULL,
  target         TEXT NOT NULL,           -- adapter id
  idempotency    TEXT NOT NULL,           -- computed key, see §8 deliver
  run_id         TEXT NOT NULL,
  created_at     TEXT NOT NULL,
  UNIQUE(target, idempotency)
);

-- The current-value projection (ADR-003) is two views, not one: "highest
-- confidence within the freshness window" cannot be answered by an
-- unparameterized view, because the window is a per-caller argument (§7's
-- per-step `cache:`), not a fact about the row. field_value_ranks is the one
-- definition of the RANKING rule (confidence DESC, ties broken by newest
-- created_at); current_fields is that ranking with no window applied
-- (rank = 1) — the plain `gtm query` answer. The runner's windowed
-- projection reads field_value_ranks directly and takes the first in-window
-- row per field, falling through a stale top-ranked row to the next-best
-- one — the same ranking, just windowed where the window is actually known.
CREATE VIEW field_value_ranks AS
SELECT id, identity_id, field, value, source, confidence, run_id, created_at,
       ROW_NUMBER() OVER (
         PARTITION BY identity_id, field
         ORDER BY confidence DESC, created_at DESC
       ) AS rank
FROM field_values;

CREATE VIEW current_fields AS
SELECT identity_id, field, value, source, confidence, run_id, created_at
FROM field_value_ranks
WHERE rank = 1;
```

**Current-value resolution (ADR-003):** the current value of a field is the
row with the highest confidence among rows within the field's freshness
window; ties broken by newest `created_at`. The *ranking* half of this rule
MUST be expressed as exactly one artifact — the `field_value_ranks` view
defined above, mirrored verbatim in `spec/ledger.sql` — and both the
runner's per-record projection (internal, `internal/ledger/project.go`,
which applies the per-step window against ranked rows) and `gtm query`'s
`current_fields`-referencing examples (§8, the unwindowed rank-1 answer)
MUST resolve through it. No second implementation of the ranking rule may
exist.

---

## 4. Identity keys — DECIDED

Canonicalization lives in the runner (`internal/identity/`), never in adapters.
The normalization rules the tiers below depend on are defined once, in the
canonical field registry (§4a), and MUST be implemented exactly once — key
derivation here and ingress normalization (§10.1) share the same
implementations.

- `person`: first non-empty of
  1. lowercased, trimmed email
  2. normalized **public-form** LinkedIn slug (strip protocol, host, trailing
     slash, query; lowercase; e.g. `in/jane-doe`) — internal-form URLs are
     excluded; see below
  3. normalized GitHub username, prefixed `gh:` (reserved tier — see below)
  4. normalized Twitter/X handle, prefixed `tw:` (reserved tier — see below)
  5. `sha256(lower(full_name) + "|" + lower(company_domain))` prefixed `nh:`
- `company`: registrable domain (eTLD+1, lowercased; use
  `golang.org/x/net/publicsuffix`), else `sha256(lower(name))` prefixed `nh:`.

If an incoming record matches an existing identity on a *stronger* key than
it was created with (e.g. name-hash identity later gains an email), the
runner MUST update `identity_key` in place — it MUST NOT create a
duplicate. It MUST log a `step_events` entry `event='identity_upgraded'`.

**LinkedIn internal forms (ADR-020).** A LinkedIn URL in the wild is one of
two incompatible shapes: the public vanity URL (`linkedin.com/in/jane-doe`)
or an internal/member-ID form — an opaque member-id token in the path (e.g.
`in/ACwAA…`), or a Sales-Navigator-style path (`sales/lead/…`). Applying
the strip-and-lowercase rule to both would silently yield two different
keys for the same real person, a dedup failure within a single tier that
the §4 upgrade mechanism (which only handles weak→strong across tiers)
cannot catch. The registry therefore separates the observable URL shapes
into explicitly distinct fields, so they can never collide under one name —
each stored as the URL it is, never reinterpreted as an extracted
identifier (gtme distinguishes shapes; it does not claim to know LinkedIn's
identifier semantics):

- `linkedin_url` admits the **public vanity URL only**. Its normalization
  rule MUST reject any other shape as invalid — rejected, not silently
  reshaped — so key derivation only ever sees public-form slugs.
- `linkedin_internal_url` holds an internal-form profile URL (an opaque
  token in the `in/` path); `linkedin_sales_nav_url` holds a Sales
  Navigator URL (`sales/…` path). Both trimmed, case preserved. Neither is
  key material in v0: v0 never merges identities (DECISIONS.md, identity
  aliases), so keying on an internal shape would permanently fork a person
  who later arrives under the public form, whereas falling through to a
  weaker tier converges via the upgrade path when an enrichment resolves
  the profile and writes `linkedin_url`.

An adapter whose provider hands it a LinkedIn-URL-shaped value MUST
classify it at its own boundary (§4a: vendor dialect → canonical) and emit
the matching field: a `sales/…` path is `linkedin_sales_nav_url`; an
`in/` (or `pub/`) path whose slug begins with an opaque member token
(case-insensitive `acwaa` / `acoaa` followed by a base64-like tail) — or a
`profile`/`talent` first path segment — is `linkedin_internal_url`;
everything else under `in/`/`pub/` is `linkedin_url`. The heuristic errs
toward the non-key fields (recoverable by enrichment + upgrade) over
keying on a non-public shape (not recoverable).

**Reserved handle tiers (ADR-020).** `github_username` (key prefix `gh:`)
and `twitter_handle` (key prefix `tw:`) are person identity tiers ranked
below the LinkedIn slug (LinkedIn stays primary for B2B) and above the
name-hash fallback, in that order. Both are globally unique, public,
low-collision handles. No v0 adapter provides either field; the tiers are
fixed now so the ordering does not get designed around later. Normalization
is the registry's `handle` rule: trim, strip a leading `@`, strip a
`github.com/` / `twitter.com/` / `x.com/` URL prefix, lowercase.

---

## 4a. Canonical field registry — DECIDED

`needs`/`provides` matching is string equality; it is only meaningful if
adapters agree on field names *and* value shapes (ADR-017). The registry is
that agreement: a canonical field registry per entity type lives in
`spec/fields/<entity_type>.json` (currently `person.json`, `company.json`) —
machine-checkable artifacts, loaded directly by the implementation and the
test suite, per ADR-010. `spec/schemas/field-registry.schema.json` is the
schema for the registry files themselves.

Each registry entry declares: `name`, `tier`, `type`, optional `format`,
`normalization` (a named rule, see below), optional `enum` (the canonical
value domain, where comparability matters), optional `items_type` (for
arrays), optional `reserved` (declared but provided by no v0 adapter),
`description`, and `example`.

**Scope rule:** a field is canonical when it crosses an adapter boundary.
Three tiers:

1. **Identity fields** (`tier: identity`, mandatory): person = `email`,
   `linkedin_url`, `first_name`, `last_name` (plus the reserved
   `github_username`, `twitter_handle` — §4); company = `company_domain`,
   `company_name`. These back identity-key derivation (§4); their
   normalization rules are part of the registry, not implicit in the
   key-derivation spec.
2. **Canonical core** (`tier: core`): any field that (a) ≥2 adapters
   provide, (b) is waterfall/dedupe-relevant, or (c) is commonly consumed
   by compose/deliver steps. Canonical fields MAY declare a canonical
   VALUE domain (`enum`) — without value normalization, waterfalls compare
   incomparables — but a domain is declared only where real providers
   demonstrably converge, never guessed at design time: a wrong guess is a
   breaking change waiting to happen. The v0 seed declares none. The
   milder, structural form of the same idea does apply from day one:
   canonical types are exact (`company_employees` is an integer, never a
   range string).
3. **Vendor namespace**: everything else as `<vendor>.<field>` (e.g.
   `apollo.id`). Stored with provenance, queryable, usable in `uses:` and
   `variables:`. A canonical name MUST NOT contain a dot; a dot marks a
   namespaced name. Namespaced fields in a pipeline's needs make vendor
   coupling visible; `gtm plan` MUST note them.

**Promotion:** namespaced → core when a second adapter provides the same
fact (the rule of two). Additive registry changes are non-breaking (one
ADR line). Renames are breaking: spec amendment + version bump.

**Normalization rules** are named, and each name maps to exactly one
implementation (`internal/identity` and nothing else — the same functions
§4 key derivation uses). v0 rule ids: `none`, `trim`, `lower` (trim +
lowercase), `email` (§4 tier 1), `domain` (registrable domain, eTLD+1),
`linkedin_url` (public vanity URLs canonicalized to
`https://www.linkedin.com/<slug>`; any other LinkedIn shape is an
*invalid* value for this rule — it belongs in `linkedin_internal_url` or
`linkedin_sales_nav_url`, §4), `handle` (§4 reserved tiers). A stored canonical value MUST be a
fixed point of its field's rule.

**Enforcement, three layers:**

1. **Manifest validation:** every property name in a manifest's static
   `needs`/`provides` schemas — and every field named in a step's `uses:`
   or `variables:` values — MUST exist in the registry for the adapter's
   `entity_type`, or be vendor-namespaced. A bare unknown name is a
   validation error (`gtm plan` exit 2), which names the field and the
   nearest registry match when one exists.
2. **Runtime:** RECORD output validation (§5) additionally checks canonical
   fields against the registry: declared `type`, `enum` domain, and
   normalized form. A violating record fails per §5 (the run continues).
3. **Adapter conformance kit:** every built-in adapter MUST ship golden
   vendor-payload fixtures and a conformance test asserting fixtures in →
   expected canonical records out, registry-valid. This is the
   machine-checkable finish line adapter authoring (human or generated)
   targets.

Consequence: adapters map vendor dialect → canonical at their own boundary;
nobody downstream thinks about mappings (see §9 and ADR-018: the only two
mapping sites are the csv/source ingress `columns:` and the deliver egress
`variables:`). The registry starts small (seeded from the fields the v0
adapters touch plus the curated cross-provider overlap, ≤60 entries) and
grows by demand, never by design session.

---

## 5. Wire protocol — DECIDED

NDJSON messages on stdout (adapter → runner) and stdin (runner → adapter).
Every message: `{"type": "...", ...}`. Unknown message types MUST be
ignored (forward compatibility). JSON Schemas for every message type below
live in `spec/schemas/` and are the normative machine-checkable form of
this section; the examples here MUST match them.

Runner → adapter:
```
{"type":"OPEN","step_id":"...","run_id":"...","config":{...}}
{"type":"RECORD","key":{"entity_type":"person","identity_key":"..."},"fields":{...}}
{"type":"END"}
```
`fields` contains exactly the projection of the adapter's `needs` — nothing more.

Adapter → runner:
```
{"type":"SCHEMA","provides":{...json schema...}}          // first message
{"type":"RECORD","key":{...},"fields":{...},"confidence":{"email":0.93}}
{"type":"VERDICT","key":{...},"pass":true,"reason":"..."}  // filter steps
{"type":"COST","key":{...}|null,"provider":"harvest","amount_usd":0.012,"detail":{...}}
{"type":"STATE","cursor":{...}}                            // resumable sources
{"type":"LOG","level":"info|warn|error","msg":"..."}
{"type":"END"}
```

A source's outbound RECORD carries `key` OPTIONAL rather than required:
```
{"type":"RECORD","fields":{"email":"jane@acme.com","full_name":"Jane Doe"}}
```
The runner canonicalizes the identity key from `fields` itself (§4) when a
source has no key of its own to offer; every other adapter role receives an
inbound RECORD whose `key` the runner already resolved, and MUST echo it
back on any outbound RECORD/VERDICT/COST it emits for that record.

Rules:
- Sources MUST NOT receive RECORDs; they emit them (after SCHEMA).
- `confidence` is per-field, OPTIONAL, default 1.0.
- COST is best-effort but every v0 built-in adapter that spends money or
  tokens MUST emit it (estimate token cost from the API usage response).
- Output RECORD `fields` MUST be validated against the manifest `provides`
  schema before ledger write; on invalid input the record MUST fail
  (`step_events.event='failed'`), and the run MUST continue.
- Adapters MUST exit 0 on success, non-zero on fatal error; partial output
  before a crash MUST be kept (ledger is append-only), and the run MUST be
  resumable from that point.

---

## 6. Adapter manifest — DECIDED

Each adapter has a `manifest.json`. Built-ins embed theirs; external adapters
ship it next to the executable. Discovery path for external adapters:
`~/.gtm/adapters/<name>/` containing `manifest.json` + executable named `run`.
The canonical schema for this file is `spec/schemas/manifest.schema.json`.

```json
{
  "id": "harvest/profile",
  "version": 1,
  "role": "enrich",              // source|filter|enrich|verify|compose|deliver
  "entity_type": "person",
  "needs": {
    "type": "object",
    "required": ["linkedin_url"],
    "properties": { "linkedin_url": {"type": "string"} }
  },
  "provides": {
    "type": "object",
    "properties": {
      "headline": {"type": "string"},
      "recent_posts": {"type": "array", "items": {"type": "string"}},
      "role_history": {"type": "array", "items": {"type": "string"}}
    }
  },
  "credentials": ["HARVEST_API_KEY"],
  "credentials_optional": ["ANTHROPIC_API_KEY"],
  "config_schema": { "type": "object", "properties": {} },
  "freshness_days": 30,
  "batch": false
}
```

- `credentials`: env var names the runner MUST inject into the adapter
  process. The runner MUST source them from the OS env first, then
  `~/.gtm/secrets` (a `KEY=value` file, mode 0600, written by `gtm secret
  set KEY`). A missing declared credential is a plan-time error.
- `credentials_optional`: env var names injected when present, exactly like
  `credentials`, but a missing one is a `gtm plan` warning, never an error.
  For an adapter that can genuinely work more than one way — an AI step on
  the `claude-code` engine needs no API key at all (§2) — declaring the key
  as `credentials` would fail plans that are actually fine.
- **Dynamic needs (ADR-019):** a manifest whose step contract is defined by
  external user-authored content (an AI prompt, a campaign template) cannot
  enumerate its needs statically. Such a manifest MAY declare its `needs`
  as the string `"dynamic"`, or as an object `{"dynamic": true, ...}` where
  the rest of the object is an ordinary JSON Schema acting as a **static
  floor** (its `required` fields are always needed regardless of config —
  e.g. a deliver adapter that cannot function without `email`). The step's
  *effective* needs are then the floor (if any) plus the config-derived
  list: `uses:` for `filter`/`compose` steps (ADR-004, mechanics
  unchanged), the values of `variables:` for `deliver` steps (§9). The
  planner validates the effective needs exactly as it validates
  `needs.required` (§7), and the runner projects exactly those fields. A
  dynamic filter/compose step declaring no `uses:` projects every field the
  ledger holds for the record (the needs-all behavior, as before ADR-004);
  a dynamic deliver step declaring no `variables:` needs only its floor.
- **Registry validation (ADR-017, §4a):** every property name in a static
  `needs` or `provides` schema MUST be canonical for the manifest's
  `entity_type` or vendor-namespaced (`<vendor>.<field>`). Schemas that
  name no properties (the needs-all wildcard, an open source schema) have
  nothing to check.
- `freshness_days`: default cache window for fields this adapter provides;
  overridable per step (`cache:` in YAML).
- `batch`: marks an adapter the runner MUST feed in `batch_size`-sized
  invocations (§9, default 25 — one adapter session per batch) rather than
  dispatching it across the normal per-record worker pool. `ai/filter` and
  `ai/compose` set this; it is what makes their one-call-per-batch-of-25
  behavior (§10.3, §10.5) a manifest fact the runner reads, not a
  special case hard-coded to those two adapter ids.
- Sources additionally MAY declare `emits_key_fields` (which output fields
  the runner should build the identity key from).

---

## 7. Contract validation & the planner — DECIDED

`gtm plan` (and implicitly `gtm run`) MUST:

1. Resolve every step's adapter + manifest.
2. Walk the pipeline: maintain the set of available fields (source `provides`
   ∪ each prior step's `provides`). For each step, every property in
   `needs.required` MUST be available; failure MUST produce an error naming
   the step and the missing field. No network calls, no spend.
3. Verify all `credentials` across all steps are resolvable.
4. Print the resolved plan: steps, projections, cache windows, and known
   per-record cost estimates (from a static `cost_estimate_usd` optional
   manifest field; print `?` when absent).

**AI-step needs (ADR-004):** an AI-role step's config MAY declare
`uses: [field, ...]` (see §9). When present, the planner MUST treat `uses`
exactly as it treats `needs.required` for step 2 above: every field named in
`uses` MUST be available from the source or a prior step's `provides`, or
`gtm plan` MUST fail naming the step and the missing field. At execution
time, the fields object projected into the step MUST be built from `uses`
rather than from the adapter's static manifest `needs` (which, for AI
adapters, is open-ended precisely because the real needs vary per-prompt and
can only be declared per-step). A step declaring no `uses` and whose
manifest `needs` is the needs-all wildcard (`additionalProperties: true`, no
`properties`) projects every field the ledger holds for the record, as
before ADR-004 — `uses` is how an AI step narrows that to what it actually
needs, gaining plan-time validation in exchange.

**Dynamic needs generalized (ADR-019):** `uses:` is one instance of a
general mechanism. For any step whose manifest declares dynamic needs (§6),
the planner MUST derive the step's effective needs from config — `uses:`
for filter/compose, the *values* of `variables:` for deliver — union the
manifest's static floor, and validate every derived field against the
available set exactly as step 2 above: a referenced field nothing provides
is a plan error naming the step and the field. Effective needs referencing
vendor-namespaced fields (§4a) are valid, but `gtm plan` MUST note the
vendor coupling in its output.

**One-of needs (ADR-020 corollary):** a static `needs` schema whose top
level is `anyOf` of object schemas declares *alternative* ways to satisfy
the step — "at least one of these field sets." The planner MUST accept the
step when at least one branch's `required` fields are all available, and a
failure MUST name every branch and what each branch is missing. The
projection is built from the union of all branches' declared properties
(fields with no value are simply absent, as ever). At runtime, needs
validation validates against the schema itself, where `anyOf` already
carries the same meaning — the planner's walk is the only place needing
the rule stated. The motivating case: `harvest/profile` needs at least one
kind of LinkedIn URL (§10.4), which no flat `required` list can say.

**Dynamic provides (ADR-024; decided 2026-08-16, build queued — see
§11):** the mirror of dynamic needs. A step whose *output* field names are
defined by user config rather than a static manifest — `http/enrich`'s
configured content field, a `sql/enrich` step's declared `provides:`
(§10a) — MAY declare its manifest `provides` as dynamic; its *effective*
provides are then derived from config. The planner MUST add the derived
names to the available-field set for downstream validation exactly as it
adds static `provides`, and downstream `uses:`/`needs` referencing them
validate normally. A derived name MUST be canonical or vendor-namespaced
per §4a — an ad-hoc name is namespaced unless the config maps it to a
canonical field — and the runtime MUST validate the step's output against
the derived provides exactly as it validates static provides (§5).

**Ingress mapping checks (ADR-018):** for a source with a `columns:`
mapping (§10.1), the planner MUST additionally verify, still with no
network calls and no spend: (a) every mapped header exists in the source's
probed schema (a mapping to a column the file does not have is a plan
error); (b) headers that already match canonical names auto-map with zero
config, and a near-miss (an unmapped header a small edit away from a
canonical name) is SUGGESTED in plan output, never silently guessed;
(c) the mapped-plus-auto-mapped field set yields at least one identity-key
tier (§4) — none at all is a plan error, and only the name-hash fallback
tier is a plan warning.

At execution time, per step, per record:
- **Cache check (enrich/verify only):** if every field in the step's
  `provides` already has a current value within the freshness window,
  the runner MUST skip the record (`step_events.event='skipped_cache'`, no
  adapter call, no cost).
- **Projection:** the runner MUST build `fields` strictly from `needs`
  properties (or from `uses`, for AI steps that declare it).
- **Filter verdicts** MUST be stored in `run_records.verdicts`; records with
  `pass=false` stop advancing (state freezes at the filter step) but MUST
  remain in the ledger.

---

## 8. CLI surface — DECIDED

```
gtm init                          # create ledger + ~/.gtm
gtm secret set KEY [VALUE]        # VALUE omitted → prompt, no echo
gtm plan pipeline.yaml            # validate + print plan, no execution
gtm run  pipeline.yaml [--resume RUN_ID] [--dry-run] [--simulate]
gtm query "SQL"                   # read-only SQL against the ledger
gtm query --save NAME "SQL"       # saved segment
gtm show <identity-key>           # read-only projection inspector
gtm show --run last [--fields ...] [--provenance] [--limit N]
gtm runs [RUN_ID]                 # list runs / show one run's receipt
gtm freeze [RUN_ID|last] [--bundle DIR] # bare: YAML to stdout; --bundle: campaign bundle
gtm help --agent                  # machine-readable full CLI + adapter surface
```

This is the entire v0 verb set (ADR-005). There is no `gtm x`, no
multi-process pipe chaining, and no standalone `source`/`filter`/`enrich`/
`compose`/`deliver` subcommands — those existed only to support pipe mode
and are cut along with it. `uses:`, `cache:`, `when:` and every other
per-step option are YAML config (§9), never CLI flags. All human-facing
output (receipts, progress, errors) MUST go to stderr; stdout is reserved
for data (`gtm query`'s result rows, `gtm freeze`'s YAML). Exit codes: 0 ok,
2 validation/contract error, 3 auth/credential, 4 rate-limited, 5 network, 1
other.

**Terminal receipt** (stderr, end of run): records in/out per step, cache
skips, cost per step and total, cost avoided via cache (sum of
`cost_estimate_usd` for skipped records; `?` if unknown).

### `gtm show` (ADR-006)

`gtm show <identity-key>` prints the full current-value projection
(`current_fields`, §3) for that identity: every field, its current value,
and — with `--provenance` — the source adapter, confidence, and run that
wrote it. `gtm show --run last` (or a specific `RUN_ID`) lists the records
touched by that run instead of a single identity. `--fields a,b,c` narrows
the printed fields; `--limit N` caps rows for `--run` mode. `gtm show` is
strictly read-only: it MUST NOT write to the ledger, and it MUST NOT appear
in `gtm freeze` output (it is an inspection tool, not a pipeline step).

### `gtm help --agent` (ADR-007)

Emits the full CLI + adapter surface as one compact (~1–2k token)
machine-readable document, regenerated from the live verb table and the
installed adapter registry — never hand-maintained. It MUST include every
verb and flag from this section, and every installed adapter's manifest
(`needs`/`provides`, in the shape of §6), plus 3 canonical example
pipelines. Acceptance criterion: the document MUST round-trip — an agent
given only `gtm help --agent`'s output and no other context MUST be able to
author a valid `pipeline.yaml` that passes `gtm plan`.

### Event-driven pipelines: webhook/source + cron (ADR-009)

There is no daemon and no long-running receiver process (§13 non-goals). The
v0 answer to "run a pipeline when an event happens" is: a commodity webhook
receiver you already have (a Cloudflare Worker, a Zapier/Make webhook
action, a GitHub Action) appends each incoming payload as one line to a
spool file or directory; a `webhook/source` adapter (§10) reads and drains
that spool the same way `csv/source` reads a CSV; a scheduled `gtm run`
(cron, launchd, CI schedule) invokes the pipeline periodically. At-least-once
redelivery from the receiver is absorbed structurally by the `deliveries`
table's `UNIQUE(target, idempotency)` constraint (§3) — replaying the same
event through the pipeline twice produces at most one delivery. Per-event
low latency is explicitly out of scope for v0.

### deliver idempotency

Idempotency key = the value of the field named by `idempotency` config
(default: the identity key). Before calling the adapter for a record, the
runner MUST check `deliveries`; on hit, it MUST skip (`skipped_cache`
semantics, reason `already_delivered`). On successful adapter RECORD/END
for that record, the runner MUST insert into `deliveries`.

### deliver completeness — `on_missing` (ADR-019)

Per-record completeness at deliver time is a runtime contract: every
`variables:` target (§9) MUST resolve to a non-empty value for a record
before that record may deliver — blank merge fields MUST never send. The
policy when one does not resolve is `on_missing: skip | fail` on the
deliver step, default **skip**:

- `skip`: the record does not deliver; the runner records a fail verdict
  for the deliver step in `run_records.verdicts` with the missing field
  names as the reason, and the terminal receipt lists every skipped record
  with its reason.
- `fail`: the record fails (`step_events.event='failed'`, naming the
  missing fields); the run continues, per §5.

A record missing a *floor* field (§6 dynamic needs) fails needs validation
as any record would; `on_missing` governs the `variables:`-derived fields.

### Dry-run and the armed gate (ADR-019)

`gtm run --dry-run` executes the pipeline normally **except** deliver
steps: for each record reaching a deliver step, the runner resolves the
step's `variables:` per record, applies the `on_missing` policy, and
records `step_events.event='dry_run'` with the fully RESOLVED variable
values in `detail` — but MUST NOT invoke the deliver adapter and MUST NOT
write to `deliveries`. The terminal receipt renders each record's resolved
variables: this is the approval artifact a human reviews before arming.
Non-deliver steps run normally under `--dry-run` — delivery is the gated
destructive edge (§0 principle 9); everything upstream is replayable and
cache-covered, and its spend is already visible in `gtm plan`. Arming is
the same command without the flag; a dry run is an ordinary run in every
other respect (its own run id, receipt, and `gtm runs` entry), and because
it writes no deliveries, the armed run's idempotency behaves as if the dry
run had never happened.

### Simulation gate — `gtm run --simulate` (ADR-028; decided 2026-08-16, build queued — see §11)

`gtm run --simulate` executes the ENTIRE pipeline offline: every binding
(§10a) is served from its conformance fixtures, and every process/AI step
is either fixture-served or stubbed — an AI step replays a recorded
fixture response when one exists, else emits a synthetic verdict marked
as synthetic. A simulated run MUST perform zero network calls, zero
spend, and zero sends, and MUST be deterministic. Its output is a full
receipt marked **SIMULATED** (the receipt schema gains a `simulated`
flag), and a simulated run MUST NOT contribute to the durable identity
layer: its writes are excluded from projection and cache (whether by
ephemerality or by flagging is an implementation decision, recorded in
DECISIONS.md at build time). With this the gate ladder is complete:
**simulate → plan → dry-run → armed** — behavior offline, then contracts,
then live-reads with delivery withheld, then live. An agent that authors
a pipeline can fully validate it (structure via plan, behavior via
simulate) before a human reviews anything. A binding without fixtures
MUST surface in the simulated receipt as a simulation gap, not silently
pass. Acceptance criterion: the campaign-zero pipeline simulates
end-to-end with zero network calls.

### Campaign bundles — `gtm freeze --bundle` (ADR-029; decided 2026-08-16, build queued — see §11)

`gtm freeze --bundle DIR` produces a **campaign bundle**: a directory (or
tarball) containing the pipeline YAML, every referenced binding at its
exact version, AI prompt files, saved queries, the relevant registry
slice, and a manifest — bundle format version, content hashes, source run
id — per `spec/bundle-manifest.json`. (Bare `gtm freeze` keeps its
existing job: the reconstructed `pipeline.yaml` on stdout.) Guarantees:
(a) **self-contained** — `gtm run <bundle-path>` resolves nothing outside
the bundle except credentials; (b) **diffable** — text files, stable
ordering; (c) **portable** — the same bundle runs on any machine/ledger;
membership and cache naturally differ, contracts don't. `gtm run` MUST
accept a bundle path wherever it accepts a pipeline path, and
`--simulate` MUST work on a bundle using fixtures included in it, making
a bundle a fully offline-verifiable artifact. Group references in a
bundled pipeline (ADR-021) are names resolved against the target ledger —
membership travels with the ledger, not the bundle — and plan's
referenced-groups-exist check applies, so a bundle moved to a clean
ledger fails loudly at plan rather than running ungated. Acceptance
criterion: freeze campaign zero, move the bundle to a clean ledger,
simulate and dry-run it successfully.

---

## 9. pipeline.yaml — DECIDED

```yaml
name: apollo-to-instantly
version: 1

source:
  use: apollo/search
  with:
    query: "vp marketing, saas, 50-200 employees"
    limit: 500

steps:
  - id: icp-filter
    use: ai/filter
    uses: [full_name, title, company_domain]   # ADR-004: AI step's dynamic needs
    with:
      prompt: >
        Keep only contacts likely to own outbound tooling decisions.
      batch_size: 25

  - id: linkedin
    use: harvest/profile
    when: icp-filter.passed
    cache: 30d

  - id: personalize
    use: ai/compose
    uses: [recent_posts, role_history]
    with:
      prompt: >
        Write first_line and ps_line using recent_posts and role_history.
      batch_size: 25

deliver:
  use: instantly/add-to-campaign
  with:
    campaign: "Q3 VP Marketing"
  variables:            # ADR-018/019: egress mapping, and the step's dynamic needs
    first_line: first_line
    ps_line: ps_line
  idempotency: email
```

Schema rules: `when:` supports only `<step_id>.passed` in v0. `cache:` takes
`Nd`. `uses:` (ADR-004) is a list of field names, valid only on steps whose
adapter role is `filter`/`compose`/an AI-backed role; the planner validates
it exactly as `needs.required` (§7). `variables:` (ADR-018/019) is valid
only on the deliver step: a map of *target merge-field name* → *canonical
or namespaced ledger field*; its values are the step's dynamic needs (§6,
§7), and the mapping is the egress half of ADR-018 — the only place the
target's foreign vocabulary appears. The runner hands the mapping to the
deliver adapter as `variables` in OPEN `config` (§5) — the adapter owns
applying it; the runner owns projecting and completeness-checking the
fields it references (§8). `on_missing: skip | fail` (default
`skip`) is valid only on the deliver step (§8). The ingress half is
`columns:` inside `csv/source`'s `with:` (§10.1): a map of *canonical field
name* → *CSV header as written*. Both mapping keys read
destination-vocabulary → source-vocabulary. No interior step may carry a
mapping block (ADR-018): declarative mappings at the two edges are
plan-validatable; the interior speaks only canonical names. Steps execute
strictly in order; within
a step, records process with a worker pool (default concurrency 4,
`GTM_CONCURRENCY` to override), except AI steps which process in batches of
`batch_size` (default 25) — one adapter invocation per batch. `waterfall:`
is reserved syntax: the YAML parser MUST accept and reject it with "not
implemented in v0" (so v1 adds it without a format break). This is the only
pipeline authoring surface in v0 (ADR-005); the JSON Schema for this format
is `spec/schemas/pipeline.schema.json`.

---

## 10. v0 adapters — DECIDED

All built-in, Go, under `internal/adapters/<name>/`, each with embedded
manifest + a `fixtures/` dir (sample request/response JSON) + unit tests
that run offline against fixtures.

1. **`csv/source`** — reads a CSV path from config; header row → field
   names; `email`/`linkedin_url`/`name`/`company_domain` columns feed
   identity keys. (Exists so everything is testable with zero API keys.)
   Ingress mapping (ADR-018): config `columns:` maps canonical field names →
   CSV headers as written. Headers already matching canonical names (after
   header normalization: lowercase, separators → underscores) auto-map with
   zero config; near-misses are SUGGESTED at plan time, never guessed (§7).
   Unmapped, non-canonical headers are namespaced `csv.<normalized_header>`
   (§4a tier 3) — kept, queryable, visibly non-canonical. Mapped canonical
   values are normalized per the registry at ingress; a value that fails
   its rule never crashes the run and never reaches the ledger — the field
   is dropped from that record with the reason recorded in `step_events`,
   and the record then fails only if what remains cannot satisfy identity
   derivation or a downstream need.
2. **`apollo/search`** (source, person) — POST
   `api.apollo.io/api/v1/mixed_people/search` with `X-Api-Key`
   (`APOLLO_API_KEY`); config: `query`, `limit`; paginate; map
   name/title/email/linkedin/org fields; also emit the company as a related
   identity: create `works_at` relation when org domain present.
3. **`ai/filter`** (filter) — batch records into the prompt with a strict
   JSON-array output schema `[{identity_key, pass, reason}]`; emit VERDICTs;
   config supports `uses:` (§9, ADR-004).
4. **`harvest/profile`** (enrich, person) — HarvestAPI LinkedIn profile
   lookup (`HARVEST_API_KEY`) by any one LinkedIn URL shape: needs are
   one-of `linkedin_url` | `linkedin_internal_url` |
   `linkedin_sales_nav_url` (§7 one-of needs), preferring the public form
   when several are present. Provides `headline`, `recent_posts`,
   `role_history` — and, when the lookup started from a non-public shape,
   the resolved public `linkedin_url`, which is ADR-020's recovery path
   (the key upgrade to the slug tier follows automatically, §4). Emit COST
   from response metadata if present, else config-estimated.
5. **`ai/compose`** (compose) — batch; provides `first_line`, `ps_line`
   (strings); output schema enforced; config supports `uses:`.
6. **`instantly/add-to-campaign`** (deliver, person) — Instantly v2 API,
   `Authorization: Bearer $INSTANTLY_API_KEY`: create/attach lead to
   campaign by name (resolve campaign name → id via list endpoint once per
   run; error if absent). Declares dynamic needs (§6, ADR-019) with a
   static floor of `email`; everything else it sends derives from the
   step's `variables:` mapping (ADR-018) — a target name matching one of
   Instantly's first-class lead fields (`first_name`, `last_name`,
   `company_name`, `personalization`) maps into the lead body, and any
   other target name becomes a custom variable of that name. No merge
   field is hard-coded in the adapter.
7. **`mock-enrich-py`** (external, Python 3 stdlib only) — reads protocol
   from stdin, adds field `mock_score` (random but seeded from identity
   key), emits COST 0. Proves the external adapter path.
8. **`webhook/source`** (source; entity_type per payload) — near-clone of
   `csv/source` (ADR-009): config `spool_path` (a file or directory of
   NDJSON-per-line event payloads written by an external receiver); each
   line becomes a candidate record the same way a CSV row does; successfully
   ledger-written lines MUST be marked consumed (moved/truncated/deleted per
   `spool_path`'s shape) so a re-run does not re-source them — this is the
   adapter-level half of the redelivery story, distinct from and in addition
   to the delivery-side idempotency in §8.

For Apollo/Harvest/Instantly: implement against their current public docs
(fetch docs at build time via web access if available; otherwise implement
from the fixture shapes and mark the HTTP layer clearly in one file per
adapter so field mappings are trivially correctable). Every HTTP call MUST
be behind an interface so fixture tests never touch the network.

---

## 10a. The binding tier & universal steps — DECIDED (ADR-022..027; decided 2026-08-16, build queued — see §11)

Everything in this section is decided contract; none of it is shipped
behavior until its milestone builds (§11). Until then code/spec
divergence here is queued work, not an AUDIT.md finding.

### Two-tier adapters (ADR-022)

The adapter model is two-tier. **Tier 1 — bindings:** a binding is a YAML
document conforming to `spec/binding-schema.json`, interpreted
deterministically by one generic HTTP execution engine in the runner. All
judgment is frozen at authoring time, never exercised per call. The
schema is kept to ~8 primitives: auth (type, header/param name, env var
ref); a request template (method, URL, body AND query-param templating
from config + canonical fields); pagination (strategy page|cursor|offset,
termination, max); extraction (records JSONPath plus per-field
response→canonical paths, with a `transform:` hook restricted to registry
normalization rules — never arbitrary logic); error→verdict mapping; an
idempotency declaration `native | ledger` (which party guarantees dedupe:
Attio assert = native; Instantly = ledger via the deliveries table); a
cost declaration (per record / per request / unit); and a retry/rate
policy including hourly windows, with an optional session declaration
(a UUID-per-run passed through, for vendors offering
pagination-consistency sessions). Binding roles are source (pagination +
cursor/STATE), enrich (per-record request), and deliver (idempotency +
dry-run receipts). A binding declares the same manifest surface as a
process adapter (`needs`/`provides`/`config_schema`/`freshness_days`, §6)
so `gtm plan` treats both tiers identically; named external bindings are
discovered on the §6 path (`~/.gtm/adapters/<name>/` containing
`binding.yaml` instead of an executable).

**Tier 2 — process adapters:** the §5/§6 NDJSON contract, unchanged.
**Graduation rule (hard):** the moment a binding needs logic —
conditionals, expressions, multi-call workflows, OAuth dances, request
signing, computation — it graduates to a process adapter. No expression
language may ever grow inside binding YAML. Bindings cover anything that
sells an API; process adapters cover anything that must be fought for
(a managed provider absorbing the fight — HarvestAPI over scraping — is
tier 1; only DIY scraping is tier 2).

**Engine unification:** inline `http/*` steps are the binding engine
invoked anonymously with config carried in the pipeline YAML; a named
binding is the same config published, versioned, and conformance-tested.
Recurring inline config across pipelines is the signal to extract and
name a binding.

**Conformance:** the §4a conformance kit extends to bindings: every
binding MUST ship golden fixture payloads and a conformance test
asserting fixtures in → canonical, registry-valid records out. These
fixtures are also what `--simulate` (§8) executes against.

**Security invariant:** bindings MUST NOT be able to execute code; a
binding's blast radius is exactly what the engine permits. This is what
makes third-party bindings reviewable, diffable data (see ROADMAP.md on
the marketplace framing).

### OpenAPI is codegen input, never runtime input (ADR-025)

A runtime OpenAPI-driven generic adapter MUST NOT be built: an API spec
describes endpoints (syntax), while an adapter encodes operation
selection, idempotency keys, verdicts, and canonical mapping (semantics
no spec contains) — and per-call model judgment would mean per-row cost,
nondeterminism where money moves, and records in model context, violating
the ledger-as-bus. OpenAPI's place is bind time: the adapter-authoring
skill's happy path is paste an OpenAPI URL → a model proposes a binding
(operation, mapping, idempotency, pagination) → conformance tests pass →
the adapter exists.

### Adapter naming — the contract owner names the adapter (ADR-026)

An adapter is named by whoever defines its contract. `apollo/search`:
Apollo's API defines the step's meaning → vendor-named. `ai/filter`,
`ai/compose`: the contract is the operation (uses: in, verdict/fields
out); the model provider is an interchangeable engine → operation-named,
provider is config. When a provider capability leaks into the contract
(a response format that IS the product), it takes the vendor name — the
same canonical-when-shared, namespaced-when-proprietary logic as fields
(§4a). Provenance carries the engine: for `ai/*` steps,
`field_values.source` MUST record the model identifier in the form
`ai/compose @ <model-id>` (e.g. `ai/compose @ claude-sonnet-4-6`), and
COST attributes spend per model.

### `http/enrich` — generic fetch enricher (ADR-024)

Per-record HTTP request templated from canonical fields; two modes:
(a) JSON extraction — the binding engine's enrich role invoked inline;
(b) `markdown: true` — fetch a page, convert to markdown, store it as a
ledger field (e.g. `homepage_markdown`) whose name the step declares in
config (dynamic provides, §7). Fetched content fields are facts with
provenance; the step MUST declare `freshness_days` (web content rots — a
missing value is a plan error, no default), and the engine MUST enforce a
response size cap (default an implementation decision recorded at build
time). Division of labor: `http/enrich` does deterministic acquisition;
`ai/*` steps judge the stored content via `uses:` — fetch-once economics
means N AI steps across M runs reuse one fetch, and receipts show exactly
what content was judged. Scope stated honestly: no-JS fetching only;
JS-heavy pages route to a reader-provider binding (see ROADMAP.md).

**`ai/*` purity invariant:** an AI step's inputs are exactly its
projected fields, and its only network access is its model engine's API
(§2). An `ai/*` step MUST NOT fetch external content — acquisition
belongs to `http/enrich` and bindings, which are deterministic and
cacheable.

### `sql/enrich` and `sql/filter` — the deterministic transform floor (ADR-027)

`sql/enrich`: a single SELECT over the projection view (plus relations),
scoped to the run's records, executed read-only (`mode=ro`) and
timeboxed, with no side effects. Result columns become field values
appended by the ENGINE like any adapter output — the step never writes
storage directly; append-only, provenance `sql/enrich @ <query-hash>`,
freshness semantics all preserved. Contracts are DECLARED, not parsed
from SQL: the step's config carries `uses:` and `provides:`; `gtm plan`
validates both (§7 dynamic needs and provides), and the engine MUST check
result columns match the declared provides. `sql/filter`: the same
mechanism producing per-record VERDICTs from a predicate — closing
membership-by-ledger-facts cases ("has replied ever", "3+ known contacts
at company") that `where=` combinators don't reach. `sql/filter` derives
verdicts from what the ledger implies, re-evaluated each run; ADR-021's
`require:`/`exclude:` gates assert recorded membership — complementary,
not competing. The transform floor is symmetric: `sql/*` for the
computable, `ai/*` for the judgeable; both read projections, both write
facts, both free to re-run. (This shrinks ADR-018's code-transform
escape hatch to nearly nothing.)

### The universal floor (ADR-023)

The smallest adapter set with near-total reach docks onto the three
universal transports — files, webhooks, the web. Universality is bought
by pushing semantics into user config, so universal adapters are always
the worst version of any given integration: their job is the guarantee
("wireable today"), not excellence; bindings are the ceiling. The set:
In — `csv/source` (§10.1), `webhook/source` (§10.8), and group-as-source
(ADR-021, spec impact pending); transform — `ai/*` (pure, above) and
`sql/*`; out — `http/deliver` and `csv/deliver` (decided, post-binding-
engine; see ROADMAP.md for their contracts, including `http/deliver`'s
REQUIRED idempotency-key template). Receipts showing the same `http/*`
target recurring across runs are the cue to mint a named binding.

---

## 11. Milestones & acceptance criteria — build in this order

Each milestone ends with `make check` green (fmt, vet, unit tests) plus the
listed acceptance test. Acceptance tests live in `test/e2e/` as Go tests
shelling out to the built binary, using fixture adapters — no network, no
real keys — and MUST load their expected shapes from `spec/` (schemas,
`ledger.sql`, golden wire transcripts, acceptance scripts) rather than
re-encoding them, per ADR-010. Milestones M1–M5 must be completable fully
offline.

- **M1 — ledger + identity.** `gtm init`; internal APIs for upsert identity,
  write field, project record; migrations, including the `current_fields`
  view (ADR-003). ✅ Unit tests: identity canonicalization table-driven
  tests; projection picks highest-confidence-in-window via the view; identity
  upgrade path; a schema-conformance test asserting the migrated schema
  matches `spec/ledger.sql`.
- **M2 — protocol + runner core.** Message types, manifest loading, schema
  validation, projection, cache-skip, worker pool, run/step_events/costs
  writing. ✅ E2E: `csv/source → mock-enrich-py` via `gtm run`, ledger
  contains fields with provenance; second run reports 100% cache skips and
  $ avoided; protocol messages round-trip against `spec/schemas/` and the
  golden wire transcript in `spec/wire/`.
- **M3 — plan + receipts + resume + inspection.** `gtm plan` output;
  missing-field and missing-credential plan errors; AI-step `uses:`
  plan-time validation (ADR-004); `--resume`; terminal receipt; `gtm show`
  (ADR-006); `gtm help --agent` (ADR-007). ✅ E2E: a pipeline with an
  unsatisfiable `needs` (or `uses`) fails at plan with the right step named;
  kill a run mid-step (fixture adapter with induced failure), `--resume`
  completes without re-processing done records; `gtm show` on a known
  identity and on `--run last` matches the ledger; `gtm help --agent`'s
  output round-trips per its acceptance criterion in §8.
- **M4 — AI steps + query + deliver semantics.** `ai/filter`, `ai/compose`
  with output-schema validation + one retry + `uses:` projection; `gtm
  query` (+ `--save`); deliveries idempotency with a `mock/deliver` fixture
  adapter. ✅ E2E: filter verdicts gate downstream steps; malformed AI
  output (fixture engine returning garbage once) retries then succeeds;
  double `gtm run` produces zero duplicate deliveries; saved query returns
  expected rows. (AI engine behind an interface; tests use a fake engine.)
- **M5 — real adapters.** Apollo, Harvest, Instantly against live APIs,
  each with fixture-based unit tests plus a `--live` build tag for manual
  smoke tests; `webhook/source` against a local spool fixture (no live
  dependency — offline-testable like `csv/source`). ✅ The pipeline in §9
  runs end-to-end with real keys (manual gate — see §12); `webhook/source`
  drains a fixture spool without re-sourcing consumed lines on a second run.
- **M6 — polish.** `gtm runs`, README with 60-second quickstart,
  `brew`-style install script, `gtm secret set`.
- **M7 — canonical vocabulary & edge contracts (ADR-017/018/019/020).**
  The field registry (`spec/fields/`, §4a) with all three enforcement
  layers; identity-tier amendments (§4: internal-form LinkedIn rule,
  reserved handle tiers); `columns:` ingress mapping with auto-map,
  suggestions, and the identity-path plan check (§7, §10.1); `needs:
  dynamic` with `variables:` egress mapping (§6, §9, §10.6); `on_missing`
  and `--dry-run` (§8). ✅ Unit: registry files validate against their
  schema and every built-in manifest passes registry validation; internal-
  form LinkedIn URLs fall through to the next tier (table-driven, with the
  public/internal shapes §4 names); each rule id normalizes per §4a; a
  one-of needs step plans when any one branch is available and fails
  naming every branch otherwise (§7). ✅
  E2E, offline: a `columns:`-mapped CSV plans and runs with values
  normalized at ingress and unmapped headers namespaced; a plan against a
  CSV with no identity-key path fails naming the rule, and a name-hash-only
  CSV warns; a deliver step with `variables:` referencing an unprovided
  field fails plan naming step and field; `--dry-run` against a fixture
  deliver adapter renders resolved variables in the receipt, writes zero
  `deliveries` rows, and applies `on_missing: skip` verdicts; the same
  pipeline armed delivers, and re-run delivers nothing twice.

The following milestones were sequenced by the human on 2026-08-16
(bindings before groups: receipt-diff acceptance is strongest while
campaign-zero data and the Go twins are current, and `--simulate` then
de-risks the groups build). Until each builds, its sections above are
decided contract, not shipped behavior.

- **M8 — binding engine + simulation gate (ADR-022, ADR-028; §10a, §8).**
  The generic HTTP engine interpreting `spec/binding-schema.json`;
  `apollo/search`, `harvest/profile`, and `instantly/add-to-campaign`
  ported to bindings; the conformance kit extended to bindings;
  `gtm run --simulate`. ✅ Acceptance: receipt diff against each Go twin
  on campaign-zero data matches (dry runs where delivery is involved);
  first net-new integration is a pure-YAML **Attio** binding (assert
  endpoint, idempotency: native) passing conformance; the campaign-zero
  pipeline simulates end-to-end with zero network calls.
- **M9 — groups (ADR-021).** The reconciliation-plus-build pass its ADR
  defers: spec impact applied (§3 DDL, §7/§8/§9 semantics, `gtm groups`
  verbs), then built. ✅ Acceptance: per the ADR's spec-impact list once
  applied.
- **M10 — campaign bundles (ADR-029; §8).** `gtm freeze --bundle`,
  `gtm run` on a bundle path, simulate-on-bundle. ✅ Acceptance: freeze
  campaign zero, move the bundle to a clean ledger, simulate and dry-run
  it successfully. Sequenced after groups so bundled pipelines carry
  group references against built semantics from day one.

Repo layout:
```
cmd/gtm/            # main
internal/{ledger,identity,protocol,runner,planner,cli,adapters/...}
adapters/mock-enrich-py/
spec/               # machine-checkable artifacts: schemas, fields/, ledger.sql, wire/, acceptance/
test/e2e/
SPEC.md  CLAUDE.md  DECISIONS.md  PROCESS.md  ROADMAP.md  VALIDATION.md  AUDIT.md  Makefile
```

`CLAUDE.md` is the one-page constitution; see that file for its exact
contents (this section does not duplicate it, to avoid the two drifting).

---

## 12. Autonomy protocol

- **Don't ask when:** the spec decides it; or it's an internal detail
  (naming, file layout within the layout above, test structure).
- **Decide-and-record when:** something small is underspecified. Append to
  `DECISIONS.md`: date, question, choice, why. Keep building.
- **Stop and ask when:** (a) an external API's real shape contradicts the
  spec in a way that changes a contract or the ledger schema; (b) anything
  requires spending real money or sending real email — M5 (real adapters)
  live runs and the VALIDATION.md campaign are always a human gate; (c) a
  DECIDED section appears internally inconsistent.
- **Never:** send to a real Instantly campaign, call paid provider
  endpoints, or commit secrets, without an explicit human go.

## 13. Non-goals for v0 (do not build)

Scheduler/daemon (answered instead by the webhook/source + cron spool
recipe, §8), dashboards or any UI, DAG/branching beyond
`when:`, `waterfall:` execution (parse-and-reject only), email waterfall
providers, company-pipeline fan-out verbs (the relations table exists; no
verbs over it — see ROADMAP.md's `expand` role for the deferred version),
teams/auth, MCP server mode (see ROADMAP.md), adapter marketplace,
migrations of the YAML format, Windows support (build for darwin/linux).
Pipe syntax is deferred (ADR-005): all v0 pipeline surfaces compile to the
same pipeline object, authored only as YAML — there is no second, informal
surface to keep in sync with it.

---

## Operator stories — acceptance criteria (ADR-012, normative)

Each story below states its invariant and Given/When/Then acceptance
criteria against the DECIDED mechanics above. These are the normative
counterpart of VALIDATION.md's enactment scripts, which run the same eight
stories as a living, non-normative campaign against real data. A story
"passing" here means: the mechanism it depends on behaves as this section
says, verifiable offline against fixtures.

### Launch
**Invariant:** starting a new pipeline against fresh data produces a
receipted run and a ledger that now knows those identities.
**Given** a valid `pipeline.yaml` and a source with records the ledger has
never seen, **when** the operator runs `gtm plan` then `gtm run
pipeline.yaml`, **then** the run completes with status `done`, every
sourced identity exists in `identities` with at least one `field_values`
row, and the terminal receipt reports non-zero records processed and a
total cost (or `?` where unpriced).

### Top-up
**Invariant:** re-running a pipeline against overlapping data costs less
and delivers nothing twice.
**Given** a completed run whose identities are still within their
adapters' freshness windows, **when** the operator runs the same
`pipeline.yaml` again (e.g. against an updated source with some overlap),
**then** every overlapping identity's enrich/verify steps are skipped via
`step_events.event='skipped_cache'` (§7), the receipt reports cost avoided
> 0, and no identity that was already in `deliveries` for this target
produces a second `deliveries` row (§8 idempotency).

### Interrogate
**Invariant:** what the system knows about one record is always one
command away.
**Given** an identity with field values from more than one adapter,
**when** the operator runs `gtm show <identity-key> --provenance`,
**then** the output lists every current field value (per the
`current_fields` view, §3) together with its source adapter, confidence,
and the run that wrote it, and the command performs no ledger write.

### Iterate
**Invariant:** a pipeline change is checked before it is trusted, cheaply.
**Given** a `pipeline.yaml` edited to add or change a step, **when** the
operator runs `gtm plan` before `gtm run`, **then** any unsatisfiable
`needs`/`uses` or missing credential is reported as a plan error naming
the step and the missing field (§7) before any adapter is invoked; **and**,
once `gtm plan` succeeds, a `gtm run` scoped to a small source (e.g. a
low `limit` in `source.with`) exercises the full step chain end-to-end at
minimal cost before a full-size run.

### Segment
**Invariant:** a slice of accumulated knowledge is a SQL statement away,
and the same slice can seed a new pipeline.
**Given** a ledger with records from one or more prior runs, **when** the
operator runs `gtm query --save NAME "SELECT ..."` with a single
SELECT/WITH/EXPLAIN statement, **then** the statement is stored and
re-executable by name, it runs against a read-only connection (`mode=ro`)
so it cannot mutate the ledger, and its result set can be used to scope
which identities a subsequent pipeline or `gtm show` inspects.

### Guard
**Invariant:** a pipeline that would fail or overspend is caught before
it starts, not partway through.
**Given** a `pipeline.yaml` with a step whose `needs.required` (or `uses`,
for AI steps) is not satisfied by any upstream `provides`, **when** the
operator runs `gtm plan`, **then** the command exits non-zero with an
error naming the specific step and field (§7), performs zero network
calls, and writes zero cost rows — the same guarantee holds for an
unresolvable declared credential (§6).

### Recover
**Invariant:** a killed run picks up where it left off, never redoing
completed, paid-for work.
**Given** a run killed or crashed partway through (some records past a
step, some not), **when** the operator runs `gtm run pipeline.yaml
--resume RUN_ID`, **then** every record whose `run_records.state` already
reflects completion of a step does not re-invoke that step's adapter or
incur that step's cost again, and the run reaches `status='done'` covering
the records that had not yet completed.

### Report
**Invariant:** what happened in a run, and what it cost, is always
reconstructable after the fact.
**Given** one or more completed runs, **when** the operator runs `gtm
runs` (to list) or `gtm runs RUN_ID` (for one run's receipt), **then** the
output reports, per step: records in/out, cache skips, and cost — matching
the sums in `step_events` and `costs` for that `run_id` exactly, with
no reconstruction required from raw table scans.

---

## Changelog

Format: [Keep a Changelog](https://keepachangelog.com/). This project does
not yet have numbered releases; entries are keyed by the reconciliation
pass that produced them.

### v0.5 — 2026-08-16 (ADR-022..029 reconciliation — spec only, build queued)
**Added:** §10a two-tier adapter model: declarative bindings interpreted
by one generic HTTP engine, `spec/binding-schema.json`, the hard
graduation rule, conformance-kit extension to bindings, engine
unification of inline `http/*` steps, the contract-owner naming rule with
`ai/*` model-identifier provenance, the OpenAPI-as-codegen-input rule,
`http/enrich` (JSON + markdown modes, mandatory `freshness_days`, size
cap, no-JS scope), `sql/enrich`/`sql/filter` with declared SQL contracts,
the `ai/*` purity invariant, and the universal-floor framing
(ADR-022..027); §7 dynamic provides beside dynamic needs (ADR-024); §8
simulation gate — `gtm run --simulate`, the completed
simulate → plan → dry-run → armed ladder, SIMULATED receipts excluded
from projection/cache (ADR-028); §8 campaign bundles — `gtm freeze
--bundle`, `spec/bundle-manifest.json`, `gtm run` accepting a bundle
path, simulate-on-bundle (ADR-029); §11 next-milestones note (binding
engine vs groups sequencing pending).
**Changed:** nothing shipped changes in this pass — every v0.5 section is
decided contract with build queued, flagged as such in place.

### v0.4 — 2026-08-15 (ADR-017/018/019 reconciliation + ADR-020)
**Added:** §4a canonical field registry with `spec/fields/person.json`,
`spec/fields/company.json`, `spec/schemas/field-registry.schema.json`, and
the three enforcement layers (ADR-017); §4 internal-form LinkedIn rule and
reserved `github_username`/`twitter_handle` identity tiers (ADR-020,
resolving the gap flagged under ADR-017); `needs: dynamic` manifest form
with static floor (§6, ADR-019); `variables:` egress mapping and
`on_missing: skip|fail` on deliver steps (§8, §9, ADR-018/019); `columns:`
ingress mapping on `csv/source` with auto-map, plan-time suggestions, the
identity-path check, and `csv.*` namespacing of unmapped headers (§7,
§10.1, ADR-018); `gtm run --dry-run` and the armed gate (§8, ADR-019);
one-of (`anyOf`) needs in the planner's contract walk, motivated by
`harvest/profile` needing any one LinkedIn URL shape (§7, §10.4, ADR-020);
milestone M7.
**Changed:** §10.6 `instantly/add-to-campaign` re-contracted to dynamic
needs with an `email` floor and `variables:`-driven merge fields — its
hard-coded `first_name` need and default `first_line`/`ps_line` custom
variables are removed; §9's example gained the deliver `variables:` block;
§7 gained the ADR-019 generalization of ADR-004's `uses:` mechanics.

### v0.2 — 2026-08-15 (spec reconciliation)
**Added:** §0 Design Principles; RFC 2119 declaration; `current_fields` SQL
view as the sole current-value projection (ADR-003); `uses:` for AI-step
dynamic needs and its plan-time validation (ADR-004); `gtm show` (ADR-006);
`gtm help --agent` (ADR-007); `webhook/source` adapter and the spool+cron
event-driven recipe (ADR-009); the eight operator-story acceptance sections
(ADR-012); pointers to `spec/` machine-checkable artifacts throughout.
**Changed:** CLI surface reduced to exactly `init, secret, plan, run, query,
show, runs, freeze, help --agent` (ADR-005); milestones renumbered M1–M6
with M3 (old M4) absorbing plan/receipts/resume plus the new show/help
verbs, M6 (old M7) polish; §1 bet 4 rewritten from "two modes, one engine"
to YAML as the sole pipeline surface, with `gtm freeze`'s job redefined as
run-to-YAML reconstruction rather than pipe-to-YAML conversion; non-goals
gained the pipe-syntax-deferred line and a pointer from the daemon non-goal
to the webhook/source recipe.
**Removed:** §8 pipe-mode mechanics (the RUN-handshake-over-stdout scheme,
mini-runner-per-process embedding) and the standalone `source`/`filter`/
`enrich`/`compose`/`deliver` CLI subcommands (ADR-005); the M3 pipe-mode
milestone.
### v0.3 — 2026-08-15 (AUDIT.md category-(b) diffs, approved)
**Added:** §5 gains a note and worked example that a source's outbound
RECORD `key` is OPTIONAL (the runner canonicalizes it from `fields`); §6
gains `credentials_optional` and `batch` to its manifest shape and prose —
both already load-bearing in the shipped adapters, previously undocumented.
Also corrects §3 and §9: the `field_value_ranks`/`current_fields` view pair
(ADR-003) replaced an earlier, buggy single-view design from this same v0.2
pass, and the `uses:` example fields were corrected from `name` to
`full_name` to match `apollo/search`'s real manifest — see AUDIT.md
category (a) for detail on both; not re-listed here since they were bugs
in v0.2's own text, not new decisions.

### v0.1 — 2026-08-12 (initial spec)
Initial DECIDED sections 1–13 as designed before the 2026-08-14 session;
see git history for the full text.
