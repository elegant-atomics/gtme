package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/elegant-atomics/gtme/internal/identity"
	"github.com/elegant-atomics/gtme/internal/ulid"
)

// Identity is a row of the identities table.
type Identity struct {
	ID          string
	EntityType  string
	IdentityKey string
	CreatedAt   string
}

// Provenance labels a write. RunID and StepID are empty for imports; the
// events they produce are recorded against the sentinels below so the NOT NULL
// columns stay honest.
type Provenance struct {
	RunID  string
	StepID string
}

const (
	noRun  = "(none)"
	noStep = "(import)"
)

func (p Provenance) runID() string {
	if p.RunID == "" {
		return noRun
	}
	return p.RunID
}

func (p Provenance) stepID() string {
	if p.StepID == "" {
		return noStep
	}
	return p.StepID
}

// UpsertResult reports what UpsertIdentity did.
type UpsertResult struct {
	Identity Identity
	Created  bool
	Upgraded bool   // identity_key was replaced by a stronger key
	OldKey   string // previous key when Upgraded
}

// UpsertIdentity resolves a record's fields to an identity, creating it when
// unseen. Candidate keys are tried strongest-first (SPEC §4); if the record
// matches an existing identity under a weaker key than it now carries, the
// stored key is upgraded in place and an 'identity_upgraded' step event is
// written — never a duplicate identity.
func (l *Ledger) UpsertIdentity(ctx context.Context, entityType string, fields map[string]any, prov Provenance) (UpsertResult, error) {
	cands, err := identity.Candidates(entityType, fields)
	if err != nil {
		return UpsertResult{}, err
	}
	if len(cands) == 0 {
		return UpsertResult{}, fmt.Errorf("ledger: no identity key derivable for %s record", entityType)
	}
	best := cands[0]

	var res UpsertResult
	err = l.tx(ctx, func(tx *sql.Tx) error {
		// Look up every candidate, strongest first; the record may already exist
		// under a weaker key, or under a key it has since been upgraded away from.
		var found *Identity
		var foundStrength identity.Strength
		for _, c := range cands {
			row, err := resolveKey(ctx, tx, c.EntityType, c.Value)
			if err != nil {
				return err
			}
			if row != nil {
				found, foundStrength = row, c.Strength
				break
			}
		}

		now := l.stamp(l.now())
		if found == nil {
			id := ulid.New()
			_, err := tx.ExecContext(ctx,
				`INSERT INTO identities (id, entity_type, identity_key, created_at) VALUES (?, ?, ?, ?)`,
				id, best.EntityType, best.Value, now)
			if err != nil {
				return fmt.Errorf("ledger: inserting identity: %w", err)
			}
			// Record the weaker keys this record also carries, so a later record
			// bearing only one of them resolves here.
			if err := addAliases(ctx, tx, id, now, cands[1:]); err != nil {
				return err
			}
			res = UpsertResult{
				Identity: Identity{ID: id, EntityType: best.EntityType, IdentityKey: best.Value, CreatedAt: now},
				Created:  true,
			}
			return nil
		}

		res.Identity = *found
		if foundStrength >= best.Strength || found.IdentityKey == best.Value {
			return nil
		}

		// Upgrade in place. If the stronger key is somehow already taken by
		// another identity, leave this one alone rather than merging — merges are
		// out of scope for v0 and silently losing rows would be worse.
		other, err := findIdentity(ctx, tx, best.EntityType, best.Value)
		if err != nil {
			return err
		}
		if other != nil {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE identities SET identity_key = ? WHERE id = ?`, best.Value, found.ID); err != nil {
			return fmt.Errorf("ledger: upgrading identity key: %w", err)
		}
		// The vacated key, and any other key this record carries, keep resolving here.
		aliases := append([]identity.Key{{EntityType: found.EntityType, Value: found.IdentityKey}}, cands[1:]...)
		if err := addAliases(ctx, tx, found.ID, now, aliases); err != nil {
			return err
		}
		detail, _ := json.Marshal(map[string]string{"from": found.IdentityKey, "to": best.Value})
		if err := insertStepEvent(ctx, tx, l, prov, found.ID, "identity_upgraded", string(detail)); err != nil {
			return err
		}
		res.Upgraded, res.OldKey = true, found.IdentityKey
		res.Identity.IdentityKey = best.Value
		return nil
	})
	if err != nil {
		return UpsertResult{}, err
	}
	return res, nil
}

// EnsureIdentity resolves an identity by an already-canonical key, creating it
// if unseen. Used when an adapter knows who a record is but its fields do not
// say so.
func (l *Ledger) EnsureIdentity(ctx context.Context, entityType, key string, prov Provenance) (Identity, error) {
	if entityType == "" || key == "" {
		return Identity{}, fmt.Errorf("ledger: entity_type and identity_key are required")
	}
	var out Identity
	err := l.tx(ctx, func(tx *sql.Tx) error {
		found, err := resolveKey(ctx, tx, entityType, key)
		if err != nil {
			return err
		}
		if found != nil {
			out = *found
			return nil
		}
		now := l.stamp(l.now())
		id := ulid.New()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO identities (id, entity_type, identity_key, created_at) VALUES (?, ?, ?, ?)`,
			id, entityType, key, now); err != nil {
			return fmt.Errorf("ledger: inserting identity: %w", err)
		}
		out = Identity{ID: id, EntityType: entityType, IdentityKey: key, CreatedAt: now}
		return nil
	})
	if err != nil {
		return Identity{}, err
	}
	return out, nil
}

// IdentityByKey looks up one identity by its canonical key, including keys it
// has been upgraded away from.
func (l *Ledger) IdentityByKey(ctx context.Context, entityType, key string) (Identity, error) {
	row, err := resolveKey(ctx, l.db, entityType, key)
	if err != nil {
		return Identity{}, err
	}
	if row == nil {
		return Identity{}, ErrNotFound
	}
	return *row, nil
}

// FindByKey looks up one identity by its key alone, across entity types —
// `gtme show <identity-key>` (SPEC §8, ADR-006) takes no entity_type, since the
// operator is naming a record, not describing its shape. Person and company
// keys never collide in practice (different formats: email/LinkedIn-slug/
// name-hash vs. domain/name-hash), but the schema's UNIQUE constraint is
// per-entity-type, so an ambiguous match is reported rather than guessed at.
func (l *Ledger) FindByKey(ctx context.Context, key string) (Identity, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT id, entity_type, identity_key, created_at FROM identities WHERE identity_key = ?`, key)
	if err != nil {
		return Identity{}, fmt.Errorf("ledger: looking up %q: %w", key, err)
	}
	var matches []Identity
	for rows.Next() {
		var out Identity
		if err := rows.Scan(&out.ID, &out.EntityType, &out.IdentityKey, &out.CreatedAt); err != nil {
			rows.Close()
			return Identity{}, fmt.Errorf("ledger: looking up %q: %w", key, err)
		}
		matches = append(matches, out)
	}
	if err := rows.Err(); err != nil {
		return Identity{}, fmt.Errorf("ledger: looking up %q: %w", key, err)
	}
	rows.Close()

	if len(matches) == 0 {
		// The live key missed; it may be one this identity was upgraded away
		// from (SPEC §4). identity_aliases carries an entity_type, so try every
		// type it was aliased under.
		aliasRows, err := l.db.QueryContext(ctx,
			`SELECT DISTINCT identity_id FROM identity_aliases WHERE identity_key = ?`, key)
		if err != nil {
			return Identity{}, fmt.Errorf("ledger: looking up %q: %w", key, err)
		}
		var ids []string
		for aliasRows.Next() {
			var id string
			if err := aliasRows.Scan(&id); err != nil {
				aliasRows.Close()
				return Identity{}, fmt.Errorf("ledger: looking up %q: %w", key, err)
			}
			ids = append(ids, id)
		}
		aliasRows.Close()
		if err := aliasRows.Err(); err != nil {
			return Identity{}, fmt.Errorf("ledger: looking up %q: %w", key, err)
		}
		for _, id := range ids {
			ident, err := l.IdentityByID(ctx, id)
			if err != nil && err != ErrNotFound {
				return Identity{}, err
			}
			if err == nil {
				matches = append(matches, ident)
			}
		}
	}

	switch len(matches) {
	case 0:
		return Identity{}, ErrNotFound
	case 1:
		return matches[0], nil
	default:
		types := make([]string, 0, len(matches))
		for _, m := range matches {
			types = append(types, m.EntityType)
		}
		return Identity{}, fmt.Errorf("ledger: %q matches more than one identity (%s) — this should not happen for v0's key formats", key, strings.Join(types, ", "))
	}
}

// IdentityByID looks up one identity by ULID.
func (l *Ledger) IdentityByID(ctx context.Context, id string) (Identity, error) {
	var out Identity
	err := l.db.QueryRowContext(ctx,
		`SELECT id, entity_type, identity_key, created_at FROM identities WHERE id = ?`, id).
		Scan(&out.ID, &out.EntityType, &out.IdentityKey, &out.CreatedAt)
	if err == sql.ErrNoRows {
		return Identity{}, ErrNotFound
	}
	if err != nil {
		return Identity{}, fmt.Errorf("ledger: reading identity: %w", err)
	}
	return out, nil
}

// Relate records a relation between two identities (idempotent).
func (l *Ledger) Relate(ctx context.Context, fromID, relation, toID string) error {
	_, err := l.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO relations (from_id, relation, to_id, created_at) VALUES (?, ?, ?, ?)`,
		fromID, relation, toID, l.stamp(l.now()))
	if err != nil {
		return fmt.Errorf("ledger: inserting relation: %w", err)
	}
	return nil
}

// querier is satisfied by both *sql.DB and *sql.Tx.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// resolveKey finds the identity a key points at, preferring a live
// identity_key over an alias left behind by an upgrade.
func resolveKey(ctx context.Context, q querier, entityType, key string) (*Identity, error) {
	if row, err := findIdentity(ctx, q, entityType, key); err != nil || row != nil {
		return row, err
	}
	var id string
	err := q.QueryRowContext(ctx,
		`SELECT identity_id FROM identity_aliases WHERE entity_type = ? AND identity_key = ?`,
		entityType, key).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: looking up identity alias: %w", err)
	}
	var out Identity
	err = q.QueryRowContext(ctx,
		`SELECT id, entity_type, identity_key, created_at FROM identities WHERE id = ?`, id).
		Scan(&out.ID, &out.EntityType, &out.IdentityKey, &out.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil // dangling alias; treat as a miss
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: looking up identity alias: %w", err)
	}
	return &out, nil
}

// addAliases records keys as aliases of identityID. Existing rows win: an alias
// never re-points at a different identity, so v0 never merges identities.
func addAliases(ctx context.Context, tx *sql.Tx, identityID, now string, keys []identity.Key) error {
	for _, k := range keys {
		if k.Value == "" {
			continue
		}
		_, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO identity_aliases (entity_type, identity_key, identity_id, created_at)
			 VALUES (?, ?, ?, ?)`, k.EntityType, k.Value, identityID, now)
		if err != nil {
			return fmt.Errorf("ledger: inserting identity alias: %w", err)
		}
	}
	return nil
}

func findIdentity(ctx context.Context, q querier, entityType, key string) (*Identity, error) {
	var out Identity
	err := q.QueryRowContext(ctx,
		`SELECT id, entity_type, identity_key, created_at FROM identities WHERE entity_type = ? AND identity_key = ?`,
		entityType, key).Scan(&out.ID, &out.EntityType, &out.IdentityKey, &out.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: looking up identity: %w", err)
	}
	return &out, nil
}

func insertStepEvent(ctx context.Context, tx *sql.Tx, l *Ledger, prov Provenance, identityID, event, detail string) error {
	var idArg any
	if identityID != "" {
		idArg = identityID
	}
	var detailArg any
	if detail != "" {
		detailArg = detail
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO step_events (id, run_id, step_id, identity_id, event, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ulid.New(), prov.runID(), prov.stepID(), idArg, event, detailArg, l.stamp(l.now()))
	if err != nil {
		return fmt.Errorf("ledger: inserting step event: %w", err)
	}
	return nil
}
