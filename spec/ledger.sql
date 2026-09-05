-- spec/ledger.sql — the canonical gtme ledger schema (SPEC.md §3, DECIDED).
--
-- This file is the machine-checkable form of SPEC.md §3, per ADR-010. It is
-- transcribed verbatim from that section and MUST stay identical to it. Running
-- it against an empty SQLite database produces the canonical schema:
--
--   sqlite3 canonical.db < spec/ledger.sql
--
-- The Go test suite applies it to a fresh in-memory database and compares the
-- resulting object names against a ledger opened through the real migration
-- path (internal/ledger). Implementations MAY add tables the spec does not name
-- (they are spec-invisible per ADR-010's litmus); they MUST NOT omit anything
-- defined here.

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

-- The current-value projection (ADR-003) is two views, not one: "highest
-- confidence within the freshness window" (SPEC §3) cannot be answered by an
-- unparameterized view, because the window is a per-caller argument (SPEC
-- §7's per-step `cache:`), not a fact about the row. field_value_ranks is the
-- one definition of the RANKING rule (confidence DESC, ties broken by newest
-- created_at); current_fields is that ranking with no window applied
-- (rank = 1) — the plain `gtme query` answer. The runner's windowed
-- projection (internal/ledger/project.go) reads field_value_ranks directly
-- and takes the first in-window row per field, falling through a stale
-- top-ranked row to the next-best one — the same ranking, just windowed
-- where the window is actually known.
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

-- Layer 3: groups — named associations between identities and a context
-- (ADR-021, SPEC §3). A group carries no type field and no executable logic:
-- its character is derived from its events and the pipelines that reference
-- it. Members are identities, so groups hold people and companies alike.

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
-- read (SPEC §8).
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

-- Payloads: raw vendor responses as CACHE, not facts (ADR-030, SPEC §3).
-- Extracted = fact (append-only, above); unextracted = cache (purgeable).
-- Never projected into any step; evicted opportunistically at run start and
-- by `gtme vacuum` (SPEC §8). NULL expires_at = keep until explicit vacuum.

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
