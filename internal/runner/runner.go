// Package runner executes a plan: it projects records out of the ledger, feeds
// them to adapters over the wire protocol, validates what comes back, and writes
// it to the ledger (SPEC §7).
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"strings"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/ai"
	"github.com/elegant-atomics/gtme/internal/binding"
	"github.com/elegant-atomics/gtme/internal/identity"
	"github.com/elegant-atomics/gtme/internal/ledger"
	"github.com/elegant-atomics/gtme/internal/planner"
	"github.com/elegant-atomics/gtme/internal/protocol"
	"github.com/elegant-atomics/gtme/internal/registry"
)

// DefaultConcurrency is the per-step worker pool size (SPEC §9).
const DefaultConcurrency = 4

// MaxChunk bounds how many records one adapter session handles for non-batch
// steps, so progress and memory stay bounded on large runs.
const MaxChunk = 64

// Options configures one execution.
type Options struct {
	Ledger      *ledger.Ledger
	Plan        *planner.Plan
	Stderr      io.Writer
	Concurrency int
	// ResumeRunID continues an existing run instead of minting one.
	ResumeRunID string
	// DryRun holds deliver steps back (SPEC §8, ADR-019): variables are
	// resolved and receipted per record, but no deliver adapter is invoked and
	// no deliveries row is written. Every other step runs normally.
	DryRun bool
	// Simulate executes the whole pipeline offline (SPEC §8, ADR-028):
	// bindings serve their conformance fixtures, AI steps run on the fixture
	// engine, credentialed process adapters are stubbed (a visible simulation
	// gap), and deliver steps behave as under DryRun. The caller is expected
	// to hand in an ephemeral ledger — nothing a simulated run writes may
	// reach the durable identity layer.
	Simulate bool
}

// StepStat is one step's contribution to the receipt.
type StepStat struct {
	ID             string
	Use            string
	Role           string
	In             int // records dispatched to the adapter
	Out            int // records that advanced
	CacheSkips     int
	Filtered       int // failed a filter verdict
	Failed         int
	Gated          int  // excluded by when:
	Skipped        int  // deliver records held back by on_missing (SPEC §8)
	SimGap         bool // stubbed under --simulate: no fixtures to serve (SPEC §8)
	SimGapRecords  int  // records that passed through the gap untouched
	CostUSD        float64
	AvoidedUSD     float64
	AvoidedUnknown bool

	// MissingSkips lists every record on_missing held back, with its reason —
	// the receipt shows them (SPEC §8).
	MissingSkips []RecordVariables
	// Suppressed lists every record the suppression window held back
	// (SPEC §8, ADR-021).
	Suppressed []SuppressedRecord
	// DryRun lists each record's RESOLVED variables when the run was dry —
	// the approval artifact a human reviews before arming (SPEC §8).
	DryRun []RecordVariables
}

// SuppressedRecord is one record a suppression window held back (SPEC §8).
type SuppressedRecord struct {
	IdentityKey string
	Group       string
	Age         string
}

// RecordVariables is one record's resolved (or unresolvable) deliver variables.
type RecordVariables struct {
	IdentityKey string
	Resolved    map[string]string // target merge-field name → resolved value
	Missing     []string          // ledger fields with no non-empty value
}

// Result is the outcome of a run.
type Result struct {
	RunID     string
	Status    string
	DryRun    bool
	Simulated bool
	Steps     []StepStat

	// TerminusGroup/TerminusAdded report the membership terminus (SPEC §8);
	// TerminusWould counts what a dry/simulated run held back.
	TerminusGroup string
	TerminusAdded int
	TerminusWould int
}

// Concurrency resolves the worker pool size: the option, else GTME_CONCURRENCY,
// else the default.
func Concurrency(opt int) int {
	if opt > 0 {
		return opt
	}
	if v := os.Getenv("GTME_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return DefaultConcurrency
}

type runner struct {
	l        *ledger.Ledger
	plan     *planner.Plan
	stderr   io.Writer
	conc     int
	runID    string
	dry      bool
	simulate bool
	// aiFixture is the synthesized $auto script injected into AI steps under
	// --simulate when the operator has no recorded one (SPEC §8).
	aiFixture string
	reg       *registry.Registry
	// deliverSteps holds the plan's deliver step ids: a fail verdict at one of
	// these records a withheld send, not a stopped record (SPEC §8, ADR-031).
	deliverSteps map[string]bool
	now          func() time.Time
	// out is the downstream NDJSON stream in pipe mode, nil for `gtme run`.
	out *protocol.Writer

	mu    sync.Mutex
	stats map[string]*StepStat
	order []string

	// Terminus outcome (SPEC §8, ADR-021): the group completers were added to,
	// how many were, or how many WOULD have been on a dry/simulated run.
	terminusGroup string
	terminusAdded int
	terminusWould int
}

// Execute runs a plan to completion. A fatal adapter error fails the run but
// keeps everything already written — the ledger is append-only and the run is
// resumable (SPEC §5).
func Execute(ctx context.Context, o Options) (*Result, error) {
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	reg, err := registry.Load()
	if err != nil {
		return nil, fmt.Errorf("runner: %w", err)
	}
	r := &runner{
		l:            o.Ledger,
		plan:         o.Plan,
		stderr:       o.Stderr,
		conc:         Concurrency(o.Concurrency),
		dry:          o.DryRun || o.Simulate,
		simulate:     o.Simulate,
		reg:          reg,
		deliverSteps: map[string]bool{},
		now:          time.Now,
		stats:        map[string]*StepStat{},
	}
	for i := range o.Plan.Steps {
		if o.Plan.Steps[i].IsDeliver {
			r.deliverSteps[o.Plan.Steps[i].ID] = true
		}
	}
	switch {
	case r.simulate:
		fmt.Fprintln(r.stderr, "simulate: fixtures only — no network, no spend, nothing sends, nothing persists")
		path, err := writeAutoFixture()
		if err != nil {
			return nil, fmt.Errorf("runner: %w", err)
		}
		r.aiFixture = path
		defer os.Remove(path)
	case r.dry:
		fmt.Fprintln(r.stderr, "dry run: deliver steps will resolve and receipt their variables, but nothing sends")
	}

	if o.ResumeRunID != "" {
		run, err := r.l.GetRun(ctx, o.ResumeRunID)
		if err != nil {
			return nil, fmt.Errorf("runner: run %s: %w", o.ResumeRunID, err)
		}
		r.runID = run.ID
		if err := r.l.ReopenRun(ctx, run.ID); err != nil {
			return nil, err
		}
		if run.Pipeline != o.Plan.Pipeline.Name {
			// Resuming with a different pipeline is allowed — the run's membership and
			// per-step state are what matter — but it is worth saying out loud.
			fmt.Fprintf(r.stderr, "warning: run %s was started by pipeline %q, resuming it as %q\n",
				run.ID, run.Pipeline, o.Plan.Pipeline.Name)
		}
		fmt.Fprintf(r.stderr, "resuming run %s (%s)\n", run.ID, run.Pipeline)
	} else {
		run, err := r.l.CreateRun(ctx, o.Plan.Pipeline.Name, o.Plan.Pipeline)
		if err != nil {
			return nil, err
		}
		r.runID = run.ID
		fmt.Fprintf(r.stderr, "run %s (%s)\n", run.ID, run.Pipeline)
	}

	// Opportunistic payload eviction (SPEC §8, ADR-030): every armed run keeps
	// the cache tier bounded without a daemon. Simulated runs skip it — their
	// ledger is a throwaway copy.
	if !r.simulate {
		if n, err := r.l.PurgeExpiredPayloads(ctx); err == nil && n > 0 {
			fmt.Fprintf(r.stderr, "evicted %d expired payload(s) (ADR-030)\n", n)
		}
	}

	runErr := r.execute(ctx)

	status := ledger.StatusDone
	if runErr != nil {
		status = ledger.StatusFailed
	}
	if err := r.l.FinishRun(ctx, r.runID, status); err != nil && runErr == nil {
		runErr = err
	}

	return &Result{RunID: r.runID, Status: status, DryRun: r.dry && !r.simulate,
		Simulated: r.simulate, Steps: r.collect(),
		TerminusGroup: r.terminusGroup, TerminusAdded: r.terminusAdded,
		TerminusWould: r.terminusWould}, runErr
}

// writeAutoFixture synthesizes the fixture-engine script simulated AI steps
// fall back to when the operator recorded none: every batch gets a valid,
// visibly synthetic answer (SPEC §8; the fixture engine marks its output with
// model "fixture", which the ai/* provenance carries).
func writeAutoFixture() (string, error) {
	f, err := os.CreateTemp("", "gtme-simulate-ai-*.json")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(`["$auto"]`); err != nil {
		f.Close()
		return "", err
	}
	return f.Name(), f.Close()
}

// isAIStep reports an operation-named AI step (ADR-026).
func isAIStep(st *planner.Step) bool {
	return st.Manifest != nil && st.Manifest.IsAI()
}

// stubbed reports whether a step is served nothing under --simulate: a binding
// without fixtures, or a credentialed process adapter (network by declaration)
// that is not an AI step. Stubbed steps are the simulation gaps the receipt
// must surface (SPEC §8).
func (r *runner) stubbed(st *planner.Step) bool {
	if !r.simulate || isAIStep(st) || st.IsDeliver || st.IsGroupSource || st.IsSQL {
		return false
	}
	if st.Adapter != nil && st.Adapter.Binding {
		return !st.Adapter.HasFixtures
	}
	if st.Manifest != nil && st.Manifest.ID == binding.HTTPEnrichID {
		// Live fetching only; replaying retained payloads is the ROADMAP
		// simulate-replay verb (SPEC §10a).
		return true
	}
	return st.Manifest != nil && len(st.Manifest.Credentials) > 0
}

func (r *runner) execute(ctx context.Context) error {
	if err := r.runSource(ctx); err != nil {
		return err
	}
	for i := 1; i < len(r.plan.Steps); i++ {
		if err := r.runStep(ctx, i); err != nil {
			return err
		}
	}
	return r.assertTerminus(ctx)
}

// assertTerminus adds every record that completed the run's final step to the
// pipeline's terminus group (SPEC §8, ADR-021). A dry or simulated run asserts
// nothing durable — the receipt reports what an armed run would have recorded.
func (r *runner) assertTerminus(ctx context.Context) error {
	name := strings.TrimSpace(r.plan.Pipeline.Group)
	if name == "" {
		return nil
	}
	final := ledger.StateSourced
	if n := len(r.plan.Steps); n > 1 {
		final = r.plan.Steps[n-1].ID
	}
	records, err := r.l.RunRecords(ctx, r.runID)
	if err != nil {
		return err
	}
	// A withheld send (on_missing skip, suppression) leaves a deliver-step fail
	// verdict but the record advanced — it completes and joins; the terminus
	// captures completers, not sends (SPEC §8, ADR-031).
	var completers []string
	for _, rr := range records {
		if rr.State == final && !r.stopped(rr) {
			completers = append(completers, rr.IdentityID)
		}
	}
	r.terminusGroup = name
	if r.dry {
		r.terminusWould = len(completers)
		return nil
	}
	g, err := r.l.EnsureGroup(ctx, name)
	if err != nil {
		return err
	}
	members, err := r.l.GroupMembership(ctx, g.ID)
	if err != nil {
		return err
	}
	for _, id := range completers {
		if members[id] {
			continue // already a member; re-asserting would only add noise
		}
		if err := r.l.AddGroupEvent(ctx, g.ID, id, ledger.GroupAdded,
			map[string]any{"pipeline": r.plan.Pipeline.Name}, r.runID); err != nil {
			return err
		}
		r.terminusAdded++
	}
	return nil
}

// stopped reports whether a verdict froze this record. A filter's fail stops
// it from advancing (SPEC §7); a deliver step's fail verdict records a
// withheld send and the record advances (SPEC §8, ADR-031), so those do not
// count.
func (r *runner) stopped(rr ledger.RunRecord) bool {
	for step, v := range rr.Verdicts {
		if v == "fail" && !r.deliverSteps[step] {
			return true
		}
	}
	return false
}

// stat returns the mutable stat block for a step.
func (r *runner) stat(st *planner.Step) *StepStat {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.stats[st.ID]
	if !ok {
		s = &StepStat{ID: st.ID, Use: st.Use, Role: st.Role}
		r.stats[st.ID] = s
		r.order = append(r.order, st.ID)
	}
	return s
}

func (r *runner) collect() []StepStat {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]StepStat, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, *r.stats[id])
	}
	return out
}

func (r *runner) prov(stepID string) ledger.Provenance {
	return ledger.Provenance{RunID: r.runID, StepID: stepID}
}

// openSession launches an adapter with its declared credentials. Nothing is sent
// yet: the caller streams OPEN plus its records with Session.SendStream so writes
// and reads overlap.
func (r *runner) openSession(ctx context.Context, st *planner.Step) (*adapters.Session, error) {
	sess, err := st.Adapter.Open(ctx, adapters.Ports{Env: r.sessionEnv(st), Log: r.stderr})
	if err != nil {
		return nil, fmt.Errorf("runner: %s: %w", st.ID, err)
	}
	return sess, nil
}

// sessionEnv is a step's Ports environment: its resolved credentials, plus the
// simulate signals (SPEC §8) — bindings switch to fixture-served mode, AI
// steps to the fixture engine (an operator-recorded GTME_AI_FIXTURE in the
// process env still wins over the synthesized $auto script).
func (r *runner) sessionEnv(st *planner.Step) map[string]string {
	if !r.simulate {
		return st.Credentials
	}
	env := make(map[string]string, len(st.Credentials)+3)
	for k, v := range st.Credentials {
		env[k] = v
	}
	env[binding.SimulateEnv] = "1"
	if isAIStep(st) {
		env["GTME_AI_ENGINE"] = ai.EngineFixture
		if os.Getenv("GTME_AI_FIXTURE") == "" {
			env["GTME_AI_FIXTURE"] = r.aiFixture
		}
	}
	return env
}

// openMessage is the OPEN that starts every session (SPEC §5). A deliver
// step's variables: mapping rides in as config — the adapter owns the egress
// mapping (ADR-018), the runner owns projecting the fields it references. An
// AI step's derived provides schema rides in the same way (ADR-033): the
// adapter generates its output shape from it, the runner validates against it.
func (r *runner) openMessage(st *planner.Step) protocol.Message {
	config := st.Config
	if len(st.Variables) > 0 || len(st.AIProvides) > 0 {
		config = make(map[string]any, len(st.Config)+2)
		for k, v := range st.Config {
			config[k] = v
		}
		if len(st.Variables) > 0 {
			config["variables"] = st.Variables
		}
		if len(st.AIProvides) > 0 {
			var schema map[string]any
			if err := json.Unmarshal(st.AIProvides, &schema); err == nil {
				config[adapters.ProvidesConfigKey] = schema
			}
		}
	}
	return protocol.Message{
		Type:   protocol.TypeOpen,
		StepID: st.ID,
		RunID:  r.runID,
		Config: config,
	}
}

// runSource drains the source adapter into the ledger and the run's membership.
func (r *runner) runSource(ctx context.Context) error {
	st := r.plan.Source()
	stat := r.stat(st)

	done, err := r.l.StepEventSeen(ctx, r.runID, st.ID, "done")
	if err != nil {
		return err
	}
	if done {
		// Resuming a run whose source already finished: membership is in the ledger.
		records, err := r.l.RunRecords(ctx, r.runID)
		if err != nil {
			return err
		}
		stat.Out = len(records)
		fmt.Fprintf(r.stderr, "%s: already sourced (%d records)\n", st.ID, len(records))
		return nil
	}

	if st.IsGroupSource {
		return r.runGroupSource(ctx, st, stat)
	}

	if r.stubbed(st) {
		// A stubbed source under --simulate sources nothing: a visible gap, not a
		// silent pass (SPEC §8).
		r.bump(st, func(s *StepStat) { s.SimGap = true })
		fmt.Fprintf(r.stderr, "%s: simulation gap — nothing to source from (no fixtures)\n", st.ID)
		return r.l.LogStepEvent(ctx, r.prov(st.ID), "", "done",
			map[string]any{"records": 0, "simulation_gap": true})
	}

	sess, err := r.openSession(ctx, st)
	if err != nil {
		return err
	}
	// A source receives no records; END says "there is no input coming".
	sendErr := sess.SendStream([]protocol.Message{r.openMessage(st), protocol.End()})

	count := 0
	for {
		m, err := sess.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			r.logStepFailure(ctx, st, err)
			return fmt.Errorf("runner: %s: %w", st.ID, err)
		}
		switch m.Type {
		case protocol.TypeRecord:
			if err := r.ingestSourceRecord(ctx, st, m); err != nil {
				stat.Failed++
				fmt.Fprintf(r.stderr, "%s: dropped a record: %v\n", st.ID, err)
				continue
			}
			count++
		case protocol.TypeCost:
			if err := r.recordCost(ctx, st, "", m); err != nil {
				return err
			}
		case protocol.TypeLog:
			r.forwardLog(st, m)
		case protocol.TypeState:
			if err := r.l.LogStepEvent(ctx, r.prov(st.ID), "", "state", m.Cursor); err != nil {
				return err
			}
		case protocol.TypeSchema, protocol.TypeVerdict:
			// SCHEMA is informational at run time; the plan already fixed the
			// contract. A source has no verdicts.
		case protocol.TypeEnd:
			// Keep reading until EOF so the adapter can flush trailing COST lines.
		}
	}
	if err := sess.Wait(); err != nil {
		r.logStepFailure(ctx, st, err)
		return fmt.Errorf("runner: %s: %w", st.ID, err)
	}
	// A source that never reads its (empty) input closes the pipe early; that is
	// not a failure.
	if err := <-sendErr; err != nil && !isBrokenPipe(err) {
		r.logStepFailure(ctx, st, err)
		return fmt.Errorf("runner: %s: %w", st.ID, err)
	}

	stat.Out = count
	if err := r.l.LogStepEvent(ctx, r.prov(st.ID), "", "done", map[string]any{"records": count}); err != nil {
		return err
	}
	fmt.Fprintf(r.stderr, "%s: sourced %d records\n", st.ID, count)
	return nil
}

// runGroupSource sources a run from a group's current membership (SPEC §9,
// ADR-021): members are projected from the ledger like any record —
// runner-owned, no adapter, no spend.
func (r *runner) runGroupSource(ctx context.Context, st *planner.Step, stat *StepStat) error {
	g, err := r.l.GetGroup(ctx, st.SourceGroup)
	if err != nil {
		return fmt.Errorf("runner: %s: %w", st.ID, err)
	}
	members, err := r.l.GroupMembers(ctx, g.ID)
	if err != nil {
		return err
	}
	for _, ident := range members {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.l.AddRunRecord(ctx, r.runID, ident.ID, ledger.StateSourced); err != nil {
			return err
		}
		r.emit(protocol.Key{EntityType: ident.EntityType, IdentityKey: ident.IdentityKey}, nil)
	}
	stat.Out = len(members)
	if err := r.l.LogStepEvent(ctx, r.prov(st.ID), "", "done",
		map[string]any{"records": len(members), "group": g.Name}); err != nil {
		return err
	}
	fmt.Fprintf(r.stderr, "%s: sourced %d members of group %q\n", st.ID, len(members), g.Name)
	return nil
}

// ingestSourceRecord canonicalizes a sourced record into an identity, writes its
// fields, and adds it to the run.
func (r *runner) ingestSourceRecord(ctx context.Context, st *planner.Step, m protocol.Message) error {
	if len(m.Fields) == 0 && m.Key == nil {
		return fmt.Errorf("record has neither fields nor a key")
	}
	if err := st.Manifest.ValidateProvides(m.Fields); err != nil {
		return err
	}
	if err := r.checkRegistry(st.EntityType, m.Fields); err != nil {
		return err
	}

	var ident ledger.Identity
	res, err := r.l.UpsertIdentity(ctx, st.EntityType, m.Fields, r.prov(st.ID))
	switch {
	case err == nil:
		ident = res.Identity
	case m.Key != nil && m.Key.IdentityKey != "":
		// The adapter knows who this is even though the fields do not say so.
		ident, err = r.l.EnsureIdentity(ctx, m.Key.EntityType, m.Key.IdentityKey, r.prov(st.ID))
		if err != nil {
			return err
		}
	default:
		return err
	}

	if _, err := r.l.WriteFieldMap(ctx, ident.ID, r.source(st), r.prov(st.ID), m.Fields, m.Confidence); err != nil {
		return err
	}
	if err := r.keepPayload(ctx, st, ident.ID, m); err != nil {
		return err
	}
	if err := r.l.AddRunRecord(ctx, r.runID, ident.ID, ledger.StateSourced); err != nil {
		return err
	}
	r.emit(protocol.Key{EntityType: ident.EntityType, IdentityKey: ident.IdentityKey}, m.Fields)
	// Relate the person to their company when the source gave us a domain
	// (SPEC §10.2 wants the relation; the runner owns identity, so it lives here).
	if st.EntityType == identity.Person {
		if err := r.relateCompany(ctx, st, ident.ID, m.Fields); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) relateCompany(ctx context.Context, st *planner.Step, personID string, fields map[string]any) error {
	domain := identity.NormalizeDomain(str(fields["company_domain"]))
	if domain == "" {
		return nil
	}
	company := map[string]any{"domain": domain}
	if name := str(fields["company_name"]); name != "" {
		company["name"] = name
	}
	res, err := r.l.UpsertIdentity(ctx, identity.Company, company, r.prov(st.ID))
	if err != nil {
		return nil // a company we cannot key is not worth failing a person over
	}
	if _, err := r.l.WriteFieldMap(ctx, res.Identity.ID, st.Manifest.Source(), r.prov(st.ID), company, nil); err != nil {
		return err
	}
	return r.l.Relate(ctx, personID, "works_at", res.Identity.ID)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func (r *runner) forwardLog(st *planner.Step, m protocol.Message) {
	level := m.Level
	if level == "" {
		level = "info"
	}
	fmt.Fprintf(r.stderr, "%s [%s]: %s\n", st.ID, level, m.Msg)
}

func (r *runner) recordCost(ctx context.Context, st *planner.Step, identityID string, m protocol.Message) error {
	if err := r.l.RecordCost(ctx, r.runID, st.ID, identityID, m.Provider, m.Amount(), m.Detail); err != nil {
		return err
	}
	r.bump(st, func(s *StepStat) { s.CostUSD += m.Amount() })
	return nil
}

// emit passes a record downstream in pipe mode. Only the key matters — the next
// process re-projects from the ledger, which is the bus — but the fields this
// step just wrote ride along so a human watching the stream can see the work.
func (r *runner) emit(key protocol.Key, fields map[string]any) {
	if r.out == nil {
		return
	}
	if err := r.out.Write(protocol.Record(key, fields, nil)); err != nil {
		fmt.Fprintf(r.stderr, "warning: writing downstream: %v\n", err)
	}
}

// keepPayload retains a RECORD's raw-response attachment when the adapter's
// ADR-030 declaration says to (SPEC §5/§6). The runner is the authority —
// adapters only offer.
func (r *runner) keepPayload(ctx context.Context, st *planner.Step, identityID string, m protocol.Message) error {
	if m.Payload == nil || m.Payload.Body == "" || st.Manifest == nil {
		return nil
	}
	keep, ttlDays := st.Manifest.PayloadRetention(st.Config)
	if !keep {
		return nil
	}
	return r.l.WritePayload(ctx, identityID, st.Manifest.ID, r.runID,
		m.Payload.ContentType, m.Payload.Body, ttlDays)
}

// source is the provenance string a step's writes carry. AI steps record the
// engine's model identifier (SPEC §10a, ADR-026): the id says what kind of
// fact, provenance says who produced it.
func (r *runner) source(st *planner.Step) string {
	if st.Manifest == nil {
		return st.Use // a group source writes no fields; this labels events only
	}
	if isAIStep(st) {
		engine, _ := st.Config["engine"].(string)
		model, _ := st.Config["model"].(string)
		getenv := func(k string) string { return r.sessionEnv(st)[k] }
		return st.Manifest.ID + " @ " + ai.ProvenanceModel(engine, model, getenv)
	}
	return st.Manifest.Source()
}

// checkRegistry is enforcement layer 2 (SPEC §4a): canonical fields in adapter
// output must match the registry's declared type, value domain and normalized
// form — the providing adapter was required to normalize at its own boundary.
func (r *runner) checkRegistry(entityType string, fields map[string]any) error {
	for name, v := range fields {
		if err := r.reg.CheckValue(entityType, name, v); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) logStepFailure(ctx context.Context, st *planner.Step, cause error) {
	_ = r.l.LogStepEvent(ctx, r.prov(st.ID), "", "failed", map[string]any{"error": cause.Error()})
}
