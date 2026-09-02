package ledger

// Answers (SPEC §3/§8, ADR-049): a participant's answer for a record pending
// under a human/* or agent/* step is an `answered` step event — ledger
// state, idempotent per (run, step, identity), the latest before collection
// wins. `gtme answer` writes them; the runner's collection reads them.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// EventAnswered is the step event a participant's answer lands as.
const EventAnswered = "answered"

// Answer is one participant's recorded answer for one pending record.
type Answer struct {
	// Fields are the answer's values: for a filter, `pass` (bool) and
	// `reason`; for a compose or review, the declared outputs.
	Fields map[string]any
	// Participant is who answered, with its prefix: human/<name> or
	// agent/<name>.
	Participant string
	// Note is free text kept with the event (never a cache key).
	Note string
	// Cost is what the participant spent, if reported; Measured its basis
	// (ADR-046).
	Cost     *float64
	Measured bool
	// Token is the pending token the answer was recorded against.
	Token     string
	CreatedAt string
}

// answerDetail is the JSON shape of an `answered` event's detail.
type answerDetail struct {
	Fields      map[string]any `json:"fields"`
	Participant string         `json:"participant"`
	Note        string         `json:"note,omitempty"`
	Cost        *float64       `json:"cost_usd,omitempty"`
	Measured    bool           `json:"measured,omitempty"`
	Token       string         `json:"token,omitempty"`
}

// RecordAnswer appends an `answered` event for a record under a step.
func (l *Ledger) RecordAnswer(ctx context.Context, runID, stepID, identityID string, a Answer) error {
	if identityID == "" {
		return fmt.Errorf("ledger: an answer needs a record")
	}
	detail := answerDetail{
		Fields: a.Fields, Participant: a.Participant, Note: a.Note,
		Cost: a.Cost, Measured: a.Measured, Token: a.Token,
	}
	return l.LogStepEvent(ctx, Provenance{RunID: runID, StepID: stepID}, identityID, EventAnswered, detail)
}

// Answers reads the latest answer per record for a step of a run: every
// `answered` event, newest winning — an earlier answer is superseded, never
// deleted (the ledger is append-only).
func (l *Ledger) Answers(ctx context.Context, runID, stepID string) (map[string]Answer, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT identity_id, detail, created_at FROM step_events
		 WHERE run_id = ? AND step_id = ? AND event = ? AND identity_id IS NOT NULL
		 ORDER BY created_at, id`, runID, stepID, EventAnswered)
	if err != nil {
		return nil, fmt.Errorf("ledger: reading answers: %w", err)
	}
	defer rows.Close()
	out := map[string]Answer{}
	for rows.Next() {
		var id, createdAt string
		var detail sql.NullString
		if err := rows.Scan(&id, &detail, &createdAt); err != nil {
			return nil, fmt.Errorf("ledger: reading answers: %w", err)
		}
		var d answerDetail
		if err := json.Unmarshal([]byte(detail.String), &d); err != nil {
			continue // a malformed answer is not an answer
		}
		out[id] = Answer{
			Fields: d.Fields, Participant: d.Participant, Note: d.Note,
			Cost: d.Cost, Measured: d.Measured, Token: d.Token, CreatedAt: createdAt,
		}
	}
	return out, rows.Err()
}

// PendingSteps lists the steps of a run with records still pending, in the
// order the steps first appeared, each with its pending count.
func (l *Ledger) PendingSteps(ctx context.Context, runID string) ([]string, map[string]int, error) {
	steps, err := l.StepIDs(ctx, runID)
	if err != nil {
		return nil, nil, err
	}
	var out []string
	counts := map[string]int{}
	for _, step := range steps {
		tokens, err := l.PendingTokens(ctx, runID, step)
		if err != nil {
			return nil, nil, err
		}
		if len(tokens) == 0 {
			continue
		}
		out = append(out, step)
		counts[step] = len(tokens)
	}
	return out, counts, nil
}

// AnswerNotes returns the note a participant left with its answer for one
// identity, keyed by run (SPEC §8, ADR-049): `gtme show --provenance` prints
// it beside the values that answer wrote. The latest answer in a run wins,
// matching collection's own rule.
func (l *Ledger) AnswerNotes(ctx context.Context, identityID string) (map[string]string, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT run_id, detail FROM step_events
		 WHERE identity_id = ? AND event = ? ORDER BY created_at, id`, identityID, EventAnswered)
	if err != nil {
		return nil, fmt.Errorf("ledger: reading answer notes: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var runID sql.NullString
		var detail sql.NullString
		if err := rows.Scan(&runID, &detail); err != nil {
			return nil, fmt.Errorf("ledger: reading answer notes: %w", err)
		}
		var d answerDetail
		if err := json.Unmarshal([]byte(detail.String), &d); err != nil || d.Note == "" {
			continue
		}
		out[runID.String] = d.Note
	}
	return out, rows.Err()
}
