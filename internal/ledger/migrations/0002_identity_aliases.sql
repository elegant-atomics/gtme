-- Identity keys are upgraded in place when a record arrives with a stronger key
-- (SPEC §4). Aliases keep every key an identity has ever been known by pointing
-- at it, so a later record carrying only a weaker key still resolves to the same
-- identity instead of creating a duplicate. See DECISIONS.md (2026-08-12).

CREATE TABLE identity_aliases (
  entity_type  TEXT NOT NULL,
  identity_key TEXT NOT NULL,
  identity_id  TEXT NOT NULL REFERENCES identities(id),
  created_at   TEXT NOT NULL,
  PRIMARY KEY (entity_type, identity_key)
);
CREATE INDEX ix_identity_aliases_identity ON identity_aliases(identity_id);
