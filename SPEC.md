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

- `person`: first non-empty of
  1. lowercased, trimmed email
  2. normalized LinkedIn slug (strip protocol, host, trailing slash, query;
     lowercase; e.g. `in/jane-doe`)
  3. `sha256(lower(full_name) + "|" + lower(company_domain))` prefixed `nh:`
- `company`: registrable domain (eTLD+1, lowercased; use
  `golang.org/x/net/publicsuffix`), else `sha256(lower(name))` prefixed `nh:`.

If an incoming record matches an existing identity on a *stronger* key than
it was created with (e.g. name-hash identity later gains an email), the
runner MUST update `identity_key` in place — it MUST NOT create a
duplicate. It MUST log a `step_events` entry `event='identity_upgraded'`.

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
gtm run  pipeline.yaml [--resume RUN_ID]
gtm query "SQL"                   # read-only SQL against the ledger
gtm query --save NAME "SQL"       # saved segment
gtm show <identity-key>           # read-only projection inspector
gtm show --run last [--fields ...] [--provenance] [--limit N]
gtm runs [RUN_ID]                 # list runs / show one run's receipt
gtm freeze [RUN_ID|last] > pipeline.yaml
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
  idempotency: email
```

Schema rules: `when:` supports only `<step_id>.passed` in v0. `cache:` takes
`Nd`. `uses:` (ADR-004) is a list of field names, valid only on steps whose
adapter role is `filter`/`compose`/an AI-backed role; the planner validates
it exactly as `needs.required` (§7). Steps execute strictly in order; within
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
2. **`apollo/search`** (source, person) — POST
   `api.apollo.io/api/v1/mixed_people/search` with `X-Api-Key`
   (`APOLLO_API_KEY`); config: `query`, `limit`; paginate; map
   name/title/email/linkedin/org fields; also emit the company as a related
   identity: create `works_at` relation when org domain present.
3. **`ai/filter`** (filter) — batch records into the prompt with a strict
   JSON-array output schema `[{identity_key, pass, reason}]`; emit VERDICTs;
   config supports `uses:` (§9, ADR-004).
4. **`harvest/profile`** (enrich, person) — HarvestAPI LinkedIn profile
   lookup by `linkedin_url` (`HARVEST_API_KEY`); provides `headline`,
   `recent_posts`, `role_history`; emit COST from response metadata if
   present, else config-estimated.
5. **`ai/compose`** (compose) — batch; provides `first_line`, `ps_line`
   (strings); output schema enforced; config supports `uses:`.
6. **`instantly/add-to-campaign`** (deliver, person) — Instantly v2 API,
   `Authorization: Bearer $INSTANTLY_API_KEY`: create/attach lead to
   campaign by name (resolve campaign name → id via list endpoint once per
   run; error if absent); needs `email`, `first_name`; optional
   `first_line`, `ps_line` as custom variables.
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

Repo layout:
```
cmd/gtm/            # main
internal/{ledger,identity,protocol,runner,planner,cli,adapters/...}
adapters/mock-enrich-py/
spec/               # machine-checkable artifacts: schemas, ledger.sql, wire/, acceptance/
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
