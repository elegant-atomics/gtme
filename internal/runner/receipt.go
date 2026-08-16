package runner

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// PrintReceipt writes the end-of-run receipt: records in and out per step, cache
// skips, cost per step and total, and cost avoided via cache (SPEC §8). It goes
// to stderr, because stdout is data.
func PrintReceipt(w io.Writer, res *Result) {
	title := res.Status
	if res.DryRun {
		title += " (dry run — nothing sent)"
	}
	fmt.Fprintf(w, "\nrun %s — %s\n", res.RunID, title)

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "step\tadapter\tin\tout\tcached\tfiltered\tfailed\tcost\tavoided")

	var totalCost, totalAvoided float64
	avoidedUnknown := false
	totalSkips := 0
	for _, s := range res.Steps {
		totalSkips += s.CacheSkips
		avoided := "-"
		if s.CacheSkips > 0 {
			switch {
			case s.AvoidedUnknown && s.AvoidedUSD == 0:
				avoided = "?"
			case s.AvoidedUnknown:
				avoided = fmt.Sprintf("$%.4f+?", s.AvoidedUSD)
			default:
				avoided = fmt.Sprintf("$%.4f", s.AvoidedUSD)
			}
		}
		if s.AvoidedUnknown {
			avoidedUnknown = true
		}
		totalCost += s.CostUSD
		totalAvoided += s.AvoidedUSD

		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\t%s\n",
			s.ID, s.Use, s.In, s.Out, s.CacheSkips,
			dash(s.Filtered), dash(s.Failed), money(s.CostUSD), avoided)
	}
	tw.Flush()

	// Deliver records held back by on_missing, each with its reason (SPEC §8).
	for _, s := range res.Steps {
		if len(s.MissingSkips) == 0 {
			continue
		}
		fmt.Fprintf(w, "%s: %d record(s) held back by on_missing:\n", s.ID, len(s.MissingSkips))
		for _, rv := range s.MissingSkips {
			fmt.Fprintf(w, "  %s: missing %s\n", rv.IdentityKey, strings.Join(rv.Missing, ", "))
		}
	}
	// The dry-run approval artifact (SPEC §8, ADR-019): each record's RESOLVED
	// variables, exactly what an armed run would send.
	for _, s := range res.Steps {
		if len(s.DryRun) == 0 {
			continue
		}
		fmt.Fprintf(w, "%s: resolved variables for %d record(s) — review, then run again without --dry-run to arm:\n", s.ID, len(s.DryRun))
		for _, rv := range s.DryRun {
			fmt.Fprintf(w, "  %s\n", rv.IdentityKey)
			targets := make([]string, 0, len(rv.Resolved))
			for t := range rv.Resolved {
				targets = append(targets, t)
			}
			sort.Strings(targets)
			for _, t := range targets {
				fmt.Fprintf(w, "    %s: %q\n", t, rv.Resolved[t])
			}
		}
	}

	total := fmt.Sprintf("total: %s spent", money(totalCost))
	if totalSkips > 0 {
		amount := fmt.Sprintf("$%.4f", totalAvoided)
		if avoidedUnknown {
			// Some skipped adapters publish no cost_estimate_usd, so the saving is a
			// floor, not a total (SPEC §8).
			amount += "+?"
		}
		total += fmt.Sprintf(", %s avoided via cache (%d records skipped)", amount, totalSkips)
	}
	fmt.Fprintln(w, total)
}

func money(v float64) string {
	if v == 0 {
		return "$0"
	}
	return fmt.Sprintf("$%.4f", v)
}

func dash(n int) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprint(n)
}

// Summary is a one-line description of a result, for logs.
func Summary(res *Result) string {
	parts := make([]string, 0, len(res.Steps))
	for _, s := range res.Steps {
		parts = append(parts, fmt.Sprintf("%s=%d", s.ID, s.Out))
	}
	return strings.Join(parts, " ")
}
