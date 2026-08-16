// Package cli dispatches the gtm command line. stdout is data (NDJSON); every
// human-facing byte goes to stderr (SPEC §8).
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	// Built-in adapters register themselves; the CLI is what needs them present.
	_ "github.com/trevorfox/gtm/internal/adapters/all"
)

// Exit codes (SPEC §8).
const (
	ExitOK         = 0
	ExitOther      = 1
	ExitValidation = 2
	ExitAuth       = 3
	ExitRateLimit  = 4
	ExitNetwork    = 5
)

// Env carries the process environment a command runs in, so tests can drive the
// CLI without touching the real stdio.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Args   []string // arguments after the program name
}

// DefaultEnv wires Env to the real process.
func DefaultEnv() Env {
	return Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Args: os.Args[1:]}
}

// exitError carries an exit code out of a command.
type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string { return e.err.Error() }
func (e exitError) Unwrap() error { return e.err }

func fail(code int, format string, args ...any) error {
	return exitError{code: code, err: fmt.Errorf(format, args...)}
}

// parseFlags parses args allowing flags to appear before or after positional
// arguments — `gtm enrich harvest/profile --cache 90d` reads naturally, and Go's
// flag package stops at the first positional on its own.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	if err := fs.Parse(args); err != nil {
		return nil, exitError{code: ExitValidation, err: err}
	}
	for fs.NArg() > 0 {
		positional = append(positional, fs.Arg(0))
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return nil, exitError{code: ExitValidation, err: err}
		}
	}
	return positional, nil
}

// Run executes one command and returns its exit code.
func Run(ctx context.Context, env Env) int {
	if len(env.Args) == 0 {
		usage(env.Stderr)
		return ExitValidation
	}

	verb, rest := env.Args[0], env.Args[1:]
	var err error
	switch verb {
	case "init":
		err = cmdInit(ctx, env, rest)
	case "version", "--version", "-v":
		fmt.Fprintln(env.Stderr, "gtm "+Version)
	case "help", "--help", "-h":
		if len(rest) == 1 && rest[0] == "--agent" {
			err = cmdHelpAgent(env)
		} else {
			usage(env.Stderr)
		}
	case "plan":
		err = cmdPlan(ctx, env, rest)
	case "run":
		err = cmdRun(ctx, env, rest)
	case "show":
		err = cmdShow(ctx, env, rest)
	case "freeze":
		err = cmdFreeze(ctx, env, rest)
	case "query":
		err = cmdQuery(ctx, env, rest)
	case "runs":
		err = cmdRuns(ctx, env, rest)
	case "secret":
		err = cmdSecret(ctx, env, rest)
	case "groups":
		err = cmdGroups(ctx, env, rest)
	default:
		fmt.Fprintf(env.Stderr, "gtm: unknown command %q\n\n", verb)
		usage(env.Stderr)
		return ExitValidation
	}

	if err != nil {
		var ee exitError
		if errors.As(err, &ee) {
			fmt.Fprintln(env.Stderr, "gtm: "+ee.Error())
			return ee.code
		}
		fmt.Fprintln(env.Stderr, "gtm: "+err.Error())
		return ExitOther
	}
	return ExitOK
}

// Version is the binary version, overridable at link time.
var Version = "0.0.0-dev"

func usage(w io.Writer) {
	fmt.Fprint(w, `gtm — a CLI for GTM data pipelines

Usage:
  gtm init                          create ~/.gtm and the ledger
  gtm plan pipeline.yaml            validate + print a plan, no execution
  gtm run  pipeline.yaml [--resume RUN_ID]
  gtm query "SQL"                   read-only SQL against the ledger
  gtm query --save NAME "SQL"       save a segment
  gtm show <identity-key>           print what the ledger knows about a record
  gtm show --run last               list a run's records
  gtm runs [RUN_ID|last]            list runs / show one run's receipt
  gtm freeze [RUN_ID|last]          rebuild a pipeline.yaml from a run
  gtm freeze [RUN_ID|last] --bundle DIR   assemble a portable campaign bundle
  gtm secret set KEY [VALUE]        store a credential in ~/.gtm/secrets
  gtm groups                        list groups with their derived character
  gtm groups show NAME              members and recent events
  gtm groups add NAME KEY... [--from-segment NAME | --query "SQL"]
  gtm groups remove NAME KEY...
  gtm help --agent                  machine-readable CLI + adapter surface
  gtm version

This is the entire v0 verb set (SPEC.md §8, ADR-005). uses:, cache:, when:
and every other per-step option are pipeline.yaml config, never flags.

Environment:
  GTM_LEDGER      ledger path (default ~/.gtm/ledger.db)
  GTM_CONCURRENCY worker pool size per step (default 4)
`)
}
