package cli

import (
	"context"
	"errors"
	"flag"

	"github.com/trevorfox/gtm/internal/httpx"
	"github.com/trevorfox/gtm/internal/ledger"
	"github.com/trevorfox/gtm/internal/pipeline"
	"github.com/trevorfox/gtm/internal/planner"
	"github.com/trevorfox/gtm/internal/runner"
)

// cmdRun executes a pipeline file.
func cmdRun(ctx context.Context, env Env, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	resume := fs.String("resume", "", "resume an existing run by id (or 'last')")
	concurrency := fs.Int("concurrency", 0, "worker pool size per step (default 4 or $GTM_CONCURRENCY)")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fail(ExitValidation, "usage: gtm run pipeline.yaml [--resume RUN_ID]")
	}

	p, err := pipeline.Load(positional[0])
	if err != nil {
		return fail(ExitValidation, "%v", err)
	}
	plan, err := planner.Build(p)
	if err != nil {
		return planFailure(err)
	}

	l, err := openLedger(ctx)
	if err != nil {
		return err
	}
	defer l.Close()

	runID, err := resolveRunID(ctx, l, *resume)
	if err != nil {
		return err
	}

	res, runErr := runner.Execute(ctx, runner.Options{
		Ledger:      l,
		Plan:        plan,
		Stderr:      env.Stderr,
		Concurrency: *concurrency,
		ResumeRunID: runID,
	})
	if res != nil {
		runner.PrintReceipt(env.Stderr, res)
	}
	if runErr != nil {
		// A provider that rejected our credentials or rate-limited us deserves its
		// own exit code, not a generic failure (SPEC §8).
		if code := httpx.ExitCodeFor(runErr); code != 0 {
			return exitError{code: code, err: runErr}
		}
		return fail(ExitOther, "%v", runErr)
	}
	return nil
}

// cmdPlan validates a pipeline and prints the resolved plan without executing it.
func cmdPlan(ctx context.Context, env Env, args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fail(ExitValidation, "usage: gtm plan pipeline.yaml")
	}

	p, err := pipeline.Load(positional[0])
	if err != nil {
		return fail(ExitValidation, "%v", err)
	}
	plan, err := planner.Build(p)
	if err != nil {
		return planFailure(err)
	}
	planner.Print(env.Stderr, plan)
	return nil
}

// planFailure maps plan problems onto exit codes (SPEC §8): contract and config
// problems are validation errors, missing credentials are auth errors.
func planFailure(err error) error {
	var pe *planner.Errors
	if errors.As(err, &pe) {
		return exitError{code: pe.ExitCode(), err: err}
	}
	return exitError{code: ExitValidation, err: err}
}

// resolveRunID turns a --resume value into a run id.
func resolveRunID(ctx context.Context, l *ledger.Ledger, resume string) (string, error) {
	switch resume {
	case "":
		return "", nil
	case "last":
		run, err := l.LastRun(ctx)
		if err != nil {
			if errors.Is(err, ledger.ErrNotFound) {
				return "", fail(ExitValidation, "no runs to resume")
			}
			return "", fail(ExitOther, "%v", err)
		}
		return run.ID, nil
	default:
		if _, err := l.GetRun(ctx, resume); err != nil {
			if errors.Is(err, ledger.ErrNotFound) {
				return "", fail(ExitValidation, "unknown run %s", resume)
			}
			return "", fail(ExitOther, "%v", err)
		}
		return resume, nil
	}
}

// openLedger opens the ledger, telling the operator to run init when it is
// missing rather than silently creating a half-configured home.
func openLedger(ctx context.Context) (*ledger.Ledger, error) {
	l, err := ledger.Open(ctx, "")
	if err != nil {
		return nil, fail(ExitOther, "%v", err)
	}
	return l, nil
}
