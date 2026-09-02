package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/elegant-atomics/gtme/internal/ledger"
	"github.com/elegant-atomics/gtme/internal/participant"
)

// cmdShow is the read-only projection inspector (SPEC §8, DECISIONS.md
// ADR-006). It never writes to the ledger, and — unlike every other verb here
// — its result is the point, so it goes to stdout as data rather than to
// stderr as a receipt: `gtme show` answers "what does the system know", the
// same job `gtme query` does for a whole segment.
func cmdShow(ctx context.Context, env Env, args []string) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	run := fs.String("run", "", "show a run's records instead of one identity (RUN_ID or 'last')")
	fields := fs.String("fields", "", "comma-separated field names to print (default: every known field)")
	provenance := fs.Bool("provenance", false, "include each field's source adapter, confidence and run")
	limit := fs.Int("limit", 0, "cap the number of records printed in --run mode (0 = all)")
	pending := fs.Bool("pending", false, "in --run mode, print the records awaiting a participant and the surface they are shown (ADR-049)")
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
		// --pending takes an optional step, the one positional show has.
		if *pending {
			if len(positional) > 1 {
				return fail(ExitValidation, "usage: gtme show --run RUN_ID|last --pending [STEP]")
			}
			step := ""
			if len(positional) == 1 {
				step = positional[0]
			}
			return showPending(ctx, env, l, *run, step)
		}
		if len(positional) != 0 {
			return fail(ExitValidation, "usage: gtme show --run RUN_ID|last [--fields a,b] [--provenance] [--limit N] [--pending [STEP]]")
		}
		return showRun(ctx, env, l, *run, only, *provenance, *limit)
	}
	if *pending {
		return fail(ExitValidation, "--pending reads one run's waiting records: `gtme show --run RUN_ID --pending [STEP]`")
	}
	if len(positional) != 1 {
		return fail(ExitValidation, "usage: gtme show <identity-key> [--fields a,b] [--provenance]")
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

	var notes map[string]string
	if provenance {
		if notes, err = l.AnswerNotes(ctx, ident.ID); err != nil {
			return fail(ExitOther, "%v", err)
		}
	}
	out := map[string]any{
		"entity_type":  ident.EntityType,
		"identity_key": ident.IdentityKey,
		"fields":       renderFieldsWithNotes(rec, provenance, notes),
	}
	// Deliveries with their status (SPEC §8, ADR-036): accepted is what the
	// provider took; confirmed/contradicted what a re-read said; sent only
	// when a provider attested execution, with its own timestamp.
	deliveries, err := l.Deliveries(ctx, ident.ID)
	if err != nil {
		return fail(ExitOther, "%v", err)
	}
	if len(deliveries) > 0 {
		list := make([]map[string]any, 0, len(deliveries))
		for _, d := range deliveries {
			entry := map[string]any{
				"target": d.Target, "status": d.Status, "run_id": d.RunID, "created_at": d.CreatedAt,
			}
			if d.Scope != "" {
				entry["scope"] = d.Scope
			}
			if d.SentAt != "" {
				entry["sent_at"] = d.SentAt
			}
			list = append(list, entry)
		}
		out["deliveries"] = list
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
		var notes map[string]string
		if provenance {
			if notes, err = l.AnswerNotes(ctx, ident.ID); err != nil {
				return fail(ExitOther, "%v", err)
			}
		}
		line := map[string]any{
			"entity_type":  ident.EntityType,
			"identity_key": ident.IdentityKey,
			"state":        rr.State,
			"fields":       renderFieldsWithNotes(rec, provenance, notes),
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
	return renderFieldsWithNotes(rec, provenance, nil)
}

// renderFieldsWithNotes is renderFields with the participant notes a run left
// (SPEC §8, ADR-049): a value written by a human/* or agent/* step carries the
// note its answer came with, and the referent it was about (ADR-048).
func renderFieldsWithNotes(rec ledger.Record, provenance bool, notes map[string]string) map[string]any {
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
		entry := map[string]any{
			"value":      v.Any(),
			"source":     v.Source,
			"confidence": v.Confidence,
			"run_id":     v.RunID,
			"created_at": v.CreatedAt.Format(ledger.TimeFormat),
		}
		if v.Referent != "" {
			entry["referent"] = v.Referent
		}
		if note := notes[v.RunID]; note != "" {
			entry["note"] = note
		}
		out[f] = entry
	}
	return out
}

// writeJSON writes one indented JSON object to stdout, for the single-record
// form of `gtme show` (SPEC §8's --run form is NDJSON instead, since it can be
// many records).
func writeJSON(env Env, v any) error {
	enc := json.NewEncoder(env.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fail(ExitOther, "writing record: %v", err)
	}
	return nil
}

// showPending prints the records awaiting a participant and the surface each
// is shown (SPEC §8, ADR-049) — what an agent reads before it answers. Like
// every other read verb the data goes to stdout as NDJSON and the readable
// form to stderr, so a person and a script get the same answer from one call.
func showPending(ctx context.Context, env Env, l *ledger.Ledger, target, step string) error {
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

	steps, _, err := l.PendingSteps(ctx, run.ID)
	if err != nil {
		return fail(ExitOther, "%v", err)
	}
	if step != "" {
		if !containsString(steps, step) {
			return fail(ExitValidation, "step %q has nothing pending in run %s", step, run.ID)
		}
		steps = []string{step}
	}
	if len(steps) == 0 {
		fmt.Fprintf(env.Stderr, "run %s: nothing pending\n", run.ID)
		return nil
	}

	enc := json.NewEncoder(env.Stdout)
	for _, id := range steps {
		st, err := participantStep(ctx, l, run, id)
		if err != nil {
			// A deferred adapter batch is pending too, and is not answered
			// here: name it and move on rather than failing the listing.
			fmt.Fprintf(env.Stderr, "%s: pending with its provider, not a participant — `gtme run` collects it\n", id)
			continue
		}
		contract, err := participant.ContractFor(st.Role, st.ProvidesSchema)
		if err != nil {
			return fail(ExitOther, "step %q: %v", id, err)
		}
		tokens, err := l.PendingTokens(ctx, run.ID, id)
		if err != nil {
			return fail(ExitOther, "%v", err)
		}
		records, byKey, err := pendingRecords(ctx, l, st, tokens)
		if err != nil {
			return err
		}
		surface := answerSurface(st)
		outputs := contract.Outputs()

		fmt.Fprintf(env.Stderr, "%s: %d awaiting %s — `gtme answer %s %s <identity-key> --set %s`\n",
			id, len(records), st.Manifest.ID, run.Pipeline, id, strings.Join(outputs, " --set "))
		for _, rec := range records {
			rendered := surface.Render(rec.Fields)
			fmt.Fprintf(env.Stderr, "\n  %s\n", rec.IdentityKey)
			for _, line := range strings.Split(strings.TrimRight(rendered, "\n"), "\n") {
				fmt.Fprintf(env.Stderr, "    %s\n", line)
			}
			if err := enc.Encode(map[string]any{
				"run_id":       run.ID,
				"step":         id,
				"adapter":      st.Manifest.ID,
				"role":         st.Role,
				"identity_key": rec.IdentityKey,
				"token":        tokens[byKey[rec.IdentityKey]],
				"surface":      rendered,
				"outputs":      outputs,
			}); err != nil {
				return fail(ExitOther, "writing pending record: %v", err)
			}
		}
	}
	return nil
}
