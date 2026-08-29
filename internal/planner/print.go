package planner

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/elegant-atomics/gtme/internal/pipeline"
)

// Print writes the resolved plan: steps, projections, cache windows and known
// per-record cost estimates (SPEC §7.4). Estimates print as "?" when the adapter
// publishes none.
func Print(w io.Writer, p *Plan) {
	fmt.Fprintf(w, "pipeline %s (version %d)\n", p.Pipeline.Name, p.Pipeline.Version)

	for i := range p.Steps {
		s := &p.Steps[i]
		kind := s.Role
		switch {
		case s.IsSource:
			kind = "source"
		case s.IsDeliver:
			kind = "deliver"
		}

		fmt.Fprintf(w, "\n%d. %s [%s] — %s", i+1, s.ID, kind, s.Use)
		if s.Manifest != nil {
			fmt.Fprintf(w, "@%d", s.Manifest.Version)
			if s.Adapter != nil && s.Adapter.External {
				fmt.Fprintf(w, " (external: %s)", s.Adapter.Dir)
			}
		}
		fmt.Fprintln(w)

		if s.When != "" {
			fmt.Fprintf(w, "     when:      %s\n", s.When)
		}
		if len(s.Require) > 0 {
			fmt.Fprintf(w, "     require:   members of %s\n", list(s.Require))
		}
		if len(s.Exclude) > 0 {
			fmt.Fprintf(w, "     exclude:   members of %s\n", list(s.Exclude))
		}
		if s.IsDeliver && s.RecordGroup != "" {
			fmt.Fprintf(w, "     record:    touched → %s\n", s.RecordGroup)
		}
		if s.SuppressGroup != "" {
			fmt.Fprintf(w, "     suppress:  touched in %s within %s\n",
				s.SuppressGroup, pipeline.FormatCache(s.SuppressWithin))
		}
		if s.IsGroupSource && s.Limit > 0 {
			fmt.Fprintf(w, "     limit:     %d member(s), oldest-added first\n", s.Limit)
		}
		if s.IsGroupDeliver {
			fmt.Fprintf(w, "     handoff:   → group %q (created on demand)\n", s.TargetGroup)
		}
		fmt.Fprintf(w, "     entity:    %s\n", s.EntityType)
		if !s.IsSource {
			projects := list(s.Needs)
			if s.NeedsAll {
				projects = "(every field known about the record)"
			}
			fmt.Fprintf(w, "     projects:  %s\n", projects)
			if len(s.Required) > 0 {
				fmt.Fprintf(w, "     requires:  %s\n", list(s.Required))
			}
			if len(s.NeedsBranches) > 0 {
				parts := make([]string, 0, len(s.NeedsBranches))
				for _, b := range s.NeedsBranches {
					parts = append(parts, strings.Join(b, "+"))
				}
				fmt.Fprintf(w, "     requires:  any of %s\n", strings.Join(parts, " | "))
			}
		}
		provides := list(s.Provides)
		if s.Wildcard {
			if len(s.Provides) == 0 {
				provides = "(any field the adapter emits)"
			} else {
				provides += " (+ any other field it emits)"
			}
		}
		fmt.Fprintf(w, "     provides:  %s\n", provides)

		switch {
		case s.Cache > 0:
			fmt.Fprintf(w, "     cache:     %s\n", pipeline.FormatCache(s.Cache))
		case s.Role == "enrich" || s.Role == "verify":
			fmt.Fprintf(w, "     cache:     off (no freshness_days, no cache:)\n")
		}
		if s.Batch {
			fmt.Fprintf(w, "     batch:     %d records per invocation\n", s.BatchSize)
		}
		if s.IsDeliver {
			idem := s.Idempotency
			if idem == "" {
				idem = "(identity key)"
			}
			fmt.Fprintf(w, "     idempotency: %s\n", idem)
			if len(s.Variables) > 0 {
				targets := make([]string, 0, len(s.Variables))
				for t := range s.Variables {
					targets = append(targets, t)
				}
				sort.Strings(targets)
				pairs := make([]string, 0, len(targets))
				for _, t := range targets {
					pairs = append(pairs, fmt.Sprintf("%s ← %s", t, s.Variables[t]))
				}
				fmt.Fprintf(w, "     variables: %s\n", strings.Join(pairs, ", "))
				fmt.Fprintf(w, "     on_missing: %s\n", s.OnMissing)
			}
		}
		for _, note := range s.Notes {
			fmt.Fprintf(w, "     note:      %s\n", note)
		}
		if s.Manifest != nil && len(s.Manifest.Credentials) > 0 {
			fmt.Fprintf(w, "     creds:     %s (resolved)\n", list(s.Manifest.Credentials))
		}
		for _, name := range s.MissingOptional {
			fmt.Fprintf(w, "     warning:   optional credential %s is not set; this step will fail at run time if it needs it\n", name)
		}
		est := "?"
		if s.CostEstimate != nil {
			est = fmt.Sprintf("$%.4f", *s.CostEstimate)
		}
		fmt.Fprintf(w, "     est/record: %s\n", est)
	}

	// The at-a-glance send surface (SPEC §7, ADR-031): every deliver step —
	// target adapter and touch scope — reviewable in one place, since YAML
	// position no longer marks the send points.
	var delivers []*Step
	for i := range p.Steps {
		if p.Steps[i].IsDeliver {
			delivers = append(delivers, &p.Steps[i])
		}
	}
	if len(delivers) > 0 {
		fmt.Fprintf(w, "\nsend surface: %d deliver step(s) (ADR-031)\n", len(delivers))
		for _, s := range delivers {
			target := s.Use
			if s.IsGroupDeliver {
				target = fmt.Sprintf("group %q (handoff, no network)", s.TargetGroup)
			}
			fmt.Fprintf(w, "  %s → %s (touch scope: %s)\n", s.ID, target, s.RecordGroup)
		}
	}
	for _, warning := range p.Warnings {
		fmt.Fprintf(w, "\nwarning: %s\n", warning)
	}

	if p.Pipeline.Group != "" {
		fmt.Fprintf(w, "\nterminus: records completing the run are added to group %q (ADR-021)\n", p.Pipeline.Group)
	}
	fmt.Fprintf(w, "\navailable fields after the last step: %s\n", list(p.Available))
	if p.Wildcard {
		fmt.Fprintln(w, "note: a step provides open-ended fields, so per-record needs are re-checked at run time")
	}
	fmt.Fprintln(w, "plan ok — nothing has been spent")
}

func list(v []string) string {
	if len(v) == 0 {
		return "(none)"
	}
	return strings.Join(v, ", ")
}
