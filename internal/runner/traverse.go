package runner

// The traverse step (SPEC §6/§7, ADR-054): records of one type in, records
// of a type out, each related to the parent that produced it. The runner
// dispatches one parent per session, accepts RECORDs whose key names the
// output type, mints or resolves each child, writes the relation to the
// parent, and opens the next segment: a child enters the run at the
// traverse's state, the parent is finished there. A child that resolves to
// an identity already in the run coalesces (ADR-053 (3)) — one row, its
// state advancing. sql/traverse is the runner-owned floor: it follows a
// relation the ledger already holds, mints nothing, writes no relation.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/ledger"
	"github.com/elegant-atomics/gtme/internal/planner"
	"github.com/elegant-atomics/gtme/internal/protocol"
)

// applyTraverseRecord ingests one child RECORD a traverse session emitted
// for parent.
func (r *runner) applyTraverseRecord(ctx context.Context, st *planner.Step, parent *item, m protocol.Message) error {
	parent.output = true
	if m.Key != nil && m.Key.EntityType != "" && m.Key.EntityType != st.EntityType {
		return r.failChild(ctx, st, parent, fmt.Sprintf("a %s RECORD from a traverse to %s (SPEC §5)", m.Key.EntityType, st.EntityType))
	}
	if len(m.Fields) == 0 && (m.Key == nil || m.Key.IdentityKey == "") {
		return r.failChild(ctx, st, parent, "a child record with neither fields nor a key")
	}
	if len(m.Fields) > 0 {
		if err := st.ValidateProvides(m.Fields); err != nil {
			return r.failChild(ctx, st, parent, err.Error())
		}
		if err := r.checkRegistry(st.EntityType, m.Fields); err != nil {
			return r.failChild(ctx, st, parent, err.Error())
		}
	}

	// Mint or resolve the child (SPEC §4): from its fields, else from the
	// key the adapter asserted.
	var child ledger.Identity
	res, err := r.l.UpsertIdentity(ctx, st.EntityType, m.Fields, r.prov(st.ID))
	switch {
	case err == nil:
		child = res.Identity
	case m.Key != nil && m.Key.IdentityKey != "":
		child, err = r.l.EnsureIdentity(ctx, st.EntityType, m.Key.IdentityKey, r.prov(st.ID))
		if err != nil {
			return err
		}
	default:
		return r.failChild(ctx, st, parent, err.Error())
	}
	if len(m.Fields) > 0 {
		if _, err := r.l.WriteFieldMap(ctx, child.ID, r.source(st), r.prov(st.ID), m.Fields, m.Confidence); err != nil {
			return err
		}
	}
	if err := r.keepPayload(ctx, st, child.ID, m); err != nil {
		return err
	}
	if err := r.relateChild(ctx, st, parent.identityID, child.ID); err != nil {
		return err
	}
	if err := r.writeReferences(ctx, st, child.EntityType, child.ID, m.Fields); err != nil {
		return err
	}
	if err := r.admitChild(ctx, st, parent, child, rowKeys(r.reg, st.EntityType, m)); err != nil {
		return err
	}
	r.emit(protocol.Key{EntityType: child.EntityType, IdentityKey: child.IdentityKey}, m.Fields)
	return nil
}

// relateChild writes the traverse's relation between a child and its parent,
// from whichever end the manifest declared (SPEC §6).
func (r *runner) relateChild(ctx context.Context, st *planner.Step, parentID, childID string) error {
	if st.Relation == nil {
		return nil
	}
	if st.Relation.From == adapters.RelationFromParent {
		return r.l.Relate(ctx, parentID, st.Relation.Name, childID)
	}
	return r.l.Relate(ctx, childID, st.Relation.Name, parentID)
}

// admitChild opens the next segment for one child: it joins the run at the
// traverse's state as `traversed`, or — already in the run — coalesces, its
// state advancing to this step (SPEC §7, ADR-053 (3)). Either event is what
// the next step's eligibility reads.
func (r *runner) admitChild(ctx context.Context, st *planner.Step, parent *item, child ledger.Identity, keys []string) error {
	parent.children++
	added, err := r.l.AddRunRecord(ctx, r.runID, child.ID, st.ID)
	if err != nil {
		return err
	}
	detail := map[string]any{"parent": parent.key.IdentityKey}
	if added {
		if err := r.l.LogStepEvent(ctx, r.prov(st.ID), child.ID, "traversed", detail); err != nil {
			return err
		}
		r.bump(st, func(s *StepStat) { s.Traversed++; s.ChildType = st.EntityType })
		return nil
	}
	detail["into"] = child.IdentityKey
	if len(keys) > 0 {
		detail["keys"] = keys
	}
	if err := r.l.LogStepEvent(ctx, r.prov(st.ID), child.ID, "coalesced", detail); err != nil {
		return err
	}
	if err := r.l.SetRunRecordState(ctx, r.runID, child.ID, st.ID); err != nil {
		return err
	}
	r.bump(st, func(s *StepStat) { s.Coalesced++; s.ChildType = st.EntityType })
	return nil
}

// failChild records an invalid child: the parent's run continues (SPEC §5),
// the failure is a ledger event on the parent naming the child's problem.
func (r *runner) failChild(ctx context.Context, st *planner.Step, parent *item, reason string) error {
	fmt.Fprintf(r.stderr, "%s: dropped a child record of %s: %s\n", st.ID, parent.key.IdentityKey, reason)
	return r.l.LogStepEvent(ctx, r.prov(st.ID), parent.identityID, "failed",
		map[string]any{"reason": reason, "child": true})
}

// runSQLTraverse is sql/traverse (SPEC §10a, ADR-054): one read-only,
// timeboxed query yielding identity_id (of the output type) and parent_id
// (of the run's records); rows whose parent is not eligible here are dropped
// and counted; each child is admitted to the next segment; every parent is
// finished with its child count. Nothing is minted, no relation is written
// — the edge it follows already exists.
func (r *runner) runSQLTraverse(ctx context.Context, st *planner.Step, parentIDs []string) error {
	if len(parentIDs) == 0 {
		return nil
	}
	eligible := map[string]bool{}
	for _, id := range parentIDs {
		eligible[id] = true
	}
	pairs, dropped, err := r.sqlTraversePairs(ctx, st, eligible)
	if err != nil {
		r.logStepFailure(ctx, st, err)
		return fmt.Errorf("runner: %s: %w", st.ID, err)
	}
	if dropped > 0 {
		fmt.Fprintf(r.stderr, "%s: %d result row(s) ignored (parent outside the run)\n", st.ID, dropped)
	}
	byParent := map[string]*item{}
	for _, id := range parentIDs {
		ident, err := r.l.IdentityByID(ctx, id)
		if err != nil {
			return err
		}
		byParent[id] = &item{identityID: id, key: protocol.Key{EntityType: ident.EntityType, IdentityKey: ident.IdentityKey}}
	}
	for _, pr := range pairs {
		parent := byParent[pr.parentID]
		child, err := r.l.IdentityByID(ctx, pr.childID)
		if err == ledger.ErrNotFound {
			if err := r.failChild(ctx, st, parent, fmt.Sprintf("identity_id %q is not in the ledger", pr.childID)); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if child.EntityType != st.EntityType {
			if err := r.failChild(ctx, st, parent, fmt.Sprintf("%s is a %s, not a %s", child.IdentityKey, child.EntityType, st.EntityType)); err != nil {
				return err
			}
			continue
		}
		if err := r.admitChild(ctx, st, parent, child, nil); err != nil {
			return err
		}
	}
	for _, id := range parentIDs {
		it := byParent[id]
		if err := r.l.LogStepEvent(ctx, r.prov(st.ID), id, "claimed", nil); err != nil {
			return err
		}
		if err := r.advance(ctx, st, it, map[string]any{"children": it.children}, nil); err != nil {
			return err
		}
	}
	r.bump(st, func(s *StepStat) { s.ChildType = st.EntityType })
	return nil
}

type traversePair struct{ childID, parentID string }

// sqlTraversePairs runs the step's query read-only and keeps the (child,
// parent) pairs whose parent is eligible, in result order.
func (r *runner) sqlTraversePairs(ctx context.Context, st *planner.Step, eligible map[string]bool) ([]traversePair, int, error) {
	if err := ledger.ReadOnlyStatement(st.Query); err != nil {
		return nil, 0, err
	}
	db, err := ledger.OpenReadOnly(ctx, r.l.Path())
	if err != nil {
		return nil, 0, err
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
		return nil, 0, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, 0, err
	}
	idCol, parentCol := -1, -1
	for i, c := range cols {
		switch strings.ToLower(c) {
		case "identity_id":
			idCol = i
		case "parent_id":
			parentCol = i
		}
	}
	if idCol < 0 || parentCol < 0 {
		return nil, 0, fmt.Errorf("the query must yield identity_id and parent_id columns (got: %s) — SPEC §10a", strings.Join(cols, ", "))
	}
	var out []traversePair
	dropped := 0
	seen := map[traversePair]bool{}
	for rows.Next() {
		scan := make([]any, len(cols))
		for i := range scan {
			var v any
			scan[i] = &v
		}
		if err := rows.Scan(scan...); err != nil {
			return nil, 0, err
		}
		get := func(i int) string {
			v := *(scan[i].(*any))
			if b, ok := v.([]byte); ok {
				return string(b)
			}
			s, _ := v.(string)
			return s
		}
		pr := traversePair{childID: get(idCol), parentID: get(parentCol)}
		if pr.childID == "" || !eligible[pr.parentID] {
			dropped++
			continue
		}
		if seen[pr] {
			continue
		}
		seen[pr] = true
		out = append(out, pr)
	}
	return out, dropped, rows.Err()
}
