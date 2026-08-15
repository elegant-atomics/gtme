-- The current-value projection (SPEC §3, DECISIONS.md ADR-003). Two views,
-- not one, because "highest confidence within the freshness window" (SPEC
-- §3's actual wording) cannot be answered by a single unparameterized view:
-- freshness is a per-caller window (SPEC §7's per-step `cache:`), so a view
-- with no notion of "now" can only ever answer the *unwindowed* question.
--
-- field_value_ranks is the one definition of the ranking rule (confidence
-- DESC, ties broken by newest created_at) — both current_fields and the
-- runner's windowed projection (internal/ledger/project.go) read through it,
-- so the rule itself can never drift between query-land and runner-land, even
-- though windowing still has to happen where the window is known.
--
-- current_fields = field_value_ranks with no window applied (rank = 1): the
-- plain, ad hoc `gtm query` answer to "what's the current value", matching
-- SPEC §8's query examples. The runner's Project() instead reads every rank
-- for a field, in order, and takes the first row that falls inside its
-- freshness window — the same rule, applied with a window current_fields
-- alone cannot express.
--
-- Mirrored verbatim in spec/ledger.sql.

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
