package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/elegant-atomics/gtme/internal/bundle"
	"github.com/elegant-atomics/gtme/internal/ledger"
	"github.com/elegant-atomics/gtme/internal/pipeline"
)

// cmdFreeze reconstructs the pipeline.yaml that produced a run, from the
// config snapshot CreateRun stored at the start of `gtme run` (SPEC §8, §1 bet
// 4) — a reproducibility and audit tool, not a mode-conversion one (that was
// pipe mode's job pre-ADR-005). The YAML goes to stdout so it can be
// redirected into a file.
func cmdFreeze(ctx context.Context, env Env, args []string) error {
	fs := flag.NewFlagSet("freeze", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	name := fs.String("name", "", "name for the frozen pipeline (default: frozen-<run id>)")
	bundleDir := fs.String("bundle", "", "assemble a campaign bundle into this directory instead of printing YAML (SPEC §8, ADR-029)")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) > 1 {
		return fail(ExitValidation, "usage: gtme freeze [RUN_ID|last] [--bundle DIR]")
	}
	target := "last"
	if len(positional) == 1 {
		target = positional[0]
	}

	l, err := openLedger(ctx)
	if err != nil {
		return err
	}
	defer l.Close()

	var run ledger.Run
	if target == "last" {
		run, err = l.LastRun(ctx)
	} else {
		run, err = l.GetRun(ctx, target)
	}
	if err != nil {
		if err == ledger.ErrNotFound {
			return fail(ExitValidation, "no run to freeze (%s)", target)
		}
		return fail(ExitOther, "%v", err)
	}

	p, err := frozenPipeline(run, *name)
	if err != nil {
		return err
	}

	// --bundle: the campaign bundle form (SPEC §8, ADR-029). Bare freeze keeps
	// its original job, YAML to stdout.
	if *bundleDir != "" {
		warnings, err := bundle.Write(*bundleDir, p, run.ID, Version, time.Now().UTC().Format(ledger.TimeFormat))
		if err != nil {
			return fail(ExitOther, "%v", err)
		}
		for _, w := range warnings {
			fmt.Fprintf(env.Stderr, "warning: %s\n", w)
		}
		fmt.Fprintf(env.Stderr, "froze run %s into bundle %s (%d steps) — self-contained except credentials and input files\n",
			run.ID, *bundleDir, len(p.AllSteps()))
		return nil
	}

	raw, err := pipeline.Marshal(p)
	if err != nil {
		return fail(ExitOther, "%v", err)
	}
	if _, err := env.Stdout.Write(raw); err != nil {
		return fail(ExitOther, "writing pipeline: %v", err)
	}
	fmt.Fprintf(env.Stderr, "froze run %s (%d steps)\n", run.ID, len(p.AllSteps()))
	return nil
}

// frozenPipeline decodes a run's config snapshot back into a pipeline. It is
// already in execution order: CreateRun stores the whole resolved Pipeline
// (source, steps, deliver) once, atomically, at the start of the run, in the
// order pipeline.yaml declared them — which is also the order they ran in
// (SPEC §9: "Steps execute strictly in order").
func frozenPipeline(run ledger.Run, name string) (*pipeline.Pipeline, error) {
	var p pipeline.Pipeline
	if run.ConfigJSON != "" {
		if err := json.Unmarshal([]byte(run.ConfigJSON), &p); err != nil {
			return nil, fail(ExitOther, "run %s: decoding config: %v", run.ID, err)
		}
	}
	if p.Source == nil {
		return nil, fail(ExitValidation,
			"run %s has no source step recorded, so there is nothing to freeze", run.ID)
	}

	p.Version = 1
	// --name wins; otherwise the pipeline keeps its own name (a bundle should
	// carry the campaign's identity, ADR-029), falling back to frozen-<id>
	// only for ad hoc runs that never had one.
	if name != "" {
		p.Name = name
	}
	if p.Name == "" || p.Name == ledger.AdhocPipeline {
		p.Name = "frozen-" + shortID(run.ID)
	}
	return &p, nil
}

func shortID(id string) string {
	if len(id) <= 8 {
		return strings.ToLower(id)
	}
	return strings.ToLower(id[len(id)-8:])
}
