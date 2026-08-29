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
	switch {
	case res.Simulated:
		title += " (SIMULATED — fixtures only; nothing sent, nothing persisted)"
	case res.DryRun:
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

	// Simulation gaps (SPEC §8): a binding without fixtures, or a credentialed
	// process adapter with nothing to serve, is surfaced — never silently passed.
	for _, s := range res.Steps {
		if !s.SimGap {
			continue
		}
		if s.SimGapRecords > 0 {
			fmt.Fprintf(w, "simulation gap: %s (%s) — %d record(s) passed through untouched (no fixtures to serve)\n",
				s.ID, s.Use, s.SimGapRecords)
		} else {
			fmt.Fprintf(w, "simulation gap: %s (%s) — no fixtures to serve\n", s.ID, s.Use)
		}
	}

	// Suppression holds (SPEC §8, ADR-021): a chosen contact policy, receipted.
	for _, s := range res.Steps {
		if len(s.Suppressed) == 0 {
			continue
		}
		fmt.Fprintf(w, "%s: %d record(s) suppressed:\n", s.ID, len(s.Suppressed))
		for _, sr := range s.Suppressed {
			fmt.Fprintf(w, "  %s: touched in %q %s ago\n", sr.IdentityKey, sr.Group, sr.Age)
		}
	}
	// The membership terminus (SPEC §8, ADR-021).
	switch {
	case res.TerminusGroup == "":
	case res.DryRun || res.Simulated:
		fmt.Fprintf(w, "group %q: %d record(s) would be added (held back — %s)\n",
			res.TerminusGroup, res.TerminusWould, holdReason(res))
	default:
		fmt.Fprintf(w, "group %q: %d record(s) added\n", res.TerminusGroup, res.TerminusAdded)
	}

	// Handoffs (SPEC §8, ADR-032): what each group/deliver step committed to
	// its group, or would have.
	for _, s := range res.Steps {
		switch {
		case s.TargetGroup == "":
		case s.GroupWould > 0:
			fmt.Fprintf(w, "%s: %d record(s) would be handed off to group %q (held back — %s)\n",
				s.ID, s.GroupWould, s.TargetGroup, holdReason(res))
		default:
			fmt.Fprintf(w, "%s: %d record(s) handed off to group %q\n", s.ID, s.GroupAdded, s.TargetGroup)
		}
	}
	// Attestation (SPEC §8, ADR-036): accepted is never sent; an attesting
	// adapter's confirmed/contradicted refine it, and every inconclusive
	// delivery is named — accepted, not confirmed.
	for _, s := range res.Steps {
		if !s.Attests {
			continue
		}
		fmt.Fprintf(w, "%s: attested %d confirmed, %d contradicted, %d inconclusive (deliveries are accepted, never sent, until a provider attests)\n",
			s.ID, s.Confirmed, s.Contradicted, len(s.Inconclusive))
		for _, a := range s.Inconclusive {
			fmt.Fprintf(w, "  %s: accepted, not confirmed — %s\n", a.IdentityKey, a.Reason)
		}
	}
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

func holdReason(res *Result) string {
	if res.Simulated {
		return "simulated run"
	}
	return "dry run"
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
