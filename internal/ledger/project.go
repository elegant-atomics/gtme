package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Value is one resolved field value with its provenance.
type Value struct {
	// ID is the field_values row that won — what a review or edit of this
	// value records as its referent (ADR-048).
	ID         string
	Raw        json.RawMessage
	Source     string
	Confidence float64
	RunID      string
	// Referent is the field_values.id this value was about, when the step
	// that wrote it declared of: (SPEC §3); "" otherwise.
	Referent  string
	CreatedAt time.Time
}

// Decode unmarshals the stored JSON value into v.
func (v Value) Decode(out any) error { return json.Unmarshal(v.Raw, out) }

// Any decodes the stored JSON value into an any.
func (v Value) Any() any {
	var out any
	if err := json.Unmarshal(v.Raw, &out); err != nil {
		return string(v.Raw)
	}
	return out
}

// Record is an identity plus its currently-resolved field values.
type Record struct {
	Identity Identity
	Values   map[string]Value
}

// Fields returns the decoded values, ready to hand to an adapter.
func (r Record) Fields() map[string]any {
	out := make(map[string]any, len(r.Values))
	for k, v := range r.Values {
		out[k] = v.Any()
	}
	return out
}

// Has reports whether every named field resolved to a value.
func (r Record) Has(fields ...string) bool {
	for _, f := range fields {
		if _, ok := r.Values[f]; !ok {
			return false
		}
	}
	return true
}

// Projection describes which fields to resolve and how fresh they must be.
type Projection struct {
	// Fields restricts the projection to these field names. Nil or empty means
	// every field the identity has.
	Fields []string
	// Freshness bounds how old a value may be, per field. A zero duration means
	// unbounded.
	Freshness map[string]time.Duration
	// DefaultFreshness applies to fields with no Freshness entry. Zero means
	// unbounded.
	DefaultFreshness time.Duration
	// AsOf is the clock for freshness windows; zero means the ledger's now.
	AsOf time.Time
}

func (p Projection) window(field string) time.Duration {
	if d, ok := p.Freshness[field]; ok {
		return d
	}
	return p.DefaultFreshness
}

// Project resolves the current value of each requested field for one identity.
//
// Resolution rule (SPEC §3, DECISIONS.md ADR-003): the current value of a
// field is the row with the highest confidence *among rows within the
// field's freshness window*, ties broken by newest created_at. The window is
// per-step (SPEC §7's `cache:`), so it cannot live in a view with no notion
// of "now" — but the ranking itself (confidence DESC, created_at DESC) is
// expressed exactly once, as the field_value_ranks SQL view (migration
// 0004). This method reads every ranked row for a field, in order, and takes
// the first one inside the window — falling through a stale top-ranked row
// to the next-best one, exactly as the rule requires. `gtme query`'s
// current_fields view is the same ranking with no window applied (rank = 1);
// the two can never disagree about *what outranks what*, only about whether a
// window trims the answer.
func (l *Ledger) Project(ctx context.Context, identityID string, p Projection) (Record, error) {
	ident, err := l.IdentityByID(ctx, identityID)
	if err != nil {
		return Record{}, err
	}

	q := strings.Builder{}
	// The ranking view carries no referent (it is SPEC §3's view verbatim);
	// the row's own referent is read back through its id.
	q.WriteString(`SELECT r.id, r.field, r.value, r.source, r.confidence, COALESCE(r.run_id, ''), COALESCE(fv.referent, ''), r.created_at
	               FROM field_value_ranks r JOIN field_values fv ON fv.id = r.id WHERE r.identity_id = ?`)
	args := []any{identityID}
	if len(p.Fields) > 0 {
		q.WriteString(" AND r.field IN (" + placeholders(len(p.Fields)) + ")")
		for _, f := range p.Fields {
			args = append(args, f)
		}
	}
	q.WriteString(" ORDER BY r.field ASC, r.rank ASC")

	rows, err := l.db.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return Record{}, fmt.Errorf("ledger: projecting record: %w", err)
	}
	defer rows.Close()

	asOf := p.AsOf
	if asOf.IsZero() {
		asOf = l.now()
	}

	out := Record{Identity: ident, Values: map[string]Value{}}
	for rows.Next() {
		var (
			id, field, raw, source, runID, referent, createdAt string
			confidence                                         float64
		)
		if err := rows.Scan(&id, &field, &raw, &source, &confidence, &runID, &referent, &createdAt); err != nil {
			return Record{}, fmt.Errorf("ledger: projecting record: %w", err)
		}
		if _, done := out.Values[field]; done {
			continue // a higher-ranked row already won this field
		}
		created, err := ParseTime(createdAt)
		if err != nil {
			return Record{}, err
		}
		if w := p.window(field); w > 0 && asOf.Sub(created) > w {
			continue // stale: outside this field's freshness window; try the next rank
		}
		out.Values[field] = Value{
			ID:         id,
			Raw:        json.RawMessage(raw),
			Source:     source,
			Confidence: confidence,
			RunID:      runID,
			Referent:   referent,
			CreatedAt:  created,
		}
	}
	if err := rows.Err(); err != nil {
		return Record{}, fmt.Errorf("ledger: projecting record: %w", err)
	}
	return out, nil
}

// LastWriteBySource reports when a given adapter last wrote anything about an
// identity. This is how the runner asks "have I already paid this provider for
// this record?", which is a sharper question than "are all its fields present"
// when a provides schema has optional properties.
func (l *Ledger) LastWriteBySource(ctx context.Context, identityID, source string) (time.Time, bool, error) {
	// max() over no rows is NULL, which is the "never written" case.
	var createdAt sql.NullString
	err := l.db.QueryRowContext(ctx,
		`SELECT max(created_at) FROM field_values WHERE identity_id = ? AND source = ?`,
		identityID, source).Scan(&createdAt)
	if err != nil && err != sql.ErrNoRows {
		return time.Time{}, false, fmt.Errorf("ledger: reading last write: %w", err)
	}
	if !createdAt.Valid || createdAt.String == "" {
		return time.Time{}, false, nil
	}
	t, err := ParseTime(createdAt.String)
	if err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
