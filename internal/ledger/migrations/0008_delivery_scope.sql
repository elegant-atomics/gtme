-- ADR-044: delivery dedupe scopes to the campaign, not the adapter.
-- deliveries gains `scope` — the resolved value of the config key the
-- manifest names in idempotency_scope (SPEC §6, §8) — and the dedupe key
-- becomes UNIQUE(target, scope, idempotency). Mirrored verbatim in
-- spec/ledger.sql. SQLite cannot alter a UNIQUE constraint, so the table
-- is rebuilt; existing rows backfill scope = '' (the one-time consequence
-- is stated in DECISIONS.md ADR-044).
CREATE TABLE deliveries_new (
  id             TEXT PRIMARY KEY,
  identity_id    TEXT NOT NULL,
  target         TEXT NOT NULL,
  scope          TEXT NOT NULL DEFAULT '',
  idempotency    TEXT NOT NULL,
  run_id         TEXT NOT NULL,
  created_at     TEXT NOT NULL,
  status         TEXT NOT NULL DEFAULT 'accepted',
  sent_at        TEXT,
  UNIQUE(target, scope, idempotency)
);
INSERT INTO deliveries_new (id, identity_id, target, scope, idempotency, run_id, created_at, status, sent_at)
  SELECT id, identity_id, target, '', idempotency, run_id, created_at, status, sent_at FROM deliveries;
DROP TABLE deliveries;
ALTER TABLE deliveries_new RENAME TO deliveries;
