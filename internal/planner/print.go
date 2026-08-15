package planner

import (
	"fmt"
	"io"
	"strings"

	"github.com/trevorfox/gtm/internal/pipeline"
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
		}
		if len(s.Manifest.Credentials) > 0 {
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
