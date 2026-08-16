-- Payloads: raw vendor responses as cache, not facts (SPEC §3, DECISIONS.md
-- ADR-030). The one purgeable table in the ledger. Mirrored verbatim in
-- spec/ledger.sql.

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
