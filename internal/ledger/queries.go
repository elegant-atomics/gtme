package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
)

// SavedQuery is a named segment.
type SavedQuery struct {
	Name      string
	SQL       string
	CreatedAt string
	UpdatedAt string
}

// SaveQuery stores or replaces a named segment.
func (l *Ledger) SaveQuery(ctx context.Context, name, query string) error {
	now := l.stamp(l.now())
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO saved_queries (name, query_sql, created_at, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET query_sql = excluded.query_sql, updated_at = excluded.updated_at`,
		name, query, now, now)
	if err != nil {
		return fmt.Errorf("ledger: saving query %q: %w", name, err)
	}
	return nil
}

// SavedQuery reads one segment.
func (l *Ledger) SavedQuery(ctx context.Context, name string) (SavedQuery, error) {
	var q SavedQuery
	err := l.db.QueryRowContext(ctx,
		`SELECT name, query_sql, created_at, updated_at FROM saved_queries WHERE name = ?`, name).
		Scan(&q.Name, &q.SQL, &q.CreatedAt, &q.UpdatedAt)
	if err == sql.ErrNoRows {
		return SavedQuery{}, ErrNotFound
	}
	if err != nil {
		return SavedQuery{}, fmt.Errorf("ledger: reading query %q: %w", name, err)
	}
	return q, nil
}

// SavedQueries lists segments by name.
func (l *Ledger) SavedQueries(ctx context.Context) ([]SavedQuery, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT name, query_sql, created_at, updated_at FROM saved_queries ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("ledger: listing queries: %w", err)
	}
	defer rows.Close()
	var out []SavedQuery
	for rows.Next() {
		var q SavedQuery
		if err := rows.Scan(&q.Name, &q.SQL, &q.CreatedAt, &q.UpdatedAt); err != nil {
			return nil, fmt.Errorf("ledger: listing queries: %w", err)
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: listing queries: %w", err)
	}
	return out, nil
}

// OpenReadOnly opens the ledger read-only, for `gtm query`. Two layers keep a
// query harmless: SQLite itself refuses writes on this handle, and the statement
// is checked before it runs (see ReadOnlyStatement).
func OpenReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("ledger: %s does not exist yet — run `gtm init`", path)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("ledger: opening %s read-only: %w", path, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger: opening %s read-only: %w", path, err)
	}
	return db, nil
}

// ReadOnlyStatement checks that a statement only reads. It rejects anything that
// is not a single SELECT/WITH/EXPLAIN — belt and braces alongside the read-only
// connection, and it produces a clearer error than SQLite's.
func ReadOnlyStatement(query string) error {
	trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(query), ";"))
	if trimmed == "" {
		return fmt.Errorf("ledger: empty query")
	}
	if strings.Contains(trimmed, ";") {
		return fmt.Errorf("ledger: one statement at a time (found a ';' mid-query)")
	}
	first := strings.ToLower(strings.Fields(trimmed)[0])
	switch first {
	case "select", "with", "explain":
		return nil
	default:
		return fmt.Errorf("ledger: `gtm query` is read-only; %q is not a SELECT", first)
	}
}
