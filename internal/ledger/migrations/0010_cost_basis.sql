-- ADR-046: honest costs. Each cost row records whether its amount was
-- measured (vendor-reported cost metadata in the response) or estimated
-- (multiplied out from a config or manifest rate). Existing rows backfill
-- `estimated`, which under that rule is what every pre-M23 amount was.
-- Mirrored verbatim in spec/ledger.sql; the table is rebuilt (not
-- ALTERed) so the column lands where SPEC §3 declares it, before detail.
CREATE TABLE costs_new (
  id          TEXT PRIMARY KEY,
  run_id      TEXT NOT NULL,
  step_id     TEXT NOT NULL,
  identity_id TEXT,
  provider    TEXT NOT NULL,
  amount_usd  REAL NOT NULL,
  basis       TEXT NOT NULL DEFAULT 'estimated',
  detail      TEXT,
  created_at  TEXT NOT NULL
);
INSERT INTO costs_new (id, run_id, step_id, identity_id, provider, amount_usd, basis, detail, created_at)
  SELECT id, run_id, step_id, identity_id, provider, amount_usd, 'estimated', detail, created_at FROM costs;
DROP TABLE costs;
ALTER TABLE costs_new RENAME TO costs;
CREATE INDEX ix_costs_run ON costs(run_id, step_id);
