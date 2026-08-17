package ledger

// Payloads: raw vendor responses as cache, not facts (SPEC §3, ADR-030).
// The one purgeable table in the ledger — facts stay append-only forever;
// payloads carry an expiry and are evicted opportunistically at run start
// and by `gtme vacuum` (SPEC §8). Never projected into any step.

import (
	"context"
	"time"

	"github.com/elegant-atomics/gtme/internal/ulid"
)

// WritePayload retains one raw response. ttlDays 0 means no expiry — kept
// until an operator vacuums explicitly (SPEC §3).
func (l *Ledger) WritePayload(ctx context.Context, identityID, adapter, runID, contentType, body string, ttlDays int) error {
	now := l.now()
	var expires any
	if ttlDays > 0 {
		expires = l.stamp(now.Add(time.Duration(ttlDays) * 24 * time.Hour))
	}
	var run any
	if runID != "" {
		run = runID
	}
	var ct any
	if contentType != "" {
		ct = contentType
	}
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO payloads (id, identity_id, adapter, run_id, content_type, body, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ulid.New(), identityID, adapter, run, ct, body, l.stamp(now), expires)
	return err
}

// PurgeExpiredPayloads evicts payloads whose expiry has passed — and nothing
// else (SPEC §8).
func (l *Ledger) PurgeExpiredPayloads(ctx context.Context) (int, error) {
	res, err := l.db.ExecContext(ctx,
		`DELETE FROM payloads WHERE expires_at IS NOT NULL AND expires_at <= ?`,
		l.stamp(l.now()))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// PayloadCount reports how many payloads an identity holds for an adapter
// ("" = any adapter) — the acceptance tests' window into the cache tier.
func (l *Ledger) PayloadCount(ctx context.Context, identityID, adapter string) (int, error) {
	query := `SELECT count(*) FROM payloads WHERE 1=1`
	var args []any
	if identityID != "" {
		query += ` AND identity_id = ?`
		args = append(args, identityID)
	}
	if adapter != "" {
		query += ` AND adapter = ?`
		args = append(args, adapter)
	}
	var n int
	err := l.db.QueryRowContext(ctx, query, args...).Scan(&n)
	return n, err
}
