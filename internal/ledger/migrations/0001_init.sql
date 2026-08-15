-- Layer 1: identity (durable, cross-run, the cache)

CREATE TABLE identities (
  id           TEXT PRIMARY KEY,          -- ULID
  entity_type  TEXT NOT NULL,             -- 'person' | 'company' (extensible)
  identity_key TEXT NOT NULL,             -- canonical key, see SPEC §4
  created_at   TEXT NOT NULL,             -- RFC3339
  UNIQUE(entity_type, identity_key)
);

CREATE TABLE field_values (
  id          TEXT PRIMARY KEY,           -- ULID
  identity_id TEXT NOT NULL REFERENCES identities(id),
  field       TEXT NOT NULL,              -- e.g. 'email', 'linkedin_url'
  value       TEXT NOT NULL,              -- JSON-encoded value
  source      TEXT NOT NULL,              -- adapter id, e.g. 'harvest/profile@1'
  confidence  REAL NOT NULL DEFAULT 1.0,  -- 0.0-1.0
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
CREATE INDEX ix_step_events_run ON step_events(run_id, step_id, created_at);

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
CREATE INDEX ix_costs_run ON costs(run_id, step_id);

CREATE TABLE deliveries (
  id             TEXT PRIMARY KEY,
  identity_id    TEXT NOT NULL,
  target         TEXT NOT NULL,           -- adapter id
  idempotency    TEXT NOT NULL,           -- computed key, see SPEC §8 deliver
  run_id         TEXT NOT NULL,
  created_at     TEXT NOT NULL,
  UNIQUE(target, idempotency)
);
