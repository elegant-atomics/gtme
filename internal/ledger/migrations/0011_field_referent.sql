-- ADR-048: the referent. field_values gains `referent` — the field_values.id
-- of the value a review or edit was about (SPEC §3, "was-about"); null unless
-- the step declared of:. Mirrored verbatim in spec/ledger.sql. SQLite cannot
-- insert a column mid-table, so the table is rebuilt (transient
-- field_values_new, dropped by the rename) to match §3's column order, which
-- the schema-conformance test compares. The three views that read the table
-- (field_value_ranks, current_fields, current_values) are dropped first and
-- recreated verbatim after the rename: SQLite re-parses every view when a
-- table is renamed, and a view over a table that no longer exists is an
-- error at that moment.
DROP VIEW current_values;
DROP VIEW current_fields;
DROP VIEW field_value_ranks;

CREATE TABLE field_values_new (
  id          TEXT PRIMARY KEY,
  identity_id TEXT NOT NULL REFERENCES identities(id),
  field       TEXT NOT NULL,
  value       TEXT NOT NULL,
  source      TEXT NOT NULL,
  confidence  REAL NOT NULL DEFAULT 1.0,
  run_id      TEXT,
  referent    TEXT,
  created_at  TEXT NOT NULL
);
INSERT INTO field_values_new (id, identity_id, field, value, source, confidence, run_id, referent, created_at)
  SELECT id, identity_id, field, value, source, confidence, run_id, NULL, created_at FROM field_values;
DROP TABLE field_values;
ALTER TABLE field_values_new RENAME TO field_values;
CREATE INDEX ix_fv_lookup ON field_values(identity_id, field, created_at DESC);

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

CREATE VIEW current_values AS
SELECT identity_id, field, json_extract(value, '$') AS value, source, confidence, run_id, created_at
FROM current_fields;
