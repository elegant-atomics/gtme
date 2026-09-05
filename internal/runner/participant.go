package runner

// Human and agent steps (SPEC §8 "People and agents answer", ADR-049): a
// human/* or agent/* step opens no session. At a terminal, with prompt: tty,
// the run walks its records and asks; otherwise every unanswered record
// ends pending under the runner-owned token <run-id>/<step-id>, and a later
// `gtme run` collects what `gtme answer` recorded — each answer completing
// the step exactly as an adapter's RECORD or VERDICT would, with provenance
// naming the participant, the referent when of: was declared, and COST
// under the run.

import (
	"context"
	"errors"
	"fmt"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/ledger"
	"github.com/elegant-atomics/gtme/internal/participant"
	"github.com/elegant-atomics/gtme/internal/planner"
)

// runParticipantStep completes a human/agent step's eligible records:
// collections from the ledger's answers first, then fresh records — walked
// in-run when the step may ask, pended otherwise.
func (r *runner) runParticipantStep(ctx context.Context, st *planner.Step, work []*item) error {
	token := planner.PendingToken(r.runID, st.ID)
	r.bump(st, func(s *StepStat) { s.Awaiting = st.Manifest.ID })

	answers, err := r.l.Answers(ctx, r.runID, st.ID)
	if err != nil {
		return err
	}
	var fresh []*item
	for _, it := range work {
		if it.token == "" {
			fresh = append(fresh, it)
			continue
		}
		// Collecting (ADR-038 applied to a person): the answer is in the
		// ledger or it is not — nothing waits.
		ans, ok := answers[it.identityID]
		if !ok {
			r.stillPending(st, it)
			continue
		}
		if err := r.applyAnswer(ctx, st, it, ans); err != nil {
			return err
		}
	}

	if len(fresh) > 0 {
		for _, it := range fresh {
			if err := r.l.LogStepEvent(ctx, r.prov(st.ID), it.identityID, "claimed", nil); err != nil {
				return err
			}
		}
		var walkErr error
		if r.canAsk(st) {
			walkErr = r.walk(ctx, st, fresh, token)
			if walkErr != nil && !errors.Is(walkErr, participant.ErrInterrupted) {
				return walkErr
			}
			if walkErr != nil {
				// The rest is pended on a context the signal did not cancel.
				ctx = context.WithoutCancel(ctx)
			}
		}
		for _, it := range fresh {
			if it.advanced || it.failed || it.pending {
				continue
			}
			if err := r.pend(ctx, st, it, token); err != nil {
				return err
			}
		}
		r.printStepLine(st)
		if walkErr != nil {
			fmt.Fprintf(r.stderr, "%s: interrupted — the remaining record(s) stay pending; `gtme answer %s` records, the next `gtme run %s` collects\n",
				st.ID, r.plan.Pipeline.Name, r.plan.Pipeline.Name)
			return errInterrupted
		}
		return nil
	}
	r.printStepLine(st)
	return nil
}

// canAsk reports whether the step may walk its records in-run (SPEC §8):
// a human/* step with prompt: tty, a terminal on stdin, and a run that is
// not a rehearsal (a simulated human step is a gap, never reached here).
func (r *runner) canAsk(st *planner.Step) bool {
	return st.Participant == adapters.KindHuman && st.Prompt != "never" && r.interactive && !r.simulate
}

// pend leaves one record waiting in the ledger under the runner-owned token.
func (r *runner) pend(ctx context.Context, st *planner.Step, it *item, token string) error {
	it.pending = true
	if err := r.l.LogStepEvent(ctx, r.prov(st.ID), it.identityID, ledger.EventPending,
		map[string]any{"token": token, "awaiting": st.Manifest.ID}); err != nil {
		return err
	}
	r.stillPending(st, it)
	return nil
}

// stillPending counts a record that waits on, without re-logging a pending
// event it already has.
func (r *runner) stillPending(st *planner.Step, it *item) {
	it.pending = true
	r.bump(st, func(s *StepStat) {
		s.InFlight++
		if !containsString(s.Tokens, it.token) && it.token != "" {
			s.Tokens = append(s.Tokens, it.token)
		}
	})
}

// contractFor is the step's answer contract (SPEC §8): its role and its
// effective provides — the declared schema, else the manifest's static one.
func contractFor(st *planner.Step) (participant.Contract, error) {
	return participant.ContractFor(st.Role, st.ProvidesSchema)
}

// surfaceFor is what the participant is shown (SPEC §8, ADR-049).
func surfaceFor(st *planner.Step) participant.Surface {
	s := participant.Surface{Fields: st.RenderFields, Template: st.RenderTemplate, Of: st.Of}
	if !st.NeedsAll {
		s.Uses = st.Needs
	}
	return s
}

// walk asks about each fresh record at the terminal (SPEC §8): every
// answer is recorded as an `answered` event — the same ledger trail an
// answer given later leaves — and applied on the spot.
func (r *runner) walk(ctx context.Context, st *planner.Step, fresh []*item, token string) error {
	contract, err := contractFor(st)
	if err != nil {
		return fmt.Errorf("runner: %s: %w", st.ID, err)
	}
	byKey := map[string]*item{}
	records := make([]participant.Pending, 0, len(fresh))
	for _, it := range fresh {
		byKey[it.key.IdentityKey] = it
		records = append(records, participant.Pending{IdentityKey: it.key.IdentityKey, Fields: it.fields})
	}
	name := participant.Qualify(st.Manifest.ID, participant.DefaultName())
	w := &participant.Walker{In: r.stdin, Out: r.stderr, Contract: contract, Surface: surfaceFor(st), StepID: st.ID, Adapter: st.Manifest.ID}
	_, err = w.Walk(ctx, records, func(p participant.Pending, a participant.Answer) error {
		it := byKey[p.IdentityKey]
		ans := ledger.Answer{Fields: a.Wire(st.Role), Participant: name, Token: token}
		if err := r.l.RecordAnswer(ctx, r.runID, st.ID, it.identityID, ans); err != nil {
			return err
		}
		return r.applyAnswer(ctx, st, it, ans)
	})
	return err
}

// applyAnswer completes a record with a participant's answer (SPEC §8): the
// verdict for a filter, the fields for a compose or review — validated
// against the step's outputs like any adapter RECORD, written with
// provenance `<adapter> @ <participant>#<signature>` and the referent, the
// participant's cost under the run — and, when collected, the token it
// answered.
func (r *runner) applyAnswer(ctx context.Context, st *planner.Step, it *item, ans ledger.Answer) error {
	contract, err := contractFor(st)
	if err != nil {
		return fmt.Errorf("runner: %s: %w", st.ID, err)
	}
	a, err := contract.Validate(ans.Fields)
	if err != nil {
		if err := r.failItem(ctx, st, it, "recorded answer does not match the step's outputs: "+err.Error()); err != nil {
			return err
		}
		return r.settle(ctx, st, it)
	}
	participantName := ans.Participant
	if participantName == "" {
		participantName = participant.Qualify(st.Manifest.ID, participant.DefaultName())
	}
	source := r.participantSource(st, participantName)
	written := 0
	if len(a.Fields) > 0 {
		if err := st.ValidateProvides(a.Fields); err != nil {
			if err := r.failItem(ctx, st, it, err.Error()); err != nil {
				return err
			}
			return r.settle(ctx, st, it)
		}
		if err := r.checkRegistry(it.key.EntityType, a.Fields); err != nil {
			if err := r.failItem(ctx, st, it, err.Error()); err != nil {
				return err
			}
			return r.settle(ctx, st, it)
		}
		n, err := r.l.WriteFieldMapAbout(ctx, it.identityID, source, r.prov(st.ID), a.Fields, nil, it.referent)
		if err != nil {
			return err
		}
		written = n
		if _, err := r.l.UpsertIdentity(ctx, it.key.EntityType, a.Fields, r.prov(st.ID)); err != nil {
			_ = err // not every answer carries identifying fields
		}
		r.emit(it.key, a.Fields)
	}
	if ans.Cost != nil {
		basis := ledger.BasisEstimated
		if ans.Measured {
			basis = ledger.BasisMeasured
		}
		detail := map[string]any{"participant": participantName, "basis": basis}
		if err := r.l.RecordCost(ctx, r.runID, st.ID, it.identityID, participantName, *ans.Cost, basis, detail); err != nil {
			return err
		}
		r.bump(st, func(s *StepStat) { s.Cost.AddAmount(*ans.Cost, basis) })
	}
	detail := map[string]any{"participant": participantName}
	if ans.Note != "" {
		detail["note"] = ans.Note
	}
	r.bump(st, func(s *StepStat) { s.Answered++ })
	if st.Role == adapters.RoleFilter {
		it.verdict = true
		if err := r.l.SetVerdict(ctx, r.runID, it.identityID, st.ID, a.Pass); err != nil {
			return err
		}
		detail["pass"], detail["reason"] = a.Pass, a.Reason
		if !a.Pass {
			r.bump(st, func(s *StepStat) { s.Filtered++ })
			if err := r.l.LogStepEvent(ctx, r.prov(st.ID), it.identityID, "done", it.judgmentDetail(detail)); err != nil {
				return err
			}
			return r.settle(ctx, st, it)
		}
		if err := r.advance(ctx, st, it, detail, nil); err != nil {
			return err
		}
		return r.settle(ctx, st, it)
	}
	detail["fields"] = written
	it.output = true
	if err := r.advance(ctx, st, it, detail, a.Fields); err != nil {
		return err
	}
	return r.settle(ctx, st, it)
}

// settle notes a collected record's token so it is not collected twice
// (ADR-038); a fresh record has none.
func (r *runner) settle(ctx context.Context, st *planner.Step, it *item) error {
	if it.token == "" {
		return nil
	}
	return r.l.LogStepEvent(ctx, r.prov(st.ID), it.identityID, ledger.EventCollected, map[string]any{"token": it.token})
}
