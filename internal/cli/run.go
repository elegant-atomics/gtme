package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/bundle"
	"github.com/elegant-atomics/gtme/internal/httpx"
	"github.com/elegant-atomics/gtme/internal/ledger"
	"github.com/elegant-atomics/gtme/internal/pipeline"
	"github.com/elegant-atomics/gtme/internal/planner"
	"github.com/elegant-atomics/gtme/internal/runner"
)

// cmdRun executes a pipeline file.
func cmdRun(ctx context.Context, env Env, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	resume := fs.String("resume", "", "resume an existing run by id (or 'last')")
	concurrency := fs.Int("concurrency", 0, "worker pool size per step (default 4 or $GTME_CONCURRENCY)")
	dryRun := fs.Bool("dry-run", false, "hold deliver steps back: resolve and receipt their variables, send nothing (SPEC §8)")
	simulate := fs.Bool("simulate", false, "execute the whole pipeline offline from fixtures: no network, no spend, nothing sends, nothing persists (SPEC §8)")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fail(ExitValidation, "usage: gtme run pipeline.yaml [--resume RUN_ID] [--dry-run] [--simulate]")
	}
	if *simulate && *dryRun {
		return fail(ExitValidation, "--simulate already withholds delivery; drop --dry-run")
	}
	if *simulate && *resume != "" {
		return fail(ExitValidation, "--simulate runs are ephemeral and cannot be resumed")
	}

	// A bundle path is accepted wherever a pipeline path is (SPEC §8,
	// ADR-029): hashes verify, the pipeline loads from inside, and the
	// bundle's own bindings resolve first — nothing outside it except
	// credentials.
	var p *pipeline.Pipeline
	if bundle.IsBundle(positional[0]) {
		m, bp, err := bundle.Load(positional[0])
		if err != nil {
			return fail(ExitValidation, "%v", err)
		}
		p = bp
		adapters.BundleDir = bundle.AdaptersDir(positional[0])
		defer func() { adapters.BundleDir = "" }()
		fmt.Fprintf(env.Stderr, "bundle %s (frozen from run %s) — hashes verified\n", m.Name, m.SourceRunID)
	} else {
		loaded, err := pipeline.Load(positional[0])
		if err != nil {
			return fail(ExitValidation, "%v", err)
		}
		p = loaded
	}
	plan, err := planner.Build(p)
	if err != nil {
		// A simulated run touches no live API, so a missing credential must not
		// block it (SPEC §8: an agent validates offline before anyone sets keys);
		// every other plan problem still does.
		if !*simulate || !onlyCredentialProblems(err) {
			return planFailure(err)
		}
		fmt.Fprintf(env.Stderr, "simulate: ignoring missing credentials (%v)\n", err)
	}

	l, err := openLedger(ctx)
	if err != nil {
		return err
	}
	defer l.Close()

	// Group references resolve against the ledger before anything runs
	// (SPEC §7, ADR-021) — enforced under --simulate too: a missing group is
	// a contract error, not a credential.
	if len(plan.ReferencedGroups()) > 0 {
		if err := plan.CheckGroups(ctx, l); err != nil {
			return planFailure(err)
		}
	}

	if *simulate {
		// Ephemerality is the durability exclusion (SPEC §8): the simulated run
		// executes against a throwaway copy of the ledger, so its writes cannot
		// reach projection or cache and it never appears in `gtme runs`.
		tmp, cleanup, err := ephemeralLedger(ctx, l)
		if err != nil {
			return fail(ExitOther, "%v", err)
		}
		defer cleanup()
		l.Close()
		l = tmp
	}

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
		DryRun:      *dryRun,
		Simulate:    *simulate,
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
		return fail(ExitValidation, "usage: gtme plan pipeline.yaml")
	}

	p, err := pipeline.Load(positional[0])
	if err != nil {
		return fail(ExitValidation, "%v", err)
	}
	plan, err := planner.Build(p)
	if err != nil {
		return planFailure(err)
	}
	if err := checkGroups(ctx, plan); err != nil {
		return err
	}
	planner.Print(env.Stderr, plan)
	return nil
}

// checkGroups resolves a plan's group references against the ledger (SPEC §7,
// ADR-021) — opened only when the plan references any, so a group-free
// pipeline still plans without a ledger.
func checkGroups(ctx context.Context, plan *planner.Plan) error {
	if len(plan.ReferencedGroups()) == 0 {
		return nil
	}
	l, err := openLedger(ctx)
	if err != nil {
		return err
	}
	defer l.Close()
	if err := plan.CheckGroups(ctx, l); err != nil {
		return planFailure(err)
	}
	return nil
}

// onlyCredentialProblems reports whether every plan problem is a missing
// credential.
func onlyCredentialProblems(err error) bool {
	var pe *planner.Errors
	if !errors.As(err, &pe) || len(pe.Problems) == 0 {
		return false
	}
	for _, p := range pe.Problems {
		if p.Kind != planner.KindCredential {
			return false
		}
	}
	return true
}

// ephemeralLedger copies the ledger into a throwaway file (VACUUM INTO — a
// consistent snapshot even under WAL) and opens it. cleanup removes it.
func ephemeralLedger(ctx context.Context, l *ledger.Ledger) (*ledger.Ledger, func(), error) {
	f, err := os.CreateTemp("", "gtme-simulate-*.db")
	if err != nil {
		return nil, nil, err
	}
	path := f.Name()
	f.Close()
	os.Remove(path) // VACUUM INTO refuses an existing file
	if _, err := l.DB().ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return nil, nil, fmt.Errorf("copying ledger for simulation: %w", err)
	}
	tmp, err := ledger.Open(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		tmp.Close()
		os.Remove(path)
		os.Remove(path + "-wal")
		os.Remove(path + "-shm")
	}
	return tmp, cleanup, nil
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
