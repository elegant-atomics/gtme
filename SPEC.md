# gtme — SPEC.md (v0)

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
7. **Plan is the turn-taking point.** `gtme plan` is where contract
   validation, missing-credential checks, and cost estimation happen —
   before any record moves and before any money is spent. It is the single
   place a human or an agent checks work before committing to it.
8. **Every question has a deterministic answer path.** "What do we know
   about this record" is `gtme show`. "What happened in this run" is the
   receipt or `gtme runs`. "Who's in this segment" is `gtme query`. No
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
4. **YAML is the one pipeline surface.** `gtme run pipeline.yaml` is how a
   pipeline executes, full stop — see DECISIONS.md ADR-005, which cut the
   pipe-mode authoring surface (`gtme source ... | gtme enrich ...`) from v0
   entirely: it tested none of v0's real hypotheses and was the recurring
   source of spec inconsistencies. `gtme freeze RUN_ID` still exists, doing
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
  named `gtme`. Implementation MUST use the standard library plus, at most,
  these dependencies: `modernc.org/sqlite` (pure-Go SQLite, no cgo),
  `santhosh-tekuri/jsonschema/v5` (JSON Schema validation), `gopkg.in/
  yaml.v3`. Any additional dependency MUST be recorded as a Decision in
  DECISIONS.md before use (per §12; see the existing dependency-addition
  entries for the ones already justified: the Anthropic SDK, `x/term`,
  `x/net/publicsuffix`).
- **Ledger:** SQLite, single file, default `~/.gtme/ledger.db`, overridable
  via `GTME_LEDGER` env var. MUST run in WAL mode.
- **Wire format:** NDJSON (newline-delimited JSON) on stdin/stdout.
- **External adapter proof:** one adapter MUST be written in Python
  (`adapters/mock-enrich-py/`), a plain script speaking the protocol.
- **AI engine:** the Anthropic Messages API (model `claude-sonnet-4-6`,
  key from `ANTHROPIC_API_KEY`) is the only model engine; the fixture
  engine is test-only, selected by environment (`GTME_AI_ENGINE=fixture`)
  or `--simulate`, never in YAML. There is no `engine:` key (ADR-050):
  `engine:` in a step is a plan error, and `engine: claude-code` names
  the replacement — an `agent/*` step answered through `gtme answer`
  (§8, ADR-049); the former `claude -p` subprocess is retired. Output
  MUST validate against the step's `provides` schema; on validation
  failure the runner MUST retry once with the validation error appended
  to the prompt, then fail the batch.
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
  referent    TEXT,                       -- was-about: field_values.id of the value a review or edit concerned (ADR-048); null unless the step declared of:
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
  status      TEXT NOT NULL DEFAULT 'running',  -- running|done|failed|pending (ADR-038: ended with a step in flight)
  dry         INTEGER NOT NULL DEFAULT 0  -- 1 for a --dry-run rehearsal (ADR-052 (7)): finishes nothing a once: source counts
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
  event       TEXT NOT NULL,              -- claimed|done|failed|skipped_cache|pending|collected (ADR-038)|answered (ADR-049: a participant's answer awaiting collection)
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
  basis       TEXT NOT NULL DEFAULT 'estimated', -- measured|estimated (ADR-046)
  detail      TEXT,                       -- JSON (credits, tokens, etc.)
  created_at  TEXT NOT NULL
);

CREATE TABLE deliveries (
  id             TEXT PRIMARY KEY,
  identity_id    TEXT NOT NULL,
  target         TEXT NOT NULL,           -- adapter id, or group:<name> for a handoff (ADR-032)
  scope          TEXT NOT NULL DEFAULT '', -- resolved idempotency_scope config value (ADR-044); '' = unscoped
  idempotency    TEXT NOT NULL,           -- computed key, see §8 deliver
  run_id         TEXT NOT NULL,
  created_at     TEXT NOT NULL,
  status         TEXT NOT NULL DEFAULT 'accepted',  -- accepted|confirmed|contradicted|sent (ADR-036)
  sent_at        TEXT,                    -- set only by attestation (ADR-036)
  variables_hash TEXT NOT NULL DEFAULT '', -- resolved variables at delivery (ADR-045); drives redeliver: on_change
  UNIQUE(target, scope, idempotency)
);

-- Layer 3: groups — named associations between identities and a context
-- (ADR-021). A group carries no type field and no executable logic: its
-- character (campaign-like, DNC-like, pool-like) is derived from its events
-- and the pipelines that reference it. Members are identities, so groups
-- hold people and companies alike.

CREATE TABLE groups (
  id         TEXT PRIMARY KEY,           -- ULID
  name       TEXT NOT NULL UNIQUE,
  note       TEXT,
  created_at TEXT NOT NULL
);

CREATE TABLE group_events (
  id          TEXT PRIMARY KEY,          -- ULID
  group_id    TEXT NOT NULL REFERENCES groups(id),
  identity_id TEXT NOT NULL REFERENCES identities(id),
  event       TEXT NOT NULL,             -- 'added' | 'removed' | 'touched'
  detail      TEXT,                      -- JSON provenance
  run_id      TEXT,                      -- nullable: hand edits have no run
  created_at  TEXT NOT NULL
);
CREATE INDEX ix_ge_lookup ON group_events(group_id, identity_id, event, created_at DESC);

-- Current membership: the newest added/removed event wins per (group,
-- identity) — the ADR-003 append-then-derive pattern. touched events never
-- affect membership; they are the delivery-history trail suppression windows
-- read (§8).
CREATE VIEW group_members AS
SELECT group_id, identity_id
FROM (
  SELECT group_id, identity_id, event,
         ROW_NUMBER() OVER (
           PARTITION BY group_id, identity_id
           ORDER BY created_at DESC, id DESC
         ) AS rn
  FROM group_events
  WHERE event IN ('added', 'removed')
)
WHERE rn = 1 AND event = 'added';

-- Vocabulary views (ADR-037): the read surface user-authored SQL (§10a) and
-- config queries (§9) are written against, so a query reads as vocabulary
-- rather than as table internals. current_values is current_fields with the
-- JSON encoding unwrapped; group_membership is group_members keyed by name.
CREATE VIEW current_values AS
SELECT identity_id, field, json_extract(value, '$') AS value, source, confidence, run_id, created_at
FROM current_fields;

CREATE VIEW group_membership AS
SELECT g.name AS group_name, m.group_id, m.identity_id
FROM group_members m
JOIN groups g ON g.id = m.group_id;

-- Payloads: raw vendor responses as CACHE, not facts (ADR-030). Extracted =
-- fact (append-only, above); unextracted = cache (this table, purgeable).
-- Never projected into any step and absent from `gtme show`'s default output;
-- the only paths out are extraction (which writes facts) and deliberate
-- promotion into a content field. expires_at drives eviction: opportunistic
-- at run start plus `gtme vacuum` (§8). NULL expires_at means keep until an
-- operator vacuums explicitly.

CREATE TABLE payloads (
  id           TEXT PRIMARY KEY,          -- ULID
  identity_id  TEXT NOT NULL REFERENCES identities(id),
  adapter      TEXT NOT NULL,             -- adapter id that fetched it
  run_id       TEXT,
  content_type TEXT,                      -- e.g. application/json, text/markdown
  body         TEXT NOT NULL,
  created_at   TEXT NOT NULL,
  expires_at   TEXT
);
CREATE INDEX ix_payloads_lookup ON payloads(identity_id, adapter, created_at DESC);
CREATE INDEX ix_payloads_expiry ON payloads(expires_at);

-- The current-value projection (ADR-003) is two views, not one: "highest
-- confidence within the freshness window" cannot be answered by an
-- unparameterized view, because the window is a per-caller argument (§7's
-- per-step `cache:`), not a fact about the row. field_value_ranks is the one
-- definition of the RANKING rule (confidence DESC, ties broken by newest
-- created_at); current_fields is that ranking with no window applied
-- (rank = 1) — the plain `gtme query` answer. The runner's windowed
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
which applies the per-step window against ranked rows) and `gtme query`'s
`current_fields`-referencing examples (§8, the unwindowed rank-1 answer)
MUST resolve through it. No second implementation of the ranking rule may
exist.

**M14's schema deltas (ADR-036, ADR-037; migration `0007`, mirrored
above):** `deliveries` carries `status` (`accepted` | `confirmed` |
`contradicted` | `sent`; default `accepted`) and `sent_at` (nullable —
empty until a provider attests). Two views join the public read surface
beside `current_fields` and `group_members`: `current_values` presents
current values pre-unwrapped from their JSON encoding, and
`group_membership` presents current membership keyed by group *name*, so
user-authored SQL (§10a) and config queries (§9) read as vocabulary
rather than as table internals.

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
   `apollo.id`). Declared AI outputs (ADR-033) default into this tier
   under the *pipeline's* name — `<pipeline>.<field>` — unless the
   declaration marks the field `canonical: true` (§7, §9), in which case
   it lands on the canonical field of that name: a judgment is a fact about
   working the entity in one campaign, and two campaigns' judgments about
   one identity MUST NOT collide in `field_values`. Stored with provenance, queryable, usable in `uses:` and
   `variables:`. A canonical name MUST NOT contain a dot; a dot marks a
   namespaced name. Namespaced fields in a pipeline's needs make vendor
   coupling visible; `gtme plan` MUST note them.

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
   validation error (`gtme plan` exit 2), which names the field and the
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
{"type":"OPEN","step_id":"...","run_id":"...","config":{...},"pending":{"token":"..."}}  // collecting (ADR-038)
{"type":"OPEN","step_id":"...","run_id":"...","config":{...},"preflight":true}           // preflight session (ADR-040): no records follow
{"type":"RECORD","key":{"entity_type":"person","identity_key":"..."},"fields":{...}}
{"type":"END"}
```
`fields` contains exactly the projection of the adapter's `needs` — nothing more.

Adapter → runner:
```
{"type":"SCHEMA","provides":{...json schema...}}          // first message
{"type":"RECORD","key":{...},"fields":{...},"confidence":{"email":0.93}}
{"type":"VERDICT","key":{...},"pass":true,"reason":"..."}  // filter steps
{"type":"ATTEST","key":{...},"status":"confirmed|contradicted|inconclusive","reason":"..."}  // deliver steps declaring attests (§6)
{"type":"PENDING","token":"...","detail":{...}}           // work in flight (ADR-038): collect under token
{"type":"PREFLIGHT","status":"ok|blocked|inconclusive","checks":[{"name":"...","ok":true,"detail":"..."}]}  // deliver steps declaring preflights (§6)
{"type":"COST","key":{...}|null,"provider":"harvest","amount_usd":0.012,"basis":"estimated","detail":{...}}
{"type":"STATE","cursor":{...}}                            // resumable sources
{"type":"LOG","level":"info|warn|error","msg":"..."}
{"type":"END"}
```

An outbound RECORD MAY carry a `payload` attachment — the raw vendor
response (or its per-record slice, for sources) the record was extracted
from, for ADR-030 retention:
```
{"type":"RECORD","key":{...},"fields":{...},"payload":{"content_type":"application/json","body":"..."}}
```
The runner owns whether it is kept: it consults the adapter's retention
declaration (§6) and writes the `payloads` table (§3) with the declared
TTL; an adapter that emits no payloads simply retains nothing. Old readers
ignore the field, per this section's forward-compatibility rule. (This is
the capture path ADR-030's mechanism requires; its spec-impact list named
§3/§6/§8/§10a and this RECORD extension is the minimal §5 counterpart.)

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
- A filter-role step MAY emit both a VERDICT and a RECORD for the same
  key (ADR-033): the VERDICT gates advancement as ever; the RECORD's
  fields are its declared provides (§7), stored like any adapter output,
  so a judge's reasoning is queryable without a second call.
- A deliver adapter declaring `attests` (§6) MAY emit an ATTEST after the
  RECORD that acknowledges a delivery (ADR-036): `confirmed` and
  `inconclusive` let the record advance — the latter with a receipt
  warning, its delivery still `accepted` — and `contradicted` fails it,
  keeping the `deliveries` row with that status, since the record exists
  at the target and re-sending would duplicate it. An attesting adapter
  that says nothing about a record is `inconclusive`. An adapter that does
  not declare `attests` MUST NOT emit ATTEST; a runner ignores one that
  does. Runners that predate the message ignore it, per the rule above.
- **In flight (ADR-038):** a session MAY end with work it cannot answer
  yet by emitting one PENDING carrying a provider-opaque `token`, after any
  records it could answer and before END. Every dispatched record the
  session did not answer is then pending under that token: the runner
  records it, does not advance it, and finishes the run `pending` (§8). To
  collect, the runner opens a session whose OPEN carries `pending: {token}`
  followed by the same records and END; the adapter MUST NOT dispatch new
  work in such a session — it answers from the token with the ordinary
  messages, or emits PENDING again if the work is not ready. A record a
  collection does not answer fails as in any session. COST for deferred
  work arrives at collection. A runner that predates PENDING ignores it and
  fails the unanswered records — the honest degradation.
- **Preflight (ADR-040):** a deliver adapter declaring `preflights` (§6)
  answers a *preflight session* — OPEN with `preflight: true`, then END,
  no records — with one PREFLIGHT then END: `ok`; `blocked`, a readable
  fact says sends would be meaningless or wrong (the runner fails the step
  before any record is dispatched, §8); `inconclusive`, the target could
  not be read (reported ok with a warning). `checks` lists what was
  examined for the receipt. An adapter that does not declare `preflights`
  is never asked; one that is asked MUST NOT send anything in that session.
- `confidence` is per-field, OPTIONAL, default 1.0.
- COST is best-effort but every v0 built-in adapter that spends money or
  tokens MUST emit it (estimate token cost from the API usage response).
  COST MAY carry `basis: "measured" | "estimated"` (ADR-046); absent
  means `estimated`. `measured` is reserved for an amount derived from
  vendor-reported cost metadata in the response — an amount multiplied
  out from a config or manifest rate is `estimated` even when the unit
  count is exact.
- Output RECORD `fields` MUST be validated against the manifest `provides`
  schema before ledger write; on invalid input the record MUST fail
  (`step_events.event='failed'`), and the run MUST continue. A RECORD
  with no `fields` asserts nothing and is not validated: it means "nothing
  acquired for this record", exactly as no RECORD would, and it MAY carry
  the `payload` the adapter could not use — the record advances counted
  `empty` (§8, ADR-053). A filter's or an attesting deliverer's RECORD is
  never read this way; their verdict or attestation decides.
- Adapters MUST exit 0 on success, non-zero on fatal error; partial output
  before a crash MUST be kept (ledger is append-only), and the run MUST be
  resumable from that point.

---

## 6. Adapter manifest — DECIDED

Each adapter has a `manifest.json`. Built-ins embed theirs; external adapters
ship it next to the executable. Discovery path for external adapters:
`~/.gtme/adapters/<name>/` containing `manifest.json` + executable named `run`,
or a `binding.yaml` (§10a) — installed by hand or by `gtme adapters add`
(§8, ADR-042), which records the binding's source and pin in `.source.json`
beside it. The canonical schema for this file is
`spec/schemas/manifest.schema.json`.

```json
{
  "id": "harvest/profile",
  "version": 1,
  "role": "enrich",              // source|filter|enrich|verify|compose|review|deliver (review: ADR-048 — compose-shaped, of: required, never gates)
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
  `~/.gtme/secrets` (a `KEY=value` file, mode 0600, written by `gtme secret
  set KEY`). A missing declared credential is a plan-time error.
- `idempotency` (deliver adapters, optional; ADR-045, mirroring §10a's
  binding key): `native` declares the target upserts (re-delivery cannot
  duplicate — Attio's assert), which unlocks `redeliver:` (§8, §9);
  `ledger` or undeclared keeps the hard dedupe floor.
- `idempotency_scope` (deliver adapters, optional; ADR-044): the name of a
  config key whose resolved value scopes this adapter's `deliveries` rows —
  see §8 deliver idempotency. Undeclared means unscoped (`''`).
- `credentials_optional`: env var names injected when present, exactly like
  `credentials`, but a missing one is a `gtme plan` warning, never an error.
  For an adapter that can genuinely work more than one way — a step that
  can answer from a local fixture as well as a keyed API — declaring the
  key as `credentials` would fail plans that are actually fine.
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
- **Entity-agnostic manifests (ADR-033):** an adapter whose contract does
  not depend on the entity type — the AI steps (§10.3, §10.5) — declares
  `"entity_type": "*"`. Its steps take the pipeline's entity type (the
  source's; none after a group source, in which case name validation is
  entity-blind), and a static `needs`/`provides` schema on such a manifest
  is validated against that type at plan time. A source MUST NOT be
  entity-agnostic: it has no pipeline type to take, and the planner
  rejects one.
- `freshness_days`: default cache window for fields this adapter provides;
  overridable per step (`cache:` in YAML).
- **Payload retention (ADR-030):** `keep_payloads` (default true) and
  `payload_ttl_days` (default 90) declare whether raw responses this
  adapter attaches to its RECORDs (§5) are retained and for how long; a
  step MAY override with `keep_payloads: false` in its config. Retention
  only applies where an adapter actually attaches payloads — in this
  build, the binding engine and `http/enrich`; the Go vendor adapters do
  not attach them yet (a queued adoption, recorded, not silent).
- `batch`: marks an adapter the runner MUST feed in `batch_size`-sized
  invocations (§9, default 25 — one adapter session per batch) rather than
  dispatching it across the normal per-record worker pool. `ai/filter` and
  `ai/compose` set this; it is what makes their one-call-per-batch-of-25
  behavior (§10.3, §10.5) a manifest fact the runner reads, not a
  special case hard-coded to those two adapter ids.
- **Preflight (ADR-040):** a deliver adapter MAY declare `preflights:
  true`, meaning it can check the live target against what a step is
  about to send — read-only, zero spend, once per run — and answer
  PREFLIGHT (§5) in a preflight session. The checks derive from the
  step's own config and `variables:`; the operator configures nothing, and
  `preflight: false` in adapter config skips them.
- **Attestation (ADR-036):** a deliver adapter MAY declare `attests:
  true`, meaning that after a successful create it re-reads the target
  and reports a three-way verdict per record — `confirmed` (every
  non-blank field sent is present), `contradicted` (a readable value says
  it did not persist; hard fail), `inconclusive` (the re-read failed or
  the shape was unrecognised; reported ok with a warning). An adapter that
  does not declare it yields `inconclusive` — the honest default.
- Sources additionally MAY declare `emits_key_fields` (which output fields
  the runner should build the identity key from).

---

## 7. Contract validation & the planner — DECIDED

`gtme plan` (and implicitly `gtme run`) MUST:

1. Resolve every step's adapter + manifest.
2. Walk the pipeline: maintain the set of available fields (source `provides`
   ∪ each prior step's `provides`). For each step, every property in
   `needs.required` MUST be available; failure MUST produce an error naming
   the step and the missing field. No network calls, no spend.
3. Verify all `credentials` across all steps are resolvable.
4. Print the resolved plan: steps, projections, cache windows, and known
   per-record cost estimates (from a static `cost_estimate_usd` optional
   manifest field; print `?` when absent). A binding whose
   `cost.amount_usd` is a template that resolves to nothing (the operator
   set no rate) prints `est/record: unset`, never `$0.0000` (ADR-046) —
   the gap is visible before anything is spent.

**Plan rendering — `--viz` (ADR-051):** `gtme plan --viz` appends a diagram
of the resolved plan to the output above; `--viz-only` prints the diagram in
place of it. Both are renderings of the same resolved plan: same validation,
same stderr stream, no network calls and no spend, and neither changes what
`gtme plan` accepts nor what it exits with. The default output — no flag —
is unchanged and remains the normative surface: everything item 4 above
requires MUST appear there, and a fact that appears only in the diagram is a
defect. A second implementation MUST print the default output and MAY render
the diagram in any form, or not at all. The diagram's vocabulary (one
silhouette per role, an executor-and-role glyph pair, and edges labelled
with the fields each step adds to the available set) is recorded in ADR-051,
not here, precisely because it is not something a second implementation must
match to interoperate.

**AI-step needs (ADR-004):** an AI-role step's config MAY declare
`uses: [field, ...]` (see §9). When present, the planner MUST treat `uses`
exactly as it treats `needs.required` for step 2 above: every field named in
`uses` MUST be available from the source or a prior step's `provides`, or
`gtme plan` MUST fail naming the step and the missing field. At execution
time, the fields object projected into the step MUST be built from `uses`
rather than from the adapter's static manifest `needs` (which, for AI
adapters, is open-ended precisely because the real needs vary per-prompt and
can only be declared per-step). A step declaring no `uses` and whose
manifest `needs` is the needs-all wildcard (`additionalProperties: true`, no
`properties`) projects every field the ledger holds for the record, as
before ADR-004 — `uses` is how an AI step narrows that to what it actually
needs, gaining plan-time validation in exchange.

**A declared field absent at run time (ADR-053).** `uses:` and `of:` are
validated at plan time for *availability* (above), which does not make
them present on every record: an enrich step may legitimately produce
nothing for one. A participant step MAY therefore carry `on_missing: run
| skip | fail` — the vocabulary deliver steps already use for `variables:`
(§8). `run` (the DEFAULT) dispatches the record with the field absent, as
before; `skip` advances the record untouched without dispatching it;
`fail` fails the record naming the missing fields. Whatever the setting,
the receipt MUST report how many records were dispatched with a declared
field absent, so the gap is visible without the operator having chosen
anything. The default is `run` because a sparse `uses:` is a legitimate
pattern — a compose working from whatever the ledger holds — and changing
it would alter the meaning of pipelines already written.

**Referents and human surfaces (ADR-048, ADR-049):** a compose or review
step MAY declare `of: <field>` — the value it is about (required on a
review); the planner MUST validate it exactly as one more `uses:` entry,
the runtime includes its current value in the record's input hash
(ADR-039) and its `field_values.id` in the provenance of everything the
step writes (`field_values.referent`, §3). A `human/*` step's `render:`
fields are validated the same way. A `human/*` or `agent/*` step may sit
at any position; when a deliver step follows one, `gtme plan` prints one
note — under cron this pipeline waits for a person — and names the
pattern (§8: review into a group, send from the group).

**Dynamic needs generalized (ADR-019):** `uses:` is one instance of a
general mechanism. For any step whose manifest declares dynamic needs (§6),
the planner MUST derive the step's effective needs from config — `uses:`
for filter/compose, the *values* of `variables:` for deliver — union the
manifest's static floor, and validate every derived field against the
available set exactly as step 2 above: a referenced field nothing provides
is a plan error naming the step and the field. Effective needs referencing
vendor-namespaced fields (§4a) are valid, but `gtme plan` MUST note the
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
configured content field, a `sql/transform` step's declared `provides:`
(§10a) — MAY declare its manifest `provides` as dynamic; its *effective*
provides are then derived from config. The planner MUST add the derived
names to the available-field set for downstream validation exactly as it
adds static `provides`, and downstream `uses:`/`needs` referencing them
validate normally. A derived name MUST be canonical or vendor-namespaced
per §4a — an ad-hoc name is namespaced unless the config maps it to a
canonical field — and the runtime MUST validate the step's output against
the derived provides exactly as it validates static provides (§5).

**Declared AI provides (ADR-033):** an AI-role step MAY carry a step-level
`provides:` — a list of field names, or a JSON-Schema object whose
property names are the fields and whose values MAY declare `type`,
`enum`, and `canonical`. The planner treats it as dynamic provides above.
Every bare name defaults into `<pipeline>.<field>` (§4a) — a name that
happens to coincide with a canonical field included, which `gtme plan`
MUST note — and a name written with a dot is kept as written. A field
marked `canonical: true` lands on the canonical field of that name
instead (global, not per-campaign): the name MUST be canonical for the
pipeline's entity type, and a declared `type` or `enum` MUST agree with
the registry entry, or the plan fails naming the step and field. The
runtime validates the model's output against the derived schema with the
existing retry (§10.3), and an `enum` violation is a validation failure,
never stored. A filter with `provides:` emits VERDICT and RECORD (§5). A
step declaring nothing keeps its manifest's static shape.

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

**Group checks (ADR-021):** group references are resolved against the
local ledger — read-only, still zero network calls and zero spend. A
group named by `require:`, `exclude:`, `suppress:`, or a group source
(§9) MUST exist, or the plan fails naming the group and the fix
(`gtme groups add <name> …`). A `record:` target and a group terminus are
*created on demand* at run time (they record outcomes; requiring them to
pre-exist would make every new pipeline a two-step dance), so plan only
checks their names are non-empty. A `suppress.within` window MUST parse
as `Nd` (§9's cache grammar). The plan output lists each step's
membership gates, and MUST call out each deliver step — target adapter
and touch scope — so the pipeline's full send surface is reviewable in
one place (ADR-031: with delivers as ordinary steps, plan output is
where the send points are obvious at a glance, not YAML position).

**Config values from the ledger (ADR-037):** any value inside a step's
`with:` MAY be `{query: <SQL>}` or `{segment: <name>}` (a saved query,
§8). The planner resolves it read-only against the local ledger — no
network, no spend — *before* validating the step's config against the
adapter's `config_schema`, substitutes the result (one column → a list;
one row and one column → a scalar), prints the resolved rows in plan
output, and fails the plan on zero rows (an empty list handed to a vendor
search is the shape that searches everything). The run resolves again at
start and records the resolved config in `runs.config_json`. A missing
segment is a plan error naming the fix (`gtme query --save`).

**SQL steps at plan (ADR-037):** a `sql/*` step's query is run through
`EXPLAIN QUERY PLAN` against the local ledger at plan time, so an unknown
table or column fails the plan rather than the run. Plan output annotates
a SQL step whose query references `relations` or `group_members` as
*cross-record*, so a reviewer sees its character without reading the
join.

**Respend (ADR-038, narrowed by ADR-039):** plan MUST warn when a paid
enrich/verify step would pay for the same records again on a re-run with
nothing to remember the answer — its adapter declares credentials or a
cost estimate and it has no freshness window (no manifest
`freshness_days`, no `cache:`). `respend: true` on the step (§9) silences
the warning; it is the operator saying so in the reviewable file. AI
steps do not warn: the judgment cache (above) remembers by default.

**One commit point (ADR-032):** plan MUST warn when a pipeline carries
both a `group/deliver` step and a network-side deliver step — ADR-031
arms every deliver in a run at once, so approving the handoff would
approve the send.

At execution time, per step, per record:
- **Membership gates (ADR-021):** a step with `require:` processes only
  records currently members of EVERY listed group; a step with `exclude:`
  skips records currently members of ANY listed group. Gated records do
  not advance past the step and are counted like `when:` gates. Exclusion
  is the judgment-memory mechanism: a qualify pipeline that excludes its
  own output groups sends only never-judged records to its filter, so an
  identity is judged once per scope — determinism first, cost second.
- **Cache check (enrich/verify):** if every field in the step's
  `provides` already has a current value within the freshness window,
  the runner MUST skip the record (`step_events.event='skipped_cache'`, no
  adapter call, no cost).
- **Judgment cache (AI roles; ADR-039):** before dispatching a record to
  an AI step the runner MUST compute the step's *judgment signature* — a
  hash over the adapter id, the model identifier, the operator prompt,
  the output shape (declared or default provides) and the `uses:` list —
  and the record's *input hash* — a hash, as canonical sorted JSON, over
  the fields the judgment reads: the `uses:` fields when declared, else
  the projection minus the step's own provides and minus every field
  namespaced by this pipeline (§4a), so a step never sees its own last
  answer as a changed input. If a `done` event for this identity carries the
  same signature and input hash (any run), the runner MUST skip the
  record (`skipped_cache`, reason `same_judgment`): a filter re-applies
  the stored verdict (pass advances, fail freezes), a compose writes
  nothing (its fields are current with that provenance). There is no time
  window by default — the same question about the same facts has the same
  answer; `cache: Nd` bounds reuse to N days (what a prompt that reads
  the clock itself needs); `respend: true` or
  `cache: 0d` disables it. A deferred step (ADR-038) cache-checks before
  it submits.
- **Projection:** the runner MUST build `fields` strictly from `needs`
  properties (or from `uses`, for AI steps that declare it).
- **Filter verdicts** MUST be stored in `run_records.verdicts`; records with
  `pass=false` stop advancing (state freezes at the filter step) but MUST
  remain in the ledger.

---

## 8. CLI surface — DECIDED

```
gtme init                          # create ledger + ~/.gtme
gtme secret set KEY [VALUE]        # VALUE omitted → prompt, no echo
gtme plan pipeline.yaml [--viz|--viz-only]   # validate + print plan, no execution; --viz appends the diagram, --viz-only prints it alone
gtme run  pipeline.yaml [--resume RUN_ID] [--dry-run] [--simulate]
gtme query "SQL"                   # read-only SQL against the ledger
gtme query --save NAME "SQL"       # saved segment
gtme show <identity-key>           # read-only projection inspector
gtme show --run last [--fields ...] [--provenance] [--limit N]
gtme runs [RUN_ID]                 # list runs / show one run's receipt
gtme answer [RUN_ID|last|PIPELINE] [STEP] [IDENTITY_KEY] [--set f=v ...] [--as NAME] [--cost USD [--measured]] [--note TEXT]   # ADR-049: a participant's answer for a pending human/agent step; no key + TTY walks the records
gtme show --run RUN_ID --pending [STEP]   # the pending records with their rendered surface (ADR-049)
gtme freeze [RUN_ID|last] [--bundle DIR] # bare: YAML to stdout; --bundle: campaign bundle
gtme groups [show NAME | add NAME ... | remove NAME ...]   # ADR-021, see below
gtme vacuum                        # evict expired payloads (ADR-030), nothing else
gtme help --agent                  # machine-readable full CLI + adapter surface
gtme help --bindings               # the binding contract: schema, discovery path, a reference binding (ADR-041)
gtme adapters                      # installed adapters with source and pin (ADR-042)
gtme adapters search TEXT          # search the registry index
gtme adapters add REF              # install a binding from github.com/<owner>/<repo>/<path>[@ref], verified first
gtme adapters verify ID            # schema + fixtures offline; prints hosts and credentials it will use
gtme adapters update ID [@ref]     # re-fetch at a newer ref, explicitly
```

This is the entire v0 verb set (ADR-005, extended by ADR-021's `gtme
groups` in M9, ADR-030's `gtme vacuum` in M11, ADR-042's `gtme adapters`
in M19, and ADR-049's `gtme answer`). ADR-051 adds no verb: `--viz` and
`--viz-only` are rendering options on `gtme plan`.

### Payload eviction — `gtme vacuum` (ADR-030; built in M11)

`gtme vacuum` deletes payloads whose `expires_at` has passed — and nothing
else; facts are append-only forever, and payload eviction is the one
legitimate deletion in the system (§3: cache, not knowledge). It reports
how many rows went. The runner also evicts opportunistically at the start
of every armed run, so a busy ledger stays bounded without a daemon
(ADR-009's stance); `gtme vacuum` exists for quiet ledgers and for
operators who want eviction on their own schedule. There is no `gtme x`, no
multi-process pipe chaining, and no standalone `source`/`filter`/`enrich`/
`compose`/`deliver` subcommands — those existed only to support pipe mode
and are cut along with it. `uses:`, `cache:`, `when:` and every other
per-step option are YAML config (§9), never CLI flags. All human-facing
output (receipts, progress, errors) MUST go to stderr; stdout is reserved
for data (`gtme query`'s result rows, `gtme freeze`'s YAML). Exit codes: 0 ok,
2 validation/contract error, 3 auth/credential, 4 rate-limited, 5 network, 1
other.

**Record accounting (ADR-053).** A step's `out` counts records the step
*contributed to*, not merely records that advanced. For a step whose
output is fields — enrich, verify, compose, review, `sql/transform` — a
record that advanced without the step writing any field MUST be counted
`empty` and printed beside `out`; `out + empty` is what advanced. A
filter's output is a verdict and a deliver's is a send, so neither counts
`empty`. Per step, `in` MUST reconcile: `out + empty + filtered + failed
+ gated + skipped + cached`. `in` therefore counts every record eligible at
the step, not only those handed to the adapter; a record still in flight,
held by a dry run, or passed through a simulation gap is the non-terminal
remainder and the line names it (`N in flight`, `N held (dry run)`,
`N simulated`), so the identity holds for a step that has not settled.

A source MUST reconcile what it read against what it sourced, classifying
the difference — records that coalesced into identities the ledger already
held are the ordinary cause, and the coalescing MUST be recorded per
record (a `step_events` row) so which row merged into which identity is
answerable afterward, not merely counted:

```
source [info]: read 940 rows from acquired-contacts.csv
source: sourced 931 records (9 coalesced into known identities)
website: 10 in, 2 out, 8 empty
write:   10 in, 10 out (8 missing web.homepage)
```

A run that spent money and produced no records MUST say so on the receipt
and in `gtme runs` — `done — 0 records, $4.10 spent (estimated)`. Its exit
code is unchanged: §8's codes are a scripting contract, and this is
information, not a new outcome.

**Terminal receipt** (stderr, end of run): records in/out per step, cache
skips, cost per step and total, cost avoided via cache (sum of
`cost_estimate_usd` for skipped records; `?` if unknown). Totals carry
their basis (ADR-046): a purely measured total prints bare; a purely
estimated one prints `total: $X (estimated)`; a mixed run splits —
`total: $X ($Y measured + $Z estimated)`. `gtme runs <id>` mirrors the
live receipt.

### `gtme show` (ADR-006)

`gtme show <identity-key>` prints the full current-value projection
(`current_fields`, §3) for that identity: every field, its current value,
and — with `--provenance` — the source adapter, confidence, run that
wrote it, and, for a review's or edit's outputs, the referent (ADR-048:
the `field_values` row it was about) and the participant's `--note`. `gtme show --run last` (or a specific `RUN_ID`) lists the records
touched by that run instead of a single identity. `--fields a,b,c` narrows
the printed fields; `--limit N` caps rows for `--run` mode. `gtme show` is
strictly read-only: it MUST NOT write to the ledger, and it MUST NOT appear
in `gtme freeze` output (it is an inspection tool, not a pipeline step).

### `gtme help --agent` (ADR-007)

Emits the full CLI + adapter surface as one compact (~1–2k token)
machine-readable document, regenerated from the live verb table and the
installed adapter registry — never hand-maintained. It MUST include every
verb and flag from this section, and every installed adapter's manifest
(`needs`/`provides`, in the shape of §6), plus 3 canonical example
pipelines. Acceptance criterion: the document MUST round-trip — an agent
given only `gtme help --agent`'s output and no other context MUST be able to
author a valid `pipeline.yaml` that passes `gtme plan`. It MUST also
include the ledger's public read surface — the tables and views of §3 —
and the canonical query shapes (a cross-type membership gate, a
cross-record aggregate, a config-value query), so an agent can write a
`sql/*` step or a `{query:}` value without reading `spec/ledger.sql`
(ADR-037). It MUST NOT carry the binding contract; it MUST point at the
surface that does: **`gtme help --bindings`** (ADR-041) prints, as one
machine-readable document, the binding schema (`spec/binding-schema.json`,
byte-identical to the artifact), the discovery path (§6, §10a), one
reference binding verbatim as a worked example, the conformance
expectation (fixtures beside the binding, served by `--simulate`), and the
`adapters` verbs (ADR-042). Its acceptance criterion mirrors the first:
an agent given only `gtme help --bindings` and no other context MUST be
able to author a binding that `gtme plan` resolves and `gtme adapters
verify` passes.

### `gtme adapters` — the bindings registry (ADR-042)

Bindings are URL-addressed and pinned. `gtme adapters add
github.com/<owner>/<repo>/<path>[@<tag|sha>]` fetches the repository over
HTTPS at that ref (no `git` dependency; a `GITHUB_TOKEN` stored with `gtme
secret` for private repositories), copies the binding directory —
`binding.yaml` and `fixtures/` — into `~/.gtme/adapters/<id, slashes →
dashes>/`, and writes `.source.json` beside it: the ref as given, the
resolved commit, the content sha256, the install time. **Nothing installs
unverified:** `add` runs `gtme adapters verify` first — the binding
validates against the schema, its conformance fixtures run offline, and
the reviewable surface is printed (the hosts its requests will call, the
credentials it will demand, its needs and provides); a binding with no
fixtures, or failing ones, does not install. `gtme adapters update <id>
[@ref]` re-fetches only when asked; nothing moves a pin implicitly. `gtme
adapters search <text>` reads the registry index
(`spec/schemas/registry-index.schema.json`; `GTME_REGISTRY` overrides the
URL) and matches id, vendor, description and role; `gtme adapters` lists
what is installed with its source and pin. The index is published by the
`gtme-bindings` repository, which holds the *verified* entries (its CI
runs their fixtures) and points at *community* entries in their authors'
repositories. The binary carries the floor (`csv/*`, `http/*`, `sql/*`,
`ai/*`, `group/*`) and the reference twins in `spec/bindings/`; every
other vendor is a registry entry.

### Event-driven pipelines: webhook/source + cron (ADR-009)

There is no daemon and no long-running receiver process (§13 non-goals). The
v0 answer to "run a pipeline when an event happens" is: a commodity webhook
receiver you already have (a Cloudflare Worker, a Zapier/Make webhook
action, a GitHub Action) appends each incoming payload as one line to a
spool file or directory; a `webhook/source` adapter (§10) reads and drains
that spool the same way `csv/source` reads a CSV; a scheduled `gtme run`
(cron, launchd, CI schedule) invokes the pipeline periodically. At-least-once
redelivery from the receiver is absorbed structurally by the `deliveries`
table's `UNIQUE(target, scope, idempotency)` constraint (§3, ADR-044) — replaying the same
event through the pipeline twice produces at most one delivery. Per-event
low latency is explicitly out of scope for v0.

### In-flight steps — `deferred: true`, the pipeline's last step; `run` collects (ADR-038)

An AI step MAY carry `deferred: true` in its config (§10.3, §10.5): the
`api` engine submits the batch to the provider's batch surface (the
Message Batches API, `custom_id` = identity key) instead of answering in
the request, and the session ends with PENDING (§5). **A deferred step
MUST be the pipeline's last step**; `gtme plan` rejects it anywhere else,
naming the fix: land the step's output through what already carries it —
declared `provides:` fields (§7) and the `group:` terminus (§8) — and let
a consumer pipeline pull from the group. The consequence is the point: no
deliver step can follow a deferred judgment, so a send is always its own
pipeline with its own dry-run receipt showing the judgments as
*collected*, and approval never precedes the judgment. One deferred step
per pipeline follows.

The run then finishes with status **`pending`** — a run that ended with a
step in flight is not `done` — and the receipt reports how many records
are in flight, the token, and that the next `gtme run` collects:

```
judge: 25 in, 0 out — 25 in flight (msgbatch_01…); the next `gtme run judge.yaml` collects
```

**`gtme run` collects before it starts.** When the most recent run of the
same pipeline is `pending`, `gtme run <pipeline>` resumes that run — it
says so on stderr — rather than sourcing anew, so the cron recipe (§8) is
unchanged and nothing is ever submitted twice by habit. `gtme run
--resume RUN_ID|last` is the explicit form of the same thing. Collection
reaches the step, finds the records pending under it, and collects (§5).
If the provider is still processing, the step emits PENDING again and the
run stays `pending`; run again later — from a shell, a cron, whatever
invokes gtme, since gtme has no daemon and nothing waits (§13). Once every
pending record is answered the terminus asserts and the run finishes
`done` (or `failed`). Only after that does a `gtme run` of the pipeline
source fresh records. `gtme runs` lists `pending` runs with their
in-flight count. `--simulate` runs a deferred step synchronously on the
fixture engine (a rehearsal that ended in flight would rehearse nothing)
and says so; `--dry-run` on a deferred pipeline is a plan warning — there
is no deliver step to hold back.

### People and agents answer — `human/*`, `agent/*`, `gtme answer` (ADR-048, ADR-049)

`human/filter`, `human/compose` and `human/review` (§10) fill the three
roles for a person; `agent/*` are the same adapters for an agent driving
gtme, which never prompts. Neither opens an adapter session.

**At a terminal the run asks.** With `prompt: tty` (the default) and a
TTY, a `human/*` step walks its records inside `gtme run`: the rendered
record (`render:`, §9; default the `uses:` fields or the `of:` value),
then the declared outputs as a menu or a field to fill — a filter takes
pass/fail and a reason, a compose the declared fields, a review the
declared labels — validated on the spot; Ctrl-C leaves the rest pending.
Answered records continue to the next stage with the others.

**Otherwise it waits in the ledger.** With no TTY, `prompt: never`, or an
`agent/*` step, every unanswered record ends `pending` under the
runner-owned token `<run-id>/<step-id>`, the run finishes `pending`
exactly as a deferred step does (above), and the receipt says so:

```
grade: 12 in, 0 out — 12 awaiting human/review; `gtme answer review.yaml` records, the next `gtme run review.yaml` collects
```

**`gtme answer` is the write path.** `gtme answer [RUN_ID|last|PIPELINE]
[STEP] IDENTITY_KEY --set field=value ...` records one participant's
answer for one pending record as an `answered` step event (§3). The run
is a `RUN_ID`, `last`, or a pipeline name or path — the most recent
pending run of that pipeline, the lookup collect-first makes; `STEP` MAY
be omitted when one step is pending. The answer is validated against the
step's declared or default outputs (§7): a value outside an enum is
refused naming the allowed values, a filter accepts only
`pass=true|false` and `reason`, and a record not pending under that step
is refused. `--as NAME` names the participant (default the OS user; the
prefix — `human/` or `agent/` — follows the adapter); `--cost USD`
records what the participant spent, `estimated` unless `--measured`
(ADR-046); `--note TEXT` is kept in the event and shown by `gtme show
--provenance`, never part of a cache key. With no identity key and a
TTY, `gtme answer` walks the pending records interactively, exactly as
the in-run walk does. Answers are ledger state — idempotent per (run,
step, identity), the latest before collection wins, readable through
`gtme show --run` and SQL. `gtme answer` writes only `answered` events,
never sends, and MUST NOT appear in `gtme freeze` output. `gtme show
--run RUN_ID --pending [STEP]` prints the pending records with their
rendered surface, as text and as JSON — what an agent reads before it
answers.

**`gtme run` collects answers as it collects batches.** When a pending
step is `human/*` or `agent/*`, collection reads the `answered` events
instead of opening a session: each answered record completes the step —
a VERDICT for a filter, fields for a compose or review, provenance
`human/<name>` or `agent/<name>` (§10a), the referent when `of:` was
declared, COST under the run — and continues; unanswered records stay
pending and the run stays `pending`. Under cron the consequence is the
stage-by-stage model's: a pending run is resumed, not re-sourced, so a
pipeline with a human step waits for its person and sources nothing new
until answered. The documented pattern: the reviewing pipeline is one a
person runs and ends in a `group:`; the cron pipeline sources from that
group. Under `--simulate` a `human/*`/`agent/*` step is a simulation gap
(below): records pass through untouched and the receipt counts them —
there is no prompt to script and no person to rehearse.

### deliver idempotency

Per deliver step: idempotency key = the value of the field named by the
step's `idempotency` config (default: the identity key). Before calling
the adapter for a record, the runner MUST check `deliveries`; on hit, it
MUST skip (`skipped_cache` semantics, reason `already_delivered`). On
successful adapter RECORD/END for that record, the runner MUST insert
into `deliveries` with `status = accepted` (ADR-036; column lands with
M14). `accepted` means the provider took the request; `sent` is written
only when a provider attests it and `sent_at` carries the provider's own
timestamp — a 2xx is never a delivery. Adapters declaring `attests` (§6)
refine `accepted` to `confirmed` or `contradicted` per record after a
re-read; `inconclusive` stays `accepted` with a receipt warning. Promotion
to `sent` is the `listen` verb's job (ROADMAP.md) and MUST be
compare-and-swap on the observed `(status, sent_at)` pair.

The dedupe key is `(target, scope, idempotency)` (ADR-044). `target` is
the adapter id (or `group:<name>`, ADR-032), so a pipeline delivering to
a campaign and to a CRM dedupes each independently (ADR-031). `scope` is
the resolved value of the config key the manifest names in
`idempotency_scope` (§6), `''` when it declares none — so the same
record into the *same* campaign can never double-add, while delivery
into a different campaign is a fresh decision. A global "never touch
this address twice through this adapter" is a policy, not a constraint:
declare it as a suppression group (ADR-021), which sees touches across
every adapter.

Re-delivery (ADR-045): a deliver step MAY set `redeliver: always |
on_change | never` (§9). The default is `on_change` when the manifest
declares `idempotency: native` (§6) and `never` otherwise, and `always`/
`on_change` are plan errors on a target that did not declare `native` —
repeat-safety belongs to the adapter, intent to the step. Each delivery
records the hash of its resolved `variables:` values; under `on_change`
an already-delivered record re-delivers only when that hash changed
(skip reason `unchanged` otherwise, distinct from `already_delivered` in
events and receipts), and a re-delivery updates the row's hash and run
and resets `status` to `accepted` for a fresh attestation cycle —
`created_at` keeps the first delivery.

### deliver completeness — `on_missing` (ADR-019)

Per-record completeness at deliver time is a runtime contract: every
`variables:` target (§9) MUST resolve to a non-empty value for a record
before that record may deliver — blank merge fields MUST never send. The
policy when one does not resolve is `on_missing: skip | fail`, declared
per deliver step, default **skip**:

- `skip`: the record does not deliver at this step (later steps,
  including later deliver steps, still see it); the runner records a fail
  verdict for that deliver step in `run_records.verdicts` with the missing field
  names as the reason, and the terminal receipt lists every skipped record
  with its reason.
- `fail`: the record fails (`step_events.event='failed'`, naming the
  missing fields); the run continues, per §5.

A record missing a *floor* field (§6 dynamic needs) fails needs validation
as any record would; `on_missing` governs the `variables:`-derived fields.

With delivers as ordinary steps (ADR-031), the skip/fail distinction is
also an advancement distinction: `skip` withholds *this step's send* but
the record advances (a later deliver step with its own `variables:` may
still deliver it); `fail` fails the *record* (§5 semantics — state
freezes, it does not advance). Deliver-step fail verdicts in
`run_records.verdicts` record withheld sends; unlike a filter's
`pass=false` (§7), they do not stop the record.

### `group/deliver` — the handoff is a delivery (ADR-032)

A deliver-role step, runner-owned like the SQL steps (no adapter, no
network), whose target is a group: `use: group/deliver`, `with: {group:
<name>}`, created on demand. Every deliver-step key applies — `variables:`
(the receipt renders them per record, which is how a review gate shows a
brief rather than a list of keys), `on_missing:`, `idempotency:`,
`record:`, `suppress:`, `require:`/`exclude:` — and `--dry-run` withholds
it like any deliver. Committing a record to a downstream stage authorises
downstream spend, which is why it takes the send's gate. A pipeline MAY
carry several, routing different `when:` outcomes to different groups. A
group with no consumer pipeline is a hold; release is `gtme groups add`;
review is `gtme groups show`. It is a write, not a trigger: nothing runs
the consumer, which pulls on its own schedule. One commit point per
pipeline (§7): a `group/deliver` and a network-side deliver do not share
a pipeline.

The group source (§9) takes `limit: N`: members served in `group_events`
insertion order, oldest first. Ranked serve order is deliberately not
provided; an upstream `sql/filter` narrows, `limit:` bounds.

### Deliver preflight — the target is checked before anything sends (ADR-040)

`gtme plan` proves gtme's contracts with zero network; it cannot know the
target's state. For a deliver step whose adapter declares `preflights`
(§6), the runner opens a preflight session (§5) **before any record
session** — at `--dry-run` and at the start of an armed run — and reads
the answer: `ok` proceeds; `inconclusive` proceeds with a receipt
warning; **`blocked` fails the step before a single record is dispatched**
— its records stay at the previous state, the run finishes `failed`, and
`--resume` after the fix preflights again. A dry run reports the checks
either way:

```
send: preflight ok — 4 checks (campaign active, 3 sequence steps, every variable referenced, no unfilled variants)
send: preflight BLOCKED — sequence step 2 does not reference {{body_step_2}}
```

This is the class of failure attestation cannot see: every request
succeeds and nothing meaningful sends. `plan` stays zero-network; under
`--simulate` a stubbed adapter's preflight is part of the counted gap.

### Dry-run and the armed gate (ADR-019)

`gtme run --dry-run` executes the pipeline normally **except** deliver
steps: for each record reaching a deliver step, the runner resolves the
step's `variables:` per record, applies the `on_missing` policy, and
records `step_events.event='dry_run'` with the fully RESOLVED variable
values in `detail` — but MUST NOT invoke the deliver adapter and MUST NOT
write to `deliveries`. The terminal receipt renders each record's resolved
variables per deliver step: this is the approval artifact a human reviews
before arming. Arming is all-or-nothing — the armed run arms every
deliver step in the pipeline (ADR-031).
Non-deliver steps run normally under `--dry-run` — delivery is the gated
destructive edge (§0 principle 9); everything upstream is replayable and
cache-covered, and its spend is already visible in `gtme plan`. Arming is
the same command without the flag; a dry run is an ordinary run in every
other respect (its own run id, receipt, and `gtme runs` entry), and because
it writes no deliveries, the armed run's idempotency behaves as if the dry
run had never happened. Its run row carries `dry = 1` (§3, ADR-052 (7)) so
that anything reading run history for what a pipeline has *finished* — a
`once:` group source — leaves rehearsals out; `gtme runs` marks the entry.

### Groups: touch scoping, suppression, terminus, and `gtme groups` (ADR-021)

Groups are runner-owned semantics: adapters see only projections, never
the ledger, and nothing below changes the wire protocol.

**Touch scoping — `record:`.** On a successful delivery, the runner MUST
append a `touched` event to the group named by that deliver step's
`record:` — **defaulting to the pipeline name** — creating the group on
demand. Every pipeline is thereby safely scoped by default; sharing a
scope across pipelines is an explicit override. Multiple deliver steps
sharing the default share the scope — the correct reading of "this
pipeline touched them" — and distinct scopes are explicit per-step
`record:` overrides (ADR-031). The event's `detail` carries the target
adapter and run id.

**Suppression — `suppress: {group: G, within: Nd}`.** Before delivering
a record at a deliver step carrying the key, the runner MUST skip it
when the record has a `touched` event in G within the window. A
suppressed record records a fail verdict for that deliver step with
reason `suppressed` (the `on_missing` pattern — and, like `on_missing:
skip`, the record advances: suppression gates this step's send, not the
record), and the terminal receipt lists every suppressed record with the
group and the age of the blocking touch. Suppression layers above the §8
idempotency floor: idempotency stops the *same* delivery twice (or, on a
natively idempotent target under `redeliver: on_change`, the same
delivery *with the same values* twice, ADR-045);
suppression enforces a *chosen* contact policy across deliveries.

**Terminus — top-level `group: <name>`.** A pipeline MAY end in group
membership instead of (or in addition to) deliver steps: every record
that completes the run's final step is `added` to the named group
(created on demand), with the pipeline and run id as provenance. The
terminus captures *completers*, not sends (ADR-031): a record that
delivered at a mid-pipeline deliver step and then failed a later step
has delivered but does not join, and a record whose send was withheld
(`on_missing` skip, suppression) but that completed the run does join —
`record:`'s `touched` events, not the terminus, are what remember actual
sends. This is
the recommended campaign decomposition: a qualify pipeline
(source → enrich → filter ⇒ group) runs cheaply and often; the group is
a durable, reviewable, hand-editable artifact; a separate send pipeline
consumes it deliberately.

**Dry runs assert nothing durable:** under `--dry-run` (and `--simulate`)
the runner MUST NOT write `touched` events or terminus `added` events —
the receipt reports what an armed run would have recorded. A rehearsal
that mutated durable associations would not be a rehearsal.

**`gtme groups` verbs:**
```
gtme groups                          # list groups with derived character
gtme groups show NAME                # members (current), recent events
gtme groups add NAME KEY...          # hand-edit membership by identity key
gtme groups add NAME --from-segment SEGMENT | --query "SQL"
gtme groups remove NAME KEY... [--note TEXT]
```
`gtme groups` lists each group with member count and event tallies
(added/removed/touched) — character is derived, never stored. The
snapshot forms evaluate an intensional definition into extensional
membership: `--from-segment` runs a saved segment, `--query` a one-off
SELECT; either MUST yield an `identity_id` column (segments-as-SQL
naturally join `identities`), and each `added` event's detail records
what was evaluated and when. A KEY is matched against
`identities.identity_key`; if it matches more than one entity type, the
command fails asking for `--type`. All writes are events — membership
edits are append-only like everything else. `--note` (ADR-032) records
the reason for a removal in the event's `detail` — the reject-with-reason
a review gate needs.

### Simulation gate — `gtme run --simulate` (ADR-028; built in M8)

`gtme run --simulate` executes the ENTIRE pipeline offline: every binding
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

### Campaign bundles — `gtme freeze --bundle` (ADR-029; built in M10)

`gtme freeze --bundle DIR` produces a **campaign bundle**: a directory (or
tarball) containing the pipeline YAML, every referenced binding at its
exact version, AI prompt files, saved queries, the relevant registry
slice, and a manifest — bundle format version, content hashes, source run
id — per `spec/bundle-manifest.json`. (Bare `gtme freeze` keeps its
existing job: the reconstructed `pipeline.yaml` on stdout.) Guarantees:
(a) **self-contained** — `gtme run <bundle-path>` resolves nothing outside
the bundle except credentials; (b) **diffable** — text files, stable
ordering; (c) **portable** — the same bundle runs on any machine/ledger;
membership and cache naturally differ, contracts don't. `gtme run` MUST
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
    uses: [first_name, title, company_name]   # ADR-004; masked fields only — free (ADR-043)
    with:
      prompt: >
        Keep only contacts likely to own outbound tooling decisions.
      batch_size: 25

  - id: reveal                # ADR-043: pay Apollo's per-credit match only past the filter
    use: apollo/enrich
    when: icp-filter.passed
    cache: 30d

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

  - id: send              # ADR-031: a deliver adapter is an ordinary step
    use: instantly/add-to-campaign
    with:
      campaign: "Q3 VP Marketing"
    variables:            # ADR-018/019: egress mapping, and the step's dynamic needs
      first_line: first_line
      ps_line: ps_line
    idempotency: email
```

Schema rules: `deferred: true` inside an AI step's `with:` sends its batch
to the provider's batch surface and ends the run in flight (§8, ADR-038);
it is adapter config, not grammar, and valid only on the last step.
`redeliver: always | on_change | never` on a deliver step (ADR-045) sets
its repeat policy — defaulting per the adapter's declared idempotency
(§8), and refusing `always`/`on_change` on a target that is not natively
idempotent. `respend: true` on a step (grammar, any paid step) declares that re-running
the pipeline MAY pay for that step's records again — it silences the §7
respend warning and nothing else. `when:` supports only `<step_id>.passed` in v0. `cache:` takes
`Nd`. `uses:` (ADR-004) is a list of field names, valid only on steps whose
adapter role is `filter`/`compose`/`review` (the participant roles,
ADR-048: `ai/*`, `human/*`, `agent/*`); the planner validates it exactly
as `needs.required` (§7). `provides:` (ADR-033) is likewise valid only on
those roles: the step's declared output fields, a list of names or a map
of name → `{type, enum, canonical}` (§7); the planner rejects it
elsewhere. `of: <field>` (ADR-048) is valid on compose and review steps
and required on a review — the value the step is about, validated as
`uses:`, recorded as the referent of everything the step writes. A
`human/*` step MAY carry `render: {fields: [..], template: ".."}` and
`prompt: tty | never` (default `tty`) in `with:` (§8, ADR-049); an
`agent/*` step never prompts. `engine:` is not a key (ADR-050): its
presence is a plan error. Deliver adapters are ordinary
`steps:` entries (ADR-031): a pipeline MAY carry zero, one, or many, at
any position — steps execute strictly in order, so a deliver step sends
exactly the records that survived everything before it. `variables:`
(ADR-018/019) is valid only on steps whose adapter role is `deliver`: a
map of *target merge-field name* → *canonical
or namespaced ledger field*; its values are the step's dynamic needs (§6,
§7), and the mapping is the egress half of ADR-018 — the only place the
target's foreign vocabulary appears. The runner hands the mapping to the
deliver adapter as `variables` in OPEN `config` (§5) — the adapter owns
applying it; the runner owns projecting and completeness-checking the
fields it references (§8). `on_missing: skip | fail` (default
`skip`) and `idempotency:` are likewise valid only on deliver steps
(§8); the planner MUST reject any of these keys elsewhere, exactly as it
rejects `uses:` outside filter/compose. The ingress half is
`columns:` inside `csv/source`'s `with:` (§10.1): a map of *canonical field
name* → *CSV header as written*. Both mapping keys read
destination-vocabulary → source-vocabulary. No interior step may carry a
mapping block (ADR-018): declarative mappings at the two edges are
plan-validatable; the interior speaks only canonical names. Steps execute
strictly in order; within
a step, records process with a worker pool (default concurrency 4,
`GTME_CONCURRENCY` to override), except AI steps which process in batches of
`batch_size` (default 25) — one adapter invocation per batch. `waterfall:`
is reserved syntax: the YAML parser MUST accept and reject it with "not
implemented in v0" (so v1 adds it without a format break). This is the only
pipeline authoring surface in v0 (ADR-005); the JSON Schema for this format
is `spec/schemas/pipeline.schema.json`.

**Groups in pipeline YAML (ADR-021).** Five keys, all plan-validated
(§7), all runner-owned (§8):

```yaml
name: q3-qualify
source:
  group: q3-warm            # group as source: members projected from the ledger

steps:
  - id: judge
    use: ai/filter
    exclude: [q3-qualified, q3-rejected]   # judgment memory: judge once per scope
    uses: [full_name, title]
    with: { prompt: ... }

group: q3-qualified         # terminus: records completing the run are added

# …and on a send pipeline's deliver step:
#   record: q3-sent                        # touch scope; defaults to pipeline name
#   suppress: { group: q3-sent, within: 30d }
```

- A **group source** is `source: {group: <name>}` — no `use:`, mutually
  exclusive with it. Members (people and companies alike) are projected
  from the ledger like any record. A group source declares no static
  provides: the plan treats the available-field set as open, and each
  step's needs are enforced per record at run time, exactly like the
  needs-all wildcard.
- `require: [<group>, …]` / `exclude: [<group>, …]` are valid on any
  non-source step, deliver steps included: membership gates, checked
  per record against current membership (§7).
- `record: <name>` and `suppress: {group: <name>, within: Nd}` are valid
  only on deliver steps (§8). `record:` defaults to the pipeline name,
  per deliver step (ADR-031: steps sharing the default share the scope).
- Top-level `group: <name>` is the membership terminus (§8), valid with
  or without deliver steps; a pipeline with neither simply enriches.
- A group source MAY carry `limit: N` (ADR-032, §8): at most N members,
  oldest-added first. `use: group/deliver` with `with: {group: <name>}`
  is a deliver step targeting a group (§8).
- A group source MAY carry `once: true` (ADR-052, §8): select only members
  this pipeline has not already finished, so a bounded consumer advances
  instead of replaying its first batch. Opt-in; a source without it is
  unchanged.

**Bounded group consumers — `once:` (ADR-052).** A group source with
`once: true` selects, among the group's current members, only those this
pipeline has not **finished** — oldest-added first, at most `limit` when
one is set. A record is finished when it completed the pipeline's final
step, or when a filter's fail verdict stopped it; both are outcomes the
pipeline reached deliberately. A record that **failed** a step is NOT
finished and MUST be selected again, so a transient provider error is
retried rather than silently dropping the record; a record left `pending`
is NOT finished either, and collect-first (above) resumes its run rather
than re-sourcing it. The scope is the pipeline's name. `once:` changes
what a source selects and never what a group contains: no membership is
added, removed or touched by it, so the group remains the reviewed
decision ADR-021 made it and an operator MAY re-run the whole set by
removing the key. `gtme plan` MUST print the eligible count beside the
selection, since how much work remains is the fact a scheduled run turns
on and it is knowable before anything is spent:

```
source   group "candidates" — 481 member(s), 471 not yet worked, sourcing 10 (oldest first)
```

A dry or simulated run finishes nothing: a simulated run executes against
a throwaway copy of the ledger, and a dry run's row is marked `dry` (§3)
and left out of terminality. A rehearsal therefore repeats rather than
advancing the queue.

**Config values from the ledger (ADR-037).** Any value under `with:` MAY
be `{query: <SQL>}` or `{segment: <name>}`; the planner resolves it
against the local ledger before config validation, shows the rows, and
fails on zero (§7). This is how a pipeline's source is parameterised by
what an earlier pipeline decided — fan-out at the pipeline boundary,
where run membership is fresh:

```yaml
source:
  use: apollo/search
  with:
    domains: {segment: qualified-domains}   # a saved SELECT yielding one column
    titles: [CFO, "Head of RevOps"]
```

Read a *segment* when the list is a live computed fact that should
drift; read a *group* (snapshot first with `gtme groups add
--from-segment`) when it is a reviewed decision that should not.

---

## 10. v0 adapters — DECIDED

All built-in: Go process adapters under `internal/adapters/<name>/`
(embedded manifest + a `fixtures/` dir + unit tests that run offline
against fixtures), except where an entry notes it now ships as a
registered binding under `spec/bindings/` (§10a) — the same id, the same
contract, pure YAML.

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
   `api.apollo.io/api/v1/mixed_people/api_search` with `X-Api-Key`
   (`APOLLO_API_KEY`); config: `query`, `limit`, `per_page`. **Masked by
   the vendor** (ADR-043; Apollo withdrew value fields from API search
   2026-08-30): provides `apollo.id`, `first_name`, `title`,
   `company_name`, `last_name` (the vendor's obfuscated form, e.g. "D." —
   build-found in M20: it keys the §4 name-hash identity tier, and the
   reveal supersedes it), and `apollo.has_email` (the pay-signal — a
   filter can prefer reachable contacts before anything is spent). Pages by `page`;
   termination on empty/short pages (the response carries `total_entries`
   and no pagination object). -e per record. The revealed person is
   `apollo/enrich`'s job, and `works_at` emission (runner-owned, keyed on
   org domain) fires after it — search alone carries no domain. **Ships
   as the registered built-in binding** `spec/bindings/apollo-search/`
   since v0.11 (rewritten against the masked shape in M20).
2a. **`apollo/enrich`** (enrich, person) — POST
   `api.apollo.io/api/v1/people/match` with `X-Api-Key`
   (`APOLLO_API_KEY`); `needs.required: [apollo.id]`; provides the
   revealed surface: `email` (+`email_status`), `last_name`, `full_name`,
   `linkedin_url`, `city`/`state`/`country`, `company_name`,
   `company_website`, `company_linkedin_url`, `company_industry`,
   `company_domain`, `company_employees`. Declares its per-credit cost;
   retains payloads (ADR-030). The canonical composition (§9): filter on
   the masked fields first, reveal `when: <filter>.passed` — credits are
   spent only past judgment. Ships as the built-in binding
   `spec/bindings/apollo-enrich/` (M20).
3. **`ai/filter`** (filter) — batch records into the prompt with a strict
   JSON-array output schema `[{identity_key, pass, reason}]`; emit VERDICTs;
   config supports `uses:` (§9, ADR-004); MAY declare `provides:` (ADR-033),
   in which case the required output shape is generated from the declared
   schema and the step also emits RECORDs (§5). Prompt assembly (ADR-035):
   records encoded compactly, never pretty-printed; long values wrapped at
   structural breaks so no line exceeds what the engine's tooling reads
   intact; fields whose provenance is an external fetch wrapped in a
   delimiter and labelled in-band as subject-supplied data, the delimiter
   neutralised inside the body *before* wrapping (encode → neutralise →
   wrap) — default on, `fence: false` in config opts out; the operator
   prompt precedes the records as a stated default, with the
   shared/payload split exposed so the order is A/B-able and a cache
   breakpoint can sit between them. AI steps hold no tools. The manifest is
   entity-agnostic (`"entity_type": "*"`, §6): the step's entity type is
   the pipeline's. `deferred: true` (ADR-038) routes the batch to the
   Message Batches API under `custom_id` = identity key and ends the
   session with PENDING (§5, §8); the fixture engine answers
   synchronously. `of:` (ADR-048) names a value the step is about.
3a. **`ai/review`** (role `review`, ADR-048) — same adapter code as item
   3 under the review role: `of:` required, the prompt presents that value
   as the subject and the `uses:` fields as context, the output is the
   declared labels (a grade, a yes/no, notes); emits RECORDs only, never a
   VERDICT — a review does not gate. Prompt assembly and
   entity-agnosticism as item 3.
3b. **`human/filter`, `human/compose`, `human/review`** and their
   **`agent/*`** aliases (ADR-049) — runner-owned; no protocol session, no
   credentials, no cost of their own. Config: `render:` and `prompt:` (§9);
   the declared outputs are the menu. Behaviour in §8 ("People and agents
   answer"). `agent/*` never prompts and records provenance under
   `agent/`.
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
   (strings) by default, or whatever the step's `provides:` declares
   (ADR-033); output schema enforced; config supports `uses:` and `of:`
   (a revision of an existing value is a compose with a referent,
   ADR-048); prompt assembly and entity-agnosticism as item 3.
6. **`instantly/add-to-campaign`** (deliver, person; `idempotency_scope:
   campaign`, ADR-044 — the scope is the configured campaign *name*, so a
   renamed campaign is a new dedupe scope) — Instantly v2 API,
   `Authorization: Bearer $INSTANTLY_API_KEY`: create/attach lead to
   campaign by name (resolve campaign name → id via list endpoint once per
   run; error if absent). Declares dynamic needs (§6, ADR-019) with a
   static floor of `email`; everything else it sends derives from the
   step's `variables:` mapping (ADR-018) — a target name matching one of
   Instantly's first-class lead fields (`first_name`, `last_name`,
   `company_name`, `personalization`) maps into the lead body, and any
   other target name becomes a custom variable of that name. No merge
   field is hard-coded in the adapter.
   Declares `preflights` (ADR-040): before sending, it checks that the
   campaign exists and is Active, that the sequence has at least as many
   steps as the copy assumes (the highest `_step_N` suffix among the
   `variables:` targets), that every `variables:` target appears as
   `{{name}}` in some step body, and that no A/B variant lacks one.
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

## 10a. The binding tier & universal steps — DECIDED (ADR-022..027)

The binding tier, the conformance-kit extension, the naming rule with its
ai/* provenance format, and the OpenAPI rule below were built in milestone
M8 (§11, changelog v0.6); `http/enrich` and `sql/transform`/`sql/filter` in
milestone M11 (changelog v0.9), together with ADR-030's payload retention.

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
cost declaration (per record / per request / unit — `amount_usd` a
number, or a template resolved from config for vendors whose price is
plan-dependent, ADR-046; a page-billed endpoint declares `per: request`,
because `per: record` counts *emitted* records and a `limit` truncates
emission after the vendor has billed the page); and a retry/rate
policy including hourly windows, with an optional session declaration
(a UUID-per-run passed through, for vendors offering
pagination-consistency sessions). Binding roles are source (pagination +
cursor/STATE; `limit` is a reserved engine key, ADR-047 — config
validation accepts it whether or not the binding's `config_schema`
declares it, the engine caps emitted records and terminates pagination
at the cap, and a binding that does declare it receives it unchanged),
enrich (per-record request), and deliver (idempotency + dry-run
receipts). A binding declares the same manifest surface as a
process adapter (`needs`/`provides`/`config_schema`/`freshness_days`, §6)
so `gtme plan` treats both tiers identically; named external bindings are
discovered on the §6 path (`~/.gtme/adapters/<name>/` containing
`binding.yaml` instead of an executable), and reach that path by hand or
from the registry (§8 `gtme adapters`, ADR-042) — the binary ships the
floor and these reference twins only; vendor bindings are registry
entries, verified before they install. `gtme help --bindings` (§8,
ADR-041) is the contract an author works from.

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
(§4a). Provenance carries the engine and the question: for `ai/*` steps,
`field_values.source` MUST record the model identifier and the judgment
signature (§7, ADR-039) in the form `ai/compose @ <model-id>#<signature>`
(e.g. `ai/compose @ claude-sonnet-4-6#1a2b3c4d5e6f`), so two prompts'
outputs are distinguishable in provenance, and COST attributes spend per
model. A `human/*` or `agent/*` step (ADR-049) takes the same form with
the participant in the model's place — `human/review @ trevor#<sig>`,
`agent/filter @ claude-code#<sig>` — the signature over the step
declaration alone (adapter id, `render:`, the declared outputs, `uses:`,
`of:`), never the name: the cache is checked at dispatch, before anyone
has answered. The `done` step event for an AI judgment carries `signature` and
`input` in its detail (§3) — the cache entry the runner reads back.

### `http/enrich` — generic fetch enricher (ADR-024; built in M11)

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

Contract as built (M11): config carries `url` (templated from
`{{record.<field>}}` placeholders, which are also the step's derived
dynamic needs), optional `method`/`query`/`headers`/`auth`, and exactly
one of `markdown: true` + `field: <name>` (the declared content field —
canonical or namespaced per §4a) or `extract: {<field>: <path>, …}` (the
engine's inline JSON mode). `freshness_days` is REQUIRED config with no
default and doubles as the step's cache window, so N AI steps across M
runs reuse one fetch. The engine-enforced size cap defaults to 256 KB
(`max_bytes` overrides); an oversized response is dropped with a warning,
never truncated silently — the record advances counted `empty`, not `out`
(ADR-053), and the oversized response is still retained as a payload for
the step's declared window, because a response the run threw away is
precisely the one an operator needs to look at. Fetched responses attach
as payloads (§5) under the ADR-030 declaration. Under `--simulate` the step is a counted
simulation gap — replaying retained payloads is the ROADMAP
simulate-replay verb, not this build.

**`ai/*` purity invariant:** an AI step's inputs are exactly its
projected fields, and its only network access is its model engine's API
(§2). An `ai/*` step MUST NOT fetch external content — acquisition
belongs to `http/enrich` and bindings, which are deterministic and
cacheable.

### `sql/transform` and `sql/filter` — the deterministic transform floor (ADR-027; built in M11; `sql/enrich` renamed `sql/transform` by ADR-037)

`sql/transform`: a single SELECT over the projection view (plus relations),
scoped to the run's records, executed read-only (`mode=ro`) and
timeboxed, with no side effects. Result columns become field values
appended by the ENGINE like any adapter output — the step never writes
storage directly; append-only, provenance `sql/transform @ <query-hash>`,
freshness semantics all preserved. Contracts are DECLARED, not parsed
from SQL: the step's config carries `uses:` and `provides:`; `gtme plan`
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

Contract as built (M11): like the group source, SQL steps are
runner-owned — no adapter, no wire protocol, ledger access the runner
mediates. Config carries `query` plus declared `uses:` (fields the plan
validates as available) and, for `sql/transform`, `provides:` (the declared
output fields, canonical or namespaced). The query runs ONCE per step —
set-based, not per record — on the read-only connection, timeboxed
(30s), with the run id bound as `:run_id` for scoping convenience; the
engine guarantees scope regardless by applying result rows only to the
run's eligible records (out-of-run rows are dropped and counted). The
result MUST yield an `identity_id` column. `sql/transform`: every declared
provides field must appear as a result column; values append through the
engine with provenance `sql/transform @ <query-hash>`. `sql/filter`: a
`pass` column (with optional `reason`) judges explicitly, or — with no
`pass` column — membership-style: returned records pass, absent records
fail with the predicate named. SQL steps run normally under `--simulate`
(they are offline by construction).

Two semantics were true from M11 and are normative from ADR-037. A SQL
step's query MAY read any identity in the ledger — only its *results* are
scoped to the run — so a cross-record aggregate (a company's related
people via `works_at`, a fan-in) is in-contract, not a trick. SQL steps
never cache-skip: they recompute on every run, which is what makes a
cross-record value safe — it cannot go stale when related records
change. "Transform" is the honest name for both a per-record derivation
and a cross-record aggregate, since the output is always this record plus
fields; `sql/filter` keeps its name because a verdict is a different
output and the reviewer should see the role in the id. Plan-time
`EXPLAIN`, the cross-record annotation, and the two vocabulary views are
in §7 and §3.

### The universal floor (ADR-023; Out half built in M12)

The smallest adapter set with near-total reach docks onto the three
universal transports — files, webhooks, the web. Universality is bought
by pushing semantics into user config, so universal adapters are always
the worst version of any given integration: their job is the guarantee
("wireable today"), not excellence; bindings are the ceiling. The set:
In — `csv/source` (§10.1), `webhook/source` (§10.8), and group-as-source
(§9, built in M9); transform — `ai/*` (pure, above) and `sql/*`; out —
`http/deliver` and `csv/deliver`. Receipts showing the same `http/*`
target recurring across runs are the cue to mint a named binding.

**`http/deliver`** (built in M12): POST the resolved `variables:` per
record to any URL — the binding engine's deliver role invoked
anonymously. Config: `url` (templatable), optional
`method`/`query`/`headers`/`auth`/`body` (a template; its default is the
resolved variables object). The step-level `idempotency:` key is
REQUIRED — even the trivial case cannot infer delivery semantics, it
must be told (ADR-023) — and a missing one is a plan error, not a
defaulted identity key.

**`csv/deliver`** (built in M12; `idempotency_scope: path`, ADR-044): write delivered records to a CSV —
universal output to anything with an import button, and the natural
human-review artifact. Config: `path`; the columns are the `variables:`
targets (sorted, stable), plus a leading `identity_key`. The header is
written once on file creation; rows append across runs, and §8
idempotency is what keeps a re-run from appending duplicates.

---

## 11. Milestones & acceptance criteria — build in this order

Each milestone ends with `make check` green (fmt, vet, unit tests) plus the
listed acceptance test. Acceptance tests live in `test/e2e/` as Go tests
shelling out to the built binary, using fixture adapters — no network, no
real keys — and MUST load their expected shapes from `spec/` (schemas,
`ledger.sql`, golden wire transcripts, acceptance scripts) rather than
re-encoding them, per ADR-010. Milestones M1–M5 must be completable fully
offline.

- **M1 — ledger + identity.** `gtme init`; internal APIs for upsert identity,
  write field, project record; migrations, including the `current_fields`
  view (ADR-003). ✅ Unit tests: identity canonicalization table-driven
  tests; projection picks highest-confidence-in-window via the view; identity
  upgrade path; a schema-conformance test asserting the migrated schema
  matches `spec/ledger.sql`.
- **M2 — protocol + runner core.** Message types, manifest loading, schema
  validation, projection, cache-skip, worker pool, run/step_events/costs
  writing. ✅ E2E: `csv/source → mock-enrich-py` via `gtme run`, ledger
  contains fields with provenance; second run reports 100% cache skips and
  $ avoided; protocol messages round-trip against `spec/schemas/` and the
  golden wire transcript in `spec/wire/`.
- **M3 — plan + receipts + resume + inspection.** `gtme plan` output;
  missing-field and missing-credential plan errors; AI-step `uses:`
  plan-time validation (ADR-004); `--resume`; terminal receipt; `gtme show`
  (ADR-006); `gtme help --agent` (ADR-007). ✅ E2E: a pipeline with an
  unsatisfiable `needs` (or `uses`) fails at plan with the right step named;
  kill a run mid-step (fixture adapter with induced failure), `--resume`
  completes without re-processing done records; `gtme show` on a known
  identity and on `--run last` matches the ledger; `gtme help --agent`'s
  output round-trips per its acceptance criterion in §8.
- **M4 — AI steps + query + deliver semantics.** `ai/filter`, `ai/compose`
  with output-schema validation + one retry + `uses:` projection; `gtme
  query` (+ `--save`); deliveries idempotency with a `mock/deliver` fixture
  adapter. ✅ E2E: filter verdicts gate downstream steps; malformed AI
  output (fixture engine returning garbage once) retries then succeeds;
  double `gtme run` produces zero duplicate deliveries; saved query returns
  expected rows. (AI engine behind an interface; tests use a fake engine.)
- **M5 — real adapters.** Apollo, Harvest, Instantly against live APIs,
  each with fixture-based unit tests plus a `--live` build tag for manual
  smoke tests; `webhook/source` against a local spool fixture (no live
  dependency — offline-testable like `csv/source`). ✅ The pipeline in §9
  runs end-to-end with real keys (manual gate — see §12); `webhook/source`
  drains a fixture spool without re-sourcing consumed lines on a second run.
- **M6 — polish.** `gtme runs`, README with 60-second quickstart,
  `brew`-style install script, `gtme secret set`.
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

- **M8 — binding engine + simulation gate (ADR-022, ADR-028; §10a, §8).
  Built 2026-08-16 (changelog v0.6).**
  The generic HTTP engine interpreting `spec/binding-schema.json`;
  `apollo/search`, `harvest/profile`, and `instantly/add-to-campaign`
  ported to bindings; the conformance kit extended to bindings;
  `gtme run --simulate`. ✅ Acceptance: receipt diff against each Go twin
  on campaign-zero data matches (dry runs where delivery is involved);
  first net-new integration is a pure-YAML **Attio** binding (assert
  endpoint, idempotency: native) passing conformance; the campaign-zero
  pipeline simulates end-to-end with zero network calls.
- **M9 — groups (ADR-021). Built 2026-08-16.** Spec impact applied (changelog v0.7: §3
  DDL, §7 gates and plan checks, §8 touch/suppress/terminus and
  `gtme groups`, §9 surface), then built. ✅ Unit: the membership view
  derives current membership from added/removed sequences (last event
  wins; `touched` never affects membership); suppression window
  arithmetic. ✅ E2E, offline: a qualify pipeline (csv → ai/filter ⇒
  group terminus) adds exactly the passers; re-running it with
  `exclude:` on its own output groups judges nothing a second time
  (zero AI-engine calls — judgment memory); a send pipeline's deliver
  writes `touched` under its default (pipeline-name) scope, and a second
  pipeline sharing the scope via `record:` suppresses within the window,
  receipted with reasons; a group source feeds a pipeline the members a
  qualify run added; `gtme plan` fails naming a `require:`/`exclude:`/
  `suppress:`/source group that does not exist; a `--dry-run` writes no
  `touched` and no terminus `added` events; `gtme groups` list/show/
  add/remove round-trip, including `--query` snapshot with provenance.
- **M10 — campaign bundles (ADR-029; §8). Built 2026-08-16.**
  `gtme freeze --bundle`, `gtme run` on a bundle path, simulate-on-bundle.
  ✅ Acceptance: freeze campaign zero, move the bundle to a clean ledger,
  simulate and dry-run it successfully. Sequenced after groups so bundled
  pipelines carry group references against built semantics from day one.
- **M11 — the transform floor + payload retention (ADR-024, ADR-027,
  ADR-030; §10a, §3, §5, §6, §8).** `http/enrich` (markdown + inline JSON
  modes, mandatory freshness, size cap), `sql/transform`/`sql/filter`
  (runner-owned, declared contracts, query-hash provenance), the
  `payloads` cache tier with RECORD payload attachments, declared
  retention, opportunistic eviction, and `gtme vacuum`.
  ✅ Unit: payload eviction removes exactly the expired; the SQL contract
  (identity_id required, provides columns checked, run-scoped
  application). ✅ E2E, offline: `http/enrich` fetches a local page to
  markdown into a declared field with a payload retained, and a re-run
  within the freshness window cache-skips the fetch; `sql/transform` writes
  a derived field with `sql/enrich @ <hash>` provenance; `sql/filter`
  gates records by predicate, both explicit-`pass` and membership-style;
  a `require:`d plan names missing `uses:` fields; `gtme vacuum` reports
  evictions and touches nothing else; `--simulate` counts `http/enrich`
  as a gap while SQL steps run.
- **M12 — the universal Out floor (ADR-023; §10a).** `http/deliver` and
  `csv/deliver`. ✅ E2E, offline: `http/deliver` dry-runs to a resolved-
  variables receipt with zero calls, delivers the mapped variables to a
  local URL when armed, and re-delivers nothing on a re-run; a plan
  without its `idempotency:` key fails naming the rule; `csv/deliver`
  writes header + rows, appends nothing on a re-run, and the file is
  reviewable as written.
- **M13 — delivers as steps (ADR-031; §7, §8, §9). Built 2026-08-17
  (changelog v0.14).** Remove the top-level
  `deliver:` block from `internal/pipeline` and the schema; accept
  deliver-role adapters as ordinary `steps:` entries, any number, any
  position; role-gate `variables:`/`on_missing:`/`idempotency:`/`record:`/
  `suppress:` at plan time; dry-run withholding, resolved-variables
  receipts, `deliveries` idempotency, touch scoping, and suppression per
  deliver step, with `skip`/suppression advancing the record and `fail`
  freezing it (§8); `gtme plan` calls out every deliver step; migrate
  `examples/`, README.md's quickstart, VALIDATION.md's pipelines, and
  e2e fixtures off the block (they stay on the old shape until this
  milestone so every committed YAML runs against the shipped binary).
  ✅ E2E, offline: a pipeline with a mid-list deliver step and a final
  deliver step dry-runs to resolved variables for both with zero
  `deliveries` writes; armed, it delivers to both, and a re-run delivers
  nothing twice on either; a record failing between the two delivers to
  the first only and is absent from the terminus group, while a record
  suppressed at the final deliver step completes and joins it;
  `variables:` on a non-deliver step fails plan naming step and key; a
  document with a top-level `deliver:` block fails validation.
- **M14 — the composition pass (ADR-032, ADR-033, ADR-035, ADR-036,
  ADR-037; §3, §5, §6, §7, §8, §9, §10, §10a). Built 2026-08-28
  (changelog v0.16).**
  Build in this order, each step `make check` green: (1) ADR-033 —
  step-level `provides:` on AI roles, schema-generated output shape,
  `enum`, VERDICT+RECORD from filters, entity-agnostic AI manifests,
  `<pipeline>.<field>` default; (2) ADR-035 — compact encoding, wrapping,
  default-on fencing with `fence: false`, stated order with the split
  exposed; (3) ADR-032 — `group/deliver`, group-source `limit:`,
  `groups remove --note`, the one-commit-point plan warning; (4) ADR-037 —
  rename `sql/enrich` → `sql/transform` (examples, e2e fixtures,
  ADAPTERS.md, README follow), the two views (migration `0007`, mirrored
  to `spec/ledger.sql`), plan-time `EXPLAIN` and the cross-record
  annotation, `{query:}`/`{segment:}` config resolution ahead of
  config-schema validation, ledger read surface in `help --agent`;
  (5) ADR-036 — `deliveries.status`/`sent_at` (same migration), `attests`
  manifest capability, receipt and `gtme show` wording, Instantly as the
  first attesting adapter.
  ✅ E2E, offline: an `ai/filter` declaring `provides: {state: {enum:
  [a, b]}, rationale: {}}` stores `<pipeline>.state` and rejects a value
  outside the enum; a company-entity pipeline plans an AI step against
  the company registry; a pipeline with two `group/deliver` steps
  dry-runs to two rendered receipts with zero group events and, armed,
  routes passers and failers to different groups; a group source with
  `limit: 2` sources the two oldest members; a source `with:` carrying
  `{query: …}` plans with the rows shown and fails on zero; `sql/enrich`
  fails plan naming `sql/transform`; a `sql/transform` joining
  `relations` is annotated cross-record; a query naming an unknown column
  fails plan; a delivery lands `accepted` and a fixture adapter declaring
  `attests` yields `confirmed`, `contradicted`, and `inconclusive` in
  turn with the documented receipt behavior; the AI fixture engine
  receives compact, fenced records and `fence: false` removes the fence.
  The account pattern (four pipelines chained through groups) simulates
  end to end with zero network calls.
- **M15 — asynchronous steps (ADR-038; §3, §5, §7, §8, §9, §10). Built
  2026-08-29 (changelog v0.18).** PENDING and OPEN `pending` in `internal/protocol` and the
  schemas; the `pending`/`collected` step events and the `pending` run
  status; the last-step rule and the respend warning in the planner;
  collect-first `gtme run` and collection in the runner's dispatch; the
  API engine's batch submit/collect path (`custom_id` = identity key);
  `deferred` in the AI manifests' `config_schema` and `respend:` in the
  pipeline schema; receipt and `gtme runs` wording; the fixture engine's
  scripted PENDING for tests.
  ✅ E2E, offline: a deferred `ai/filter` as the last step ends the run
  `pending` with zero verdicts, zero cost, and a receipt naming the token;
  a plain `gtme run` of the same pipeline while it is pending collects
  rather than re-submitting — zero new batch submits, asserted — and,
  while the provider is still processing, leaves the run `pending`; the
  next `gtme run` collects — verdicts land, COST lands under the same run,
  the terminus asserts, the run finishes `done`; the following `gtme run`
  sources fresh records; a deferred step anywhere but last fails plan
  naming the fix; `--dry-run` on a deferred pipeline warns; `--simulate`
  runs the step synchronously and says so; a judgment step with no
  `exclude:` plans with the respend warning and `respend: true` silences
  it; a credentialed enrich with no window warns the same way. The API
  engine's batch path is unit-tested against a stubbed Batches endpoint
  (submit, poll, results keyed by `custom_id`).
- **M16 — the judgment cache (ADR-039; §3, §7, §10a). Built 2026-08-29
  (changelog v0.20).** Signature and input hash computed in the runner's prepare
  from the step config, the adapter's shape and the projection; lookup
  over `done` events; verdict re-application for filters; the provenance
  suffix; the AI respend warning retired; receipt wording.
  ✅ E2E, offline: a re-run of a judgment pipeline with unchanged prompt
  and inputs dispatches nothing (every record `skipped_cache` with reason
  `same_judgment`, filter verdicts re-applied so downstream gating and
  the terminus behave as in the first run, zero AI calls asserted via the
  fixture log); changing the prompt re-judges every record; changing one
  record's input field re-judges only that record; `cache: 1d` with a
  ledger clock past the window re-judges; `respend: true` re-judges; a
  compose's provenance carries the signature and `gtme show --provenance`
  shows it; the AI respend warning no longer appears while the
  paid-enrich one still does; `--simulate` of a judged pipeline
  cache-skips; a deferred step cache-checks before submitting.
- **M24 — participants (ADR-048, ADR-049, ADR-050; §2, §3, §7, §8, §9,
  §10, §10a). Built 2026-09-02 (changelog v0.35).** A migration adds
  `field_values.referent`; `step_events.event` gains `answered`; the
  pipeline schema gains `of:`, `render:`, `prompt:` and loses `engine:`
  (a plan error naming the fix);
  the `claude-code` engine is deleted; `ai/review` (manifest + prompt
  shape); `human/filter|compose|review` and `agent/*` as runner-owned
  adapters with the in-run TTY walk; the planner validates `of:` and
  `render:` as `uses:`, includes the referent's value in the input hash,
  and prints the cron note; the runner ends such a run `pending` under a
  runner-owned token and collects from `answered` events, writing facts
  with `human/`/`agent/` provenance, the referent, and the ADR-039
  signature; `gtme answer` (all three addressing forms, `--set`, `--as`,
  `--cost`, `--note`, the interactive walk, refusals); `gtme show --run
  --pending`; receipt and `gtme runs` wording; `gtme show --provenance`
  prints the referent and note; `help --agent` gains the answer rhythm and
  the routing-as-pattern example.
  ✅ E2E, offline: a review pipeline (`human/review` with `of:
  <pipeline>.first_line`, `provides: grade` enum) run with no TTY ends
  `pending` with the receipt naming the verb; `gtme answer review.yaml
  jane --set grade=Z` is refused naming A–F, `--set grade=B`
  records, and the next `gtme run` collects — `grade` lands with
  `human/<user>` provenance and a `referent` pointing at the reviewed
  `first_line` row; re-running with the same draft cache-skips
  (`same_judgment`), a rewritten draft re-pends; a `human/filter` answered
  `pass=false` freezes the record and `pass=true` advances it; an
  `agent/review` step under `--as claude-code --cost 0.01 --measured`
  lands `agent/claude-code` provenance and a measured cost row; the in-run
  walk asks, validates and stops on interruption with the rest pending
  (proved at the walk and at the runner's ask/do-not-ask decision — the
  terminal itself is `term.IsTerminal` on stdin, and a pty is a dependency
  beyond §2); `engine: claude-code` fails plan naming `agent/*`; a deliver
  step after a `human/*` step plans with the cron note; `when:
  <review>.passed` fails plan; `--simulate` counts the step as a
  simulation gap.
- **M27 — record accounting (ADR-053; §5, §7, §8, §9, §10). Built
  2026-09-04 (changelog v0.41).** A
  field-writing step counts `empty` for a record it advanced without
  writing anything, and `in` reconciles against the classified columns;
  the source line reconciles rows read against records sourced with
  coalescing recorded per record; a participant step takes `on_missing:
  run | skip | fail` (default `run`), and the receipt reports records
  dispatched with a declared field absent regardless; a dropped record
  keeps its payload under ADR-030's tier; a paid run that produced no
  records is marked on the receipt and in `gtme runs`, exit code
  unchanged. `spec/schemas/pipeline.schema.json` rides the build.
  Acceptance, offline: a `sql/transform` whose query matches nothing
  reports `2 in, 0 out, 2 empty` and the records still advance; an
  `http/enrich` over its byte cap does the same rather than reporting
  full throughput; a compose declaring `uses: [a, b]` where `b` is absent
  for some records runs on all of them by default and the receipt names
  the count, `on_missing: skip` advances those records untouched, and
  `on_missing: fail` fails them naming `b`; a CSV whose rows coalesce
  reports the reconciliation and a SQL query answers which row merged
  into which identity; a run that spent and sourced nothing is marked;
  every per-step line reconciles `in` against its classified columns.
- **M26 — bounded group consumers (ADR-052; §3, §8, §9). Built 2026-09-04
  (changelog v0.40).** A group
  source gains `once: true`: selection skips members this pipeline has
  finished — completed the final step, or stopped by a filter verdict —
  oldest-added first within `limit`, with failed and pending records
  deliberately still eligible. Scope is the pipeline name; terminality is
  read from earlier runs' records, so the only new persisted fact is
  `runs.dry` (migration 0012), which the build found ADR-052 (7) needed.
  `gtme plan` prints the eligible count beside the selection; a dry or
  simulated run advances nothing.
  `spec/schemas/pipeline.schema.json` rides the build.
  Acceptance, offline: a three-member group with `limit: 2` and
  `once: true` sources members 1–2, then 3 on the next run, then nothing;
  the same pipeline without `once:` still replays 1–2 forever (the
  reported behavior, unchanged when the key is absent); a record a filter
  froze counts as finished and is not re-offered, while a record whose
  step failed IS re-offered on the next run; a group whose members were
  all finished sources zero records and the run ends without a step; the
  group's membership is byte-identical before and after (no added,
  removed or touched events from `once:`); `gtme plan` names the eligible
  count; a `--dry-run` followed by an armed run sources the same members.
- **M25 — plan visualization (ADR-051; §7, §8, §13). Built 2026-09-04
  (changelog v0.37).**
  `gtme plan --viz` appends a diagram of the resolved plan to the default
  output; `--viz-only` prints it instead. One renderer over the existing
  resolved plan — no planner change, no network, no spend, stderr like the
  rest of plan's human-facing output. Shape carries role (rounded source,
  funnel filter, light-rect enrich, doubled-rail verify, wavy-floor
  compose, trapezoid review, heavy deliver); a two-slot glyph pair carries
  executor then role, so a free offline `sql/transform` and a paid
  `apollo/enrich` are distinguishable at a glance though both are role
  `enrich`; each edge is labelled with the fields its step added to the
  available set, making §7 step 2's walk visible. Fixed-width frame, no
  colour, no TTY or terminal-width detection, so the bytes are
  deterministic and golden-testable; padding measures display width rather
  than counting runes (emoji are two columns wide) via a hand-rolled table,
  leaving §2's dependency list untouched. Acceptance: a pipeline exercising
  every role renders under `--viz` and `--viz-only`, with every box row the
  same display width and no line wider than a conventional terminal; both
  shipped examples plan and render; the default output is byte-identical with
  no flag; every §7 fact still appears in the listing; `--viz` with
  `--viz-only` is a validation error.
- **M23 — honest costs + engine-owned limit (ADR-046, ADR-047; §3, §5,
  §7, §8, §10a). Built 2026-09-01 (changelog v0.33).** Migration 0010
  rebuilds `costs` with `basis` (backfill `estimated`); the COST message
  carries `basis` and every built-in that spends labels its emissions per
  the reserved-`measured` rule (the claude-code engine's reported
  `total_cost_usd` is the one measured source; every rate-multiplied
  amount, the binding engine's included, is estimated); receipts and
  `gtme runs` print totals with their basis (bare / `(estimated)` / split
  when mixed); `binding-schema.json` lets `amount_usd` template from
  config and `gtme plan` prints `est/record: unset` for an unresolved
  rate; the engine accepts `limit` on any source binding, declared or
  not; `gtme help --bindings` and CONTRIBUTING carry the `per: request`
  page-billing guidance and the `limit` reservation.
  ✅ E2E, offline: a fixture binding with a templated rate runs at the
  operator's figure and its cost rows say `estimated`; the same binding
  with no rate set plans as `unset` and runs at $0 `(estimated)`; a
  fixture adapter emitting vendor-reported cost lands `measured` and a
  mixed run prints the split total on both the live receipt and `gtme
  runs`; a strict binding that does not declare `limit` accepts `limit:
  1` and stops paginating after one record (one request), while an
  unknown key is still refused.
- **M22 — on-change re-delivery (ADR-045; §3, §6, §8, §9). Built 2026-08-31
  (changelog v0.31).** Migration 0009 adds `variables_hash`; manifest
  `idempotency: native|ledger` (bindings bridge theirs); `redeliver:`
  grammar + plan validation + per-adapter defaults; the runner's deliver
  path resolves variables before the dedupe decision, hashes them, and
  upserts the row on re-delivery (status back to accepted).
  ✅ E2E, offline: an armed attio/assert against a local server delivers
  once; re-run unchanged skips with reason `unchanged`; a changed value
  re-delivers (the server sees a second assert, the row's hash moves,
  rows stay 1); `redeliver: always` re-asserts regardless;
  `redeliver: on_change` on csv/deliver fails plan naming the native
  requirement; dry receipts distinguish `unchanged` from
  `already_delivered`.
- **M21 — scoped delivery dedupe (ADR-044; §3, §6, §8, §10). Built 2026-08-31
  (changelog v0.29).** Migration 0008 rebuilds `deliveries` with `scope` and
  UNIQUE(target, scope, idempotency); `idempotency_scope` in the manifest
  and binding schemas and the three declarations (instantly: campaign,
  attio: object, csv/deliver: path); the runner resolves the scope from
  step config for the check, the insert, and attestation updates;
  `spec/ledger.sql` matches the migrated shape.
  ✅ E2E, offline: the same record delivered to two campaigns through one
  adapter lands two `deliveries` rows; re-running either campaign adds
  nothing; group handoffs unchanged (`scope=''`); a pre-0008 ledger
  migrates with `scope=''` backfilled; the attestation update touches
  only its own scope's row.
- **M20 — the Apollo split (ADR-043; §9, §10). Built 2026-08-30
  (changelog v0.27).**
  `spec/bindings/apollo-search/` rewritten against `mixed_people/api_search`
  (masked provides, `page` pagination, empty/short-page termination); new
  `spec/bindings/apollo-enrich/` against `people/match` (needs `apollo.id`,
  revealed provides, per-credit cost, payloads retained); fixtures
  re-recorded from live probes, sanitized; §9's and `help --agent`'s
  examples move to filter-then-reveal.
  ✅ Offline: the conformance kit passes both bindings' new fixtures; the
  §9 example and every `help --agent` example pass `gtme plan`
  (`TestHelpAgentExamplesPassPlan`); a pipeline needing `email` off
  `apollo/search` alone fails plan naming `apollo/enrich`. Live
  (human-gated, §12): Campaign 1 story 2 sources masked and reveals only
  past the filter, credits visible per step in the receipt.
- **M18 — `help --bindings` (ADR-041; §8). Built 2026-08-30
  (changelog v0.24).** The
  second agent surface in `internal/cli`, regenerated from the embedded
  schema and bindings; `help --agent` gains its pointer.
  ✅ E2E, offline: `gtme help --bindings` is JSON whose `schema` equals
  `spec/binding-schema.json` byte for byte, whose reference binding
  validates against that schema and resolves through `gtme plan` when
  installed on the discovery path it names, and whose text names the
  path and the fixtures expectation; `help --agent` carries the pointer;
  the unknown-adapter error names `binding.yaml` and the verb.
- **M19 — the bindings registry (ADR-042; §6, §8, §10a, §13). Built 2026-08-30
  (changelog v0.25).** Tarball fetch and extract, `.source.json`, `adapters
  add / search / verify / update / list` in `internal/cli`, the index
  schema; `verify` reuses the binding conformance runner; fixture minting
  from retained payloads (ADR-030) for the first registry entry; the
  `gtme-bindings` repository seeded with the round-trip's CRM source
  binding as the first verified entry.
  ✅ E2E, offline: against a local tarball server and a local index,
  `adapters search` finds an entry by vendor; `adapters add` installs it
  under the dashed id with `.source.json` carrying the resolved commit and
  content hash, runs its fixtures first and prints the hosts and
  credentials it will use; an entry with failing fixtures, or none, does
  not install; a content-hash mismatch against the index refuses; the
  installed binding resolves through `gtme plan` and serves its fixtures
  under `--simulate`; `adapters update` moves the pin only when asked;
  `adapters` lists source and pin; a bundle (ADR-029) records the pin.
- **M17 — deliver preflight (ADR-040; §5, §6, §8, §10). Built 2026-08-29
  (changelog v0.22).** PREFLIGHT and OPEN `preflight` in `internal/protocol` and
  the schemas; `preflights` in the manifest; the preflight session in the
  runner ahead of a deliver step's record sessions at `--dry-run` and arm;
  receipt wording; Instantly's four checks in its HTTP file; a fixture
  adapter for the acceptance.
  ✅ E2E, offline: a fixture deliver adapter declaring `preflights` answers
  `ok`, `blocked`, and `inconclusive` in turn — `ok` delivers with the
  checks on the receipt; `blocked` under `--dry-run` reports the check and
  writes nothing, and armed fails the step with zero deliveries, zero
  adapter record sessions, records at the previous state, run `failed`,
  and `--resume` after flipping the fixture delivers; `inconclusive`
  delivers with a warning; a non-preflighting adapter is never asked; a
  pipeline with two deliver steps preflights each. Instantly's checks are
  unit-tested against stubbed campaign and sequence endpoints: active,
  paused, too few steps, an unreferenced variable, an unfilled variant, and
  an unreadable target.

Repo layout:
```
cmd/gtme/            # main
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
recipe, §8), dashboards or any UI (a one-shot static render such as
`gtme plan --viz` is not one: no process, no interaction, no retained
state — see ADR-051), DAG/branching beyond
`when:`, `waterfall:` execution (parse-and-reject only), email waterfall
providers, company-pipeline fan-out verbs (the relations table exists; no
verbs over it — see ROADMAP.md's `expand` role for the deferred version),
teams/auth, MCP server mode (see ROADMAP.md), a *hosted* adapter
marketplace — accounts, payments, a service (the bindings registry, an
index and a fetch verb, is in scope: §8, ADR-042),
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
never seen, **when** the operator runs `gtme plan` then `gtme run
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
**when** the operator runs `gtme show <identity-key> --provenance`,
**then** the output lists every current field value (per the
`current_fields` view, §3) together with its source adapter, confidence,
and the run that wrote it, and the command performs no ledger write.

### Iterate
**Invariant:** a pipeline change is checked before it is trusted, cheaply.
**Given** a `pipeline.yaml` edited to add or change a step, **when** the
operator runs `gtme plan` before `gtme run`, **then** any unsatisfiable
`needs`/`uses` or missing credential is reported as a plan error naming
the step and the missing field (§7) before any adapter is invoked; **and**,
once `gtme plan` succeeds, a `gtme run` scoped to a small source (e.g. a
low `limit` in `source.with`) exercises the full step chain end-to-end at
minimal cost before a full-size run.

### Segment
**Invariant:** a slice of accumulated knowledge is a SQL statement away,
and the same slice can seed a new pipeline.
**Given** a ledger with records from one or more prior runs, **when** the
operator runs `gtme query --save NAME "SELECT ..."` with a single
SELECT/WITH/EXPLAIN statement, **then** the statement is stored and
re-executable by name, it runs against a read-only connection (`mode=ro`)
so it cannot mutate the ledger, and its result set can be used to scope
which identities a subsequent pipeline or `gtme show` inspects.

### Guard
**Invariant:** a pipeline that would fail or overspend is caught before
it starts, not partway through.
**Given** a `pipeline.yaml` with a step whose `needs.required` (or `uses`,
for AI steps) is not satisfied by any upstream `provides`, **when** the
operator runs `gtme plan`, **then** the command exits non-zero with an
error naming the specific step and field (§7), performs zero network
calls, and writes zero cost rows — the same guarantee holds for an
unresolvable declared credential (§6).

### Recover
**Invariant:** a killed run picks up where it left off, never redoing
completed, paid-for work.
**Given** a run killed or crashed partway through (some records past a
step, some not), **when** the operator runs `gtme run pipeline.yaml
--resume RUN_ID`, **then** every record whose `run_records.state` already
reflects completion of a step does not re-invoke that step's adapter or
incur that step's cost again, and the run reaches `status='done'` covering
the records that had not yet completed.

### Report
**Invariant:** what happened in a run, and what it cost, is always
reconstructable after the fact.
**Given** one or more completed runs, **when** the operator runs `gtme
runs` (to list) or `gtme runs RUN_ID` (for one run's receipt), **then** the
output reports, per step: records in/out, cache skips, and cost — matching
the sums in `step_events` and `costs` for that `run_id` exactly, with
no reconstruction required from raw table scans.

---

## Changelog

Format: [Keep a Changelog](https://keepachangelog.com/). This project does
not yet have numbered releases; entries are keyed by the reconciliation
pass that produced them.

### v0.41 — 2026-09-04 (M27 build: record accounting, built)
**Changed:** §11 M27 marked built. Two clarifications the build needed,
both inside ADR-053's decision: §8 says what `in` now counts — every
record eligible at the step, since the reconciliation `in = out + empty +
filtered + failed + gated + skipped + cached` cannot hold otherwise (before
this, `in` was records handed to the adapter, so a fully cached step read
`0 in, 0 out, 3 cached`; it now reads `3 in, 0 out, 3 cached`) — and that
a record in flight, held by a dry run, or passed through a simulation gap
is the named non-terminal remainder. §5 says a RECORD with no `fields`
asserts nothing and is not validated: it is how `http/enrich` hands the
runner an oversized response to retain (ADR-053 (4)) without claiming a
field. `on_missing:` gains `run` in §9's vocabulary and
`spec/schemas/pipeline.schema.json`; `run` is refused on a deliver step.
The receipt table gains an `empty` column; the step line prints `empty`
beside `out` when non-zero, and `skipped`, `simulated` and `held (dry
run)` when non-zero; a participant step's absent declared fields print as
`(N missing f1, f2)` and a receipt line names the policy. The source line
reconciles (`sourced 931 records (9 coalesced into known identities)`) and
each coalesce is a `coalesced` step event on the identity that won, with
the row's own keys in `detail`. A paid zero-record run titles its receipt
and its `gtme runs` entry `done — 0 records, $X spent (estimated)`.
Implementation choices — what "coalesced" means, what "empty" means, and
the remainder columns — are recorded as the 2026-09-04 M27 decision.

### v0.40 — 2026-09-04 (M26 build: bounded group consumers, built)
**Changed:** §11 M26 marked built. §3 `runs` gains `dry INTEGER NOT NULL
DEFAULT 0` (migration 0012, appended last, no rebuild): the build found
that ADR-052 (7) — a dry run finishes nothing — rested on dry runs writing
no run state, which §8 (ADR-019) contradicts: a dry run has its own run
row, and its records advance to the deliver step's state. Without a
marker a rehearsal would have advanced the queue; the e2e that proves the
rule failed before the column and passes after it. §8's dry-run paragraph
says the row is marked and `gtme runs` shows it (`done (dry)`); the
`once:` paragraph names the actual mechanism instead of the premise.
Human-approved in conversation on 2026-09-04. Terminality is judged
against each run's own snapshot (the final step it declared when it ran),
so a pipeline that later grows a step does not re-offer everyone it had
already worked — recorded as an M26 implementation decision. `gtme help
--agent`'s `human-review-then-cron` recipe carries `once: true` on the
scheduled half, since that composition is what the key exists for.

### v0.39 — 2026-09-04 (ADR-053: record accounting; build queued as M27)
**Changed:** §10 (`http/enrich`: an oversized response is retained and
its record counts `empty`); §7 (`on_missing:` on a participant step, default `run`, and the
receipt's missing-field report); §8 (the `empty` column and the per-step
reconciliation, the source's rows-read/records-sourced line with
coalescing recorded per record, the paid-zero-record mark); §9
(`on_missing:` beyond deliver steps); §11 gains M27.
`spec/schemas/pipeline.schema.json` rides the build.
**Not changed:** §3 — the coalescing record is a `step_events` row, which
§3 already carries, so M27 has no migration. Exit codes are untouched by
design (ADR-053 (5)).
**Numbering:** assumes ADR-052 / M26 (the `once:` packet) lands first. If
that packet is rejected, this becomes ADR-052 / M26 at merge.
**Status:** accepted 2026-09-04 by merging the packet; built as M27.

### v0.38 — 2026-09-04 (ADR-052: bounded group consumers; build queued as M26)
**Changed:** §8 gains the `once:` selection rule (finished = completed or
filter-stopped; failed and pending stay eligible), the plan line carrying
the eligible count, and the dry-run rule; §9 gains `once:` in the group
source grammar; §11 gains M26. `spec/schemas/pipeline.schema.json` rides
the build.
**Not changed:** §3 — by ADR-052 (4) the scope is the pipeline name and
nothing new is persisted, so this milestone carries no migration. *(The
build overturned this: see v0.40 — `runs.dry` was needed for (7).)* A group
source without `once:` behaves exactly as before, so no shipped pipeline
changes meaning.
**Status:** accepted 2026-09-04 by merging the packet; built as M26 (v0.40).

### v0.37 — 2026-09-04 (M25 build: plan visualization, built)
**Changed:** §11 M25 marked built. No normative text changed beyond that
mark and one acceptance criterion: v0.36 had already written the surface,
and this pass built it. `internal/planner/viz.go` renders the resolved plan
as a diagram — a pure function over the existing plan, so the planner is
untouched and the default listing is byte-identical with no flag; `--viz`
appends it, `--viz-only` replaces the listing, and both together are a
validation error. `gtme help --agent` carries the flags, since §8 requires
it to be the full surface. M25's acceptance criterion was rewritten to name
a role-complete pipeline rather than "both shipped examples":
`examples/demo.yaml` does not pass `gtme plan` today, a divergence from
README.md that predates this work and is recorded in AUDIT.md.

### v0.36 — 2026-09-03 (ADR-051: plan visualization)
**Added:** §7 — `gtme plan --viz` appends a diagram of the resolved plan to
the default output, `--viz-only` prints it instead. Both render the same
resolved plan on stderr with no network and no spend; the default output is
unchanged and stays the normative surface for §7's MUST-print items, so a
fact appearing only in the diagram is a defect. §8's verb table gains the
flags and states that no verb is added. §13's "dashboards or any UI"
non-goal gains a parenthetical placing a one-shot static render outside it.
§11 gains M25. The shape and glyph vocabulary lives in ADR-051,
deliberately not in the spec: a second implementation must print the
listing, and is free to draw or skip the picture. No schema, ledger, wire,
or exit-code change. Build queued as M25 (until it lands, decided contract,
not shipped behavior).

### v0.35 — 2026-09-02 (M24 build: participants, built)
**Changed:** §11 M24 marked built. No normative text changed beyond that
mark: v0.34 had already written the participants surface, and this pass
built it. Migration 0011 adds `field_values.referent`; `step_events`
carries `answered`; `human/filter|compose|review` and their `agent/*`
aliases ship as runner-owned manifests with no protocol session; the
planner validates `of:`/`render:` as `uses:`, refuses `engine:` naming
`agent/*`, refuses `when: <review>.passed`, and prints the cron note; the
runner pends participant records under `<run-id>/<step-id>`, walks them
in-run at a terminal, and collects `answered` events into facts with
`human/<name>`/`agent/<name>` provenance, the referent, and the
participant's cost; `gtme answer` and `gtme show --run --pending` join the
verb set; the `claude-code` engine is deleted. `spec/ledger.sql`, the
pipeline schema and the participant manifests ride this entry.
**Not changed:** the wire protocol (a participant step never opens a
session), and every non-participant step's behavior.

### v0.34 — 2026-09-02 (ADR-048..050 reconciliation: participants; build queued as M24)
**Changed:** §2 (API is the only model engine; `engine:`
removed; `claude-code` retired); §3 `field_values.referent`,
`step_events` `answered`; §6 the `review` role and the
`credentials_optional` example; §7
`of:`/`render:` validation, the cron note; §8 `gtme answer`, `gtme show
--run --pending`, the "People and agents answer" subsection,
`--provenance` shows the referent and note, the verb set; §9 `of:`,
`render:`, `prompt:` grammar, `engine:` a plan error; §10 items 3, 3a
(`ai/review`), 3b (`human/*`, `agent/*`), 5; §10a provenance form; §11
milestone M24. Schema artifacts (`spec/ledger.sql`, the pipeline and
manifest schemas) ride the build.
**Not changed:** nothing built — M24 is queued (accepted 2026-09-02 by
merging the packet).

### v0.33 — 2026-09-01 (M23 build: honest costs + engine-owned limit, built)
**Changed:** §11 M23 marked built; no normative text changed beyond
v0.32 — migration 0010, both schema artifacts (`spec/ledger.sql`,
`spec/binding-schema.json`) and the COST message schema
(`spec/schemas/msg-cost.schema.json` admits `basis`) landed with the
build. Behavioural notes: built-ins label `estimated` explicitly on the
wire rather than relying on the absent default; a `$0` total with
estimated rows prints `$0 (estimated)` (an unset rate stays visible
after the run, not just at plan); the plan-time rate resolves with the
binding's `config_schema` defaults applied, so plan and run agree.

### v0.32 — 2026-08-31 (ADR-046/047 reconciliation: honest costs, engine-owned limit; build queued as M23)
**Changed:** §3 `costs.basis`; §5 COST `basis` with the
reserved-`measured` rule; §7 plan prints `est/record: unset` for an
unresolved templated rate; §8 receipt totals carry their basis; §10a
cost declaration may template `amount_usd` from config, page-billed
guidance (`per: request`), and `limit` as the source role's reserved
engine key; §11 milestone M23. Schema artifacts
(`spec/binding-schema.json`, `spec/ledger.sql`) ride the build.
**Not changed:** nothing built — M23 is queued. `cost_estimate_usd`
templating and registry-maintained cost declarations deferred to
ROADMAP.md (ADR-046).

### v0.31 — 2026-08-31 (M22 build: on-change re-delivery, built)
**Changed:** §11 M22 marked built; no normative text changed beyond
v0.30 — migration 0009, both schema artifacts, and the plan's
`redeliver:` line landed with the build. Behavioural notes: variables
resolve before the dedupe decision on deliver steps; a re-delivery
upserts the existing row (first `created_at` kept, hash/run/status
refreshed); the skip reasons `unchanged` and `already_delivered` are
distinct in step_events.

### v0.30 — 2026-08-31 (ADR-045 reconciliation: on-change re-delivery; build queued as M22)
**Changed:** §3 `variables_hash`; §6 manifest `idempotency:
native|ledger`; §8 redeliver modes with adapter-gated defaults; §9
`redeliver:` grammar; §10a's binding key meaning completed; §11 milestone
M22. Schema artifacts ride the build.
**Not changed:** nothing built — M22 is queued.

### v0.29 — 2026-08-31 (M21 build: scoped delivery dedupe, built)
**Changed:** §11 M21 marked built; no normative text changed beyond
v0.28 — migration 0008 rebuilds `deliveries` (SQLite cannot alter a
UNIQUE), `spec/ledger.sql` mirrors the migrated shape, and the doc's
ledger surface describes the triple key. Behavioural notes: the scope
resolves from the step's resolved config at run time (group handoffs and
undeclared adapters stay ''); `gtme show` prints `scope` on a delivery
when present; attestation updates address only their own scope's row.

### v0.28 — 2026-08-31 (ADR-044 reconciliation: scoped delivery dedupe; build queued as M21)
**Changed:** §3 `deliveries` gains `scope` and the triple
UNIQUE; §6 manifest `idempotency_scope`; §8 deliver idempotency rewritten
around `(target, scope, idempotency)` with the global guarantee moved to
suppression groups; §10 declarations (instantly: campaign — the
configured name; attio: object; csv/deliver: path); both schema
artifacts; §11 milestone M21. Editorial: §8's doubled heading. One-time
migration consequence stated in ADR-044.
**Not changed:** nothing built — M21 is queued. `spec/ledger.sql` changes
ride the M21 build (it is machine-compared to the migrated database).

### v0.27 — 2026-08-30 (M20 build: the Apollo split, built)
**Changed:** §11 M20 marked built; one normative amend found by the build —
§10 item 2's masked provides gains `last_name` (the vendor's obfuscated
form): a masked row carries no email, linkedin or domain, so §4's
name-hash tier is its only identity path and that tier requires a first
AND last name; the reveal supersedes the obfuscated value. Behavioural
notes: `apollo/search` is version 2; `apollo/enrich` joins the embedded
built-ins; pagination termination is empty/short page (the masked
response has no pagination object); a missing-need plan error now names
installed adapters that provide the field ("email ← apollo/enrich");
live smoke 2026-08-30: 3 masked rows $0, 3 reveals $0.03, one contact
legitimately email-less (absent-tolerance held).

### v0.26 — 2026-08-30 (ADR-043 reconciliation: the Apollo split; build queued as M20)
**Changed:** §10 item 2 rewritten (apollo/search masked, per
the vendor's 2026-08-30 withdrawal) and item 2a added (apollo/enrich,
`people/match`, per-credit reveal); §9's canonical example gains the
reveal step — filter on free fields, pay past the filter; §11 milestone
M20. AUDIT.md (b) item 4 is applied by this diff.
**Not changed:** nothing built — M20 is queued.

### v0.25 — 2026-08-30 (M19 build: the bindings registry, built)
**Changed:** §11 M19 marked built; no normative text changed — v0.23's
§6/§8/§10a contract is shipped behaviour, covered by M19's acceptance.
Behavioural notes from the build: the GitHub API and codeload endpoints
are env-overridable (GTME_GITHUB_API, GTME_GITHUB_CODELOAD) so the
offline acceptance drives a local tarball server with the address
grammar unchanged; the content hash is sha256 over the binding
directory's files (sorted slash paths, path NUL body NUL, `.source.json`
excluded); `.source.json` also records the repository url and path,
which `update` re-fetches from; a fixtures file may carry optional
`config` and `input` members so `verify` can drive a real engine run;
the registry index is consulted best-effort at `add` — unreachable
warns and skips the hash check, a mismatch refuses; ADR-030's fixture
minting stays ROADMAP-parked (no §8 verb exists for it) — the first
registry entry's fixtures are minted registry-side.

### v0.24 — 2026-08-30 (M18 build: `help --bindings`, built)
**Changed:** §11 M18 marked built; no normative text changed — v0.23's
§8 contract is shipped behaviour, covered by M18's acceptance.
Behavioural notes from the build: the document is one JSON object with
the schema spliced in verbatim as its last member (an encoder would
compact it); the reference binding is whichever shipped binding is
fullest by bytes (amended 2026-08-30 with ADR-041; first built as
smallest, which picked the extract-less deliver binding), with its
fixtures file beside it; the discovery
section prints the live search path; ADR-042's `adapters` verbs appear
under a `registry` member flagged as queued until M19 ships them, so
the document never names a verb this binary lacks.

### v0.23 — 2026-08-29 (ADR-041/042 reconciliation: the adapter surface; builds queued as M18 and M19)
**Added:** §8 `gtme help --bindings` (the second agent surface,
with its own round-trip criterion) and the `gtme adapters` verbs (registry
search, verified install, pin, update); §6/§10a URL-addressed, pinned
bindings via `.source.json` and the registry tier; §11 milestones M18 and
M19; `spec/schemas/registry-index.schema.json`. **Changed:**
§13's non-goal narrowed to a *hosted* marketplace — the registry (an
index and a fetch verb) is in scope. AUDIT.md (b) item 3 is applied by
this diff.
**Not changed:** nothing built — M18 and M19 are queued.

### v0.22 — 2026-08-29 (M17 build: deliver preflight, built)
**Changed:** §11 M17 marked built; no normative text changed — v0.21's
contract is shipped behaviour, covered by M17's acceptance. Behavioural
notes from the build: the preflight session runs in `runStep` before the
step's records are even prepared, so a blocked armed run leaves them at
the previous state with no `claimed` events; the outcome is recorded as a
step-level `preflight` event (detail: status, reason, checks); a dry run
reports `blocked` and continues (nothing sends anyway); Instantly's
first-class targets (`first_name`, `last_name`, `company_name`,
`personalization`) map into the lead body and are not checked against
the template, only merge variables are.

### v0.21 — 2026-08-29 (ADR-040 reconciliation: deliver preflight; build queued as M17)
**Added:** §5 PREFLIGHT and OPEN `preflight`; §6 `preflights`
capability; §8 deliver preflight (dry-run/arm behaviour, receipt); §10
item 6 Instantly's checks; §11 milestone M17;
`spec/schemas/msg-preflight.schema.json`, `msg-open.schema.json`,
`manifest.schema.json`, the wire README. Editorial: §11's M15/M16 order.
**Not changed:** nothing built — M17 is queued.

### v0.20 — 2026-08-29 (M16 build: the judgment cache, built)
**Changed:** §11 M16 marked built; no normative text changed — v0.19's
contract is shipped behaviour, covered by M16's acceptance. Behavioural
notes from the build: the runner computes both keys (the signature from
the step's config, resolved model and provides schema; the input hash
from the projection), so the adapter is untouched; the signature's model
is the one the armed run would use, never the simulate override, so a
rehearsal skips what an armed run skips; a cached filter fail counts as
both cached and filtered on the receipt; `cache: 0d` on an AI step is
`respend: true`; collected (ADR-038) records carry the keys too, so a
re-run after a collection submits only what changed.

### v0.19 — 2026-08-29 (ADR-039 reconciliation: the judgment cache; build queued as M16)
**Added:** §7 judgment cache for AI roles (signature + input
hash, no window by default, `cache:`/`respend:` as the knobs); §11
milestone M16. **Changed (proposed):** §7 respend warning narrowed to
paid enrich/verify; §10a provenance `ai/<op> @ <model-id>#<signature>` and
the `done` event's `signature`/`input` detail keys (prose; no DDL).
**Not changed:** nothing built — M16 is queued.

### v0.18 — 2026-08-29 (M15 build: asynchronous steps, built)
**Changed:** §11 M15 marked built; no normative text changed — v0.17's
contract is shipped behaviour, covered by M15's acceptance. Behavioural
notes from the build: the fixture engine's batch surface persists its
batches and script cursor beside the script (`GTME_AI_FIXTURE_DEFER`),
since a provider holds a batch across processes; a deferred submit sends
one request per record with the shared prompt as a cached block; a
collection with an invalid answer for a record fails that record by
omission (there is no retry against a batch); `gtme runs` shows an
in-flight count for `pending` runs.

### v0.17 — 2026-08-29 (ADR-038 reconciliation: asynchronous steps; build queued as M15)
**Added:** §5 PENDING message and OPEN `pending`, with the
in-flight rules; §8 in-flight steps (`deferred: true` as the pipeline's
last step, the `pending` run status, collect-first `gtme run` with
`--resume` as its explicit form, receipt wording); §7 the respend warning
and §9 `respend: true`; §9/§10 items 3 and 5 `deferred: true`; §11
milestone M15; `spec/schemas/msg-pending.schema.json`,
`msg-open.schema.json`. §3 DDL comments: `runs.status` gains `pending`,
`step_events.event` gains `pending|collected` — comments only, no
migration, mirrored to `spec/ledger.sql`.
**Not changed:** nothing built — M15 is queued.

### v0.16 — 2026-08-28 (M14 step 1 build: `canonical: true` on declared AI provides)
**Added:** §7/§9 `canonical: true` on a declared AI output field — the
explicit mapping onto a canonical field that §4a/§7 implied without
naming (found building ADR-033, queued in AUDIT.md (b), human-approved
2026-08-28); §4a tier 3 reworded to point at it. Every bare name
namespaces otherwise, canonical-looking or not, and `gtme plan` notes the
coincidence. `spec/schemas/pipeline.schema.json`: a `provides` map value
is null or an object of `type`/`enum`/`canonical`. §6 entity-agnostic
manifests: `"entity_type": "*"` names what §10.3 asserted without an
encoding (AUDIT.md (b), approved 2026-08-28); a source may not declare
it; `manifest.schema.json` describes the sentinel; the two AI manifests
declare it.
**Added:** §5 ATTEST — the wire form of ADR-036's three-way verdict,
which its spec-impact list left out (found building step (5); approved
2026-08-28): `spec/schemas/msg-attest.schema.json`, the wire README's
table, and the `attests` key in `manifest.schema.json`.
**Changed:** `spec/ledger.sql` and §3's DDL — M14's migration `0007`
landed (step (4)): `deliveries.status`/`sent_at`, the `current_values`
and `group_membership` views; §3's queued-deltas note now records them as
landed. §10a heading typo (`sql/enrich` renamed `sql/transform`).
**Changed:** §11 M14 marked built — all five steps and the account-pattern
capstone run offline in the suite.

### v0.15 — 2026-08-28 (ADR-032/033/035/036/037 reconciliation; build queued as M14)
**Added:** §7 declared AI provides, config values from the ledger, SQL at
plan (EXPLAIN, cross-record annotation), the one-commit-point warning;
§8 `group/deliver`, group-source `limit:`, `groups remove --note`,
delivery `status` semantics (`accepted`, attestation, `sent` only by
attestation), ledger read surface in `help --agent`; §9 `provides:` on AI
steps, `limit:` on the group source, `{query:}`/`{segment:}` config
values with example; §6 `attests` capability; §5 VERDICT+RECORD from a
filter; §4a `<pipeline>.<field>` default for AI outputs; §3 queued schema
deltas (`deliveries.status`/`sent_at`, two vocabulary views — DDL lands
with M14's migration); §11 milestone M14.
**Changed:** §10 items 3 and 5 (declared provides, prompt assembly rules,
`fence`, entity-agnostic manifests, AI steps hold no tools); §10a
`sql/enrich` renamed `sql/transform` throughout the normative text with
its two semantics stated (reads any identity; never caches).
`spec/schemas/pipeline.schema.json`: step gains `provides`; group source
gains `limit`.
**Not changed:** `spec/ledger.sql` — DDL moves with its migration, as
always. ROADMAP.md: `expand` retired by composition; Groups option C
refusal recorded as tested; SQL segments half delivered; three new
entries (patterns as bundles, asynchronous steps, deliver preflight).

### v0.14 — 2026-08-17 (M13 build: delivers as steps, built)
**Changed:** §11 M13 marked built; no normative text changed — v0.13's
contract is now shipped behavior, covered by M13's acceptance tests.
Behavioral notes from the build: a document carrying the old top-level
`deliver:` block fails validation with an error naming the fix (move the
block into `steps:`); a withheld send (`on_missing: skip`, suppression)
advances `run_records.state` to the deliver step, which is what makes the
record visible to later steps and the terminus; `gtme plan` renders the §7
deliver call-out as one block after the step list (`send surface: N
deliver step(s)`, one line per step with target and touch scope); two
deliver steps naming the SAME target adapter share that target's
`(target, idempotency)` dedupe scope — the §3 key is per target, not per
step, so independent dedupe (ADR-031's promise) is per distinct target;
`gtme runs`' record summary counts fail verdicts without classifying them
(a bare run id carries no step roles) and says so. `examples/`, README's
quickstart, VALIDATION.md's pipelines, `gtme help --agent`'s canonical
pipelines, and the e2e fixtures all moved off the old block in the same
change.

### v0.13 — 2026-08-17 (ADR-031: delivers as steps)
**Changed:** The top-level `deliver:` block is gone; deliver adapters
are ordinary `steps:` entries — any number, any position (§9) — with
`variables:`/`on_missing:`/`idempotency:`/`record:`/`suppress:`
role-gated to deliver steps and every §8 deliver semantic (idempotency
scope, dry-run withholding, resolved-variables receipt, touch scope,
suppression) applying per step. New advancement rule made explicit:
`on_missing: skip` and suppression withhold the step's send but the
record advances; `fail` freezes it. Terminus clarified as completers-only
— a mid-pipeline delivery followed by a later failure does not join;
`record:`'s `touched` events remember sends. §7: `gtme plan` MUST call
out each deliver step (target, touch scope) — the at-a-glance send
surface moves from YAML position to plan output.
`spec/schemas/pipeline.schema.json` updated; milestone M13 queues the
build (until it lands, decided contract, not shipped behavior). Rejected
alongside (recorded in ADR-031): a run-scoped step cardinality —
aggregate exports are `batch: true` delivers; run-summary notifications
are a run-lifecycle hook, parked in ROADMAP.md.

### v0.12 — 2026-08-16 (the name: gtme)
**Changed:** The project, binary, and every surface derived from them are
named **gtme** — as in *GTM engineer* — resolving the naming question
before first publication: `gtm` collides with existing tools. Renamed
wholesale (pre-public, zero users, no compatibility shims): the binary
(§2), the env prefix (`GTME_LEDGER`, `GTME_CONCURRENCY`, `GTME_AI_*`,
…), the home directory (`~/.gtme`), the schema `$id` host, and the
module path (`github.com/elegant-atomics/gtme`). Historical document text
was renamed too — this changelog's earlier entries read as if the tool
was always called gtme, which pre-publication it effectively was. The
one deliberate exception: `gtm-campaign-zero-*` in VALIDATION.md names
real external Instantly campaigns and keeps its historical spelling.

### v0.11 — 2026-08-16 (apollo/search Go scaffolding retired)
**Changed:** `apollo/search` is now the registered built-in binding
(`spec/bindings/apollo-search/`); the Go adapter — never run live (no key
was ever set) and proven redundant by M8's full-field-parity receipt diff
— is removed. First vendor source shipped as pure YAML. The live smoke
test drives the binding engine. harvest/instantly Go adapters remain (they
carry tier-2 capabilities their bindings deliberately omit).

### v0.10 — 2026-08-16 (M12: the universal Out floor)
**Added:** §10a `http/deliver` (anonymous deliver binding; step-level
`idempotency:` REQUIRED per ADR-023 — a plan error when missing, never a
default) and `csv/deliver` (variables as columns plus `identity_key`,
header-once, append-across-runs, §8 idempotency preventing duplicate
rows); milestone M12. Auth declared in an `http/*` step's config
(`auth.env`) now resolves through the credential machinery (§6: env
first, then `~/.gtme/secrets`) and is plan-checked. ROADMAP's
"Universal Out floor" entry retired — built.

### v0.9 — 2026-08-16 (M11: transform floor + ADR-030 payload retention)
**Added:** §3 `payloads` DDL — the ADR-030 cache tier, explicitly exempt
from append-only, never projected (mirrored in `spec/ledger.sql`); §5
optional `payload` attachment on outbound RECORDs — the capture path
ADR-030's mechanism requires, called out here because the ADR's
spec-impact list named §3/§6/§8/§10a and this is its minimal §5
counterpart (`spec/schemas/msg-record-out.schema.json` updated); §6
`keep_payloads`/`payload_ttl_days` retention surface with per-step
override (Go vendor adapters attach nothing yet — a recorded queued
adoption); §8 `gtme vacuum` and opportunistic run-start eviction; §10a
`http/enrich` and `sql/enrich`/`sql/filter` contracts as built (config
surfaces, 256 KB default cap, `:run_id` binding, `identity_id` result
contract, query-hash provenance, membership-style filters); milestone
M11 with acceptance criteria.

### v0.8 — 2026-08-16 (M10 build: campaign bundles)
**Changed:** §8 bundles marked built. Behavioral notes from the build:
`gtme freeze` now preserves the frozen pipeline's own name (falling back
to `frozen-<id>` only for ad hoc runs, `--name` still wins) — a bundle
carries the campaign's identity; bundle adapter resolution takes
precedence over built-ins and the search path while a bundle runs, so
frozen binding versions win; external process adapters do not travel
(executables are not data) and freeze warns per step; a bundle's
relative input files (a source CSV) and credentials remain
operator-provided, per "membership and cache naturally differ". Bundles
carry each referenced binding's conformance fixtures, which is what
makes simulate-on-bundle fully offline; content hashes are verified on
every bundle run and a mismatch is a validation error.

### v0.7 — 2026-08-16 (M9: groups — ADR-021 reconciliation)
**Added:** §3 layer-3 DDL — `groups`, append-only `group_events` with
exactly three event kinds (added/removed/touched), and the
`group_members` last-event-wins view (ADR-003's append-then-derive
pattern); §7 membership gates (`require:`/`exclude:`, the
judgment-memory mechanism) and plan-time group checks (referenced groups
must exist; `record:`/terminus create on demand; windows well-formed;
read-only ledger access at plan time stated); §8 touch scoping
(`record:`, defaulting to the pipeline name), suppression
(`suppress: {group, within}` layered above the idempotency floor, skips
receipted), the membership terminus, the
dry-runs-assert-nothing-durable rule, and the `gtme groups` verb set
(list with derived character, show, add/remove by key,
`--from-segment`/`--query` snapshots with evaluation provenance); §9
the five group keys with the qualify/send decomposition example;
milestone M9 acceptance criteria. Mirrored in `spec/ledger.sql` and
`spec/schemas/pipeline.schema.json`.

### v0.6 — 2026-08-16 (M8 build: binding engine + simulation gate)
**Changed:** `spec/binding-schema.json` hardened by the M8 build against the
three real ports and the Attio binding (extraction `paths:` waterfalls,
`absent:` sentinel values, `skip_if_input:`, the engine-owned `linkedin`
classify-and-route transform, the `$variables` body splice, pagination
`in: body|query`, config defaults from `config_schema`, `extract` required
only for source/enrich, declared-but-unenforced retry windows refuse to
load). No other normative text changed: §10a's binding tier, its
conformance kit, binding discovery, ai/* model provenance, and §8's
`--simulate` gate are now built and covered by M8's acceptance tests
(receipt diffs against each Go twin; the campaign-zero shape simulating
end-to-end with zero network calls); §10a's `http/enrich` and `sql/*`
sections and §8's bundles remain decided-with-build-queued.

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
simulation gate — `gtme run --simulate`, the completed
simulate → plan → dry-run → armed ladder, SIMULATED receipts excluded
from projection/cache (ADR-028); §8 campaign bundles — `gtme freeze
--bundle`, `spec/bundle-manifest.json`, `gtme run` accepting a bundle
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
§10.1, ADR-018); `gtme run --dry-run` and the armed gate (§8, ADR-019);
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
dynamic needs and its plan-time validation (ADR-004); `gtme show` (ADR-006);
`gtme help --agent` (ADR-007); `webhook/source` adapter and the spool+cron
event-driven recipe (ADR-009); the eight operator-story acceptance sections
(ADR-012); pointers to `spec/` machine-checkable artifacts throughout.
**Changed:** CLI surface reduced to exactly `init, secret, plan, run, query,
show, runs, freeze, help --agent` (ADR-005); milestones renumbered M1–M6
with M3 (old M4) absorbing plan/receipts/resume plus the new show/help
verbs, M6 (old M7) polish; §1 bet 4 rewritten from "two modes, one engine"
to YAML as the sole pipeline surface, with `gtme freeze`'s job redefined as
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
