package runner

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/ledger"
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
	if res.Status == "pending" {
		awaiting := ""
		for _, s := range res.Steps {
			if s.InFlight > 0 && s.Awaiting != "" {
				awaiting = s.Awaiting
				break
			}
		}
		switch {
		case awaiting != "" && res.Interrupted:
			title += fmt.Sprintf(" — interrupted; the rest awaits %s: `gtme answer %s` records, the next `gtme run %s` collects (ADR-049)", awaiting, res.Pipeline, res.Pipeline)
		case awaiting != "":
			title += fmt.Sprintf(" — ended awaiting %s: `gtme answer %s` records, the next `gtme run %s` collects (ADR-049)", awaiting, res.Pipeline, res.Pipeline)
		default:
			title += " — ended with a step in flight; the next `gtme run` of this pipeline collects (ADR-038)"
		}
	}
	// A paid run that produced no records says so (SPEC §8, ADR-053) —
	// information on the receipt, not a new exit code.
	if paidForNothing(res) {
		var spent ledger.CostTotal
		for _, s := range res.Steps {
			spent.Add(s.Cost)
		}
		title += " — 0 records, " + FormatSpent(spent)
	}
	fmt.Fprintf(w, "\nrun %s — %s\n", res.RunID, title)

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "step\tadapter\tin\tout\tempty\tcached\tfiltered\tfailed\tcost\tavoided")

	var totalCost ledger.CostTotal
	var totalAvoided float64
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
		totalCost.Add(s.Cost)
		totalAvoided += s.AvoidedUSD

		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%d\t%s\t%s\t%s\t%s\n",
			s.ID, s.Use, s.In, s.Out, dash(s.Empty), s.CacheSkips,
			dash(s.Filtered), dash(s.Failed), money(s.Cost.Total()), avoided)
	}
	tw.Flush()
	// Declared fields absent at dispatch (SPEC §7, ADR-053): the gap is
	// visible whether or not the operator chose a policy.
	for i := range res.Steps {
		if s := &res.Steps[i]; s.Missing > 0 {
			fmt.Fprintf(w, "%s: %s — dispatched anyway (on_missing: run); set on_missing: skip or fail to hold them\n", s.ID, missingNote(s))
		}
	}

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
	// Preflight (SPEC §8, ADR-040): the target's side of the story, before
	// anything was sent.
	for _, s := range res.Steps {
		if s.Preflight == "" {
			continue
		}
		names := make([]string, 0, len(s.PreflightChecks))
		for _, c := range s.PreflightChecks {
			mark := "✓"
			if !c.OK {
				mark = "✗"
			}
			names = append(names, mark+" "+c.Name)
		}
		switch s.Preflight {
		case "ok":
			fmt.Fprintf(w, "%s: preflight ok — %d check(s)", s.ID, len(s.PreflightChecks))
		case "blocked":
			fmt.Fprintf(w, "%s: preflight BLOCKED — %s", s.ID, s.PreflightReason)
		default:
			fmt.Fprintf(w, "%s: preflight inconclusive — %s (proceeded)", s.ID, s.PreflightReason)
		}
		if len(names) > 0 {
			fmt.Fprintf(w, " (%s)", strings.Join(names, ", "))
		}
		fmt.Fprintln(w)
	}
	// In flight (SPEC §8, ADR-038): what a deferred step left with the
	// provider, and how to collect it — or, for a human/agent step
	// (ADR-049), who is awaited and the verb that answers.
	for _, s := range res.Steps {
		if s.InFlight == 0 {
			continue
		}
		if s.Awaiting != "" {
			fmt.Fprintf(w, "%s: %d in, %d out — %d awaiting %s; `gtme answer %s` records, the next `gtme run %s` collects (or `gtme show --run %s --pending %s` to read them)\n",
				s.ID, s.In, s.Out, s.InFlight, s.Awaiting, res.Pipeline, res.Pipeline, res.RunID, s.ID)
			continue
		}
		fmt.Fprintf(w, "%s: %d record(s) in flight (%s); the next `gtme run` of this pipeline collects, or `gtme run --resume %s`\n",
			s.ID, s.InFlight, strings.Join(s.Tokens, ", "), res.RunID)
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

	total := fmt.Sprintf("total: %s spent", FormatCost(totalCost))
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

// paidForNothing reports a run that spent money and sourced no records
// (SPEC §8, ADR-053).
func paidForNothing(res *Result) bool {
	sourced, spent := 0, 0.0
	for _, s := range res.Steps {
		if s.Role == adapters.RoleSource {
			sourced += s.Out + s.Empty
		}
		spent += s.Cost.Total()
	}
	return sourced == 0 && spent > 0
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

// FormatCost renders a total with its basis (SPEC §8, ADR-046): a purely
// measured total prints bare; a purely estimated one `$X (estimated)`; a
// mixed run splits — `$X ($Y measured + $Z estimated)`. A total with no
// estimated rows at all (nothing spent, or every dollar measured) is bare.
// `gtme runs` prints the same string from the ledger.
func FormatCost(c ledger.CostTotal) string {
	switch {
	case c.Estimates == 0:
		return money(c.Total())
	case c.Measured == 0:
		return money(c.Estimated) + " (estimated)"
	default:
		return fmt.Sprintf("%s (%s measured + %s estimated)", money(c.Total()), money(c.Measured), money(c.Estimated))
	}
}

// FormatSpent renders "$X spent" with its basis after the verb — `$4.10
// spent (estimated)` — the phrase SPEC §8's paid-zero-record mark uses on
// the receipt and in `gtme runs`.
func FormatSpent(c ledger.CostTotal) string {
	switch {
	case c.Estimates == 0:
		return money(c.Total()) + " spent"
	case c.Measured == 0:
		return money(c.Estimated) + " spent (estimated)"
	default:
		return fmt.Sprintf("%s spent (%s measured + %s estimated)", money(c.Total()), money(c.Measured), money(c.Estimated))
	}
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
