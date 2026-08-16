-- Groups: the association primitive (SPEC §3 layer 3, DECISIONS.md ADR-021).
-- Mirrored verbatim in spec/ledger.sql.

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
