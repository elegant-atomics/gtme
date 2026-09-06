// Package planner resolves a pipeline into an executable plan and validates the
// contracts between its steps before anything is spent (SPEC §7).
package planner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/ledger"
	"github.com/elegant-atomics/gtme/internal/pipeline"
	"github.com/elegant-atomics/gtme/internal/registry"
	"github.com/elegant-atomics/gtme/internal/secrets"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

// DefaultBatchSize is the batch size for AI steps (SPEC §9).
const DefaultBatchSize = 25

// Step is one resolved step of a plan.
type Step struct {
	ID   string
	Use  string
	Role string

	Adapter  *adapters.Resolved
	Manifest *adapters.Manifest
	Config   map[string]any

	EntityType string

	// Needs is the projection handed to the adapter; Required is the subset that
	// must be present for a record to be processed.
	Needs    []string
	Required []string
	// NeedsAll marks an adapter whose needs schema is open-ended and names no
	// fields — "show me everything you know". AI steps are the case in point.
	NeedsAll bool

	// Provides is what the step contributes to the available field set.
	ProvidesSchema json.RawMessage
	Provides       []string
	Wildcard       bool
	// AIProvides is an AI step's derived provides schema (SPEC §7, ADR-033):
	// the step-level provides: declaration with names namespaced by pipeline
	// (unless already namespaced), in declaration order under required. Nil
	// when the step declares nothing. The runner injects it into OPEN config
	// and validates the step's RECORDs against it (ValidateProvides).
	AIProvides json.RawMessage
	aiProvides *jsonschema.Schema

	Cache       time.Duration
	When        string
	WhenStep    string
	Batch       bool
	BatchSize   int
	Idempotency string

	// Variables is a deliver step's egress mapping (SPEC §9, ADR-018/019):
	// target merge-field name → ledger field. Its values joined Needs/Required
	// (the dynamic-needs derivation, SPEC §6).
	Variables map[string]string
	// OnMissing is the per-record completeness policy: on a deliver step
	// "skip" | "fail" (SPEC §8); on a participant step "run" | "skip" |
	// "fail" for a declared uses: field absent at run time (SPEC §7,
	// ADR-053). Uses is the declared list itself, kept apart from Needs so
	// the runner can name exactly which declared fields were absent.
	OnMissing string
	Uses      []string
	// NeedsBranches are the one-of alternatives (SPEC §7): the step is
	// satisfiable when any single branch's fields are all available.
	NeedsBranches [][]string
	// Notes are non-blocking plan observations (namespaced needs, near-miss
	// column suggestions, a weak identity path) printed with the plan.
	Notes []string
	// Warnings are per-step plan warnings (SPEC §7): respend, a deferred
	// step on an engine with no batch surface.
	Warnings []string
	// Deferred marks an AI step that ends the run in flight (ADR-038).
	Deferred bool
	// Respend is the step's declared opt-out of the respend warning.
	Respend bool
	// RedeliverMode is the resolved repeat policy for a deliver step
	// (ADR-045): always | on_change | never.
	RedeliverMode string

	// Participant classifies a participant step (ADR-048/049): ai, human,
	// agent, or "" for a provider step. Of is the referent field (ADR-048):
	// the value a compose or review step is about, validated as one more
	// uses: entry. RenderFields/RenderTemplate are a human/agent step's
	// surface (ADR-049) and Prompt its policy — tty or never; an agent/*
	// step is always never.
	Participant    string
	Of             string
	RenderFields   []string
	RenderTemplate string
	Prompt         string

	Credentials map[string]string
	// MissingOptional are declared-optional credentials that did not resolve;
	// the plan reports them as warnings, not errors.
	MissingOptional []string
	CostEstimate    *float64
	// CostUnset marks a binding whose rate templates from config and
	// resolved to nothing (ADR-046): the plan prints `unset`, never $0.
	CostUnset bool

	IsSource  bool
	IsDeliver bool

	// IsTraverse marks a traverse step (SPEC §6/§7, ADR-054): records of
	// From in, records of EntityType out, each related to its parent by
	// Relation; the run opens a new segment after it. Segment is the
	// index of the segment a step belongs to (0 = the source's); after a
	// traverse it increments. Writes lists the relations a step's provides
	// will write through reference fields (SPEC §4a), for the plan.
	IsTraverse bool
	From       string
	Relation   *adapters.Relation
	Segment    int
	Writes     []string

	// IsSQL marks a runner-owned SQL step (SPEC §10a, ADR-027): no adapter,
	// declared contracts (uses:/provides: in config), one read-only query per
	// step. Query is its SQL.
	IsSQL bool
	Query string

	// Group semantics (SPEC §7/§8/§9, ADR-021) — all runner-owned.
	// IsGroupSource marks a `source: {group: ...}` step: members projected
	// from the ledger, no adapter, provides open. Limit caps it (ADR-032):
	// at most N members, oldest-added first; 0 is unbounded.
	IsGroupSource bool
	SourceGroup   string
	Limit         int
	// Once selects only members this pipeline has not finished (ADR-052).
	// OnceMembers/OnceEligible are counted by CheckGroups — current members,
	// and those not yet finished — from the ledger at plan time; OnceCounted
	// says whether that happened.
	Once         bool
	OnceMembers  int
	OnceEligible int
	OnceCounted  bool
	// IsGroupDeliver marks a `use: group/deliver` step (SPEC §8, ADR-032):
	// a runner-owned deliver step whose target is TargetGroup, created on
	// demand. Every deliver-step key applies; --dry-run withholds it.
	IsGroupDeliver bool
	TargetGroup    string
	// Require/Exclude are membership gates checked per record.
	Require []string
	Exclude []string
	// RecordGroup is the deliver step's touch scope (defaults to the
	// pipeline name); SuppressGroup/SuppressWithin the contact-policy window.
	RecordGroup    string
	SuppressGroup  string
	SuppressWithin time.Duration
}

// Plan is a validated, executable pipeline.
type Plan struct {
	Pipeline  *pipeline.Pipeline
	Steps     []Step
	Available []string
	Wildcard  bool
	// FinalType is the type of the run's last segment (SPEC §7, ADR-054):
	// the source's, or the last traverse's output type — what the terminus
	// group takes. "" for an entity-blind pipeline (an untyped legacy group
	// source).
	FinalType string
	// Warnings are plan-level observations that do not block (SPEC §7): the
	// one-commit-point rule (ADR-032) is the first.
	Warnings []string
	// Notes are plan-level observations that are neither warnings nor
	// problems: the cron note when a deliver step follows a human/agent step
	// (ADR-049).
	Notes []string
}

// RunnerOwned reports a human/* or agent/* step (ADR-049): no session is
// opened; the runner asks, or waits in the ledger for `gtme answer`.
func (s *Step) RunnerOwned() bool {
	return s.Participant == adapters.KindHuman || s.Participant == adapters.KindAgent
}

// PendingToken is the runner-owned token a human/agent step's unanswered
// records wait under (SPEC §8, ADR-049): <run-id>/<step-id>.
func PendingToken(runID, stepID string) string { return runID + "/" + stepID }

// GroupDeliverID is the runner-owned handoff step (SPEC §8, ADR-032).
const GroupDeliverID = "group/deliver"

// Target is the deliveries.target a deliver step writes under (SPEC §3): the
// adapter id, or `group:<name>` for a handoff — so each group keeps its own
// (target, idempotency) scope, as each adapter does.
func (s *Step) Target() string {
	if s.IsGroupDeliver {
		return "group:" + s.TargetGroup
	}
	if s.Manifest != nil {
		return s.Manifest.ID
	}
	return s.Use
}

// Source is the source step.
func (p *Plan) Source() *Step { return &p.Steps[0] }

// ValidateProvides checks a step's output RECORD before it reaches the ledger
// (SPEC §5): against the derived provides schema when the step declared one
// (ADR-033), else against the manifest's static schema.
func (s *Step) ValidateProvides(fields map[string]any) error {
	if s.aiProvides != nil {
		if err := s.aiProvides.Validate(adapters.NormalizeForSchema(fields)); err != nil {
			return fmt.Errorf("output does not match declared provides: %w", err)
		}
		return nil
	}
	if s.Manifest == nil {
		return nil
	}
	return s.Manifest.ValidateProvides(fields)
}

// Scope is what a step inherits from its pipeline at resolve time: the name
// (the default namespace for declared AI outputs, SPEC §4a), the entity type
// its source emits (the entity type of every entity-agnostic AI step, SPEC
// §10.3), and the local ledger, read-only — what config values from the
// ledger and plan-time EXPLAIN resolve against (SPEC §7, ADR-037). Ledger
// may be nil, in which case a step needing it is a plan problem.
type Scope struct {
	Ctx        context.Context
	Pipeline   string
	EntityType string
	Ledger     *ledger.Ledger
}

// SQLTransformID and SQLFilterID are the runner-owned SQL steps (SPEC §10a).
// SQLEnrichID is the pre-ADR-037 name, kept only to name the fix.
const (
	SQLTransformID = "sql/transform"
	SQLFilterID    = "sql/filter"
	SQLTraverseID  = "sql/traverse" // ADR-054: the runner-owned traverse over relations
	SQLEnrichID    = "sql/enrich"
)

// ResolvedPipeline is the pipeline with every step's with: replaced by its
// resolved config — {query:}/{segment:} values substituted (SPEC §7,
// ADR-037) — which is what runs.config_json records, so a run reproduces
// what it actually ran against, not what it would recompute.
func (p *Plan) ResolvedPipeline() *pipeline.Pipeline {
	out := *p.Pipeline
	if p.Pipeline.Source != nil {
		src := *p.Pipeline.Source
		out.Source = &src
	}
	out.Steps = append([]pipeline.Step(nil), p.Pipeline.Steps...)
	for i := range p.Steps {
		st := &p.Steps[i]
		if i == 0 && out.Source != nil {
			out.Source.With = st.Config
			continue
		}
		if i-1 < len(out.Steps) {
			out.Steps[i-1].With = st.Config
		}
	}
	return &out
}

// StepByID finds a step.
func (p *Plan) StepByID(id string) *Step {
	for i := range p.Steps {
		if p.Steps[i].ID == id {
			return &p.Steps[i]
		}
	}
	return nil
}

// Problem kinds. Contract, config and adapter problems are validation errors
// (exit 2); credential problems are auth errors (exit 3).
const (
	KindAdapter    = "adapter"
	KindConfig     = "config"
	KindContract   = "contract"
	KindCredential = "credential"
)

// Problem is one reason a plan is not executable.
type Problem struct {
	Step string
	Kind string
	Msg  string
}

// providerHint names installed adapters whose provides cover a missing
// need — "needs email" is only half an error message when `apollo/enrich`
// is one step away (M20; the round-trip agents read these messages as
// documentation).
func providerHint(missing []string) string {
	var parts []string
	for _, field := range missing {
		var ids []string
		for _, m := range adapters.Installed() {
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if len(m.Provides) == 0 || json.Unmarshal(m.Provides, &schema) != nil {
				continue
			}
			if _, ok := schema.Properties[field]; ok {
				ids = append(ids, m.ID)
			}
		}
		if len(ids) > 0 {
			sort.Strings(ids)
			parts = append(parts, field+" ← "+strings.Join(ids, "|"))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "installed adapters provide it: " + strings.Join(parts, ", ")
}

func (p Problem) Error() string {
	if p.Step == "" {
		return p.Msg
	}
	return fmt.Sprintf("step %q: %s", p.Step, p.Msg)
}

// Errors is every problem found in one pass, so the operator fixes them all at
// once instead of one per run.
type Errors struct{ Problems []Problem }

func (e *Errors) Error() string {
	if len(e.Problems) == 1 {
		return e.Problems[0].Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d plan problems:", len(e.Problems))
	for _, p := range e.Problems {
		fmt.Fprintf(&b, "\n  - %s", p.Error())
	}
	return b.String()
}

// ExitCode is 3 when every problem is a missing credential, else 2 (SPEC §8).
func (e *Errors) ExitCode() int {
	for _, p := range e.Problems {
		if p.Kind != KindCredential {
			return 2
		}
	}
	return 3
}

// Build resolves and validates a pipeline. It performs no network calls and
// spends nothing; the ledger, when given, is read only (SPEC §7: config
// values from the ledger, SQL at plan). ctx and l may be nil for a pipeline
// that needs neither.
func Build(ctx context.Context, p *pipeline.Pipeline, l *ledger.Ledger) (*Plan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	plan := &Plan{Pipeline: p}
	var problems []Problem

	available := map[string]bool{}
	steps := p.AllSteps()
	scope := Scope{Ctx: ctx, Pipeline: p.Name, Ledger: l}
	reg, err := registry.Load()
	if err != nil {
		return nil, &Errors{Problems: []Problem{{Kind: KindAdapter, Msg: err.Error()}}}
	}

	segment := 0
	// lastTraverse names the traverse that opened the current segment — what
	// a cross-segment when: is told to gate at instead.
	lastTraverse := ""
	for i, s := range steps {
		isSource := i == 0

		ps, stepProblems := ResolveStep(s, isSource, scope)
		problems = append(problems, stepProblems...)
		ps.Segment = segment
		if isSource {
			// The pipeline's entity type is its source's, or its group's
			// (ADR-054); an untyped legacy group offers none, so steps after
			// it validate names entity-blind, as SQL steps always have.
			scope.EntityType = ps.EntityType
		}
		// A run is a sequence of typed segments (SPEC §7, ADR-054): every
		// step is validated against the type of its segment — its manifest's
		// entity_type equals it or is "*" — and a traverse's from must equal
		// it. After a traverse only its output type moves forward.
		if !isSource && scope.EntityType != "" && reg != nil {
			switch {
			case ps.IsTraverse && ps.From != scope.EntityType:
				problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
					Msg: fmt.Sprintf("%s traverses from %s, but the records here are %s (SPEC §7, ADR-054)", ps.Use, ps.From, scope.EntityType)})
			case !ps.IsTraverse && ps.EntityType != "" && ps.EntityType != scope.EntityType:
				problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
					Msg: fmt.Sprintf("%s is a %s adapter, but the records here are %s (SPEC §7, ADR-054) — its manifest's entity_type must equal the segment's type or be \"*\"", ps.Use, ps.EntityType, scope.EntityType)})
			}
		}
		if !isSource && ps.IsTraverse && scope.EntityType == "" {
			group := "<group>"
			if len(plan.Steps) > 0 && plan.Steps[0].IsGroupSource {
				group = plan.Steps[0].SourceGroup
			}
			problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
				Msg: fmt.Sprintf("a traverse needs a typed segment to traverse from — this pipeline is entity-blind (an untyped group source); set the group's type with `gtme groups add %s --type %s` (SPEC §7, ADR-054)", group, ps.From)})
		}
		// A deliver adapter naming a signal type is a plan error (SPEC §4a):
		// a signal is found and traversed from, never delivered to.
		if ps.IsDeliver && ps.Manifest != nil && reg != nil {
			if t, err := reg.Resolve(ps.Manifest.EntityType); err == nil && t.IsSignal() {
				problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
					Msg: fmt.Sprintf("%s delivers %s records, but %s is a signal type — found and traversed from, never delivered to (SPEC §4a, ADR-054)", ps.Use, t.EntityType, t.EntityType)})
			}
		}
		// when: names a step in the current segment only (SPEC §7): a verdict
		// is a fact about the parent, not the child.
		if ps.WhenStep != "" {
			if ref := plan.StepByID(ps.WhenStep); ref != nil && ref.Segment != segment {
				problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
					Msg: fmt.Sprintf("when: %s.passed names a step before the traverse %q — a verdict is a fact about the parent, not the child (SPEC §7, ADR-054); gate at the traverse instead (when: %s.passed on %q mints only children of passing parents)",
						ps.WhenStep, lastTraverse, ps.WhenStep, lastTraverse)})
			}
		}
		// The relations a step's provides will write (SPEC §4a references).
		if reg != nil && ps.EntityType != "" && !ps.IsTraverse {
			if t, err := reg.Resolve(ps.EntityType); err == nil {
				have := map[string]bool{}
				for _, f := range ps.Provides {
					have[f] = true
				}
				for _, f := range t.References() {
					if have[f.Name] {
						ps.Writes = append(ps.Writes, fmt.Sprintf("%s → %s (from %s)", f.Reference.Relation, f.Reference.Type, f.Name))
					}
				}
			}
		}
		// A deliver step's touch scope defaults to the pipeline name (SPEC §8,
		// ADR-031: per deliver step — steps sharing the default share the
		// scope): every pipeline is safely scoped unless it opts to share.
		if ps.IsDeliver && ps.RecordGroup == "" {
			ps.RecordGroup = p.Name
		}

		// Contract walk: every required need must already be available.
		if ps.Manifest != nil && !isSource {
			var missing []string
			for _, f := range ps.Required {
				if !available[f] {
					missing = append(missing, f)
				}
			}
			if len(missing) > 0 && !plan.Wildcard {
				msg := fmt.Sprintf("needs %s, which no earlier step provides (available: %s)",
					strings.Join(missing, ", "), describe(available))
				if hint := providerHint(missing); hint != "" {
					msg += "; " + hint
				}
				problems = append(problems, Problem{Step: s.ID, Kind: KindContract, Msg: msg})
			}
			// One-of needs (SPEC §7): at least one branch must be fully
			// available; a failure names every branch and what it is missing.
			if len(ps.NeedsBranches) > 0 && !plan.Wildcard {
				if !anyBranchAvailable(ps.NeedsBranches, available) {
					problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
						Msg: fmt.Sprintf("needs at least one of %s; no earlier step provides a complete alternative (available: %s)",
							describeBranches(ps.NeedsBranches, available), describe(available))})
				}
			}
		}
		// The adapter–type contract (SPEC §4a, ADR-054): the type resolves to
		// one file, provides is canonical for it, and a source can key what
		// it emits — judged here, with no network and no spend, so an
		// unkeyable source fails the plan rather than the run, after the
		// vendor has billed (#27). Only the name-hash fallback is a note.
		if isSource && ps.EntityType != "" && ps.Manifest != nil && reg != nil {
			contract, weak := reg.Contract(ps.Manifest, ps.EntityType, ps.Provides, ps.Wildcard)
			for _, cp := range contract {
				kind := KindContract
				if cp.Check == "provides" {
					kind = KindAdapter
				}
				msg := cp.Msg
				if cp.Check == "key" {
					msg += " — map a column to an identity field with columns:"
				}
				problems = append(problems, Problem{Step: s.ID, Kind: kind, Msg: msg})
			}
			if weak {
				ps.Notes = append(ps.Notes,
					"only the name-hash fallback identity tier is derivable from this source; dedupe will be weak until something provides an email or public profile URL")
			}
			// Near-miss columns (SPEC §7): a csv.* leftover a small edit away
			// from a canonical name is SUGGESTED, never silently mapped.
			for _, f := range ps.Provides {
				bare, ok := strings.CutPrefix(f, "csv.")
				if !ok {
					continue
				}
				if s := reg.Suggest(ps.EntityType, bare); s != "" {
					ps.Notes = append(ps.Notes,
						fmt.Sprintf("column %q looks like canonical %q — map it explicitly with columns: {%s: <your header>}", bare, s, s))
				}
			}
		}
		if ps.Wildcard {
			plan.Wildcard = true
		}
		if ps.IsTraverse {
			// The next segment: only records of the output type continue, and
			// the available-field set is what the traverse provides (SPEC §7).
			// A sql/traverse's children are ledger identities projected whole,
			// so its set is open, like a group source's.
			available = map[string]bool{}
			scope.EntityType = ps.EntityType
			segment++
			lastTraverse = ps.ID
		}
		for _, f := range ps.Provides {
			available[f] = true
		}
		// A deferred step is the pipeline's last step (SPEC §8, ADR-038).
		if ps.Deferred && i != len(steps)-1 {
			problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
				Msg: "deferred: true is valid only on the pipeline's last step — land this step's output in a group (group: terminus, or its declared provides:) and let a consumer pipeline pull it (SPEC §8, ADR-038)"})
		}

		plan.Steps = append(plan.Steps, ps)
	}

	// Respend (SPEC §7, ADR-038): a paid step that would pay for the same
	// records again on a re-run, with nothing remembering the answer, is
	// warned — unless the step says respend: true.
	writes := map[string]bool{}
	if g := strings.TrimSpace(p.Group); g != "" {
		writes[g] = true
	}
	for i := range plan.Steps {
		if plan.Steps[i].IsGroupDeliver {
			writes[plan.Steps[i].TargetGroup] = true
		}
	}
	for i := range plan.Steps {
		st := &plan.Steps[i]
		if st.IsSource || st.Respend || st.Manifest == nil {
			continue
		}
		// AI steps remember by default — the judgment cache (ADR-039); the
		// warning is for paid fetches with no window.
		if (st.Role == adapters.RoleEnrich || st.Role == adapters.RoleVerify) && !st.Manifest.IsParticipant() && st.Cache <= 0 &&
			(len(st.Manifest.Credentials) > 0 || (st.CostEstimate != nil && *st.CostEstimate > 0)) {
			st.Warnings = append(st.Warnings,
				"respend: this paid step has no freshness window, so every run pays for every record again — set cache: Nd, or say respend: true (SPEC §7, ADR-038)")
		}
	}
	_ = writes

	// when: reads the filter role only (SPEC §9, ADR-048): a review labels a
	// value and never gates, so gating on one is refused naming the fix.
	for i := range plan.Steps {
		st := &plan.Steps[i]
		if st.WhenStep == "" {
			continue
		}
		if ref := plan.StepByID(st.WhenStep); ref != nil && ref.Role == adapters.RoleReview {
			problems = append(problems, Problem{Step: st.ID, Kind: KindContract,
				Msg: fmt.Sprintf("when: %s.passed reads the filter role only — %q is a review and never gates (ADR-048); add a sql/filter on its labels and gate on that", st.WhenStep, st.WhenStep)})
		}
	}

	// The cron note (SPEC §7, ADR-049): a pipeline is run stage by stage, and
	// a pending run is resumed rather than re-sourced, so a deliver step
	// after a human/agent step means the pipeline waits for its person under
	// cron and sources nothing new until answered. One note, naming the
	// documented pattern.
	var person *Step
	for i := range plan.Steps {
		st := &plan.Steps[i]
		if st.RunnerOwned() && person == nil {
			person = st
		}
		if st.IsDeliver && person != nil {
			plan.Notes = append(plan.Notes, fmt.Sprintf(
				"under cron this pipeline waits for a person: %q follows the %s step %q, and a pending run is resumed, not re-sourced, until every record is answered (ADR-049). The pattern: review into a group in one pipeline, send from the group in another (SPEC §8).",
				st.ID, person.Use, person.ID))
			break
		}
	}

	plan.Available = keys(available)
	plan.FinalType = scope.EntityType

	// One commit point (SPEC §7, ADR-032): arming is all-or-nothing
	// (ADR-031), so a handoff and a network-side send in one pipeline means
	// approving the handoff approves the send. Warned, not refused.
	var handoffs, sends []string
	for i := range plan.Steps {
		st := &plan.Steps[i]
		switch {
		case st.IsGroupDeliver:
			handoffs = append(handoffs, fmt.Sprintf("%s (→ group %q)", st.ID, st.TargetGroup))
		case st.IsDeliver:
			sends = append(sends, fmt.Sprintf("%s (→ %s)", st.ID, st.Use))
		}
	}
	if len(handoffs) > 0 && len(sends) > 0 {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"one commit point (ADR-032): this pipeline both hands off — %s — and sends — %s. Arming approves every deliver step at once, so approving the handoff approves the send; keep the handoff in its own pipeline and let the send consume the group.",
			strings.Join(handoffs, ", "), strings.Join(sends, ", ")))
	}

	if len(problems) > 0 {
		return plan, &Errors{Problems: problems}
	}
	return plan, nil
}

// ResolveStep resolves one step's adapter, config, credentials, cache window and
// schemas. Whether a step is a deliver step is a role fact read from its
// resolved manifest (ADR-031), never a position: a pipeline may carry any
// number of deliver steps, anywhere after the source. scope carries what the
// step inherits from its pipeline (name, entity type).
func ResolveStep(s pipeline.Step, isSource bool, scope Scope) (Step, []Problem) {
	var problems []Problem
	ps := Step{
		ID:        s.ID,
		Use:       s.Use,
		Config:    s.With,
		When:      s.When,
		WhenStep:  s.WhenStep(),
		IsSource:  isSource,
		BatchSize: DefaultBatchSize,
	}
	if ps.Config == nil {
		ps.Config = map[string]any{}
	}
	// Config values from the ledger (SPEC §7/§9, ADR-037): {query:} and
	// {segment:} values resolve read-only before anything reads the config —
	// the adapter's config_schema validates the substituted value.
	// The with: map itself is never a value (a sql/* step's own `query`
	// key lives there); only the values under its keys are.
	if resolved, notes, valueProblems := resolveConfigMap(scope, "with", ps.Config); len(valueProblems) > 0 {
		for _, msg := range valueProblems {
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig, Msg: msg})
		}
	} else {
		ps.Config = resolved
		ps.Notes = append(ps.Notes, notes...)
	}
	ps.Require = append([]string(nil), s.Require...)
	ps.Exclude = append([]string(nil), s.Exclude...)

	// gateDeliverKeys rejects the deliver-only keys on a step whose role is
	// not deliver (SPEC §9, ADR-031) — the uses: pattern, second instance.
	// Called once the step's role is known.
	// on_missing: is also a participant-step key (ADR-053), so a participant
	// step is the one non-deliver step that keeps it.
	gateDeliverKeys := func(participant bool) {
		for _, k := range []struct {
			key string
			set bool
		}{
			{"variables:", len(s.Variables) > 0},
			{"on_missing:", s.OnMissing != "" && !participant},
			{"idempotency:", strings.TrimSpace(s.Idempotency) != ""},
			{"record:", strings.TrimSpace(s.Record) != ""},
			{"suppress:", s.Suppress != nil},
		} {
			if k.set && k.key == "on_missing:" {
				problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
					Msg: fmt.Sprintf("on_missing: is only valid on deliver steps and participant steps (ai/*, human/*, agent/*); %s has role %q — ADR-031, ADR-053", ps.Use, ps.Role)})
				continue
			}
			if k.set {
				problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
					Msg: fmt.Sprintf("%s is only valid on deliver steps (%s has role %q) — ADR-031", k.key, ps.Use, ps.Role)})
			}
		}
	}

	// gateOnMissingRun refuses `on_missing: run` on a deliver step: blank
	// merge fields never send (SPEC §8), so there is nothing "run" could mean.
	gateOnMissingRun := func() {
		if s.OnMissing == "run" {
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
				Msg: "on_missing: run is not valid on a deliver step — blank merge fields never send; use skip or fail (SPEC §8)"})
		}
	}

	// gateProvides rejects a step-level provides: declaration anywhere but a
	// participant step in a participant role (SPEC §9, ADR-033/048) — the
	// uses: pattern, third instance. Called once the step's role is known.
	gateProvides := func(participant bool) {
		if s.Provides == nil {
			return
		}
		switch {
		case !adapters.ParticipantRole(ps.Role):
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
				Msg: fmt.Sprintf("provides: is only valid on filter/compose/review steps (%s has role %q) — ADR-033", ps.Use, ps.Role)})
		case !participant:
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
				Msg: fmt.Sprintf("provides: is only valid on participant steps (ai/*, human/*, agent/*); %s takes its outputs from its own contract, not from a step-level declaration — ADR-033", ps.Use)})
		}
	}
	// gateOf rejects of: on a step that is neither a compose nor a review
	// (SPEC §9, ADR-048); the same pattern again.
	gateOf := func() {
		if strings.TrimSpace(s.Of) != "" {
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
				Msg: fmt.Sprintf("of: is only valid on compose and review steps of a participant adapter (%s has role %q) — ADR-048", ps.Use, ps.Role)})
		}
	}

	// group/deliver (SPEC §8, ADR-032) resolves no adapter: the handoff to
	// the next stage is a delivery the runner performs itself — every
	// deliver-step key applies, the target group is created on demand.
	if s.Use == GroupDeliverID {
		ps.IsDeliver = true
		ps.IsGroupDeliver = true
		ps.Role = adapters.RoleDeliver
		if isSource {
			problems = append(problems, Problem{Step: s.ID, Kind: KindContract, Msg: GroupDeliverID + " cannot be the source"})
		}
		group, _ := ps.Config["group"].(string)
		ps.TargetGroup = strings.TrimSpace(group)
		if ps.TargetGroup == "" {
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
				Msg: GroupDeliverID + " needs with.group — the group records are handed off to (SPEC §8)"})
		}
		for k := range ps.Config {
			if k != "group" {
				problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
					Msg: fmt.Sprintf("%s takes only with.group (got %q)", GroupDeliverID, k)})
			}
		}
		if len(s.Uses) > 0 {
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
				Msg: fmt.Sprintf("uses: is only valid on filter/compose/review steps (%s has role %q)", s.Use, ps.Role)})
		}
		gateProvides(false)
		gateOf()
		// Dynamic needs from variables:, no static floor (SPEC §6/§9).
		ps.Variables = s.Variables
		ps.Needs = variableFields(s.Variables)
		ps.Required = append([]string(nil), ps.Needs...)
		ps.OnMissing = s.OnMissing
		if ps.OnMissing == "" {
			ps.OnMissing = "skip"
		}
		gateOnMissingRun()
		ps.Idempotency = s.Idempotency
		ps.RecordGroup = strings.TrimSpace(s.Record)
		if s.Suppress != nil {
			ps.SuppressGroup = strings.TrimSpace(s.Suppress.Group)
			d, err := pipeline.ParseCache(s.Suppress.Within)
			if err != nil {
				problems = append(problems, Problem{Step: s.ID, Kind: KindConfig, Msg: "suppress.within: " + err.Error()})
			}
			ps.SuppressWithin = d
		}
		ps.EntityType = scope.EntityType
		if reg, err := registry.Load(); err == nil {
			for _, field := range ps.Needs {
				if err := reg.ValidateName(ps.EntityType, field); err != nil {
					problems = append(problems, Problem{Step: s.ID, Kind: KindContract, Msg: "variables: " + err.Error()})
				}
			}
		}
		return ps, problems
	}

	// SQL steps (SPEC §10a, ADR-027/037) resolve no adapter: the runner mediates
	// their read-only ledger access, and their contracts are DECLARED —
	// uses:/provides: in config — never parsed from the SQL.
	if s.Use == SQLEnrichID {
		problems = append(problems, Problem{Step: s.ID, Kind: KindAdapter,
			Msg: fmt.Sprintf("%s was renamed %s (ADR-037) — a transform is a per-record derivation or a cross-record aggregate, not a provider lookup; change use: to %s", SQLEnrichID, SQLTransformID, SQLTransformID)})
		gateDeliverKeys(false)
		return ps, problems
	}
	if s.Use == SQLTransformID || s.Use == SQLFilterID || s.Use == SQLTraverseID {
		ps.IsSQL = true
		// A SQL step takes its segment's type (SPEC §7, ADR-054), so its
		// declared uses:/provides: validate against the vocabulary the
		// records actually belong to; an entity-blind pipeline leaves it "".
		ps.EntityType = scope.EntityType
		switch s.Use {
		case SQLTransformID:
			ps.Role = adapters.RoleEnrich
		case SQLFilterID:
			ps.Role = adapters.RoleFilter
		default:
			// sql/traverse (SPEC §10a, ADR-054): the runner-owned traverse —
			// follows relations the ledger already holds, mints nothing,
			// writes no relation. Its output type is declared; its input type
			// is the segment's; the query yields identity_id and parent_id.
			ps.Role = adapters.RoleTraverse
			ps.IsTraverse = true
			ps.From = scope.EntityType
			ps.Limit = s.Limit
			out, _ := ps.Config["entity_type"].(string)
			ps.EntityType = strings.TrimSpace(out)
			ps.Wildcard = true // children are ledger identities, projected whole
			if ps.EntityType == "" {
				problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
					Msg: SQLTraverseID + " needs config.entity_type — the output type (SPEC §10a, ADR-054)"})
			} else if reg, err := registry.Load(); err == nil {
				if _, err := reg.Resolve(ps.EntityType); err != nil {
					problems = append(problems, Problem{Step: s.ID, Kind: KindContract, Msg: err.Error()})
				}
			}
			for k := range ps.Config {
				if k != "entity_type" && k != "query" {
					problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
						Msg: fmt.Sprintf("%s takes only with.entity_type and with.query (got %q)", SQLTraverseID, k)})
				}
			}
		}
		q, _ := ps.Config["query"].(string)
		ps.Query = strings.TrimSpace(q)
		if ps.Query == "" {
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
				Msg: s.Use + " needs config.query"})
		} else if err := ledger.ReadOnlyStatement(ps.Query); err != nil {
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
				Msg: fmt.Sprintf("%s: %v", s.Use, err)})
		} else {
			// SQL at plan (SPEC §7, ADR-037): EXPLAIN QUERY PLAN against the
			// local ledger — $0, no network — so an unknown table or column
			// fails the plan rather than the run.
			if err := explainQuery(scope, ps.Query); err != nil {
				problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
					Msg: fmt.Sprintf("%s: the query does not plan against the ledger: %v", s.Use, err)})
			}
			if refs := crossRecordRefs(ps.Query); len(refs) > 0 {
				ps.Notes = append(ps.Notes,
					fmt.Sprintf("cross-record: this query reads %s — it may read any identity in the ledger; only its results are scoped to the run, and it recomputes every run (SPEC §10a)", strings.Join(refs, " and ")))
			}
		}
		ps.Needs = configStrings(ps.Config["uses"])
		ps.Required = append([]string(nil), ps.Needs...)
		if ps.IsTraverse {
			gateDeliverKeys(false)
			gateProvides(false)
			gateOf()
			if isSource {
				problems = append(problems, Problem{Step: s.ID, Kind: KindContract, Msg: s.Use + " cannot be the source"})
			}
			return ps, problems
		}
		provides := configStrings(ps.Config["provides"])
		if s.Use == SQLTransformID && len(provides) == 0 {
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
				Msg: SQLTransformID + " needs config.provides — the declared output fields (SPEC §10a)"})
		}
		if s.Use == SQLFilterID && len(provides) > 0 {
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
				Msg: SQLFilterID + " produces verdicts, not fields — drop config.provides"})
		}
		ps.Provides = provides
		if reg, err := registry.Load(); err == nil {
			for _, name := range append(append([]string{}, ps.Needs...), ps.Provides...) {
				if err := reg.ValidateName(ps.EntityType, name); err != nil {
					problems = append(problems, Problem{Step: s.ID, Kind: KindContract, Msg: err.Error()})
				}
			}
		}
		if isSource {
			problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
				Msg: s.Use + " cannot be the source"})
		}
		gateDeliverKeys(false)
		gateProvides(false)
		gateOf()
		return ps, problems
	}

	// A group source (SPEC §9, ADR-021) resolves no adapter: members are
	// projected from the ledger by the runner, and its provides are open —
	// each step's needs are enforced per record at run time, exactly like
	// the needs-all wildcard.
	if isSource && strings.TrimSpace(s.Group) != "" {
		ps.IsGroupSource = true
		ps.SourceGroup = strings.TrimSpace(s.Group)
		ps.Limit = s.Limit
		ps.Once = s.Once
		ps.Use = "group:" + ps.SourceGroup
		ps.Role = adapters.RoleSource
		ps.Wildcard = true
		// The pipeline takes the group's type (SPEC §9, ADR-054), so every
		// later step's names validate against it. A group with no type —
		// created before ADR-054 — leaves the plan entity-blind, said so.
		if scope.Ledger != nil {
			if g, err := scope.Ledger.GetGroup(scope.Ctx, ps.SourceGroup); err == nil {
				ps.EntityType = g.EntityType
				if g.EntityType == "" {
					ps.Notes = append(ps.Notes, fmt.Sprintf(
						"group %q has no entity type (created before ADR-054), so this plan is entity-blind: field names are not validated until run time — set it once with `gtme groups add %s --type <type>`",
						ps.SourceGroup, ps.SourceGroup))
				}
			}
		}
		gateDeliverKeys(false)
		gateProvides(false)
		gateOf()
		return ps, problems
	}

	resolved, err := adapters.Resolve(s.Use)
	if err != nil {
		return ps, append(problems, Problem{Step: s.ID, Kind: KindAdapter, Msg: err.Error()})
	}
	ps.Adapter = resolved
	ps.Manifest = resolved.Manifest
	ps.Role = resolved.Manifest.Role
	ps.IsDeliver = ps.Role == adapters.RoleDeliver && !isSource
	ps.EntityType = resolved.EntityType(ps.Config)
	// A traverse (SPEC §6, ADR-054) crosses from its from type to its
	// entity_type; the step-level limit: is the engine's cap per parent, as
	// with: {limit: N} is on a source (ADR-047).
	if ps.Role == adapters.RoleTraverse {
		ps.IsTraverse = true
		ps.From = resolved.Manifest.From
		ps.Relation = resolved.Manifest.Relation
		ps.Limit = s.Limit
		if isSource {
			problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
				Msg: fmt.Sprintf("%s is a traverse and cannot be the source — it traverses from the records a source (or a group) provides (SPEC §6)", s.Use)})
		}
	} else if s.Limit > 0 && !isSource {
		problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
			Msg: fmt.Sprintf("limit: is only valid on a group source or a traverse step (%s has role %q) — SPEC §9", s.Use, ps.Role)})
	}
	// An entity-agnostic manifest (SPEC §6, ADR-033 — the participant steps)
	// takes the pipeline's entity type, so uses:/provides: and its static
	// schemas validate against the registry the records actually belong to.
	// A source has no pipeline type to take.
	isAI := resolved.Manifest.IsAI()
	participant := resolved.Manifest.IsParticipant()
	ps.Participant = adapters.ParticipantKind(resolved.Manifest.ID)
	if resolved.Manifest.EntityAgnostic() {
		if isSource {
			problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
				Msg: fmt.Sprintf("%s declares entity_type \"*\" and cannot be the source — a source names the entity type its records are (SPEC §6)", s.Use)})
		}
		ps.EntityType = scope.EntityType
	}
	ps.Needs = resolved.Manifest.NeedsFields()
	ps.Required = resolved.Manifest.RequiredNeeds()
	ps.CostEstimate = resolved.Manifest.CostEstimate
	if rate := resolved.Manifest.CostRate; rate != nil {
		// The operator's figure (ADR-046), or its absence made visible.
		if v, ok := rate(ps.Config); ok {
			ps.CostEstimate = &v
		} else {
			ps.CostUnset = true
		}
	}
	ps.Batch = resolved.Manifest.Batch
	ps.NeedsAll = len(ps.Needs) == 0 && adapters.Wildcard(resolved.Manifest.Needs)

	// The deliver-only keys are role-gated (ADR-031); a deliver step reads
	// them, everything else rejects them.
	if ps.IsDeliver {
		ps.RecordGroup = strings.TrimSpace(s.Record)
		if s.Suppress != nil {
			ps.SuppressGroup = strings.TrimSpace(s.Suppress.Group)
			d, err := pipeline.ParseCache(s.Suppress.Within)
			if err != nil {
				problems = append(problems, Problem{Step: s.ID, Kind: KindConfig, Msg: "suppress.within: " + err.Error()})
			}
			ps.SuppressWithin = d
		}
	} else {
		participant := resolved.Manifest.IsParticipant()
		gateDeliverKeys(participant)
		if participant {
			// A declared field absent at run time (SPEC §7, ADR-053): run
			// (the default) dispatches and the receipt counts it.
			ps.OnMissing = s.OnMissing
			if ps.OnMissing == "" {
				ps.OnMissing = "run"
			}
		}
	}
	gateProvides(participant)

	reg, regErr := registry.Load()
	if regErr != nil {
		problems = append(problems, Problem{Step: s.ID, Kind: KindAdapter, Msg: regErr.Error()})
	}

	// Declared AI provides (SPEC §7, ADR-033): the step's effective provides
	// derive from its provides: declaration — names namespaced by pipeline
	// unless already namespaced — and replace the manifest's static shape.
	if s.Provides != nil && participant && ps.Role != adapters.RoleSource {
		decl, err := s.ProvidesFields()
		if err != nil {
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig, Msg: err.Error()})
		} else {
			schema, notes, declProblems := deriveAIProvides(decl, ps.Role, scope.Pipeline, ps.EntityType, reg)
			for _, msg := range declProblems {
				problems = append(problems, Problem{Step: s.ID, Kind: KindContract, Msg: msg})
			}
			ps.Notes = append(ps.Notes, notes...)
			if len(declProblems) == 0 {
				compiled, err := adapters.CompileSchema(s.ID+"/provides", schema)
				if err != nil {
					problems = append(problems, Problem{Step: s.ID, Kind: KindConfig, Msg: err.Error()})
				} else {
					ps.AIProvides = schema
					ps.aiProvides = compiled
				}
			}
		}
	}
	if _, ok := ps.Config["provides"]; ok && participant {
		problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
			Msg: "provides: is a step-level key, not a with: key — move it out of with: (SPEC §9, ADR-033)"})
	}
	if _, ok := ps.Config[adapters.OfConfigKey]; ok && participant {
		problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
			Msg: "of: is a step-level key, not a with: key — move it out of with: (SPEC §9, ADR-048)"})
	}
	// A review declares its labels (SPEC §10.3a, ADR-048): there is no
	// default shape for "what is true of this value".
	if participant && ps.Role == adapters.RoleReview && s.Provides == nil {
		problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
			Msg: fmt.Sprintf("%s is a review and declares its labels — add provides: (a grade enum, a yes/no, notes; ADR-048)", s.Use)})
	}

	// uses: (ADR-004) narrows an AI-backed step's needs-all wildcard to an
	// explicit, plan-time-checkable list. It overrides the manifest's static
	// needs entirely for projection and validation — see planner.Step.Needs's
	// doc and runner.prepare, which project exactly this list once it is set.
	if len(s.Uses) > 0 {
		if !adapters.ParticipantRole(ps.Role) {
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
				Msg: fmt.Sprintf("uses: is only valid on filter/compose/review steps (%s has role %q)", s.Use, ps.Role)})
		}
		ps.Needs = append([]string(nil), s.Uses...)
		ps.Required = append([]string(nil), s.Uses...)
		ps.Uses = append([]string(nil), s.Uses...)
		ps.NeedsAll = false
	}

	// Dynamic needs (SPEC §6, ADR-019): a dynamic participant step with no
	// uses: falls back to needs-all; a dynamic deliver step derives its needs
	// from variables: on top of its static floor.
	dynamic := resolved.Manifest.NeedsDynamic()
	if dynamic && len(s.Uses) == 0 && adapters.ParticipantRole(ps.Role) {
		ps.NeedsAll = true
	}

	// The referent (SPEC §7/§9, ADR-048): of: names the value a compose or
	// review step is about — required on a review — and is validated exactly
	// as one more uses: entry. A human/agent step's render: fields are
	// validated the same way (ADR-049). Neither narrows a needs-all
	// projection: a compose with of: and no uses: still sees everything.
	if of := strings.TrimSpace(s.Of); of != "" {
		switch {
		case !participant || (ps.Role != adapters.RoleCompose && ps.Role != adapters.RoleReview):
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
				Msg: fmt.Sprintf("of: is only valid on compose and review steps of a participant adapter (%s has role %q) — ADR-048", ps.Use, ps.Role)})
		default:
			ps.Of = of
			ps.Needs = appendMissing(ps.Needs, of)
			ps.Required = appendMissing(ps.Required, of)
		}
	} else if participant && ps.Role == adapters.RoleReview {
		problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
			Msg: fmt.Sprintf("%s is a review and needs of: — the field whose value it judges (ADR-048)", s.Use)})
	}
	if resolved.Manifest.RunnerOwned() {
		if render, ok := ps.Config["render"].(map[string]any); ok {
			ps.RenderFields = configStrings(render["fields"])
			ps.RenderTemplate, _ = render["template"].(string)
			for _, f := range ps.RenderFields {
				ps.Needs = appendMissing(ps.Needs, f)
				ps.Required = appendMissing(ps.Required, f)
			}
		}
		ps.Prompt, _ = ps.Config["prompt"].(string)
		switch {
		case ps.Participant == adapters.KindAgent:
			ps.Prompt = "never" // an agent/* step never prompts (ADR-049)
		case ps.Prompt == "":
			ps.Prompt = "tty"
		}
	}
	// A dynamic enrich step (http/enrich, SPEC §10a) derives its needs from
	// the {{record.<field>}} placeholders its config templates reference.
	if dynamic && ps.Role == adapters.RoleEnrich {
		refs := recordRefs(ps.Config)
		ps.Needs = refs
		ps.Required = append([]string(nil), refs...)
		ps.NeedsAll = false
	}
	if ps.IsDeliver && len(s.Variables) > 0 {
		if !dynamic {
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
				Msg: fmt.Sprintf("%s does not declare dynamic needs, so variables: has nothing to derive (SPEC §6)", s.Use)})
		} else {
			ps.Variables = s.Variables
			for _, field := range variableFields(s.Variables) {
				if !containsStr(ps.Needs, field) {
					ps.Needs = append(ps.Needs, field)
				}
				if !containsStr(ps.Required, field) {
					ps.Required = append(ps.Required, field)
				}
			}
			sort.Strings(ps.Needs)
			sort.Strings(ps.Required)
		}
	}
	if ps.IsDeliver {
		ps.OnMissing = s.OnMissing
		if ps.OnMissing == "" {
			ps.OnMissing = "skip"
		}
		gateOnMissingRun()
	}
	ps.NeedsBranches = resolved.Manifest.NeedsBranches()

	// Registry enforcement, layer 1 (SPEC §4a): every field named in uses: or
	// variables: must be canonical for the entity type or vendor-namespaced;
	// namespaced needs are noted (vendor coupling made visible).
	if reg != nil {
		for _, name := range s.Uses {
			if err := reg.ValidateName(ps.EntityType, name); err != nil {
				problems = append(problems, Problem{Step: s.ID, Kind: KindContract, Msg: "uses: " + err.Error()})
			}
		}
		if ps.Of != "" {
			if err := reg.ValidateName(ps.EntityType, ps.Of); err != nil {
				problems = append(problems, Problem{Step: s.ID, Kind: KindContract, Msg: "of: " + err.Error()})
			}
		}
		for _, name := range ps.RenderFields {
			if err := reg.ValidateName(ps.EntityType, name); err != nil {
				problems = append(problems, Problem{Step: s.ID, Kind: KindContract, Msg: "render.fields: " + err.Error()})
			}
		}
		for _, field := range variableFields(s.Variables) {
			if err := reg.ValidateName(ps.EntityType, field); err != nil {
				problems = append(problems, Problem{Step: s.ID, Kind: KindContract, Msg: "variables: " + err.Error()})
			}
		}
		for _, name := range append(append([]string{}, s.Uses...), variableFields(s.Variables)...) {
			if !registry.IsNamespaced(name) {
				continue
			}
			if strings.HasPrefix(name, scope.Pipeline+".") {
				// This pipeline's own declared AI output (ADR-033): per-campaign
				// by design, not a vendor coupling.
				ps.Notes = append(ps.Notes,
					fmt.Sprintf("needs this pipeline's own judgment field %q (declared by an earlier AI step, ADR-033)", name))
				continue
			}
			ps.Notes = append(ps.Notes,
				fmt.Sprintf("needs vendor-namespaced field %q — this pipeline is coupled to that vendor", name))
		}
		// Manifest static schemas get the same check (an external adapter's
		// authoring error surfaces here rather than as a silent mismatch). A
		// traverse's needs are of its from type (SPEC §6, ADR-054).
		needsType := ps.EntityType
		if ps.IsTraverse {
			needsType = ps.From
		}
		for _, name := range resolved.Manifest.NeedsFields() {
			if err := reg.ValidateName(needsType, name); err != nil {
				problems = append(problems, Problem{Step: s.ID, Kind: KindAdapter,
					Msg: fmt.Sprintf("manifest needs: %v", err)})
			}
		}
		// A step that declares its own provides (ADR-033) replaces the
		// manifest's static shape, so the static names are not its contract.
		if len(ps.AIProvides) == 0 {
			for _, name := range resolved.Manifest.ProvidesFields() {
				if err := reg.ValidateName(ps.EntityType, name); err != nil {
					msg := fmt.Sprintf("manifest provides: %v", err)
					if isAI {
						msg += " — or declare provides: on this step (ADR-033)"
					}
					problems = append(problems, Problem{Step: s.ID, Kind: KindAdapter, Msg: msg})
				}
			}
		}
	}

	// Only the head is position-bound: a source at the head and nowhere else.
	// A deliver adapter is an ordinary step, any position after it (ADR-031).
	switch {
	case isSource && ps.Role != adapters.RoleSource:
		problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
			Msg: fmt.Sprintf("%s has role %q but is used as the source", s.Use, ps.Role)})
	case !isSource && ps.Role == adapters.RoleSource:
		problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
			Msg: fmt.Sprintf("%s is a source adapter and can only be the pipeline source", s.Use)})
	}

	// engine: is not a key (SPEC §2/§9, ADR-050): the API is the only model
	// engine, the fixture engine is selected by environment, and the retired
	// claude-code shell-out's replacement is named — an agent/* step the
	// agent answers itself. Checked before config validation so the fix,
	// not "additional property", is what the operator reads.
	config := ps.Config
	if _, ok := ps.Config["engine"]; ok && participant {
		problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
			Msg: "engine: is not a key (ADR-050) — the API is the only model engine (the fixture engine is selected by GTME_AI_ENGINE, never in YAML); for engine: claude-code, make this an agent/* step (agent/filter, agent/compose, agent/review) and answer it with `gtme answer --as claude-code`"})
		config = make(map[string]any, len(ps.Config))
		for k, v := range ps.Config {
			if k != "engine" {
				config[k] = v
			}
		}
	}
	// limit is the engine's on a source binding (ADR-047): validated only
	// when the binding declares it, capped by the engine either way.
	if err := resolved.Manifest.ValidateConfig(withoutReservedKeys(resolved, config)); err != nil {
		problems = append(problems, Problem{Step: s.ID, Kind: KindConfig, Msg: err.Error()})
	}

	// batch_size is config for AI steps.
	if v, ok := ps.Config["batch_size"]; ok {
		switch n := v.(type) {
		case float64:
			ps.BatchSize = int(n)
		case int:
			ps.BatchSize = n
		}
		if ps.BatchSize < 1 {
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig, Msg: "batch_size must be >= 1"})
			ps.BatchSize = DefaultBatchSize
		}
	}

	ps.Respend = s.Respend
	// cache: 0d on a participant step is the judgment cache switched off
	// (SPEC §7, ADR-039) — the same thing respend: true says.
	if participant && strings.TrimSpace(s.Cache) == "0d" {
		ps.Respend = true
	}
	// Deferred (ADR-038): adapter config on an AI step; the last-step rule
	// is checked by Build, which knows the position.
	if v, ok := ps.Config["deferred"].(bool); ok && v && isAI {
		ps.Deferred = true
	}

	// Cache window: step override, else config freshness_days (http/enrich's
	// mandatory content freshness doubles as its cache window, SPEC §10a),
	// else the manifest's freshness_days.
	if s.Cache != "" {
		d, err := pipeline.ParseCache(s.Cache)
		if err != nil {
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig, Msg: err.Error()})
		}
		ps.Cache = d
	} else if days := intConfig(ps.Config, "freshness_days"); days > 0 {
		ps.Cache = time.Duration(days) * 24 * time.Hour
	} else if days := resolved.Manifest.FreshnessDays; days > 0 {
		ps.Cache = time.Duration(days) * 24 * time.Hour
	}

	if isSource && len(ps.Required) > 0 {
		problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
			Msg: fmt.Sprintf("a source cannot require input fields (%s)", strings.Join(ps.Required, ", "))})
	}

	// Provides: a config-specific probe wins over the static manifest schema,
	// and a declared AI shape (ADR-033) over both.
	ps.ProvidesSchema = resolved.Manifest.Provides
	if len(ps.AIProvides) > 0 {
		ps.ProvidesSchema = ps.AIProvides
	} else if probed, err := resolved.ProbeSchema(ps.Config); err != nil {
		problems = append(problems, Problem{Step: s.ID, Kind: KindConfig, Msg: err.Error()})
	} else if len(probed) > 0 {
		ps.ProvidesSchema = probed
		// Dynamic provides (SPEC §7): config-declared output names get the
		// same registry gate static provides do.
		if reg != nil {
			for _, name := range adapters.SchemaProperties(probed) {
				if err := reg.ValidateName(ps.EntityType, name); err != nil {
					problems = append(problems, Problem{Step: s.ID, Kind: KindContract, Msg: err.Error()})
				}
			}
		}
	}
	ps.Provides = schemaProperties(ps.ProvidesSchema)
	ps.Wildcard = adapters.Wildcard(ps.ProvidesSchema)
	// The adapter–type contract for a traverse (SPEC §4a, ADR-054): the
	// types resolve, provides is canonical for the output type, and every
	// emitted child can be keyed. A source's contract is judged in Build.
	if ps.IsTraverse && reg != nil {
		if _, err := reg.Resolve(ps.From); err != nil {
			problems = append(problems, Problem{Step: s.ID, Kind: KindContract, Msg: "from: " + err.Error()})
		}
		contract, _ := reg.Contract(resolved.Manifest, ps.EntityType, ps.Provides, ps.Wildcard)
		for _, cp := range contract {
			kind := KindContract
			if cp.Check == "provides" {
				kind = KindAdapter
			}
			problems = append(problems, Problem{Step: s.ID, Kind: kind, Msg: cp.Msg})
		}
	}

	// Credentials must be resolvable before we start (SPEC §7.3).
	creds, missing := secrets.Resolve(resolved.Manifest.Credentials)
	ps.Credentials = creds
	for _, name := range missing {
		problems = append(problems, Problem{Step: s.ID, Kind: KindCredential,
			Msg: fmt.Sprintf("missing credential %s (set it in the environment or run `gtme secret set %s`)", name, name)})
	}
	optional, missingOptional := secrets.Resolve(resolved.Manifest.CredentialsOptional)
	for k, v := range optional {
		ps.Credentials[k] = v
	}
	ps.MissingOptional = missingOptional

	// Auth declared in an http/* step's config resolves through the same
	// machinery as manifest credentials (SPEC §10a, v0.10): env first, then
	// ~/.gtme/secrets, plan-checked.
	if a, ok := ps.Config["auth"].(map[string]any); ok {
		if env, ok := a["env"].(string); ok && strings.TrimSpace(env) != "" {
			authCreds, authMissing := secrets.Resolve([]string{strings.TrimSpace(env)})
			for k, v := range authCreds {
				ps.Credentials[k] = v
			}
			for _, name := range authMissing {
				problems = append(problems, Problem{Step: s.ID, Kind: KindCredential,
					Msg: fmt.Sprintf("missing credential %s (set it in the environment or run `gtme secret set %s`)", name, name)})
			}
		}
	}

	if ps.IsDeliver {
		ps.Idempotency = s.Idempotency
		// Redeliver (ADR-045): explicit step policy, else the adapter's
		// default — on_change for a natively idempotent target, never
		// otherwise.
		ps.RedeliverMode = s.Redeliver
		if ps.RedeliverMode == "" {
			ps.RedeliverMode = "never"
			if ps.Manifest != nil && ps.Manifest.Idempotency == "native" {
				ps.RedeliverMode = "on_change"
			}
		}
		// http/deliver MUST be told its idempotency key (ADR-023, SPEC §10a):
		// even the trivial case cannot infer delivery semantics.
		if resolved.Manifest.ID == "http/deliver" && strings.TrimSpace(s.Idempotency) == "" {
			problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
				Msg: "http/deliver requires idempotency: — a generic target cannot infer delivery semantics, it must be told (ADR-023)"})
		}
		if ps.RedeliverMode != "never" && (ps.Manifest == nil || ps.Manifest.Idempotency != "native") {
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
				Msg: fmt.Sprintf("redeliver: %s needs a natively idempotent target — %s does not declare idempotency: native (§6, ADR-045), so repeats could duplicate; only `never` is safe here", ps.RedeliverMode, s.Use)})
		}
	}
	return ps, problems
}

// reservedOutputNames are the element keys the AI output shape already owns
// (SPEC §10.3): a declared field may not shadow them.
var reservedOutputNames = map[string]string{
	"identity_key": "every AI role",
	"pass":         "filter",
}

// deriveAIProvides turns a step's provides: declaration into its effective
// provides schema (SPEC §7, ADR-033): each name lands as written when it is
// already namespaced or marked canonical (a registry-checked claim), else as
// <pipeline>.<name> (SPEC §4a — a judgment is a fact about working the entity
// in one campaign; two campaigns' judgments about one identity must not
// collide). The schema carries the declared type
// and enum per field, requires every field, and admits nothing else. Notes
// surface where a bare name coincides with a canonical field, so the operator
// sees that the canonical field is untouched.
func deriveAIProvides(decl []pipeline.ProvidesField, role, pipelineName, entityType string, reg *registry.Registry) (json.RawMessage, []string, []string) {
	var problems, notes []string
	props := map[string]any{}
	required := make([]string, 0, len(decl))
	for _, f := range decl {
		if owner, ok := reservedOutputNames[f.Name]; ok && (owner == "every AI role" || owner == role) {
			problems = append(problems, fmt.Sprintf("provides: %q is reserved by the AI output shape (SPEC §10.3) — choose another name", f.Name))
			continue
		}
		name := f.Name
		switch {
		case f.Canonical:
			// The declared name IS the canonical field (SPEC §7): global, not
			// per-campaign — so the claim is checked against the registry,
			// type and domain included, before anything can land there.
			if reg != nil && reg.Known(entityType) {
				entry, ok := reg.Lookup(entityType, f.Name)
				if !ok {
					msg := fmt.Sprintf("provides: %q is marked canonical but is not a canonical %s field (see spec/fields/%s.json)", f.Name, entityType, entityType)
					if sugg := reg.Suggest(entityType, f.Name); sugg != "" {
						msg += fmt.Sprintf(" — did you mean %q?", sugg)
					}
					problems = append(problems, msg)
					continue
				}
				if f.Type != "" && f.Type != entry.Type {
					problems = append(problems, fmt.Sprintf("provides: %q declares type %s but the canonical %s field is %s", f.Name, f.Type, entityType, entry.Type))
					continue
				}
				if len(f.Enum) > 0 && entry.Type != "string" {
					problems = append(problems, fmt.Sprintf("provides: %q declares an enum but the canonical %s field is %s, not string", f.Name, entityType, entry.Type))
					continue
				}
				if len(entry.Enum) > 0 {
					for _, v := range f.Enum {
						if !containsStr(entry.Enum, v) {
							problems = append(problems, fmt.Sprintf("provides: %q enum value %q is outside the canonical domain %v", f.Name, v, entry.Enum))
						}
					}
				}
			}
		case !registry.IsNamespaced(name):
			name = pipelineName + "." + f.Name
			if reg != nil {
				if _, canonical := reg.Lookup(entityType, f.Name); canonical {
					notes = append(notes, fmt.Sprintf("provides: %q lands as %q (per-campaign, ADR-033); the canonical %s field %q is untouched — add canonical: true to write it instead",
						f.Name, name, entityType, f.Name))
				}
			}
		}
		if _, dup := props[name]; dup {
			problems = append(problems, fmt.Sprintf("provides: %q resolves to %q, which another declared field already uses", f.Name, name))
			continue
		}
		spec := map[string]any{}
		if f.Type != "" {
			spec["type"] = f.Type
		}
		if len(f.Enum) > 0 {
			spec["type"] = "string"
			spec["enum"] = f.Enum
		}
		props[name] = spec
		required = append(required, name)
	}
	if len(problems) > 0 {
		return nil, notes, problems
	}
	raw, err := json.Marshal(map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           props,
		"required":             required,
	})
	if err != nil {
		return nil, notes, []string{err.Error()}
	}
	return raw, notes, nil
}

// ReferencedGroups lists every group the plan requires to EXIST at plan time
// (SPEC §7): require:/exclude:/suppress: references and the source group.
// record: targets and the terminus create on demand and are not listed.
func (p *Plan) ReferencedGroups() []string {
	seen := map[string]bool{}
	for i := range p.Steps {
		s := &p.Steps[i]
		for _, g := range s.Require {
			seen[g] = true
		}
		for _, g := range s.Exclude {
			seen[g] = true
		}
		if s.SuppressGroup != "" {
			seen[s.SuppressGroup] = true
		}
		if s.IsGroupSource {
			seen[s.SourceGroup] = true
		}
	}
	return keys(seen)
}

// CheckGroups resolves the plan's group references against the ledger —
// read-only, zero network calls, zero spend (SPEC §7). A missing group is a
// contract error naming the fix.
func (p *Plan) CheckGroups(ctx context.Context, l *ledger.Ledger) error {
	var problems []Problem
	for _, name := range p.ReferencedGroups() {
		if _, err := l.GetGroup(ctx, name); err != nil {
			if errors.Is(err, ledger.ErrNotFound) {
				problems = append(problems, Problem{Kind: KindContract,
					Msg: fmt.Sprintf("group %q does not exist — create it with `gtme groups add %s <identity-key>...` or snapshot a segment with `gtme groups add %s --from-segment <name>`", name, name, name)})
				continue
			}
			return err
		}
	}
	// The terminus and every group/deliver are checked against their
	// group's type (SPEC §7, ADR-054): a company run ending in a group of
	// people fails naming the group, its type, and the pipeline's type. A
	// group created on demand takes the type at run time.
	check := func(step, name, entityType string) error {
		g, err := l.GetGroup(ctx, name)
		if errors.Is(err, ledger.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if g.EntityType != "" && entityType != "" && g.EntityType != entityType {
			problems = append(problems, Problem{Step: step, Kind: KindContract,
				Msg: fmt.Sprintf("group %q holds %s records, but this pipeline would add %s records to it (SPEC §7, ADR-054) — end in a group of the pipeline's type, or traverse to %s first",
					name, g.EntityType, entityType, g.EntityType)})
		}
		return nil
	}
	if name := strings.TrimSpace(p.Pipeline.Group); name != "" {
		if err := check("", name, p.FinalType); err != nil {
			return err
		}
	}
	for i := range p.Steps {
		if st := &p.Steps[i]; st.IsGroupDeliver {
			if err := check(st.ID, st.TargetGroup, st.EntityType); err != nil {
				return err
			}
		}
	}
	if len(problems) > 0 {
		return &Errors{Problems: problems}
	}
	// A `once:` source's eligible count is a plan-time fact (ADR-052 (6)).
	for i := range p.Steps {
		if s := &p.Steps[i]; s.IsGroupSource && s.Once {
			if err := p.countOnce(ctx, l, s); err != nil {
				return err
			}
		}
	}
	return nil
}

// PrevState is the run_records state a record must be in to be eligible for
// step i (SPEC §7): the id of the previous step, or 'sourced' for the first.
func (p *Plan) PrevState(i int) string {
	if i <= 1 {
		return "sourced"
	}
	return p.Steps[i-1].ID
}

// intConfig reads a numeric config value (int after YAML decode, float64
// after a JSON round trip).
func intConfig(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	default:
		return 0
	}
}

// configStrings reads a config list of strings ([]any after YAML decode).
func configStrings(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range list {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	sort.Strings(out)
	return out
}

var recordRefPattern = regexp.MustCompile(`\{\{\s*record\.([A-Za-z0-9_.]+)`)

// recordRefs finds every {{record.<field>}} placeholder in a step's config —
// the derived dynamic needs of a templated enrich step (SPEC §10a).
func recordRefs(v any) []string {
	seen := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			for _, m := range recordRefPattern.FindAllStringSubmatch(t, -1) {
				seen[m[1]] = true
			}
		case map[string]any:
			for _, item := range t {
				walk(item)
			}
		case []any:
			for _, item := range t {
				walk(item)
			}
		}
	}
	walk(v)
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// variableFields lists a variables: mapping's ledger fields (its values),
// sorted and de-duplicated — the config-derived half of a deliver step's
// dynamic needs (SPEC §6).
func variableFields(vars map[string]string) []string {
	if len(vars) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, f := range vars {
		seen[f] = true
	}
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func anyBranchAvailable(branches [][]string, available map[string]bool) bool {
	for _, branch := range branches {
		if len(branch) == 0 {
			continue
		}
		ok := true
		for _, f := range branch {
			if !available[f] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func describeBranches(branches [][]string, available map[string]bool) string {
	parts := make([]string, 0, len(branches))
	for _, b := range branches {
		var missing []string
		for _, f := range b {
			if !available[f] {
				missing = append(missing, f)
			}
		}
		desc := "[" + strings.Join(b, ", ") + "]"
		if len(missing) > 0 {
			desc += " (missing " + strings.Join(missing, ", ") + ")"
		}
		parts = append(parts, desc)
	}
	return strings.Join(parts, " or ")
}

// appendMissing adds s to list unless it is already there.
func appendMissing(list []string, s string) []string {
	if containsStr(list, s) {
		return list
	}
	return append(list, s)
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func describe(available map[string]bool) string {
	if len(available) == 0 {
		return "nothing"
	}
	return strings.Join(keys(available), ", ")
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func schemaProperties(raw json.RawMessage) []string {
	var doc struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	out := make([]string, 0, len(doc.Properties))
	for k := range doc.Properties {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// crossRecordTables are the objects whose presence in a query marks it
// cross-record (SPEC §7, ADR-037): it joins beyond the record's own facts.
var crossRecordTables = []string{"relations", "group_members", "group_membership"}

// crossRecordRefs lists the cross-record objects a query names, as whole
// words, in a stable order.
func crossRecordRefs(query string) []string {
	var out []string
	for _, name := range crossRecordTables {
		if regexp.MustCompile(`\b` + name + `\b`).MatchString(query) {
			out = append(out, name)
		}
	}
	return out
}

// explainQuery runs EXPLAIN QUERY PLAN on the read-only connection (SPEC
// §7): SQLite resolves every table and column without executing anything.
// :run_id is bound when referenced, as the runner binds it.
func explainQuery(scope Scope, query string) error {
	if scope.Ledger == nil {
		return nil // no ledger to plan against; the run will check
	}
	db, err := ledger.OpenReadOnly(scope.Ctx, scope.Ledger.Path())
	if err != nil {
		return err
	}
	defer db.Close()
	var args []any
	if strings.Contains(query, ":run_id") {
		args = append(args, sql.Named("run_id", "plan"))
	}
	rows, err := db.QueryContext(scope.Ctx, "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		return err
	}
	return rows.Close()
}

var groupNamePattern = regexp.MustCompile(`group_name\s*=\s*'([^']+)'`)

// groupsRead names the groups a query over group_membership reads, by the
// group_name literals it compares against — the vocabulary view's key
// (SPEC §10a). Empty when the query does not read group_membership.
func groupsRead(query string) []string {
	if !regexp.MustCompile(`\bgroup_membership\b`).MatchString(query) {
		return nil
	}
	var out []string
	for _, m := range groupNamePattern.FindAllStringSubmatch(query, -1) {
		out = append(out, strconv.Quote(m[1]))
	}
	return out
}

// maxShownRows bounds how many resolved values a plan note lists.
const maxShownRows = 10

// resolveConfigValues walks a step's config and substitutes every
// {query: SQL} / {segment: NAME} value with the ledger's answer (SPEC §7/§9,
// ADR-037): one column → a list, one row and one column → a scalar; zero
// rows is a plan error (an empty list handed to a vendor search is the shape
// that searches everything), as is any other column shape. Returns a copy;
// the pipeline's own config is never mutated. path names the value in
// notes and errors ("with.domains").
func resolveConfigValues(scope Scope, path string, v any) (any, []string, []string) {
	switch t := v.(type) {
	case map[string]any:
		if kind, text, ok, malformed := configQuery(t); malformed {
			return v, nil, []string{fmt.Sprintf("%s: {%s: …} must carry a non-empty string (SPEC §9)", path, kind)}
		} else if ok {
			value, note, err := resolveConfigQuery(scope, path, kind, text)
			if err != nil {
				return v, nil, []string{err.Error()}
			}
			return value, []string{note}, nil
		}
		return resolveConfigMap(scope, path, t)
	case []any:
		out := make([]any, len(t))
		var notes, problems []string
		for i, item := range t {
			r, n, p := resolveConfigValues(scope, fmt.Sprintf("%s[%d]", path, i), item)
			out[i] = r
			notes = append(notes, n...)
			problems = append(problems, p...)
		}
		return out, notes, problems
	default:
		return v, nil, nil
	}
}

// resolveConfigMap resolves the values under a map's keys — never the map
// itself, so a step's own with: block (or a sql/* step's with: {query: …})
// is a container, not a value.
func resolveConfigMap(scope Scope, path string, m map[string]any) (map[string]any, []string, []string) {
	out := make(map[string]any, len(m))
	var notes, problems []string
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r, n, p := resolveConfigValues(scope, path+"."+k, m[k])
		out[k] = r
		notes = append(notes, n...)
		problems = append(problems, p...)
	}
	return out, notes, problems
}

// configQuery recognises the two ledger-value forms: a map whose only key is
// query or segment. ok means a well-formed value; malformed means the key is
// there but its value is not a non-empty string — a plan error, never a
// literal handed to the adapter.
func configQuery(m map[string]any) (kind, text string, ok, malformed bool) {
	if len(m) != 1 {
		return "", "", false, false
	}
	for _, k := range []string{"query", "segment"} {
		if v, present := m[k]; present {
			s, isString := v.(string)
			if !isString || strings.TrimSpace(s) == "" {
				return k, "", false, true
			}
			return k, strings.TrimSpace(s), true, false
		}
	}
	return "", "", false, false
}

// resolveConfigQuery runs one config value's SQL read-only and shapes the
// result (SPEC §7).
func resolveConfigQuery(scope Scope, path, kind, text string) (any, string, error) {
	label := fmt.Sprintf("%s ← {%s: %s}", path, kind, text)
	if scope.Ledger == nil {
		return nil, "", fmt.Errorf("%s: resolving a config value from the ledger needs the ledger (run `gtme init`)", path)
	}
	query := text
	if kind == "segment" {
		saved, err := scope.Ledger.SavedQuery(scope.Ctx, text)
		if err != nil {
			return nil, "", fmt.Errorf("%s: no saved segment named %q — save one with `gtme query --save %s \"SQL\"`", path, text, text)
		}
		query = saved.SQL
		label = fmt.Sprintf("%s ← {segment: %s}", path, text)
	}
	if err := ledger.ReadOnlyStatement(query); err != nil {
		return nil, "", fmt.Errorf("%s: %v", path, err)
	}
	db, err := ledger.OpenReadOnly(scope.Ctx, scope.Ledger.Path())
	if err != nil {
		return nil, "", fmt.Errorf("%s: %v", path, err)
	}
	defer db.Close()
	rows, err := db.QueryContext(scope.Ctx, query)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %v", path, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, "", fmt.Errorf("%s: %v", path, err)
	}
	if len(cols) != 1 {
		return nil, "", fmt.Errorf("%s: a config query must yield exactly one column (got %s) — one column is a list, one row and one column a scalar (SPEC §7)", path, strings.Join(cols, ", "))
	}
	var values []any
	for rows.Next() {
		var v any
		if err := rows.Scan(&v); err != nil {
			return nil, "", fmt.Errorf("%s: %v", path, err)
		}
		if b, ok := v.([]byte); ok {
			v = string(b)
		}
		values = append(values, v)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("%s: %v", path, err)
	}
	if len(values) == 0 {
		return nil, "", fmt.Errorf("%s: the %s yielded zero rows — an empty value handed to an adapter is the shape that matches everything; fix the %s or snapshot a group first (SPEC §7)", path, kind, kind)
	}
	shown := make([]string, 0, len(values))
	for i, v := range values {
		if i == maxShownRows {
			shown = append(shown, fmt.Sprintf("… (+%d more)", len(values)-maxShownRows))
			break
		}
		shown = append(shown, fmt.Sprint(v))
	}
	reads := ""
	if groups := groupsRead(query); len(groups) > 0 {
		// The group a {query:} reads (SPEC §7, ADR-054): the chain, visible.
		reads = fmt.Sprintf(" (reads group %s)", strings.Join(groups, ", "))
	}
	if len(values) == 1 {
		return values[0], fmt.Sprintf("%s → 1 row (scalar): %s%s", label, shown[0], reads), nil
	}
	return values, fmt.Sprintf("%s → %d rows (list): %s%s", label, len(values), strings.Join(shown, ", "), reads), nil
}

// withoutReservedKeys drops the engine-owned keys a source binding's
// config_schema need not declare (ADR-047: `limit`) before validation. A
// binding that declares the key keeps it, so its own schema and templates
// see it unchanged.
func withoutReservedKeys(resolved *adapters.Resolved, config map[string]any) map[string]any {
	if !resolved.Binding || (resolved.Manifest.Role != adapters.RoleSource && resolved.Manifest.Role != adapters.RoleTraverse) {
		return config
	}
	if _, declared := config["limit"]; !declared || resolved.Manifest.DeclaresConfig("limit") {
		return config
	}
	out := make(map[string]any, len(config))
	for k, v := range config {
		if k != "limit" {
			out[k] = v
		}
	}
	return out
}
