package cli

// cmdAnswer is the participant write path (SPEC §8 "People and agents
// answer", ADR-049): one verb, for every pending human/* or agent/* step in
// every role. It records an `answered` step event and nothing else — it never
// sends, never opens an adapter session, and never appears in `gtme freeze`
// output. The next `gtme run` collects what it wrote.
//
// The step's contract comes from the run itself: `runs.config_json` holds the
// resolved pipeline (the same snapshot `gtme freeze` reads), so the answer is
// validated against exactly the declaration that pended the record, whatever
// the pipeline file says now.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/elegant-atomics/gtme/internal/ledger"
	"github.com/elegant-atomics/gtme/internal/participant"
	"github.com/elegant-atomics/gtme/internal/pipeline"
	"github.com/elegant-atomics/gtme/internal/planner"
)

const answerUsage = "usage: gtme answer [RUN_ID|last|PIPELINE] [STEP] [IDENTITY_KEY] [--set field=value ...] [--as NAME] [--cost USD [--measured]] [--note TEXT]"

// setFlag collects repeated --set field=value pairs in declaration order.
type setFlag []string

func (s *setFlag) String() string { return strings.Join(*s, ",") }

func (s *setFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func cmdAnswer(ctx context.Context, env Env, args []string) error {
	fs := flag.NewFlagSet("answer", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	var set setFlag
	fs.Var(&set, "set", "field=value the participant answers (repeatable)")
	as := fs.String("as", "", "the participant answering (default: the OS user)")
	cost := fs.String("cost", "", "USD the participant spent on this record (estimated unless --measured)")
	measured := fs.Bool("measured", false, "the --cost figure is vendor-reported, not a rate estimate (ADR-046)")
	note := fs.String("note", "", "free text kept with the answer and shown by `gtme show --provenance`")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) > 3 {
		return fail(ExitValidation, "%s", answerUsage)
	}
	if *measured && *cost == "" {
		return fail(ExitValidation, "--measured says how to read --cost, so it needs one (ADR-046)")
	}
	var spend *float64
	if *cost != "" {
		amount, err := strconv.ParseFloat(*cost, 64)
		if err != nil || amount < 0 {
			return fail(ExitValidation, "--cost takes a non-negative amount in USD, got %q", *cost)
		}
		spend = &amount
	}

	l, err := openLedger(ctx)
	if err != nil {
		return err
	}
	defer l.Close()

	target := "last"
	if len(positional) > 0 {
		target = positional[0]
	}
	run, err := resolveAnswerRun(ctx, l, target)
	if err != nil {
		return err
	}

	// STEP and IDENTITY_KEY are both optional and both bare words, so the
	// pending steps disambiguate them: a lone positional that names a
	// pending step is the step, otherwise it is the record.
	steps, counts, err := l.PendingSteps(ctx, run.ID)
	if err != nil {
		return fail(ExitOther, "%v", err)
	}
	if len(steps) == 0 {
		return fail(ExitValidation, "run %s has nothing pending — `gtme answer` records answers for records awaiting a human/* or agent/* step", run.ID)
	}
	stepID, key := "", ""
	switch rest := positional[min(len(positional), 1):]; len(rest) {
	case 0:
	case 1:
		if containsString(steps, rest[0]) {
			stepID = rest[0]
		} else {
			key = rest[0]
		}
	default:
		stepID, key = rest[0], rest[1]
	}
	if stepID == "" {
		if len(steps) > 1 {
			return fail(ExitValidation,
				"run %s has %d steps pending (%s) — name the one to answer: `gtme answer %s <step> ...`",
				run.ID, len(steps), strings.Join(steps, ", "), target)
		}
		stepID = steps[0]
	} else if !containsString(steps, stepID) {
		return fail(ExitValidation, "step %q has nothing pending in run %s (pending: %s)",
			stepID, run.ID, strings.Join(steps, ", "))
	}

	st, err := participantStep(ctx, l, run, stepID)
	if err != nil {
		return err
	}
	contract, err := participant.ContractFor(st.Role, st.ProvidesSchema)
	if err != nil {
		return fail(ExitOther, "step %q: %v", stepID, err)
	}
	pending, err := l.PendingTokens(ctx, run.ID, stepID)
	if err != nil {
		return fail(ExitOther, "%v", err)
	}

	name := *as
	if name == "" {
		name = participant.DefaultName()
	}
	who := participant.Qualify(st.Manifest.ID, name)

	if key == "" {
		if len(set) > 0 {
			return fail(ExitValidation,
				"--set answers one record, so it needs the record: `gtme answer %s %s <identity-key> --set ...` (`gtme show --run %s --pending %s` lists the %d waiting)",
				target, stepID, run.ID, stepID, counts[stepID])
		}
		return walkPending(ctx, env, l, run, st, contract, pending, who, spend, *measured, *note)
	}
	if len(set) == 0 {
		return fail(ExitValidation, "answering %s needs --set: %s", key, strings.Join(contract.Outputs(), ", "))
	}

	ident, err := l.FindByKey(ctx, key)
	if err != nil {
		if errors.Is(err, ledger.ErrNotFound) {
			return fail(ExitValidation, "no identity known by key %q", key)
		}
		return fail(ExitOther, "%v", err)
	}
	token, ok := pending[ident.ID]
	if !ok {
		return fail(ExitValidation, "%s is not pending under step %q in run %s — nothing to answer", key, stepID, run.ID)
	}

	pairs, err := parseSet(set)
	if err != nil {
		return err
	}
	answer, err := contract.Parse(pairs)
	if err != nil {
		return fail(ExitValidation, "%v", err)
	}
	if err := l.RecordAnswer(ctx, run.ID, stepID, ident.ID, ledger.Answer{
		Fields: answer.Wire(st.Role), Participant: who, Note: *note,
		Cost: spend, Measured: *measured, Token: token,
	}); err != nil {
		return fail(ExitOther, "%v", err)
	}
	fmt.Fprintf(env.Stderr, "%s: %s answered by %s — the next `gtme run %s` collects it\n",
		stepID, key, who, run.Pipeline)
	return nil
}

// resolveAnswerRun turns the run argument into a run: a RUN_ID, `last`, or a
// pipeline name or path — for a pipeline, the most recent pending run of it,
// which is the lookup collect-first already makes (SPEC §8, ADR-038).
func resolveAnswerRun(ctx context.Context, l *ledger.Ledger, target string) (ledger.Run, error) {
	if target == "last" {
		run, err := l.LastRun(ctx)
		if err != nil {
			return ledger.Run{}, fail(ExitValidation, "no runs recorded yet")
		}
		return run, nil
	}
	if run, err := l.GetRun(ctx, target); err == nil {
		return run, nil
	} else if !errors.Is(err, ledger.ErrNotFound) {
		return ledger.Run{}, fail(ExitOther, "%v", err)
	}

	// Not a run id: a pipeline name, or a path to load one from.
	name := target
	if p, err := pipeline.Load(target); err == nil {
		name = p.Name
	}
	run, err := l.LastRunForPipeline(ctx, name)
	if err != nil {
		return ledger.Run{}, fail(ExitValidation,
			"no run found for %q — `gtme answer` takes a RUN_ID, `last`, or a pipeline name or path", target)
	}
	return run, nil
}

// participantStep re-plans the run's frozen pipeline and returns the named
// step, refusing anything that is not a participant a person or agent
// answers.
func participantStep(ctx context.Context, l *ledger.Ledger, run ledger.Run, stepID string) (*planner.Step, error) {
	var p pipeline.Pipeline
	if run.ConfigJSON == "" {
		return nil, fail(ExitOther, "run %s recorded no pipeline, so its steps cannot be read back", run.ID)
	}
	if err := json.Unmarshal([]byte(run.ConfigJSON), &p); err != nil {
		return nil, fail(ExitOther, "run %s: decoding config: %v", run.ID, err)
	}
	p.Version = 1
	plan, err := planner.Build(ctx, &p, l)
	if err != nil {
		return nil, fail(ExitOther, "run %s: %v", run.ID, err)
	}
	for i := range plan.Steps {
		st := &plan.Steps[i]
		if st.ID != stepID {
			continue
		}
		if st.Manifest == nil || !st.RunnerOwned() {
			return nil, fail(ExitValidation,
				"step %q is not a human/* or agent/* step — `gtme answer` records a participant's judgment, not an adapter's", stepID)
		}
		return st, nil
	}
	return nil, fail(ExitValidation, "run %s has no step %q", run.ID, stepID)
}

// walkPending is `gtme answer` with no identity key: the same interactive
// walk the in-run one uses, over every record still pending (SPEC §8).
func walkPending(ctx context.Context, env Env, l *ledger.Ledger, run ledger.Run, st *planner.Step,
	contract participant.Contract, pending map[string]string, who string, cost *float64, measured bool, note string) error {
	if !stdinIsTerminal(env) {
		return fail(ExitValidation,
			"no identity key and no terminal — name the record and its answer (`gtme answer %s %s <identity-key> --set ...`), or read what waits with `gtme show --run %s --pending %s`",
			run.Pipeline, st.ID, run.ID, st.ID)
	}

	records, byKey, err := pendingRecords(ctx, l, st, pending)
	if err != nil {
		return err
	}
	w := &participant.Walker{In: env.Stdin, Out: env.Stderr, Contract: contract,
		Surface: answerSurface(st), StepID: st.ID, Adapter: st.Manifest.ID}
	n, walkErr := w.Walk(ctx, records, func(p participant.Pending, a participant.Answer) error {
		id := byKey[p.IdentityKey]
		return l.RecordAnswer(ctx, run.ID, st.ID, id, ledger.Answer{
			Fields: a.Wire(st.Role), Participant: who, Note: note,
			Cost: cost, Measured: measured, Token: pending[id],
		})
	})
	if walkErr != nil && !errors.Is(walkErr, participant.ErrInterrupted) {
		return fail(ExitOther, "%v", walkErr)
	}
	left := len(records) - n
	fmt.Fprintf(env.Stderr, "%s: %d answered by %s", st.ID, n, who)
	if left > 0 {
		fmt.Fprintf(env.Stderr, ", %d still pending", left)
	}
	fmt.Fprintf(env.Stderr, " — the next `gtme run %s` collects\n", run.Pipeline)
	return nil
}

// pendingRecords loads the pending identities with their projections, in a
// stable order, for the walk and for `gtme show --pending`.
func pendingRecords(ctx context.Context, l *ledger.Ledger, st *planner.Step, pending map[string]string) ([]participant.Pending, map[string]string, error) {
	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	records := make([]participant.Pending, 0, len(ids))
	byKey := make(map[string]string, len(ids))
	for _, id := range ids {
		ident, err := l.IdentityByID(ctx, id)
		if err != nil {
			return nil, nil, fail(ExitOther, "%v", err)
		}
		rec, err := l.Project(ctx, id, ledger.Projection{})
		if err != nil {
			return nil, nil, fail(ExitOther, "%v", err)
		}
		fields := make(map[string]any, len(rec.Values))
		for name, v := range rec.Values {
			fields[name] = v.Any()
		}
		records = append(records, participant.Pending{IdentityKey: ident.IdentityKey, Fields: fields})
		byKey[ident.IdentityKey] = id
	}
	return records, byKey, nil
}

// answerSurface is what a participant is shown for this step — the same
// surface the in-run walk renders (SPEC §9's render:, else uses:, else of:).
func answerSurface(st *planner.Step) participant.Surface {
	s := participant.Surface{Fields: st.RenderFields, Template: st.RenderTemplate, Of: st.Of}
	if !st.NeedsAll {
		s.Uses = st.Needs
	}
	return s
}

// parseSet splits --set field=value pairs, refusing a pair with no `=` rather
// than guessing at the operator's intent.
func parseSet(set []string) (map[string]string, error) {
	out := make(map[string]string, len(set))
	for _, pair := range set {
		name, value, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fail(ExitValidation, "--set takes field=value, got %q", pair)
		}
		out[strings.TrimSpace(name)] = value
	}
	return out, nil
}

// stdinIsTerminal reports whether the walk has someone to ask.
func stdinIsTerminal(env Env) bool {
	f, ok := env.Stdin.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// containsString is the small membership test the CLI needs in a few places.
func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
