package runner

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// PrintReceipt writes the end-of-run receipt: records in and out per step, cache
// skips, cost per step and total, and cost avoided via cache (SPEC §8). It goes
// to stderr, because stdout is data.
func PrintReceipt(w io.Writer, res *Result) {
	fmt.Fprintf(w, "\nrun %s — %s\n", res.RunID, res.Status)

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
