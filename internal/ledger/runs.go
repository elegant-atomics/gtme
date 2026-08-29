package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/elegant-atomics/gtme/internal/ulid"
)

// Run statuses. Pending (ADR-038) is a run that ended with a step in
// flight: not done, collected by the next `gtme run` of its pipeline.
const (
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
	StatusPending = "pending"
)

// Step events the in-flight mechanism adds (SPEC §3, ADR-038).
const (
	EventPending   = "pending"
	EventCollected = "collected"
)

// StateSourced is a record's state before any step has touched it.
const StateSourced = "sourced"

// AdhocPipeline is a reserved pipeline name (SPEC §3's schema comment on
// runs.pipeline). gtme freeze refuses to keep it as a frozen pipeline's name
// (see frozenPipeline in internal/cli/freeze.go) — a safeguard against a
// pipeline named literally "(adhoc)", which reads as an accident, not intent.
const AdhocPipeline = "(adhoc)"

// Run is a row of the runs table.
type Run struct {
	ID         string
	Pipeline   string
	ConfigJSON string
	StartedAt  string
	FinishedAt string
	Status     string
}

// CreateRun opens a run with a snapshot of the resolved config.
func (l *Ledger) CreateRun(ctx context.Context, pipeline string, config any) (Run, error) {
	raw := []byte("{}")
	if config != nil {
		var err error
		if raw, err = json.Marshal(config); err != nil {
			return Run{}, fmt.Errorf("ledger: encoding run config: %w", err)
		}
	}
	run := Run{
		ID:         ulid.New(),
		Pipeline:   pipeline,
		ConfigJSON: string(raw),
		StartedAt:  l.stamp(l.now()),
		Status:     StatusRunning,
	}
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO runs (id, pipeline, config_json, started_at, status) VALUES (?, ?, ?, ?, ?)`,
		run.ID, run.Pipeline, run.ConfigJSON, run.StartedAt, run.Status)
	if err != nil {
		return Run{}, fmt.Errorf("ledger: inserting run: %w", err)
	}
	return run, nil
}

// GetRun reads one run.
func (l *Ledger) GetRun(ctx context.Context, id string) (Run, error) {
	var r Run
	var finished sql.NullString
	err := l.db.QueryRowContext(ctx,
		`SELECT id, pipeline, config_json, started_at, finished_at, status FROM runs WHERE id = ?`, id).
		Scan(&r.ID, &r.Pipeline, &r.ConfigJSON, &r.StartedAt, &finished, &r.Status)
	if err == sql.ErrNoRows {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("ledger: reading run: %w", err)
	}
	r.FinishedAt = finished.String
	return r, nil
}

// ListRuns returns runs newest first, at most limit (0 = all).
func (l *Ledger) ListRuns(ctx context.Context, limit int) ([]Run, error) {
	q := `SELECT id, pipeline, config_json, started_at, finished_at, status FROM runs ORDER BY id DESC`
	args := []any{}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ledger: listing runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var finished sql.NullString
		if err := rows.Scan(&r.ID, &r.Pipeline, &r.ConfigJSON, &r.StartedAt, &finished, &r.Status); err != nil {
			return nil, fmt.Errorf("ledger: listing runs: %w", err)
		}
		r.FinishedAt = finished.String
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: listing runs: %w", err)
	}
	return out, nil
}

// LastRun returns the most recent run.
func (l *Ledger) LastRun(ctx context.Context) (Run, error) {
	runs, err := l.ListRuns(ctx, 1)
	if err != nil {
		return Run{}, err
	}
	if len(runs) == 0 {
		return Run{}, ErrNotFound
	}
	return runs[0], nil
}

// FinishRun closes a run with a terminal status. A failure is sticky: once a
// run is marked failed, a later call reporting success must not overwrite it.
// LastRunForPipeline is the most recent run of a named pipeline.
func (l *Ledger) LastRunForPipeline(ctx context.Context, pipeline string) (Run, error) {
	var run Run
	var finished sql.NullString
	err := l.db.QueryRowContext(ctx,
		`SELECT id, pipeline, config_json, started_at, finished_at, status FROM runs
		 WHERE pipeline = ? ORDER BY started_at DESC, id DESC LIMIT 1`, pipeline).
		Scan(&run.ID, &run.Pipeline, &run.ConfigJSON, &run.StartedAt, &finished, &run.Status)
	if err == sql.ErrNoRows {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("ledger: reading runs: %w", err)
	}
	run.FinishedAt = finished.String
	return run, nil
}

// PendingTokens maps each record still in flight at a step to the token
// it is pending under (ADR-038): the newest `pending` event per record with
// no `collected` event after it.
func (l *Ledger) PendingTokens(ctx context.Context, runID, stepID string) (map[string]string, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT identity_id, event, detail FROM step_events
		 WHERE run_id = ? AND step_id = ? AND event IN (?, ?) AND identity_id IS NOT NULL
		 ORDER BY created_at, id`, runID, stepID, EventPending, EventCollected)
	if err != nil {
		return nil, fmt.Errorf("ledger: reading pending events: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, event string
		var detail sql.NullString
		if err := rows.Scan(&id, &event, &detail); err != nil {
			return nil, err
		}
		if event == EventCollected {
			delete(out, id)
			continue
		}
		var d struct {
			Token string `json:"token"`
		}
		_ = json.Unmarshal([]byte(detail.String), &d)
		if d.Token != "" {
			out[id] = d.Token
		}
	}
	return out, rows.Err()
}

// InFlight counts a run's records still pending at any step (ADR-038).
func (l *Ledger) InFlight(ctx context.Context, runID string) (int, error) {
	steps, err := l.StepIDs(ctx, runID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, step := range steps {
		tokens, err := l.PendingTokens(ctx, runID, step)
		if err != nil {
			return 0, err
		}
		n += len(tokens)
	}
	return n, nil
}

// Judgment is a cached AI answer (SPEC §7, ADR-039): the newest `done`
// event for an identity whose detail carries the same signature and input.
type Judgment struct {
	Pass   *bool
	Reason string
	RunID  string
}

// LastJudgment finds a reusable judgment for an identity, any run, newer
// than since (zero = unbounded).
func (l *Ledger) LastJudgment(ctx context.Context, identityID, signature, input string, since time.Time) (Judgment, bool, error) {
	q := `SELECT run_id, detail FROM step_events
	      WHERE identity_id = ? AND event = 'done'
	        AND json_extract(detail, '$.signature') = ? AND json_extract(detail, '$.input') = ?`
	args := []any{identityID, signature, input}
	if !since.IsZero() {
		q += ` AND created_at >= ?`
		args = append(args, l.stamp(since))
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT 1`
	var j Judgment
	var detail sql.NullString
	err := l.db.QueryRowContext(ctx, q, args...).Scan(&j.RunID, &detail)
	if err == sql.ErrNoRows {
		return Judgment{}, false, nil
	}
	if err != nil {
		return Judgment{}, false, fmt.Errorf("ledger: reading judgments: %w", err)
	}
	var d struct {
		Pass   *bool  `json:"pass"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal([]byte(detail.String), &d)
	j.Pass, j.Reason = d.Pass, d.Reason
	return j, true, nil
}

func (l *Ledger) FinishRun(ctx context.Context, runID, status string) error {
	q := `UPDATE runs SET status = ?, finished_at = ? WHERE id = ?`
	if status != StatusFailed {
		q += ` AND status = '` + StatusRunning + `'`
	}
	if _, err := l.db.ExecContext(ctx, q, status, l.stamp(l.now()), runID); err != nil {
		return fmt.Errorf("ledger: finishing run: %w", err)
	}
	return nil
}

// ReopenRun marks a run running again, for --resume.
func (l *Ledger) ReopenRun(ctx context.Context, runID string) error {
	if _, err := l.db.ExecContext(ctx,
		`UPDATE runs SET status = ?, finished_at = NULL WHERE id = ?`, StatusRunning, runID); err != nil {
		return fmt.Errorf("ledger: reopening run: %w", err)
	}
	return nil
}

// RunRecord is one identity's membership in a run.
type RunRecord struct {
	IdentityID string
	State      string
	Verdicts   map[string]string // step_id → pass|fail
}

// Passed reports whether a step's verdict for this record was a pass.
func (r RunRecord) Passed(stepID string) bool { return r.Verdicts[stepID] == "pass" }

// AnyFailed reports whether any step recorded a fail verdict for this record.
// A filter's fail freezes the record (SPEC §7); a deliver step's fail records
// a withheld send and the record advances (SPEC §8, ADR-031) — callers that
// need "is this record stopped" must therefore judge verdicts against step
// roles (the runner does, via its plan), not use this alone.
func (r RunRecord) AnyFailed() bool {
	for _, v := range r.Verdicts {
		if v == "fail" {
			return true
		}
	}
	return false
}

// AddRunRecord adds an identity to a run at the given state, leaving an existing
// row (and its progress) alone.
func (l *Ledger) AddRunRecord(ctx context.Context, runID, identityID, state string) error {
	_, err := l.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO run_records (run_id, identity_id, state, verdicts) VALUES (?, ?, ?, '{}')`,
		runID, identityID, state)
	if err != nil {
		return fmt.Errorf("ledger: inserting run record: %w", err)
	}
	return nil
}

// SetRunRecordState advances a record's state to the step it just completed.
func (l *Ledger) SetRunRecordState(ctx context.Context, runID, identityID, state string) error {
	_, err := l.db.ExecContext(ctx,
		`UPDATE run_records SET state = ? WHERE run_id = ? AND identity_id = ?`, state, runID, identityID)
	if err != nil {
		return fmt.Errorf("ledger: updating run record state: %w", err)
	}
	return nil
}

// SetVerdict records a filter verdict for a record.
func (l *Ledger) SetVerdict(ctx context.Context, runID, identityID, stepID string, pass bool) error {
	return l.tx(ctx, func(tx *sql.Tx) error {
		var raw string
		err := tx.QueryRowContext(ctx,
			`SELECT verdicts FROM run_records WHERE run_id = ? AND identity_id = ?`, runID, identityID).Scan(&raw)
		if err == sql.ErrNoRows {
			return fmt.Errorf("ledger: no run record for %s in run %s", identityID, runID)
		}
		if err != nil {
			return fmt.Errorf("ledger: reading verdicts: %w", err)
		}
		verdicts := map[string]string{}
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &verdicts); err != nil {
				return fmt.Errorf("ledger: decoding verdicts: %w", err)
			}
		}
		verdicts[stepID] = "fail"
		if pass {
			verdicts[stepID] = "pass"
		}
		out, err := json.Marshal(verdicts)
		if err != nil {
			return fmt.Errorf("ledger: encoding verdicts: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE run_records SET verdicts = ? WHERE run_id = ? AND identity_id = ?`,
			string(out), runID, identityID); err != nil {
			return fmt.Errorf("ledger: updating verdicts: %w", err)
		}
		return nil
	})
}

// RunRecords lists a run's records in insertion order.
func (l *Ledger) RunRecords(ctx context.Context, runID string) ([]RunRecord, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT identity_id, state, verdicts FROM run_records WHERE run_id = ? ORDER BY identity_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("ledger: listing run records: %w", err)
	}
	defer rows.Close()
	var out []RunRecord
	for rows.Next() {
		var rr RunRecord
		var raw string
		if err := rows.Scan(&rr.IdentityID, &rr.State, &raw); err != nil {
			return nil, fmt.Errorf("ledger: listing run records: %w", err)
		}
		rr.Verdicts = map[string]string{}
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &rr.Verdicts); err != nil {
				return nil, fmt.Errorf("ledger: decoding verdicts: %w", err)
			}
		}
		out = append(out, rr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: listing run records: %w", err)
	}
	return out, nil
}

// RecordCost appends a cost row. identityID may be empty for step-level costs.
func (l *Ledger) RecordCost(ctx context.Context, runID, stepID, identityID, provider string, amountUSD float64, detail map[string]any) error {
	var idArg any
	if identityID != "" {
		idArg = identityID
	}
	var detailArg any
	if len(detail) > 0 {
		raw, err := json.Marshal(detail)
		if err != nil {
			return fmt.Errorf("ledger: encoding cost detail: %w", err)
		}
		detailArg = string(raw)
	}
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO costs (id, run_id, step_id, identity_id, provider, amount_usd, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ulid.New(), runID, stepID, idArg, provider, amountUSD, detailArg, l.stamp(l.now()))
	if err != nil {
		return fmt.Errorf("ledger: inserting cost: %w", err)
	}
	return nil
}

// CostsByStep totals a run's spend per step.
func (l *Ledger) CostsByStep(ctx context.Context, runID string) (map[string]float64, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT step_id, sum(amount_usd) FROM costs WHERE run_id = ? GROUP BY step_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("ledger: totalling costs: %w", err)
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var step string
		var total float64
		if err := rows.Scan(&step, &total); err != nil {
			return nil, fmt.Errorf("ledger: totalling costs: %w", err)
		}
		out[step] = total
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: totalling costs: %w", err)
	}
	return out, nil
}

// StepEventCounts counts a run's events per (step, event) pair.
func (l *Ledger) StepEventCounts(ctx context.Context, runID string) (map[string]map[string]int, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT step_id, event, count(*) FROM step_events WHERE run_id = ? GROUP BY step_id, event`, runID)
	if err != nil {
		return nil, fmt.Errorf("ledger: counting step events: %w", err)
	}
	defer rows.Close()
	out := map[string]map[string]int{}
	for rows.Next() {
		var step, event string
		var n int
		if err := rows.Scan(&step, &event, &n); err != nil {
			return nil, fmt.Errorf("ledger: counting step events: %w", err)
		}
		if out[step] == nil {
			out[step] = map[string]int{}
		}
		out[step][event] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: counting step events: %w", err)
	}
	return out, nil
}

// StepEventSeen reports whether a step-level event was already recorded for this
// run — how --resume knows a source has already been drained.
func (l *Ledger) StepEventSeen(ctx context.Context, runID, stepID, event string) (bool, error) {
	var n int
	err := l.db.QueryRowContext(ctx,
		`SELECT count(*) FROM step_events WHERE run_id = ? AND step_id = ? AND event = ? AND identity_id IS NULL`,
		runID, stepID, event).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("ledger: reading step events: %w", err)
	}
	return n > 0, nil
}

// StepIDs lists the step ids that appear in a run's events, in first-seen
// order — the order steps actually ran in, which gtme freeze and gtme runs
// both use to present a run's steps.
//
// Ordering is by the smallest event ULID rather than by timestamp: two steps can
// easily record their first event in the same millisecond, and ULIDs still sort
// in creation order when that happens.
func (l *Ledger) StepIDs(ctx context.Context, runID string) ([]string, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT step_id, min(id) AS first_seen FROM step_events WHERE run_id = ?
		 GROUP BY step_id ORDER BY first_seen`, runID)
	if err != nil {
		return nil, fmt.Errorf("ledger: listing step ids: %w", err)
	}
	defer rows.Close()
	type seen struct {
		step  string
		first string
	}
	var all []seen
	for rows.Next() {
		var s seen
		if err := rows.Scan(&s.step, &s.first); err != nil {
			return nil, fmt.Errorf("ledger: listing step ids: %w", err)
		}
		all = append(all, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: listing step ids: %w", err)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].first < all[j].first })
	out := make([]string, 0, len(all))
	for _, s := range all {
		out = append(out, s.step)
	}
	return out, nil
}

// AlreadyDelivered reports whether this (target, idempotency) pair was delivered
// before, in this run or any earlier one (SPEC §8).
func (l *Ledger) AlreadyDelivered(ctx context.Context, target, idempotency string) (bool, error) {
	var n int
	err := l.db.QueryRowContext(ctx,
		`SELECT count(*) FROM deliveries WHERE target = ? AND idempotency = ?`, target, idempotency).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("ledger: reading deliveries: %w", err)
	}
	return n > 0, nil
}

// RecordDelivery marks a record delivered. A duplicate key is not an error: it
// means another worker or an earlier run got there first.
func (l *Ledger) RecordDelivery(ctx context.Context, identityID, target, idempotency, runID string) error {
	_, err := l.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO deliveries (id, identity_id, target, idempotency, run_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		ulid.New(), identityID, target, idempotency, runID, l.stamp(l.now()))
	if err != nil {
		return fmt.Errorf("ledger: inserting delivery: %w", err)
	}
	return nil
}

// Delivery statuses (SPEC §8, ADR-036). A 2xx is never a delivery: a row is
// born accepted; attestation refines it to confirmed or contradicted; sent is
// written only by a provider attesting execution (the listen verb, ROADMAP).
const (
	DeliveryAccepted     = "accepted"
	DeliveryConfirmed    = "confirmed"
	DeliveryContradicted = "contradicted"
	DeliverySent         = "sent"
)

// Delivery is one deliveries row, as gtme show reports it.
type Delivery struct {
	Target      string
	Idempotency string
	Status      string
	SentAt      string
	RunID       string
	CreatedAt   string
}

// SetDeliveryStatus refines a delivery's status after attestation (ADR-036).
// Promotion to sent is not done here: it must be compare-and-swap on the
// observed (status, sent_at) pair, which is the listen verb's job.
func (l *Ledger) SetDeliveryStatus(ctx context.Context, target, idempotency, status string) error {
	switch status {
	case DeliveryAccepted, DeliveryConfirmed, DeliveryContradicted:
	default:
		return fmt.Errorf("ledger: %q is not a status attestation may set", status)
	}
	_, err := l.db.ExecContext(ctx,
		`UPDATE deliveries SET status = ? WHERE target = ? AND idempotency = ?`, status, target, idempotency)
	if err != nil {
		return fmt.Errorf("ledger: updating delivery status: %w", err)
	}
	return nil
}

// Deliveries lists an identity's deliveries, oldest first.
func (l *Ledger) Deliveries(ctx context.Context, identityID string) ([]Delivery, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT target, idempotency, status, coalesce(sent_at, ''), run_id, created_at
		 FROM deliveries WHERE identity_id = ? ORDER BY created_at, id`, identityID)
	if err != nil {
		return nil, fmt.Errorf("ledger: reading deliveries: %w", err)
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.Target, &d.Idempotency, &d.Status, &d.SentAt, &d.RunID, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
