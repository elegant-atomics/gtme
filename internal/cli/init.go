package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/trevorfox/gtm/internal/ledger"
)

// cmdInit creates ~/.gtm and the ledger, applying migrations. It is safe to run
// repeatedly.
func cmdInit(ctx context.Context, env Env, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	path := fs.String("ledger", "", "ledger path (default $GTM_LEDGER or ~/.gtm/ledger.db)")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) > 0 {
		return fail(ExitValidation, "init takes no arguments")
	}

	home, err := ledger.Home()
	if err != nil {
		return fail(ExitOther, "%v", err)
	}
	for _, dir := range []string{home, filepath.Join(home, "adapters")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fail(ExitOther, "creating %s: %v", dir, err)
		}
	}

	dbPath := *path
	if dbPath == "" {
		if dbPath, err = ledger.DefaultPath(); err != nil {
			return fail(ExitOther, "%v", err)
		}
	}
	existed := false
	if _, err := os.Stat(dbPath); err == nil {
		existed = true
	}

	l, err := ledger.Open(ctx, dbPath)
	if err != nil {
		return fail(ExitOther, "%v", err)
	}
	defer l.Close()

	verb := "created"
	if existed {
		verb = "up to date"
	}
	fmt.Fprintf(env.Stderr, "ledger %s: %s\n", verb, l.Path())
	fmt.Fprintf(env.Stderr, "gtm home: %s\n", home)
	return nil
}
