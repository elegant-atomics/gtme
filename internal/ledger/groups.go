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

	"github.com/trevorfox/gtm/internal/ulid"
)

// Group is one named association context.
type Group struct {
	ID        string
	Name      string
	Note      string
	CreatedAt time.Time
}

// GroupInfo is a group with its derived character: tallies, never a stored
// type (ADR-021).
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
		`SELECT id, name, COALESCE(note,''), created_at FROM groups WHERE name = ?`,
		strings.TrimSpace(name)).Scan(&g.ID, &g.Name, &g.Note, &created)
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
// terminus create on demand, SPEC §7).
func (l *Ledger) EnsureGroup(ctx context.Context, name string) (Group, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Group{}, fmt.Errorf("ledger: a group needs a name")
	}
	g, err := l.GetGroup(ctx, name)
	if err == nil {
		return g, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Group{}, err
	}
	g = Group{ID: ulid.New(), Name: name, CreatedAt: l.now()}
	_, err = l.db.ExecContext(ctx,
		`INSERT INTO groups (id, name, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(name) DO NOTHING`,
		g.ID, g.Name, l.stamp(g.CreatedAt))
	if err != nil {
		return Group{}, err
	}
	// A concurrent insert may have won the conflict; read back the truth.
	return l.GetGroup(ctx, name)
}

// AddGroupEvent appends one event to a group's trail. Membership edits and
// touches alike — everything is append-only.
func (l *Ledger) AddGroupEvent(ctx context.Context, groupID, identityID, event string, detail map[string]any, runID string) error {
	switch event {
	case GroupAdded, GroupRemoved, GroupTouched:
	default:
		return fmt.Errorf("ledger: unknown group event %q", event)
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

// Groups lists every group with its derived character (SPEC §8: counts,
// never a stored type).
func (l *Ledger) Groups(ctx context.Context) ([]GroupInfo, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT g.id, g.name, COALESCE(g.note,''), g.created_at,
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
		if err := rows.Scan(&gi.ID, &gi.Name, &gi.Note, &created,
			&gi.Members, &gi.Added, &gi.Removed, &gi.Touched); err != nil {
			return nil, err
		}
		gi.CreatedAt, _ = ParseTime(created)
		out = append(out, gi)
	}
	return out, rows.Err()
}

// GroupEvents lists a group's newest events (for `gtm groups show`).
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
// column — the contract `gtm groups add --query/--from-segment` requires
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
