package ledger

// Groups: the association primitive (SPEC §3 layer 3, ADR-021). A group is a
// named association between identities and a context; everything here is
// events — membership is derived by the group_members view (last added/
// removed wins), and `touched` events are the delivery-history trail
// suppression windows read (SPEC §8). Groups carry no type and no logic.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/elegant-atomics/gtme/internal/ulid"
)

// Group is one named association context.
type Group struct {
	ID        string
	Name      string
	Note      string
	CreatedAt time.Time
	// EntityType is the members' type (SPEC §3, ADR-054), set once at
	// creation; "" for a group created before ADR-054 (entity-blind until
	// `gtme groups add NAME --type TYPE` sets it).
	EntityType string
}

// GroupInfo is a group with its derived character: tallies (ADR-021). The
// entity type is the one stored fact beyond the name (ADR-054).
type GroupInfo struct {
	Group
	Members int
	Added   int
	Removed int
	Touched int
}

// GroupEvent is one row of a group's trail.
type GroupEvent struct {
	ID          string
	GroupID     string
	IdentityID  string
	IdentityKey string
	Event       string
	Detail      string
	RunID       string
	CreatedAt   time.Time
}

// Group event kinds (SPEC §3): exactly three.
const (
	GroupAdded   = "added"
	GroupRemoved = "removed"
	GroupTouched = "touched"
)

// GetGroup finds a group by name.
func (l *Ledger) GetGroup(ctx context.Context, name string) (Group, error) {
	var g Group
	var created string
	err := l.db.QueryRowContext(ctx,
		`SELECT id, name, COALESCE(note,''), created_at, COALESCE(entity_type,'') FROM groups WHERE name = ?`,
		strings.TrimSpace(name)).Scan(&g.ID, &g.Name, &g.Note, &created, &g.EntityType)
	if errors.Is(err, sql.ErrNoRows) {
		return Group{}, fmt.Errorf("group %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return Group{}, err
	}
	g.CreatedAt, _ = ParseTime(created)
	return g, nil
}

// EnsureGroup finds or creates a group (record: targets and the membership
// terminus create on demand, SPEC §7). entityType is the members' type a
// group created here takes (SPEC §3, ADR-054); an existing group keeps the
// type it has — a legacy untyped group stays untyped until --type sets it —
// and an existing group of another type is an error naming both.
func (l *Ledger) EnsureGroup(ctx context.Context, name, entityType string) (Group, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Group{}, fmt.Errorf("ledger: a group needs a name")
	}
	g, err := l.GetGroup(ctx, name)
	if err == nil {
		if g.EntityType != "" && entityType != "" && g.EntityType != entityType {
			return Group{}, fmt.Errorf("ledger: group %q holds %s records, not %s (SPEC §3, ADR-054)", name, g.EntityType, entityType)
		}
		return g, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Group{}, err
	}
	g = Group{ID: ulid.New(), Name: name, CreatedAt: l.now(), EntityType: entityType}
	var typeArg any
	if entityType != "" {
		typeArg = entityType
	}
	_, err = l.db.ExecContext(ctx,
		`INSERT INTO groups (id, name, created_at, entity_type) VALUES (?, ?, ?, ?)
		 ON CONFLICT(name) DO NOTHING`,
		g.ID, g.Name, l.stamp(g.CreatedAt), typeArg)
	if err != nil {
		return Group{}, err
	}
	// A concurrent insert may have won the conflict; read back the truth.
	return l.GetGroup(ctx, name)
}

// SetGroupType sets an untyped group's entity type once (SPEC §8, ADR-054:
// `gtme groups add NAME --type TYPE` on a group created before the type
// existed). A typed group's type is never changed.
func (l *Ledger) SetGroupType(ctx context.Context, groupID, entityType string) error {
	res, err := l.db.ExecContext(ctx,
		`UPDATE groups SET entity_type = ? WHERE id = ? AND entity_type IS NULL`, entityType, groupID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("ledger: group's type is already set")
	}
	return nil
}

// AddGroupEvent appends one event to a group's trail. Membership edits and
// touches alike — everything is append-only.
func (l *Ledger) AddGroupEvent(ctx context.Context, groupID, identityID, event string, detail map[string]any, runID string) error {
	switch event {
	case GroupAdded, GroupRemoved, GroupTouched:
	default:
		return fmt.Errorf("ledger: unknown group event %q", event)
	}
	// Homogeneity (SPEC §3, ADR-054): a typed group admits members of its
	// type only, refused naming both types.
	if event == GroupAdded {
		var groupType, identityType sql.NullString
		err := l.db.QueryRowContext(ctx,
			`SELECT g.entity_type, i.entity_type FROM groups g, identities i WHERE g.id = ? AND i.id = ?`,
			groupID, identityID).Scan(&groupType, &identityType)
		if err != nil {
			return fmt.Errorf("ledger: adding to group: %w", err)
		}
		if groupType.Valid && groupType.String != "" && groupType.String != identityType.String {
			return fmt.Errorf("ledger: the group holds %s records; a %s cannot join it (SPEC §8, ADR-054)", groupType.String, identityType.String)
		}
	}
	var detailJSON any
	if len(detail) > 0 {
		raw, err := json.Marshal(detail)
		if err != nil {
			return err
		}
		detailJSON = string(raw)
	}
	var run any
	if runID != "" {
		run = runID
	}
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO group_events (id, group_id, identity_id, event, detail, run_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ulid.New(), groupID, identityID, event, detailJSON, run, l.stamp(l.now()))
	return err
}

// GroupMembership is the current membership as a set of identity ids.
func (l *Ledger) GroupMembership(ctx context.Context, groupID string) (map[string]bool, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT identity_id FROM group_members WHERE group_id = ?`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// GroupMembers lists current members joined to their identities, ordered by
// identity key for stable output.
func (l *Ledger) GroupMembers(ctx context.Context, groupID string) ([]Identity, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT i.id, i.entity_type, i.identity_key, i.created_at
		 FROM group_members m JOIN identities i ON i.id = m.identity_id
		 WHERE m.group_id = ? ORDER BY i.identity_key`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var ident Identity
		if err := rows.Scan(&ident.ID, &ident.EntityType, &ident.IdentityKey, &ident.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ident)
	}
	return out, rows.Err()
}

// GroupMembersOldest lists current members in insertion order — by the
// `added` event that made each one a member, oldest first — capped at limit
// when limit > 0 (SPEC §8/§9, ADR-032: what a group source serves).
func (l *Ledger) GroupMembersOldest(ctx context.Context, groupID string, limit int) ([]Identity, error) {
	if limit <= 0 {
		limit = -1 // SQLite: no limit
	}
	rows, err := l.db.QueryContext(ctx,
		`SELECT i.id, i.entity_type, i.identity_key, i.created_at
		 FROM group_members m
		 JOIN identities i ON i.id = m.identity_id
		 JOIN (SELECT group_id, identity_id, max(created_at) AS added_at, max(id) AS event_id
		       FROM group_events WHERE event = 'added' GROUP BY group_id, identity_id) e
		   ON e.group_id = m.group_id AND e.identity_id = m.identity_id
		 WHERE m.group_id = ?
		 ORDER BY e.added_at, e.event_id
		 LIMIT ?`, groupID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var ident Identity
		if err := rows.Scan(&ident.ID, &ident.EntityType, &ident.IdentityKey, &ident.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ident)
	}
	return out, rows.Err()
}

// LastTouched reports the newest `touched` event for an identity in a group —
// what a suppression window reads (SPEC §8).
func (l *Ledger) LastTouched(ctx context.Context, groupID, identityID string) (time.Time, bool, error) {
	var created string
	err := l.db.QueryRowContext(ctx,
		`SELECT created_at FROM group_events
		 WHERE group_id = ? AND identity_id = ? AND event = 'touched'
		 ORDER BY created_at DESC, id DESC LIMIT 1`,
		groupID, identityID).Scan(&created)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	t, err := ParseTime(created)
	if err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

// Groups lists every group with its type and derived character (SPEC §8).
func (l *Ledger) Groups(ctx context.Context) ([]GroupInfo, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT g.id, g.name, COALESCE(g.note,''), g.created_at, COALESCE(g.entity_type,''),
		       (SELECT count(*) FROM group_members m WHERE m.group_id = g.id),
		       (SELECT count(*) FROM group_events e WHERE e.group_id = g.id AND e.event = 'added'),
		       (SELECT count(*) FROM group_events e WHERE e.group_id = g.id AND e.event = 'removed'),
		       (SELECT count(*) FROM group_events e WHERE e.group_id = g.id AND e.event = 'touched')
		FROM groups g ORDER BY g.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupInfo
	for rows.Next() {
		var gi GroupInfo
		var created string
		if err := rows.Scan(&gi.ID, &gi.Name, &gi.Note, &created, &gi.EntityType,
			&gi.Members, &gi.Added, &gi.Removed, &gi.Touched); err != nil {
			return nil, err
		}
		gi.CreatedAt, _ = ParseTime(created)
		out = append(out, gi)
	}
	return out, rows.Err()
}

// GroupEvents lists a group's newest events (for `gtme groups show`).
func (l *Ledger) GroupEvents(ctx context.Context, groupID string, limit int) ([]GroupEvent, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := l.db.QueryContext(ctx, `
		SELECT e.id, e.group_id, e.identity_id, i.identity_key, e.event,
		       COALESCE(e.detail,''), COALESCE(e.run_id,''), e.created_at
		FROM group_events e JOIN identities i ON i.id = e.identity_id
		WHERE e.group_id = ?
		ORDER BY e.created_at DESC, e.id DESC LIMIT ?`, groupID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupEvent
	for rows.Next() {
		var ev GroupEvent
		var created string
		if err := rows.Scan(&ev.ID, &ev.GroupID, &ev.IdentityID, &ev.IdentityKey,
			&ev.Event, &ev.Detail, &ev.RunID, &created); err != nil {
			return nil, err
		}
		ev.CreatedAt, _ = ParseTime(created)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// IdentityIDsFromSQL runs a read-only SELECT and returns its identity_id
// column — the contract `gtme groups add --query/--from-segment` requires
// (SPEC §8): segments-as-SQL naturally join identities.
func (l *Ledger) IdentityIDsFromSQL(ctx context.Context, query string) ([]string, error) {
	if err := ReadOnlyStatement(query); err != nil {
		return nil, err
	}
	ro, err := OpenReadOnly(ctx, l.path)
	if err != nil {
		return nil, err
	}
	defer ro.Close()
	rows, err := ro.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	idCol := -1
	for i, c := range cols {
		if strings.EqualFold(c, "identity_id") {
			idCol = i
			break
		}
	}
	if idCol < 0 {
		return nil, fmt.Errorf("ledger: the query must yield an identity_id column (got: %s)", strings.Join(cols, ", "))
	}
	var out []string
	scan := make([]any, len(cols))
	for i := range scan {
		var sink any
		scan[i] = &sink
	}
	var id sql.NullString
	scan[idCol] = &id
	for rows.Next() {
		if err := rows.Scan(scan...); err != nil {
			return nil, err
		}
		if id.Valid && id.String != "" {
			out = append(out, id.String)
		}
	}
	return out, rows.Err()
}

// GroupProducers lists the pipelines whose runs wrote to a group — added or
// touched events carrying a run_id, joined to runs — and GroupConsumers the
// pipelines whose runs sourced from it (the run snapshot's source.group).
// Derived, never stored (SPEC §8, ADR-054): the chain a group sits in.
func (l *Ledger) GroupProducers(ctx context.Context, groupID string) ([]string, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT DISTINCT r.pipeline FROM group_events e JOIN runs r ON r.id = e.run_id
		 WHERE e.group_id = ? ORDER BY r.pipeline`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStrings(rows)
}

// GroupConsumers lists the pipelines that sourced from a group by name.
func (l *Ledger) GroupConsumers(ctx context.Context, name string) ([]string, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT DISTINCT pipeline FROM runs
		 WHERE json_extract(config_json, '$.source.group') = ? ORDER BY pipeline`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStrings(rows)
}

func scanStrings(rows *sql.Rows) ([]string, error) {
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
