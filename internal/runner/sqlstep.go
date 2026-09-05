package runner

// SQL steps (SPEC §10a, ADR-027): the deterministic transform floor. Runner-
// owned like the group source — no adapter, no wire protocol. One read-only,
// timeboxed SELECT per step; its result rows apply only to the run's eligible
// records (the engine guarantees scope regardless of how the query was
// written), and everything it derives flows through the same append path as
// adapter output: registry-checked, provenance-stamped `sql/<role> @ <hash>`.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/elegant-atomics/gtme/internal/ledger"
	"github.com/elegant-atomics/gtme/internal/planner"
)

// sqlTimebox bounds one SQL step's query (SPEC §10a).
const sqlTimebox = 30 * time.Second

// sqlRow is one result row keyed to a run record.
type sqlRow struct {
	values map[string]any
	pass   *bool
	reason string
}

// runSQLStep executes one SQL step over the run's eligible records.
func (r *runner) runSQLStep(ctx context.Context, st *planner.Step, identityIDs []string) error {
	if len(identityIDs) == 0 {
		return nil
	}
	eligible := map[string]bool{}
	for _, id := range identityIDs {
		eligible[id] = true
	}
	rows, hasPass, dropped, err := r.sqlResults(ctx, st, eligible)
	if err != nil {
		r.logStepFailure(ctx, st, err)
		return fmt.Errorf("runner: %s: %w", st.ID, err)
	}
	if dropped > 0 {
		fmt.Fprintf(r.stderr, "%s: %d result row(s) ignored (outside the run)\n", st.ID, dropped)
	}

	source := fmt.Sprintf("%s @ %s", st.Use, queryHash(st.Query))
	for _, identityID := range identityIDs {
		row, found := rows[identityID]
		if st.Role == "filter" {
			if err := r.applySQLVerdict(ctx, st, identityID, row, found, hasPass); err != nil {
				return err
			}
			continue
		}
		// sql/transform: derived columns append like adapter output; a record the
		// query said nothing about simply advances with nothing derived.
		fields := map[string]any{}
		if found {
			for _, name := range st.Provides {
				if v, ok := row.values[name]; ok && v != nil {
					fields[name] = v
				}
			}
		}
		if len(fields) > 0 {
			ident, err := r.l.IdentityByID(ctx, identityID)
			if err != nil {
				return err
			}
			if err := r.checkRegistry(ident.EntityType, fields); err != nil {
				if ferr := r.failSQL(ctx, st, identityID, err.Error()); ferr != nil {
					return ferr
				}
				continue
			}
			if _, err := r.l.WriteFieldMap(ctx, identityID, source, r.prov(st.ID), fields, nil); err != nil {
				return err
			}
		}
		if err := r.l.LogStepEvent(ctx, r.prov(st.ID), identityID, "done",
			map[string]any{"fields": len(fields)}); err != nil {
			return err
		}
		if err := r.l.SetRunRecordState(ctx, r.runID, identityID, st.ID); err != nil {
			return err
		}
		// A record the query said nothing about advanced with nothing
		// derived: empty, not out (SPEC §8, ADR-053).
		if len(fields) == 0 {
			r.bump(st, func(s *StepStat) { s.Empty++ })
		} else {
			r.bump(st, func(s *StepStat) { s.Out++ })
		}
	}
	return nil
}

// applySQLVerdict judges one record: explicitly via a pass column, or
// membership-style (returned = pass) when the query has none (SPEC §10a).
func (r *runner) applySQLVerdict(ctx context.Context, st *planner.Step, identityID string, row sqlRow, found, hasPass bool) error {
	pass := found
	reason := "selected by predicate"
	switch {
	case !found:
		reason = "not selected by predicate"
	case hasPass:
		pass = row.pass != nil && *row.pass
		reason = row.reason
		if reason == "" {
			if pass {
				reason = "predicate true"
			} else {
				reason = "predicate false"
			}
		}
	}
	if err := r.l.SetVerdict(ctx, r.runID, identityID, st.ID, pass); err != nil {
		return err
	}
	if !pass {
		r.bump(st, func(s *StepStat) { s.Filtered++ })
		return r.l.LogStepEvent(ctx, r.prov(st.ID), identityID, "done",
			map[string]any{"pass": false, "reason": reason})
	}
	if err := r.l.LogStepEvent(ctx, r.prov(st.ID), identityID, "done",
		map[string]any{"pass": true, "reason": reason}); err != nil {
		return err
	}
	if err := r.l.SetRunRecordState(ctx, r.runID, identityID, st.ID); err != nil {
		return err
	}
	r.bump(st, func(s *StepStat) { s.Out++ })
	return nil
}

func (r *runner) failSQL(ctx context.Context, st *planner.Step, identityID, reason string) error {
	r.bump(st, func(s *StepStat) { s.Failed++ })
	return r.l.LogStepEvent(ctx, r.prov(st.ID), identityID, "failed", map[string]any{"reason": reason})
}

// sqlResults runs the step's query on the read-only connection and maps rows
// by identity_id, keeping only the run's eligible records.
func (r *runner) sqlResults(ctx context.Context, st *planner.Step, eligible map[string]bool) (map[string]sqlRow, bool, int, error) {
	if err := ledger.ReadOnlyStatement(st.Query); err != nil {
		return nil, false, 0, err
	}
	db, err := ledger.OpenReadOnly(ctx, r.l.Path())
	if err != nil {
		return nil, false, 0, err
	}
	defer db.Close()

	qctx, cancel := context.WithTimeout(ctx, sqlTimebox)
	defer cancel()
	var args []any
	if strings.Contains(st.Query, ":run_id") {
		args = append(args, sql.Named("run_id", r.runID))
	}
	rows, err := db.QueryContext(qctx, st.Query, args...)
	if err != nil {
		return nil, false, 0, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, false, 0, err
	}
	idCol, passCol, reasonCol := -1, -1, -1
	for i, c := range cols {
		switch strings.ToLower(c) {
		case "identity_id":
			idCol = i
		case "pass":
			passCol = i
		case "reason":
			reasonCol = i
		}
	}
	if idCol < 0 {
		return nil, false, 0, fmt.Errorf("the query must yield an identity_id column (got: %s)", strings.Join(cols, ", "))
	}
	if st.Role == "enrich" {
		have := map[string]bool{}
		for _, c := range cols {
			have[c] = true
		}
		for _, p := range st.Provides {
			if !have[p] {
				return nil, false, 0, fmt.Errorf("declared provides field %q is not a result column (got: %s)", p, strings.Join(cols, ", "))
			}
		}
	}

	out := map[string]sqlRow{}
	dropped := 0
	for rows.Next() {
		scan := make([]any, len(cols))
		for i := range scan {
			var v any
			scan[i] = &v
		}
		if err := rows.Scan(scan...); err != nil {
			return nil, false, 0, err
		}
		get := func(i int) any {
			v := *(scan[i].(*any))
			if b, ok := v.([]byte); ok {
				return string(b)
			}
			return v
		}
		id, _ := get(idCol).(string)
		if id == "" || !eligible[id] {
			dropped++
			continue
		}
		row := sqlRow{values: map[string]any{}}
		for i, c := range cols {
			row.values[c] = get(i)
		}
		if passCol >= 0 {
			p := truthy(get(passCol))
			row.pass = &p
		}
		if reasonCol >= 0 {
			row.reason, _ = get(reasonCol).(string)
		}
		out[id] = row
	}
	return out, passCol >= 0, dropped, rows.Err()
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case int64:
		return t != 0
	case float64:
		return t != 0
	case string:
		return t == "1" || strings.EqualFold(t, "true")
	default:
		return false
	}
}

// queryHash is the provenance fingerprint of a SQL step's query (SPEC §10a):
// the first 12 hex of its sha256, over the trimmed text.
func queryHash(q string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(q)))
	return hex.EncodeToString(sum[:])[:12]
}
