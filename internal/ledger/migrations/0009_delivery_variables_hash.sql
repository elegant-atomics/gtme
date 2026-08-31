-- ADR-045: on-change re-delivery. Each delivery records the hash of its
-- resolved variables: values; `redeliver: on_change` (SPEC §8, §9)
-- re-delivers only when it moves. Mirrored verbatim in spec/ledger.sql.
ALTER TABLE deliveries ADD COLUMN variables_hash TEXT NOT NULL DEFAULT '';
