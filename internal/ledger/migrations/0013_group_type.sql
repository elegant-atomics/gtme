-- M28's schema delta (SPEC §3; DECISIONS.md ADR-054 (8)). Mirrored verbatim
-- in spec/ledger.sql.
--
-- groups gains entity_type: the type of its members, set once when the
-- group is created (by a terminus or group/deliver from the run's type, by
-- `gtme groups add` from an unambiguous key, or by --type) and enforced on
-- add. Nullable: a group created before ADR-054 has no type and stays
-- entity-blind until `gtme groups add NAME --type TYPE` sets it. Appended
-- last, in §3's column order, so no table rebuild is needed.
ALTER TABLE groups ADD COLUMN entity_type TEXT;
