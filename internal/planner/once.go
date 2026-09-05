package planner

import (
	"context"

	"github.com/elegant-atomics/gtme/internal/ledger"
)

// FinishedRecords is the set of identities this pipeline has already
// finished (SPEC §8, ADR-052): in some earlier run of the same pipeline
// name, the record completed that run's final step, or a filter's fail
// verdict stopped it. Both are outcomes the pipeline reached on purpose. A
// record that failed a step is NOT finished (a transient error retries next
// run), and neither is one left pending (collect-first resumes its run).
//
// Terminality is judged against each run's own snapshot (the final step it
// declared when it ran), so a pipeline that later grew a step does not
// re-offer everyone it had already worked. The scope is the pipeline name.
func (p *Plan) FinishedRecords(ctx context.Context, l *ledger.Ledger) (map[string]bool, error) {
	recs, err := l.PipelineRecords(ctx, p.Pipeline.Name)
	if err != nil {
		return nil, err
	}
	finished := map[string]bool{}
	for _, rec := range recs {
		if rec.State == rec.FinalStep || p.Stopped(rec.RunRecord) {
			finished[rec.IdentityID] = true
		}
	}
	return finished, nil
}

// Stopped reports whether a fail verdict froze the record: a filter's fail
// stops it (SPEC §7); a deliver step's fail is a withheld send and the record
// advanced (SPEC §8, ADR-031). Deliver steps are read from this plan.
func (p *Plan) Stopped(rr ledger.RunRecord) bool {
	for step, v := range rr.Verdicts {
		if v == "fail" && !p.isDeliverStep(step) {
			return true
		}
	}
	return false
}

func (p *Plan) isDeliverStep(id string) bool {
	for i := range p.Steps {
		if p.Steps[i].ID == id {
			return p.Steps[i].IsDeliver
		}
	}
	return false
}

// OnceSourcing is how many members a counted `once:` source will select:
// the eligible count, capped by limit when one is set.
func (s *Step) OnceSourcing() int {
	if s.Limit > 0 && s.Limit < s.OnceEligible {
		return s.Limit
	}
	return s.OnceEligible
}

// countOnce fills a `once:` group source's plan-time counts from the ledger
// (SPEC §8, ADR-052: the eligible count MUST print, and it is knowable
// before anything is spent). Read-only, no network.
func (p *Plan) countOnce(ctx context.Context, l *ledger.Ledger, s *Step) error {
	g, err := l.GetGroup(ctx, s.SourceGroup)
	if err != nil {
		return err
	}
	members, err := l.GroupMembersOldest(ctx, g.ID, 0)
	if err != nil {
		return err
	}
	finished, err := p.FinishedRecords(ctx, l)
	if err != nil {
		return err
	}
	s.OnceMembers, s.OnceEligible = len(members), 0
	for _, m := range members {
		if !finished[m.ID] {
			s.OnceEligible++
		}
	}
	s.OnceCounted = true
	return nil
}
