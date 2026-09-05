-- M26's schema delta (SPEC §3; DECISIONS.md ADR-052 (7), build note).
-- Mirrored verbatim in spec/ledger.sql.
--
-- runs gains dry: 1 when the run was a --dry-run rehearsal. A rehearsal is
-- an ordinary run in every other respect (ADR-019: its own row, receipt and
-- `gtme runs` entry), but it finishes nothing a `once:` source counts, so
-- the ledger has to know which runs rehearsed. Appended last, in §3's column
-- order, so no table rebuild is needed.
ALTER TABLE runs ADD COLUMN dry INTEGER NOT NULL DEFAULT 0;
