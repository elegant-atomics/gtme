-- M14's schema deltas (SPEC §3; DECISIONS.md ADR-036, ADR-037). Mirrored
-- verbatim in spec/ledger.sql.
--
-- deliveries gains status and sent_at (ADR-036): `accepted` is what the
-- provider took; `sent` is written only by attestation.
ALTER TABLE deliveries ADD COLUMN status TEXT NOT NULL DEFAULT 'accepted';
ALTER TABLE deliveries ADD COLUMN sent_at TEXT;

-- Vocabulary views (ADR-037): the read surface user-authored SQL (§10a) and
-- config queries (§9) are written against, so a query reads as vocabulary
-- rather than as table internals. current_values is current_fields with the
-- JSON encoding unwrapped; group_membership is group_members keyed by name.
CREATE VIEW current_values AS
SELECT identity_id, field, json_extract(value, '$') AS value, source, confidence, run_id, created_at
FROM current_fields;

CREATE VIEW group_membership AS
SELECT g.name AS group_name, m.group_id, m.identity_id
FROM group_members m
JOIN groups g ON g.id = m.group_id;
