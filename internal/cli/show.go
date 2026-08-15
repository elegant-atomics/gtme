package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/trevorfox/gtm/internal/ledger"
)

// cmdShow is the read-only projection inspector (SPEC §8, DECISIONS.md
// ADR-006). It never writes to the ledger, and — unlike every other verb here
// — its result is the point, so it goes to stdout as data rather than to
// stderr as a receipt: `gtm show` answers "what does the system know", the
// same job `gtm query` does for a whole segment.
func cmdShow(ctx context.Context, env Env, args []string) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	run := fs.String("run", "", "show a run's records instead of one identity (RUN_ID or 'last')")
	fields := fs.String("fields", "", "comma-separated field names to print (default: every known field)")
	provenance := fs.Bool("provenance", false, "include each field's source adapter, confidence and run")
	limit := fs.Int("limit", 0, "cap the number of records printed in --run mode (0 = all)")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}

	var only []string
	if *fields != "" {
		for _, f := range strings.Split(*fields, ",") {
			if f = strings.TrimSpace(f); f != "" {
				only = append(only, f)
			}
		}
	}

	l, err := openLedger(ctx)
	if err != nil {
		return err
	}
	defer l.Close()

	if *run != "" {
		if len(positional) != 0 {
			return fail(ExitValidation, "usage: gtm show --run RUN_ID|last [--fields a,b] [--provenance] [--limit N]")
		}
		return showRun(ctx, env, l, *run, only, *provenance, *limit)
	}
	if len(positional) != 1 {
		return fail(ExitValidation, "usage: gtm show <identity-key> [--fields a,b] [--provenance]")
	}
	return showIdentity(ctx, env, l, positional[0], only, *provenance)
}

// showIdentity prints one identity's current-value projection.
func showIdentity(ctx context.Context, env Env, l *ledger.Ledger, key string, only []string, provenance bool) error {
	ident, err := l.FindByKey(ctx, key)
	if err != nil {
		if errors.Is(err, ledger.ErrNotFound) {
			return fail(ExitValidation, "no identity known by key %q", key)
		}
		return fail(ExitOther, "%v", err)
	}
	rec, err := l.Project(ctx, ident.ID, ledger.Projection{Fields: only})
	if err != nil {
		return fail(ExitOther, "%v", err)
	}

	out := map[string]any{
		"entity_type":  ident.EntityType,
		"identity_key": ident.IdentityKey,
		"fields":       renderFields(rec, provenance),
	}
	return writeJSON(env, out)
}

// showRun prints the records a run touched, NDJSON — one line per record, the
// same projection shape as showIdentity so the two modes are consistent.
func showRun(ctx context.Context, env Env, l *ledger.Ledger, target string, only []string, provenance bool, limit int) error {
	var run ledger.Run
	var err error
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

	records, err := l.RunRecords(ctx, run.ID)
	if err != nil {
		return fail(ExitOther, "%v", err)
	}

	enc := json.NewEncoder(env.Stdout)
	n := 0
	for _, rr := range records {
		if limit > 0 && n >= limit {
			break
		}
		ident, err := l.IdentityByID(ctx, rr.IdentityID)
		if err != nil {
			return fail(ExitOther, "%v", err)
		}
		rec, err := l.Project(ctx, ident.ID, ledger.Projection{Fields: only})
		if err != nil {
			return fail(ExitOther, "%v", err)
		}
		line := map[string]any{
			"entity_type":  ident.EntityType,
			"identity_key": ident.IdentityKey,
			"state":        rr.State,
			"fields":       renderFields(rec, provenance),
		}
		if err := enc.Encode(line); err != nil {
			return fail(ExitOther, "writing record: %v", err)
		}
		n++
	}
	fmt.Fprintf(env.Stderr, "%d record(s) from run %s\n", n, run.ID)
	return nil
}

// renderFields turns a projection into the JSON-ready shape show prints: a
// bare value normally, or a {value, source, confidence, run_id, created_at}
// object with --provenance.
func renderFields(rec ledger.Record, provenance bool) map[string]any {
	names := make([]string, 0, len(rec.Values))
	for f := range rec.Values {
		names = append(names, f)
	}
	sort.Strings(names)

	out := make(map[string]any, len(names))
	for _, f := range names {
		v := rec.Values[f]
		if !provenance {
			out[f] = v.Any()
			continue
		}
		out[f] = map[string]any{
			"value":      v.Any(),
			"source":     v.Source,
			"confidence": v.Confidence,
			"run_id":     v.RunID,
			"created_at": v.CreatedAt.Format(ledger.TimeFormat),
		}
	}
	return out
}

// writeJSON writes one indented JSON object to stdout, for the single-record
// form of `gtm show` (SPEC §8's --run form is NDJSON instead, since it can be
// many records).
func writeJSON(env Env, v any) error {
	enc := json.NewEncoder(env.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fail(ExitOther, "writing record: %v", err)
	}
	return nil
}
