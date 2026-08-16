// Package runner executes a plan: it projects records out of the ledger, feeds
// them to adapters over the wire protocol, validates what comes back, and writes
// it to the ledger (SPEC §7).
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/trevorfox/gtm/internal/adapters"
	"github.com/trevorfox/gtm/internal/identity"
	"github.com/trevorfox/gtm/internal/ledger"
	"github.com/trevorfox/gtm/internal/planner"
	"github.com/trevorfox/gtm/internal/protocol"
	"github.com/trevorfox/gtm/internal/registry"
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
	Gated          int // excluded by when:
	Skipped        int // deliver records held back by on_missing (SPEC §8)
	CostUSD        float64
	AvoidedUSD     float64
	AvoidedUnknown bool

	// MissingSkips lists every record on_missing held back, with its reason —
	// the receipt shows them (SPEC §8).
	MissingSkips []RecordVariables
	// DryRun lists each record's RESOLVED variables when the run was dry —
	// the approval artifact a human reviews before arming (SPEC §8).
	DryRun []RecordVariables
}

// RecordVariables is one record's resolved (or unresolvable) deliver variables.
type RecordVariables struct {
	IdentityKey string
	Resolved    map[string]string // target merge-field name → resolved value
	Missing     []string          // ledger fields with no non-empty value
}

// Result is the outcome of a run.
type Result struct {
	RunID  string
	Status string
	DryRun bool
	Steps  []StepStat
}

// Concurrency resolves the worker pool size: the option, else GTM_CONCURRENCY,
// else the default.
func Concurrency(opt int) int {
	if opt > 0 {
		return opt
	}
	if v := os.Getenv("GTM_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return DefaultConcurrency
}

type runner struct {
	l      *ledger.Ledger
	plan   *planner.Plan
	stderr io.Writer
	conc   int
	runID  string
	dry    bool
	reg    *registry.Registry
	now    func() time.Time
	// out is the downstream NDJSON stream in pipe mode, nil for `gtm run`.
	out *protocol.Writer

	mu    sync.Mutex
	stats map[string]*StepStat
	order []string
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
		l:      o.Ledger,
		plan:   o.Plan,
		stderr: o.Stderr,
		conc:   Concurrency(o.Concurrency),
		dry:    o.DryRun,
		reg:    reg,
		now:    time.Now,
		stats:  map[string]*StepStat{},
	}
	if r.dry {
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

	runErr := r.execute(ctx)

	status := ledger.StatusDone
	if runErr != nil {
		status = ledger.StatusFailed
	}
	if err := r.l.FinishRun(ctx, r.runID, status); err != nil && runErr == nil {
		runErr = err
	}

	return &Result{RunID: r.runID, Status: status, DryRun: r.dry, Steps: r.collect()}, runErr
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
	return nil
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
	sess, err := st.Adapter.Open(ctx, adapters.Ports{Env: st.Credentials, Log: r.stderr})
	if err != nil {
		return nil, fmt.Errorf("runner: %s: %w", st.ID, err)
	}
	return sess, nil
}

// openMessage is the OPEN that starts every session (SPEC §5). A deliver
// step's variables: mapping rides in as config — the adapter owns the egress
// mapping (ADR-018), the runner owns projecting the fields it references.
func (r *runner) openMessage(st *planner.Step) protocol.Message {
	config := st.Config
	if len(st.Variables) > 0 {
		config = make(map[string]any, len(st.Config)+1)
		for k, v := range st.Config {
			config[k] = v
		}
		config["variables"] = st.Variables
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

	if _, err := r.l.WriteFieldMap(ctx, ident.ID, st.Manifest.Source(), r.prov(st.ID), m.Fields, m.Confidence); err != nil {
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
