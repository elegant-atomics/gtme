-- Saved segments: `gtme query --save NAME "SQL"` (SPEC §8). The spec names the
-- feature but not its storage, so it lives here rather than in a dotfile —
-- a segment is data about the ledger and belongs beside it.

CREATE TABLE saved_queries (
  name       TEXT PRIMARY KEY,
  query_sql  TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
