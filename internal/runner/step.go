package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/identity"
	"github.com/elegant-atomics/gtme/internal/ledger"
	"github.com/elegant-atomics/gtme/internal/pipeline"
	"github.com/elegant-atomics/gtme/internal/planner"
	"github.com/elegant-atomics/gtme/internal/protocol"
)

// item is one record's trip through one step.
type item struct {
	identityID string
	key        protocol.Key
	fields     map[string]any
	idem       string // deliver steps

	advanced bool // state moved to this step
	verdict  bool // a VERDICT arrived (filter steps)
	output   bool // a RECORD arrived
}

// bump mutates a step's stats under the lock.
func (r *runner) bump(st *planner.Step, f func(*StepStat)) {
	stat := r.stat(st)
	r.mu.Lock()
	defer r.mu.Unlock()
	f(stat)
}

// runStep executes one non-source step.
func (r *runner) runStep(ctx context.Context, i int) error {
	st := &r.plan.Steps[i]
	r.stat(st) // ensure the step appears in the receipt even with zero records
	prev := r.plan.PrevState(i)

	records, err := r.l.RunRecords(ctx, r.runID)
	if err != nil {
		return err
	}

	gate, err := r.membershipGate(ctx, st)
	if err != nil {
		return err
	}

	stub := r.stubbed(st)
	var work []*item
	var sqlWork []string
	for _, rr := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		// A record that failed a filter stops advancing (SPEC §7). A deliver
		// step's fail verdict is a withheld send, not a stop (SPEC §8, ADR-031).
		if r.stopped(rr) {
			continue
		}
		// Strict ordering: a record is eligible for this step only if the previous
		// step completed it. This is also what makes --resume skip done work.
		if rr.State != prev {
			continue
		}
		if st.WhenStep != "" && !rr.Passed(st.WhenStep) {
			r.bump(st, func(s *StepStat) { s.Gated++ })
			continue
		}
		// Membership gates (SPEC §7, ADR-021): require = member of every group,
		// exclude = member of none. Exclusion is the judgment-memory mechanism —
		// a gated record is not dispatched, so nothing re-judges it.
		if gate != nil && !gate(rr.IdentityID) {
			r.bump(st, func(s *StepStat) { s.Gated++ })
			continue
		}

		if stub {
			// A stubbed step under --simulate passes records through untouched: no
			// adapter call, no fields, no verdicts — a counted gap (SPEC §8). A
			// stubbed filter judges nothing, so downstream when: gates will hold
			// records back; that consequence is the gap made visible, not a bug.
			if err := r.l.LogStepEvent(ctx, r.prov(st.ID), rr.IdentityID, "simulated",
				map[string]any{"simulation_gap": true}); err != nil {
				return err
			}
			if err := r.l.SetRunRecordState(ctx, r.runID, rr.IdentityID, st.ID); err != nil {
				return err
			}
			r.bump(st, func(s *StepStat) { s.SimGap = true; s.SimGapRecords++ })
			continue
		}

		if st.IsSQL {
			sqlWork = append(sqlWork, rr.IdentityID)
			continue
		}

		it, err := r.prepare(ctx, st, rr.IdentityID)
		if err != nil {
			return err
		}
		if it != nil {
			work = append(work, it)
		}
	}

	if stub {
		r.printStepLine(st)
		return nil
	}
	if st.IsSQL {
		if err := r.runSQLStep(ctx, st, sqlWork); err != nil {
			return err
		}
		r.printStepLine(st)
		return nil
	}
	return r.dispatch(ctx, st, work)
}

// membershipGate loads the step's require/exclude membership sets once and
// returns a per-record predicate, or nil when the step declares no gates.
func (r *runner) membershipGate(ctx context.Context, st *planner.Step) (func(string) bool, error) {
	if len(st.Require) == 0 && len(st.Exclude) == 0 {
		return nil, nil
	}
	load := func(names []string) ([]map[string]bool, error) {
		sets := make([]map[string]bool, 0, len(names))
		for _, name := range names {
			g, err := r.l.GetGroup(ctx, name)
			if err != nil {
				return nil, fmt.Errorf("runner: %s: %w", st.ID, err)
			}
			set, err := r.l.GroupMembership(ctx, g.ID)
			if err != nil {
				return nil, err
			}
			sets = append(sets, set)
		}
		return sets, nil
	}
	required, err := load(st.Require)
	if err != nil {
		return nil, err
	}
	excluded, err := load(st.Exclude)
	if err != nil {
		return nil, err
	}
	return func(identityID string) bool {
		for _, set := range required {
			if !set[identityID] {
				return false
			}
		}
		for _, set := range excluded {
			if set[identityID] {
				return false
			}
		}
		return true
	}, nil
}

// prepare turns one run record into a unit of work for a step, or handles it
// without the adapter — a record whose needs are unsatisfiable fails here, and
// one the ledger (or an earlier delivery) already answers is skipped. A nil item
// with a nil error means "already dealt with".
func (r *runner) prepare(ctx context.Context, st *planner.Step, identityID string) (*item, error) {
	projection := ledger.Projection{Fields: st.Needs}
	if st.NeedsAll {
		projection.Fields = nil // everything the ledger knows
	}
	rec, err := r.l.Project(ctx, identityID, projection)
	if err != nil {
		return nil, err
	}
	fields := rec.Fields()
	if err := st.Manifest.ValidateNeeds(fields); err != nil {
		r.bump(st, func(s *StepStat) { s.Failed++ })
		if err := r.l.LogStepEvent(ctx, r.prov(st.ID), identityID, "failed",
			map[string]any{"reason": err.Error()}); err != nil {
			return nil, err
		}
		return nil, nil
	}

	it := &item{
		identityID: identityID,
		key:        protocol.Key{EntityType: rec.Identity.EntityType, IdentityKey: rec.Identity.IdentityKey},
		fields:     fields,
	}

	skipped, err := r.cacheSkip(ctx, st, it)
	if err != nil {
		return nil, err
	}
	if skipped {
		return nil, nil
	}

	if st.IsDeliver {
		idem, err := r.idempotencyKey(ctx, st, it)
		if err != nil {
			return nil, err
		}
		it.idem = idem
		delivered, err := r.l.AlreadyDelivered(ctx, st.Manifest.ID, idem)
		if err != nil {
			return nil, err
		}
		if delivered {
			if err := r.skip(ctx, st, it, "already_delivered"); err != nil {
				return nil, err
			}
			return nil, nil
		}

		// Suppression (SPEC §8, ADR-021): a chosen contact policy layered above
		// the idempotency floor — skip records touched in the group within the
		// window, receipted with reasons. Applies dry and armed alike (a
		// rehearsal that ignored the policy would rehearse the wrong send).
		held, err := r.suppress(ctx, st, it)
		if err != nil {
			return nil, err
		}
		if held {
			return nil, nil
		}

		// Deliver completeness (SPEC §8, ADR-019): every variables: target must
		// resolve to a non-empty value before the record may send — blank merge
		// fields never do. The policy applies armed and dry alike.
		rv := resolveVariables(st.Variables, it)
		if len(rv.Missing) > 0 {
			return nil, r.holdMissing(ctx, st, it, rv)
		}
		// A dry run resolves and receipts, but never calls the adapter and
		// never writes a delivery (SPEC §8).
		if r.dry {
			return nil, r.dryDeliver(ctx, st, it, rv)
		}
	}
	return it, nil
}

// resolveVariables renders a record's deliver variables: each target merge
// field with its resolved value, and the ledger fields that had none.
func resolveVariables(vars map[string]string, it *item) RecordVariables {
	rv := RecordVariables{IdentityKey: it.key.IdentityKey, Resolved: map[string]string{}}
	missing := map[string]bool{}
	for target, field := range vars {
		s := stringify(it.fields[field])
		if s == "" {
			missing[field] = true
			continue
		}
		rv.Resolved[target] = s
	}
	for f := range missing {
		rv.Missing = append(rv.Missing, f)
	}
	sort.Strings(rv.Missing)
	return rv
}

// suppress checks the deliver step's suppression window (SPEC §8, ADR-021).
func (r *runner) suppress(ctx context.Context, st *planner.Step, it *item) (bool, error) {
	if st.SuppressGroup == "" {
		return false, nil
	}
	g, err := r.l.GetGroup(ctx, st.SuppressGroup)
	if err != nil {
		return false, fmt.Errorf("runner: %s: %w", st.ID, err)
	}
	last, ok, err := r.l.LastTouched(ctx, g.ID, it.identityID)
	if err != nil {
		return false, err
	}
	age := r.now().Sub(last)
	if !ok || age > st.SuppressWithin {
		return false, nil
	}
	reason := fmt.Sprintf("suppressed: touched in %q %s ago (window %s)",
		g.Name, age.Round(time.Second), pipeline.FormatCache(st.SuppressWithin))
	if err := r.l.SetVerdict(ctx, r.runID, it.identityID, st.ID, false); err != nil {
		return false, err
	}
	if err := r.l.LogStepEvent(ctx, r.prov(st.ID), it.identityID, "done",
		map[string]any{"pass": false, "reason": reason}); err != nil {
		return false, err
	}
	// Suppression gates this step's send, not the record (SPEC §8, ADR-031):
	// it advances, so later steps — and the terminus — still see it.
	if err := r.l.SetRunRecordState(ctx, r.runID, it.identityID, st.ID); err != nil {
		return false, err
	}
	r.bump(st, func(s *StepStat) {
		s.Skipped++
		s.Suppressed = append(s.Suppressed, SuppressedRecord{
			IdentityKey: it.key.IdentityKey, Group: g.Name, Age: age.Round(time.Second).String(),
		})
	})
	return true, nil
}

// holdMissing applies on_missing to a record whose variables did not resolve
// (SPEC §8): fail marks it failed (state freezes, it does not advance); skip
// (the default) records a fail verdict with the missing fields as the reason,
// lists it in the receipt, and advances the record — the withheld send is this
// step's alone, and later steps still see the record (ADR-031).
func (r *runner) holdMissing(ctx context.Context, st *planner.Step, it *item, rv RecordVariables) error {
	reason := "missing " + strings.Join(rv.Missing, ", ")
	if st.OnMissing == "fail" {
		return r.failItem(ctx, st, it, reason)
	}
	if err := r.l.SetVerdict(ctx, r.runID, it.identityID, st.ID, false); err != nil {
		return err
	}
	if err := r.l.LogStepEvent(ctx, r.prov(st.ID), it.identityID, "done",
		map[string]any{"pass": false, "reason": reason}); err != nil {
		return err
	}
	if err := r.l.SetRunRecordState(ctx, r.runID, it.identityID, st.ID); err != nil {
		return err
	}
	r.bump(st, func(s *StepStat) {
		s.Skipped++
		s.MissingSkips = append(s.MissingSkips, rv)
	})
	return nil
}

// dryDeliver records what WOULD have been sent — the resolved variables land in
// step_events (event='dry_run') and the receipt; deliveries stays untouched, so
// the armed run behaves as if the dry run never happened (SPEC §8).
func (r *runner) dryDeliver(ctx context.Context, st *planner.Step, it *item, rv RecordVariables) error {
	if err := r.l.LogStepEvent(ctx, r.prov(st.ID), it.identityID, "dry_run",
		map[string]any{"variables": rv.Resolved}); err != nil {
		return err
	}
	if err := r.l.SetRunRecordState(ctx, r.runID, it.identityID, st.ID); err != nil {
		return err
	}
	r.bump(st, func(s *StepStat) { s.DryRun = append(s.DryRun, rv) })
	return nil
}

// stringify renders a projected value for a merge field; empty means missing.
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(raw)
	}
}

// dispatch runs a step's eligible records through adapter sessions with a worker
// pool, and reports the step's tally. Shared by `gtme run` and pipe mode.
func (r *runner) dispatch(ctx context.Context, st *planner.Step, work []*item) error {
	if len(work) == 0 {
		r.printStepLine(st)
		return nil
	}

	chunks := chunk(work, chunkSize(st, len(work), r.conc))
	workers := r.conc
	if workers > len(chunks) {
		workers = len(chunks)
	}

	queue := make(chan []*item)
	var wg sync.WaitGroup
	var once sync.Once
	var fatal error

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range queue {
				if err := r.processChunk(ctx, st, c); err != nil {
					once.Do(func() { fatal = err })
					return
				}
			}
		}()
	}
	for _, c := range chunks {
		select {
		case queue <- c:
		case <-ctx.Done():
			close(queue)
			wg.Wait()
			return ctx.Err()
		}
	}
	close(queue)
	wg.Wait()

	r.printStepLine(st)
	return fatal
}

// printStepLine reports a step's tally on stderr as it finishes.
func (r *runner) printStepLine(st *planner.Step) {
	stat := r.stat(st)
	r.mu.Lock()
	defer r.mu.Unlock()
	line := fmt.Sprintf("%s: %d in, %d out, %d cached, %d filtered, %d failed",
		st.ID, stat.In, stat.Out, stat.CacheSkips, stat.Filtered, stat.Failed)
	if stat.Gated > 0 {
		line += fmt.Sprintf(", %d gated", stat.Gated)
	}
	fmt.Fprintln(r.stderr, line)
}

// cacheSkip skips a record whose output this step already knows, within the
// step's freshness window (SPEC §7). Only enrich and verify steps are cacheable:
// filters and composes are cheap-to-rerun judgements, deliveries have their own
// idempotency, and a source has no input to key on.
func (r *runner) cacheSkip(ctx context.Context, st *planner.Step, it *item) (bool, error) {
	if st.Cache <= 0 || len(st.Provides) == 0 {
		return false, nil
	}
	if st.Role != adapters.RoleEnrich && st.Role != adapters.RoleVerify {
		return false, nil
	}
	cached, err := r.l.Project(ctx, it.identityID, ledger.Projection{
		Fields:           st.Provides,
		DefaultFreshness: st.Cache,
	})
	if err != nil {
		return false, err
	}
	reason := "fresh_in_ledger"
	if !cached.Has(st.Provides...) {
		// A provides schema may declare optional fields the adapter did not return,
		// so "all fields present" is too strict on its own. Falling back to
		// provenance answers the question the cache actually exists to answer: have
		// we already paid this adapter for this record, recently?
		last, ok, err := r.l.LastWriteBySource(ctx, it.identityID, st.Manifest.Source())
		if err != nil {
			return false, err
		}
		if !ok || r.now().Sub(last) > st.Cache {
			return false, nil
		}
		reason = "already_answered_by_adapter"
	}
	if err := r.skip(ctx, st, it, reason); err != nil {
		return false, err
	}
	return true, nil
}

// skip advances a record past a step without calling the adapter.
func (r *runner) skip(ctx context.Context, st *planner.Step, it *item, reason string) error {
	if err := r.l.LogStepEvent(ctx, r.prov(st.ID), it.identityID, "skipped_cache",
		map[string]any{"reason": reason}); err != nil {
		return err
	}
	if err := r.l.SetRunRecordState(ctx, r.runID, it.identityID, st.ID); err != nil {
		return err
	}
	it.advanced = true
	r.emit(it.key, nil)
	r.bump(st, func(s *StepStat) {
		s.CacheSkips++
		if st.CostEstimate != nil {
			s.AvoidedUSD += *st.CostEstimate
		} else {
			s.AvoidedUnknown = true
		}
	})
	return nil
}

// idempotencyKey is the value of the configured field, defaulting to the
// identity key (SPEC §8).
func (r *runner) idempotencyKey(ctx context.Context, st *planner.Step, it *item) (string, error) {
	field := st.Idempotency
	if field == "" {
		return it.key.IdentityKey, nil
	}
	if v, ok := it.fields[field]; ok {
		return canonicalIdempotency(v), nil
	}
	rec, err := r.l.Project(ctx, it.identityID, ledger.Projection{Fields: []string{field}})
	if err != nil {
		return "", err
	}
	if v, ok := rec.Values[field]; ok {
		return canonicalIdempotency(v.Any()), nil
	}
	// No value for the idempotency field: fall back to the identity key rather
	// than delivering the same record twice.
	return it.key.IdentityKey, nil
}

// canonicalIdempotency normalizes a delivery key. Whitespace never distinguishes
// two records, and an email is case-insensitive everywhere it matters — so
// "Jane.Doe@Acme.com" and "jane.doe@acme.com" must not both be delivered. Other
// values keep their case, because an external id legitimately can be
// case-sensitive.
func canonicalIdempotency(v any) string {
	s, ok := v.(string)
	if !ok {
		return strings.TrimSpace(fmt.Sprint(v))
	}
	if email := identity.NormalizeEmail(s); email != "" {
		return email
	}
	return strings.TrimSpace(s)
}

// processChunk runs one adapter session over a slice of records.
func (r *runner) processChunk(ctx context.Context, st *planner.Step, items []*item) error {
	byKey := make(map[string]*item, len(items))
	for _, it := range items {
		byKey[it.key.String()] = it
	}

	sess, err := r.openSession(ctx, st)
	if err != nil {
		return err
	}

	msgs := make([]protocol.Message, 0, len(items)+2)
	msgs = append(msgs, r.openMessage(st))
	for _, it := range items {
		if err := r.l.LogStepEvent(ctx, r.prov(st.ID), it.identityID, "claimed", nil); err != nil {
			return err
		}
		r.bump(st, func(s *StepStat) { s.In++ })
		msgs = append(msgs, protocol.Record(it.key, it.fields, nil))
	}
	msgs = append(msgs, protocol.End())
	sendErr := sess.SendStream(msgs)

	for {
		m, err := sess.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A malformed line is fatal for the session; wait for the process so its
			// own error (usually more informative) wins.
			if werr := sess.Wait(); werr != nil {
				err = werr
			}
			return r.chunkFailed(ctx, st, items, err)
		}

		switch m.Type {
		case protocol.TypeRecord:
			if err := r.applyRecord(ctx, st, byKey, m); err != nil {
				return err
			}
		case protocol.TypeVerdict:
			if err := r.applyVerdict(ctx, st, byKey, m); err != nil {
				return err
			}
		case protocol.TypeCost:
			id := ""
			if m.Key != nil {
				if it, ok := byKey[m.Key.String()]; ok {
					id = it.identityID
				}
			}
			if err := r.recordCost(ctx, st, id, m); err != nil {
				return err
			}
		case protocol.TypeLog:
			r.forwardLog(st, m)
		case protocol.TypeSchema, protocol.TypeState, protocol.TypeEnd:
			// Informational here; the contract was fixed at plan time.
		}
	}

	if err := sess.Wait(); err != nil {
		return r.chunkFailed(ctx, st, items, err)
	}
	// A write error only matters once the adapter has had its say: a filter that
	// stops reading early is not a failure.
	if err := <-sendErr; err != nil && !isBrokenPipe(err) {
		return r.chunkFailed(ctx, st, items, err)
	}

	// Records the adapter said nothing about: a filter that returns no verdict has
	// failed to judge, anything else simply found nothing and moves on.
	for _, it := range items {
		if it.advanced {
			continue
		}
		if st.Role == adapters.RoleFilter {
			// A verdict already decided this record: a pass advanced it above, a fail
			// freezes it here. No verdict at all means the filter failed to judge.
			if !it.verdict {
				if err := r.failItem(ctx, st, it, "no verdict returned"); err != nil {
					return err
				}
			}
			continue
		}
		if !it.output {
			if err := r.advance(ctx, st, it, map[string]any{"fields": 0}, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *runner) applyRecord(ctx context.Context, st *planner.Step, byKey map[string]*item, m protocol.Message) error {
	if m.Key == nil {
		fmt.Fprintf(r.stderr, "%s: ignoring a RECORD with no key\n", st.ID)
		return nil
	}
	it, ok := byKey[m.Key.String()]
	if !ok {
		fmt.Fprintf(r.stderr, "%s: ignoring a RECORD for an unknown key %s\n", st.ID, m.Key)
		return nil
	}
	it.output = true

	// Output is validated against the manifest before it reaches the ledger
	// (SPEC §5): an invalid record fails, the run continues. Canonical fields
	// are additionally held to the registry's type, domain and normalized form
	// (SPEC §4a, enforcement layer 2).
	if err := st.Manifest.ValidateProvides(m.Fields); err != nil {
		return r.failItem(ctx, st, it, err.Error())
	}
	if err := r.checkRegistry(it.key.EntityType, m.Fields); err != nil {
		return r.failItem(ctx, st, it, err.Error())
	}

	n, err := r.l.WriteFieldMap(ctx, it.identityID, r.source(st), r.prov(st.ID), m.Fields, m.Confidence)
	if err != nil {
		return err
	}
	if err := r.keepPayload(ctx, st, it.identityID, m); err != nil {
		return err
	}
	// A stronger key may have arrived with the enrichment.
	if len(m.Fields) > 0 {
		if _, err := r.l.UpsertIdentity(ctx, it.key.EntityType, m.Fields, r.prov(st.ID)); err != nil {
			// Not every enrichment carries identifying fields; that is fine.
			_ = err
		}
	}
	return r.advance(ctx, st, it, map[string]any{"fields": n}, m.Fields)
}

func (r *runner) applyVerdict(ctx context.Context, st *planner.Step, byKey map[string]*item, m protocol.Message) error {
	if m.Key == nil {
		fmt.Fprintf(r.stderr, "%s: ignoring a VERDICT with no key\n", st.ID)
		return nil
	}
	it, ok := byKey[m.Key.String()]
	if !ok {
		fmt.Fprintf(r.stderr, "%s: ignoring a VERDICT for an unknown key %s\n", st.ID, m.Key)
		return nil
	}
	it.verdict = true
	pass := m.Passed()
	if err := r.l.SetVerdict(ctx, r.runID, it.identityID, st.ID, pass); err != nil {
		return err
	}
	if !pass {
		r.bump(st, func(s *StepStat) { s.Filtered++ })
		return r.l.LogStepEvent(ctx, r.prov(st.ID), it.identityID, "done",
			map[string]any{"pass": false, "reason": m.Reason})
	}
	return r.advance(ctx, st, it, map[string]any{"pass": true, "reason": m.Reason}, nil)
}

// advance marks a record done for this step, moves its state forward, and passes
// it downstream in pipe mode.
func (r *runner) advance(ctx context.Context, st *planner.Step, it *item, detail, fields map[string]any) error {
	if it.advanced {
		return nil
	}
	if err := r.l.LogStepEvent(ctx, r.prov(st.ID), it.identityID, "done", detail); err != nil {
		return err
	}
	if err := r.l.SetRunRecordState(ctx, r.runID, it.identityID, st.ID); err != nil {
		return err
	}
	if st.IsDeliver {
		if err := r.l.RecordDelivery(ctx, it.identityID, st.Manifest.ID, it.idem, r.runID); err != nil {
			return err
		}
		// Touch scoping (SPEC §8, ADR-021): a successful delivery appends a
		// `touched` event to the step's record: group (pipeline name by
		// default), created on demand. Only armed runs reach this path — dry
		// and simulated deliveries never invoke the adapter.
		if st.RecordGroup != "" {
			g, err := r.l.EnsureGroup(ctx, st.RecordGroup)
			if err != nil {
				return err
			}
			if err := r.l.AddGroupEvent(ctx, g.ID, it.identityID, ledger.GroupTouched,
				map[string]any{"target": st.Manifest.ID, "step": st.ID}, r.runID); err != nil {
				return err
			}
		}
	}
	it.advanced = true
	r.bump(st, func(s *StepStat) { s.Out++ })
	r.emit(it.key, fields)
	return nil
}

func (r *runner) failItem(ctx context.Context, st *planner.Step, it *item, reason string) error {
	if it.advanced {
		return nil
	}
	r.bump(st, func(s *StepStat) { s.Failed++ })
	return r.l.LogStepEvent(ctx, r.prov(st.ID), it.identityID, "failed", map[string]any{"reason": reason})
}

// chunkFailed marks every record in a crashed session as failed and returns the
// fatal error. Output written before the crash stays in the ledger.
func (r *runner) chunkFailed(ctx context.Context, st *planner.Step, items []*item, cause error) error {
	for _, it := range items {
		if it.advanced {
			continue
		}
		if err := r.failItem(ctx, st, it, cause.Error()); err != nil {
			return err
		}
	}
	r.logStepFailure(ctx, st, cause)
	return fmt.Errorf("runner: %s: %w", st.ID, cause)
}

// isBrokenPipe reports whether an error is just the adapter having closed its
// input.
func isBrokenPipe(err error) bool {
	if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "broken pipe") || strings.Contains(msg, "file already closed")
}

// chunkSize decides how many records one adapter session handles. AI steps take
// exactly batch_size (one invocation per batch, SPEC §9); everything else splits
// the work across the pool.
func chunkSize(st *planner.Step, n, conc int) int {
	if st.Batch {
		if st.BatchSize > 0 {
			return st.BatchSize
		}
		return planner.DefaultBatchSize
	}
	size := (n + conc - 1) / conc
	if size < 1 {
		size = 1
	}
	if size > MaxChunk {
		size = MaxChunk
	}
	return size
}

func chunk(items []*item, size int) [][]*item {
	if size < 1 {
		size = 1
	}
	var out [][]*item
	for i := 0; i < len(items); i += size {
		end := i + size
		if end > len(items) {
			end = len(items)
		}
		out = append(out, items[i:end])
	}
	return out
}
