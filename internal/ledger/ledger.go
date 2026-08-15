// Package ledger owns the SQLite ledger: the durable identity cache (layer 1)
// and per-run working state (layer 2). See SPEC §3.
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// TimeFormat is how every timestamp is stored: RFC3339 with milliseconds, in
// UTC, so string ordering equals time ordering.
const TimeFormat = "2006-01-02T15:04:05.000Z07:00"

// Ledger is a handle on the ledger database.
type Ledger struct {
	db   *sql.DB
	path string
	now  func() time.Time // swappable in tests
}

// DefaultPath returns the ledger path: $GTM_LEDGER if set, else
// ~/.gtm/ledger.db.
func DefaultPath() (string, error) {
	if p := os.Getenv("GTM_LEDGER"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("ledger: locating home directory: %w", err)
	}
	return filepath.Join(home, ".gtm", "ledger.db"), nil
}

// Home returns the ~/.gtm directory (or the directory holding $GTM_LEDGER).
func Home() (string, error) {
	if p := os.Getenv("GTM_LEDGER"); p != "" {
		return filepath.Dir(p), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("ledger: locating home directory: %w", err)
	}
	return filepath.Join(home, ".gtm"), nil
}

// Open opens (creating if needed) the ledger at path and applies any pending
// migrations. Passing an empty path uses DefaultPath.
func Open(ctx context.Context, path string) (*Ledger, error) {
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("ledger: creating %s: %w", dir, err)
		}
	}

	// _txlock=immediate takes the write lock at BEGIN rather than on the first
	// write. Without it a read-modify-write transaction can be invalidated by
	// another process writing in between (SQLITE_BUSY_SNAPSHOT), which busy_timeout
	// cannot wait out — and a `gtm run` can legitimately overlap another `gtm
	// run`, `gtm query`, or `gtm show` against the same ledger file.
	dsn := "file:" + path +
		"?_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("ledger: opening %s: %w", path, err)
	}
	// A single writer avoids SQLITE_BUSY between pooled connections; readers are
	// cheap enough that one connection is fine at v0 concurrency.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger: opening %s: %w", path, err)
	}

	l := &Ledger{db: db, path: path, now: time.Now}
	if err := l.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return l, nil
}

// Close releases the database handle.
func (l *Ledger) Close() error { return l.db.Close() }

// Path is the file backing this ledger.
func (l *Ledger) Path() string { return l.path }

// DB exposes the underlying handle for read-only queries (gtm query) and for
// packages that need a transaction. Writes should go through Ledger methods so
// timestamps and IDs stay consistent.
func (l *Ledger) DB() *sql.DB { return l.db }

// SetNow overrides the clock. Tests only.
func (l *Ledger) SetNow(f func() time.Time) { l.now = f }

func (l *Ledger) stamp(t time.Time) string { return t.UTC().Format(TimeFormat) }

// ParseTime reads a timestamp written by the ledger.
func ParseTime(s string) (time.Time, error) {
	if t, err := time.Parse(TimeFormat, s); err == nil {
		return t, nil
	}
	// Tolerate hand-written or imported rows in plain RFC3339.
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("ledger: unparseable timestamp %q", s)
	}
	return t, nil
}

// ErrNotFound is returned when a lookup finds no row.
var ErrNotFound = errors.New("ledger: not found")

func (l *Ledger) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	// Retry a genuinely contended transaction a few times: several gtm processes
	// can legitimately share one ledger file concurrently, and losing a lock
	// race is not an error worth showing an operator.
	const attempts = 5
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		err = l.txOnce(ctx, fn)
		if err == nil || !isLocked(err) || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 20 * time.Millisecond):
		}
	}
	return err
}

func (l *Ledger) txOnce(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ledger: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ledger: commit: %w", err)
	}
	return nil
}

// isLocked reports whether an error is SQLite telling us to wait our turn.
func isLocked(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "database table is locked")
}
