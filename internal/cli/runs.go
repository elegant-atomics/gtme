package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/elegant-atomics/gtme/internal/ledger"
)

// cmdRuns lists runs, or prints one run's receipt (SPEC §8).
func cmdRuns(ctx context.Context, env Env, args []string) error {
	fs := flag.NewFlagSet("runs", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	limit := fs.Int("limit", 20, "how many runs to list")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) > 1 {
		return fail(ExitValidation, "usage: gtme runs [RUN_ID|last]")
	}

	l, err := openLedger(ctx)
	if err != nil {
		return err
	}
	defer l.Close()

	if len(positional) == 0 {
		runs, err := l.ListRuns(ctx, *limit)
		if err != nil {
			return fail(ExitOther, "%v", err)
		}
		if len(runs) == 0 {
			fmt.Fprintln(env.Stderr, "no runs yet")
			return nil
		}
		tw := tabwriter.NewWriter(env.Stderr, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "run\tpipeline\tstatus\tstarted\trecords")
		for _, run := range runs {
			records, err := l.RunRecords(ctx, run.ID)
			if err != nil {
				return fail(ExitOther, "%v", err)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n", run.ID, run.Pipeline, run.Status, run.StartedAt, len(records))
		}
		tw.Flush()
		return nil
	}

	target := positional[0]
	var run ledger.Run
	if target == "last" {
		run, err = l.LastRun(ctx)
	} else {
		run, err = l.GetRun(ctx, target)
	}
	if err != nil {
		if errors.Is(err, ledger.ErrNotFound) {
			return fail(ExitValidation, "unknown run %s", target)
		}
		return fail(ExitOther, "%v", err)
	}
	return printReceipt(ctx, env, l, run)
}

// printReceipt reconstructs a run's receipt from the ledger, so it can be read
// again long after the run finished.
func printReceipt(ctx context.Context, env Env, l *ledger.Ledger, run ledger.Run) error {
	fmt.Fprintf(env.Stderr, "run %s\npipeline: %s\nstatus:   %s\nstarted:  %s\n",
		run.ID, run.Pipeline, run.Status, run.StartedAt)
	if run.FinishedAt != "" {
		fmt.Fprintf(env.Stderr, "finished: %s\n", run.FinishedAt)
	}

	events, err := l.StepEventCounts(ctx, run.ID)
	if err != nil {
		return fail(ExitOther, "%v", err)
	}
	costs, err := l.CostsByStep(ctx, run.ID)
	if err != nil {
		return fail(ExitOther, "%v", err)
	}
	order, err := l.StepIDs(ctx, run.ID)
	if err != nil {
		return fail(ExitOther, "%v", err)
	}

	tw := tabwriter.NewWriter(env.Stderr, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "\nstep\tclaimed\tdone\tcached\tfailed\tcost")
	var total float64
	for _, step := range order {
		counts := events[step]
		cost := costs[step]
		total += cost
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", step,
			count(counts["claimed"]), count(counts["done"]),
			count(counts["skipped_cache"]), count(counts["failed"]), money(cost))
	}
	tw.Flush()
	fmt.Fprintf(env.Stderr, "total: %s\n", money(total))

	// The states show where records stopped, which is the useful thing when a run
	// did not finish cleanly.
	records, err := l.RunRecords(ctx, run.ID)
	if err != nil {
		return fail(ExitOther, "%v", err)
	}
	states := map[string]int{}
	failed := 0
	for _, rr := range records {
		states[rr.State]++
		if rr.AnyFailed() {
			failed++
		}
	}
	fmt.Fprintf(env.Stderr, "records: %d", len(records))
	if len(states) > 0 {
		parts := make([]string, 0, len(states))
		for state, n := range states {
			parts = append(parts, fmt.Sprintf("%s=%d", state, n))
		}
		fmt.Fprintf(env.Stderr, " (%s)", strings.Join(parts, " "))
	}
	if failed > 0 {
		// A fail verdict is a filter stop or a withheld send (SPEC §8, ADR-031);
		// telling them apart needs step roles, which a bare run id does not carry.
		fmt.Fprintf(env.Stderr, ", %d with a fail verdict (filtered, or a send withheld)", failed)
	}
	fmt.Fprintln(env.Stderr)

	// The pipeline that produced this run, so it can be re-run or frozen.
	var config map[string]any
	if json.Unmarshal([]byte(run.ConfigJSON), &config) == nil {
		if steps, ok := config["steps"].([]any); ok {
			fmt.Fprintf(env.Stderr, "config:  %d steps recorded (`gtme freeze %s` rebuilds the pipeline)\n",
				len(steps), run.ID)
		}
	}
	return nil
}

func count(n int) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprint(n)
}

func money(v float64) string {
	if v == 0 {
		return "$0"
	}
	return fmt.Sprintf("$%.4f", v)
}
