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
	// fetched names the projected fields whose provenance is an external
	// fetch (SPEC §10.3, ADR-035) — what an AI step fences.
	fetched []string
	idem    string // deliver steps

	advanced bool // state moved to this step
	verdict  bool // a VERDICT arrived (filter steps)
	output   bool // a RECORD arrived
	failed   bool // this step failed the record; nothing advances it now
	attested bool // an ATTEST arrived (attesting deliver steps)
	// token is the in-flight handle this record is being collected under
	// (ADR-038); pending marks a PENDING that covered it this session.
	token   string
	pending bool
	// signature and input are the judgment cache keys (ADR-039) for an AI
	// step's record, recorded on its done event.
	signature string
	input     string
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
	// Deliver preflight (SPEC §8, ADR-040): before any record moves, ask a
	// preflighting adapter whether the live target is fit to send to. A
	// dry run reports; an armed run stops the step on blocked.
	if st.IsDeliver && st.Manifest != nil && st.Manifest.Preflights && !stub {
		if err := r.preflight(ctx, st); err != nil {
			return err
		}
	}
	// Records left in flight by an earlier session of this run (ADR-038)
	// are collected under their token rather than dispatched again.
	tokens, err := r.l.PendingTokens(ctx, r.runID, st.ID)
	if err != nil {
		return err
	}
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

		it, err := r.prepare(ctx, st, rr.IdentityID, tokens[rr.IdentityID])
		if err != nil {
			return err
		}
		if it != nil {
			it.token = tokens[rr.IdentityID]
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
	if st.IsGroupDeliver {
		// The handoff (SPEC §8, ADR-032): no adapter, no network — every
		// record prepare() let through is delivered to the group here. Dry
		// runs never reach this point with work (prepare receipts them).
		for _, it := range work {
			if err := r.l.LogStepEvent(ctx, r.prov(st.ID), it.identityID, "claimed", nil); err != nil {
				return err
			}
			r.bump(st, func(s *StepStat) { s.In++ })
			if err := r.advance(ctx, st, it, map[string]any{"group": st.TargetGroup}, nil); err != nil {
				return err
			}
		}
		r.printStepLine(st)
		return nil
	}
	return r.dispatch(ctx, st, work)
}

// preflight runs the preflight session (SPEC §5): OPEN with preflight:
// true, END, and reads the adapter's PREFLIGHT. blocked fails the step
// before a single record is dispatched, unless the run is dry — a
// rehearsal reports and moves on; inconclusive warns; an adapter that
// answers nothing is inconclusive.
func (r *runner) preflight(ctx context.Context, st *planner.Step) error {
	if skip, ok := st.Config["preflight"].(bool); ok && !skip {
		return nil
	}
	sess, err := r.openSession(ctx, st)
	if err != nil {
		return err
	}
	open := r.openMessage(st, nil)
	open.Preflight = true
	sendErr := sess.SendStream([]protocol.Message{open, protocol.End()})

	status, reason := protocol.PreflightInconclusive, "the adapter reported no preflight"
	var checks []protocol.Check
	for {
		m, err := sess.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if werr := sess.Wait(); werr != nil {
				err = werr
			}
			r.logStepFailure(ctx, st, err)
			return fmt.Errorf("runner: %s: preflight: %w", st.ID, err)
		}
		switch m.Type {
		case protocol.TypePreflight:
			status, reason, checks = m.Status, m.Reason, m.Checks
		case protocol.TypeLog:
			r.forwardLog(st, m)
		case protocol.TypeRecord, protocol.TypeVerdict, protocol.TypeAttest:
			return fmt.Errorf("runner: %s: preflight: the adapter sent a %s in a preflight session — it must send nothing (SPEC §5)", st.ID, m.Type)
		}
	}
	if err := sess.Wait(); err != nil {
		r.logStepFailure(ctx, st, err)
		return fmt.Errorf("runner: %s: preflight: %w", st.ID, err)
	}
	if err := <-sendErr; err != nil && !isBrokenPipe(err) {
		return fmt.Errorf("runner: %s: preflight: %w", st.ID, err)
	}
	switch status {
	case protocol.PreflightOK, protocol.PreflightBlocked, protocol.PreflightInconclusive:
	default:
		reason = fmt.Sprintf("unrecognised preflight status %q", status)
		status = protocol.PreflightInconclusive
	}
	r.bump(st, func(s *StepStat) { s.Preflight, s.PreflightReason, s.PreflightChecks = status, reason, checks })
	detail := map[string]any{"status": status, "reason": reason, "checks": checks}
	if err := r.l.LogStepEvent(ctx, r.prov(st.ID), "", "preflight", detail); err != nil {
		return err
	}
	switch status {
	case protocol.PreflightInconclusive:
		fmt.Fprintf(r.stderr, "%s [warn]: preflight inconclusive — %s; proceeding\n", st.ID, reason)
	case protocol.PreflightBlocked:
		fmt.Fprintf(r.stderr, "%s: preflight BLOCKED — %s\n", st.ID, reason)
		if r.dry {
			return nil
		}
		return fmt.Errorf("runner: %s: preflight blocked — %s (nothing was sent; fix the target and run again, or --resume)", st.ID, reason)
	default:
		fmt.Fprintf(r.stderr, "%s: preflight ok — %d check(s)\n", st.ID, len(checks))
	}
	return nil
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
func (r *runner) prepare(ctx context.Context, st *planner.Step, identityID, token string) (*item, error) {
	projection := ledger.Projection{Fields: st.Needs}
	if st.NeedsAll {
		projection.Fields = nil // everything the ledger knows
	}
	rec, err := r.l.Project(ctx, identityID, projection)
	if err != nil {
		return nil, err
	}
	fields := rec.Fields()
	if err := r.validateNeeds(st, fields); err != nil {
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
	if isAIStep(st) {
		for name, v := range rec.Values {
			if r.fetchedSource(v.Source) {
				it.fetched = append(it.fetched, name)
			}
		}
	}

	skipped, err := r.cacheSkip(ctx, st, it)
	if err != nil {
		return nil, err
	}
	if skipped {
		return nil, nil
	}
	// The judgment cache (SPEC §7, ADR-039): same question, same facts,
	// same answer — unless the step said respend. A record being collected
	// (ADR-038) is past this point by definition.
	if isAIStep(st) {
		it.signature = r.judgmentSignature(st)
		it.input = inputHash(st, r.plan.Pipeline.Name, fields)
		if token == "" && !st.Respend {
			reused, err := r.judgmentSkip(ctx, st, it)
			if err != nil {
				return nil, err
			}
			if reused {
				return nil, nil
			}
		}
	}

	if st.IsDeliver {
		idem, err := r.idempotencyKey(ctx, st, it)
		if err != nil {
			return nil, err
		}
		it.idem = idem
		delivered, err := r.l.AlreadyDelivered(ctx, st.Target(), idem)
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
	r.bump(st, func(s *StepStat) {
		s.DryRun = append(s.DryRun, rv)
		if st.IsGroupDeliver {
			s.TargetGroup = st.TargetGroup
			s.GroupWould++
		}
	})
	return nil
}

// validateNeeds checks a projection against the step's needs: the manifest's
// schema for an adapter step; a runner-owned deliver (group/deliver) has no
// static floor — variables: completeness is checked by prepare.
func (r *runner) validateNeeds(st *planner.Step, fields map[string]any) error {
	if st.Manifest == nil {
		return nil
	}
	return st.Manifest.ValidateNeeds(fields)
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

	// Collections first — one session per token, its OPEN carrying the token
	// — then fresh work in the usual chunks (ADR-038).
	var chunks [][]*item
	byToken := map[string][]*item{}
	var fresh []*item
	var tokenOrder []string
	for _, it := range work {
		if it.token == "" {
			fresh = append(fresh, it)
			continue
		}
		if _, seen := byToken[it.token]; !seen {
			tokenOrder = append(tokenOrder, it.token)
		}
		byToken[it.token] = append(byToken[it.token], it)
	}
	for _, t := range tokenOrder {
		chunks = append(chunks, byToken[t])
	}
	if len(fresh) > 0 {
		chunks = append(chunks, chunk(fresh, chunkSize(st, len(fresh), r.conc))...)
	}
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
	if stat.InFlight > 0 {
		line += fmt.Sprintf(", %d in flight", stat.InFlight)
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

// judgmentSkip reuses a stored judgment when one matches the record's cache
// keys within the step's window (ADR-039): a filter re-applies its verdict
// (pass advances, fail freezes), a compose has nothing to write. Receipted
// as skipped_cache with reason same_judgment.
func (r *runner) judgmentSkip(ctx context.Context, st *planner.Step, it *item) (bool, error) {
	var since time.Time
	if st.Cache > 0 {
		since = r.now().Add(-st.Cache)
	}
	j, found, err := r.l.LastJudgment(ctx, it.identityID, it.signature, it.input, since)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	detail := map[string]any{"reason": "same_judgment", "signature": it.signature, "input": it.input, "judged_in": j.RunID}
	if err := r.l.LogStepEvent(ctx, r.prov(st.ID), it.identityID, "skipped_cache", detail); err != nil {
		return false, err
	}
	if st.Role == adapters.RoleFilter {
		pass := j.Pass != nil && *j.Pass
		if err := r.l.SetVerdict(ctx, r.runID, it.identityID, st.ID, pass); err != nil {
			return false, err
		}
		if !pass {
			r.bump(st, func(s *StepStat) { s.CacheSkips++; s.Filtered++; s.AvoidedUnknown = true })
			return true, nil
		}
	}
	if err := r.l.SetRunRecordState(ctx, r.runID, it.identityID, st.ID); err != nil {
		return false, err
	}
	it.advanced = true
	r.emit(it.key, nil)
	r.bump(st, func(s *StepStat) { s.CacheSkips++; s.AvoidedUnknown = true })
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
	msgs = append(msgs, r.openMessage(st, items))
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
		case protocol.TypeAttest:
			if err := r.applyAttest(ctx, st, byKey, m); err != nil {
				return err
			}
		case protocol.TypePending:
			if err := r.applyPending(ctx, st, items, m); err != nil {
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

	// A collected record is settled either way (ADR-038): note which token
	// answered it, so it is not collected twice.
	for _, it := range items {
		if it.token != "" && (it.advanced || it.failed) {
			if err := r.l.LogStepEvent(ctx, r.prov(st.ID), it.identityID, ledger.EventCollected,
				map[string]any{"token": it.token}); err != nil {
				return err
			}
		}
	}

	// Records the adapter said nothing about: a filter that returns no verdict has
	// failed to judge, anything else simply found nothing and moves on. A record
	// a PENDING covered is neither — it is in flight.
	for _, it := range items {
		if it.advanced || it.pending {
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
			continue
		}
		// An attesting deliver adapter that acknowledged a record but said
		// nothing about what the target stored: inconclusive (SPEC §5).
		if attesting(st) && !it.attested && !it.failed {
			if err := r.attestInconclusive(ctx, st, it, "the adapter reported no attestation for this record"); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyPending marks every record the session did not answer as in flight
// under the token (SPEC §5, ADR-038): a pending step event each, state
// untouched, nothing downstream sees them, and the run will finish pending.
func (r *runner) applyPending(ctx context.Context, st *planner.Step, items []*item, m protocol.Message) error {
	if strings.TrimSpace(m.Token) == "" {
		fmt.Fprintf(r.stderr, "%s: ignoring a PENDING with no token\n", st.ID)
		return nil
	}
	n := 0
	for _, it := range items {
		if it.advanced || it.failed || it.verdict || it.output || it.pending {
			continue
		}
		it.pending = true
		detail := map[string]any{"token": m.Token}
		for k, v := range m.Detail {
			detail[k] = v
		}
		if err := r.l.LogStepEvent(ctx, r.prov(st.ID), it.identityID, ledger.EventPending, detail); err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return nil
	}
	r.bump(st, func(s *StepStat) {
		s.InFlight += n
		if !containsString(s.Tokens, m.Token) {
			s.Tokens = append(s.Tokens, m.Token)
		}
	})
	return nil
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// attesting reports a deliver step whose adapter declares attests (SPEC §6).
func attesting(st *planner.Step) bool {
	return st.IsDeliver && st.Manifest != nil && st.Manifest.Attests
}

// applyAttest applies a deliver adapter's three-way verdict (SPEC §5/§8,
// ADR-036). confirmed: the delivery advances and its row is refined;
// contradicted: the row is kept with that status (the record exists at the
// target — re-sending would duplicate it) and the record fails;
// inconclusive: advances, accepted, with a receipt warning. Only an adapter
// declaring attests is heard.
func (r *runner) applyAttest(ctx context.Context, st *planner.Step, byKey map[string]*item, m protocol.Message) error {
	if m.Key == nil {
		fmt.Fprintf(r.stderr, "%s: ignoring an ATTEST with no key\n", st.ID)
		return nil
	}
	if !attesting(st) {
		fmt.Fprintf(r.stderr, "%s: ignoring an ATTEST from %s, which does not declare attests (SPEC §6)\n", st.ID, st.Use)
		return nil
	}
	it, ok := byKey[m.Key.String()]
	if !ok {
		fmt.Fprintf(r.stderr, "%s: ignoring an ATTEST for an unknown key %s\n", st.ID, m.Key)
		return nil
	}
	if it.attested || it.failed || it.advanced {
		return nil
	}
	it.attested = true
	switch m.Status {
	case protocol.AttestConfirmed:
		if err := r.advance(ctx, st, it, map[string]any{"attested": m.Status, "reason": m.Reason}, nil); err != nil {
			return err
		}
		if err := r.l.SetDeliveryStatus(ctx, st.Target(), it.idem, ledger.DeliveryConfirmed); err != nil {
			return err
		}
		r.bump(st, func(s *StepStat) { s.Attests = true; s.Confirmed++ })
		return nil
	case protocol.AttestContradicted:
		reason := "contradicted by the target's re-read"
		if m.Reason != "" {
			reason += ": " + m.Reason
		}
		// The record exists at the target: keep the row so idempotency holds,
		// marked for what it is, and fail the record.
		if err := r.l.RecordDelivery(ctx, it.identityID, st.Target(), it.idem, r.runID); err != nil {
			return err
		}
		if err := r.l.SetDeliveryStatus(ctx, st.Target(), it.idem, ledger.DeliveryContradicted); err != nil {
			return err
		}
		r.bump(st, func(s *StepStat) { s.Attests = true; s.Contradicted++ })
		return r.failItem(ctx, st, it, reason)
	default:
		reason := m.Reason
		if m.Status != protocol.AttestInconclusive {
			reason = fmt.Sprintf("unrecognised attestation status %q", m.Status)
		}
		return r.attestInconclusive(ctx, st, it, reason)
	}
}

// attestInconclusive advances a delivery that could not be confirmed: it
// stays accepted, and the receipt warns (SPEC §8) — failing it would be the
// more dangerous direction to be wrong in (ADR-036).
func (r *runner) attestInconclusive(ctx context.Context, st *planner.Step, it *item, reason string) error {
	it.attested = true
	if err := r.advance(ctx, st, it, map[string]any{"attested": protocol.AttestInconclusive, "reason": reason}, nil); err != nil {
		return err
	}
	fmt.Fprintf(r.stderr, "%s [warn]: %s delivered (accepted) but not confirmed — %s\n", st.ID, it.key.IdentityKey, reason)
	r.bump(st, func(s *StepStat) {
		s.Attests = true
		s.Inconclusive = append(s.Inconclusive, Attestation{IdentityKey: it.key.IdentityKey, Reason: reason})
	})
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

	// Output is validated against the step's provides before it reaches the
	// ledger (SPEC §5) — the declared schema for an AI step that carries one
	// (ADR-033), else the manifest's: an invalid record fails, the run
	// continues. Canonical fields are additionally held to the registry's
	// type, domain and normalized form (SPEC §4a, enforcement layer 2).
	if err := st.ValidateProvides(m.Fields); err != nil {
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
	// A filter's RECORD carries its declared provides (SPEC §5, ADR-033) —
	// stored like any output, pass or fail — but only its VERDICT advances.
	if st.Role == adapters.RoleFilter {
		r.emit(it.key, m.Fields)
		return nil
	}
	// An attesting deliver adapter's acknowledgement waits for its ATTEST
	// (SPEC §5, ADR-036); a silent adapter is settled inconclusive at the
	// end of the session.
	if attesting(st) {
		return nil
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
	if it.failed {
		// Its RECORD already failed validation at this step: the failure
		// stands, whatever the verdict says (SPEC §5).
		return nil
	}
	pass := m.Passed()
	if err := r.l.SetVerdict(ctx, r.runID, it.identityID, st.ID, pass); err != nil {
		return err
	}
	if !pass {
		r.bump(st, func(s *StepStat) { s.Filtered++ })
		return r.l.LogStepEvent(ctx, r.prov(st.ID), it.identityID, "done",
			it.judgmentDetail(map[string]any{"pass": false, "reason": m.Reason}))
	}
	return r.advance(ctx, st, it, map[string]any{"pass": true, "reason": m.Reason}, nil)
}

// advance marks a record done for this step, moves its state forward, and passes
// it downstream in pipe mode.
func (r *runner) advance(ctx context.Context, st *planner.Step, it *item, detail, fields map[string]any) error {
	if it.advanced || it.failed {
		return nil
	}
	if err := r.l.LogStepEvent(ctx, r.prov(st.ID), it.identityID, "done", it.judgmentDetail(detail)); err != nil {
		return err
	}
	if err := r.l.SetRunRecordState(ctx, r.runID, it.identityID, st.ID); err != nil {
		return err
	}
	if st.IsDeliver {
		if err := r.l.RecordDelivery(ctx, it.identityID, st.Target(), it.idem, r.runID); err != nil {
			return err
		}
		// The handoff itself (SPEC §8, ADR-032): membership in the target
		// group, created on demand. An existing member is not re-asserted.
		if st.IsGroupDeliver {
			g, err := r.l.EnsureGroup(ctx, st.TargetGroup)
			if err != nil {
				return err
			}
			members, err := r.l.GroupMembership(ctx, g.ID)
			if err != nil {
				return err
			}
			if !members[it.identityID] {
				if err := r.l.AddGroupEvent(ctx, g.ID, it.identityID, ledger.GroupAdded,
					map[string]any{"pipeline": r.plan.Pipeline.Name, "step": st.ID, "handoff": true}, r.runID); err != nil {
					return err
				}
			}
			r.bump(st, func(s *StepStat) { s.TargetGroup = st.TargetGroup; s.GroupAdded++ })
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
				map[string]any{"target": st.Target(), "step": st.ID}, r.runID); err != nil {
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
	if it.advanced || it.failed {
		return nil
	}
	it.failed = true
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
