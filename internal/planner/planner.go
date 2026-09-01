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
	"strings"
	"time"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/identity"
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
	// OnMissing is the deliver completeness policy (SPEC §8): "skip" | "fail".
	OnMissing string
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
	// Warnings are plan-level observations that do not block (SPEC §7): the
	// one-commit-point rule (ADR-032) is the first.
	Warnings []string
}

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

	for i, s := range steps {
		isSource := i == 0

		ps, stepProblems := ResolveStep(s, isSource, scope)
		problems = append(problems, stepProblems...)
		if isSource {
			// The pipeline's entity type is its source's; a group source has
			// none to offer (members may be of any type), so steps after it
			// validate names entity-blind, as SQL steps always have.
			scope.EntityType = ps.EntityType
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
		// An entity_type with no §4 key derivation fails the plan outright: the
		// runner would drop every sourced record at the identity boundary,
		// after the source has been called and billed (#27). Static, so it is
		// judged here, with no network and no spend.
		if isSource && ps.EntityType != "" && !identity.Supported(ps.EntityType) {
			problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
				Msg: fmt.Sprintf("entity_type %q has no identity derivation in this build (SPEC §4 defines %s) — every sourced record would be dropped at the identity boundary",
					ps.EntityType, strings.Join(identity.SupportedTypes(), ", "))})
		} else if isSource && ps.Manifest != nil && !ps.Wildcard && len(ps.Provides) > 0 {
			// A source with an exact (probed, closed) schema is checked for an
			// identity-key path (SPEC §7, ADR-018): no derivable tier is an
			// error, only the name-hash fallback is a note. Judged here and
			// only here — downstream sufficiency is each following step's own
			// needs check.
			strong, weak := identityPath(ps.EntityType, ps.Provides)
			switch {
			case !strong && !weak:
				problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
					Msg: fmt.Sprintf("no identity-key path: none of the source's fields (%s) can derive a %s identity (SPEC §4) — map a column to an identity field with columns:",
						strings.Join(ps.Provides, ", "), ps.EntityType)})
			case !strong:
				ps.Notes = append(ps.Notes,
					"only the name-hash fallback identity tier is derivable from this source; dedupe will be weak until something provides an email or public profile URL")
			}
			// Near-miss columns (SPEC §7): a csv.* leftover a small edit away
			// from a canonical name is SUGGESTED, never silently mapped.
			if reg, err := registry.Load(); err == nil {
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
		}
		if ps.Wildcard {
			plan.Wildcard = true
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
		if (st.Role == adapters.RoleEnrich || st.Role == adapters.RoleVerify) && !st.Manifest.IsAI() && st.Cache <= 0 &&
			(len(st.Manifest.Credentials) > 0 || (st.CostEstimate != nil && *st.CostEstimate > 0)) {
			st.Warnings = append(st.Warnings,
				"respend: this paid step has no freshness window, so every run pays for every record again — set cache: Nd, or say respend: true (SPEC §7, ADR-038)")
		}
	}
	_ = writes

	plan.Available = keys(available)

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
	gateDeliverKeys := func() {
		for _, k := range []struct {
			key string
			set bool
		}{
			{"variables:", len(s.Variables) > 0},
			{"on_missing:", s.OnMissing != ""},
			{"idempotency:", strings.TrimSpace(s.Idempotency) != ""},
			{"record:", strings.TrimSpace(s.Record) != ""},
			{"suppress:", s.Suppress != nil},
		} {
			if k.set {
				problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
					Msg: fmt.Sprintf("%s is only valid on deliver steps (%s has role %q) — ADR-031", k.key, ps.Use, ps.Role)})
			}
		}
	}

	// gateProvides rejects a step-level provides: declaration anywhere but an
	// AI-backed filter/compose step (SPEC §9, ADR-033) — the uses: pattern,
	// third instance. Called once the step's role is known.
	gateProvides := func(isAI bool) {
		if s.Provides == nil {
			return
		}
		switch {
		case ps.Role != adapters.RoleFilter && ps.Role != adapters.RoleCompose:
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
				Msg: fmt.Sprintf("provides: is only valid on AI-backed filter/compose steps (%s has role %q) — ADR-033", ps.Use, ps.Role)})
		case !isAI:
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
				Msg: fmt.Sprintf("provides: is only valid on AI steps (ai/filter, ai/compose); %s takes its outputs from its own contract, not from a step-level declaration — ADR-033", ps.Use)})
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
				Msg: fmt.Sprintf("uses: is only valid on filter/compose steps (%s has role %q)", s.Use, ps.Role)})
		}
		gateProvides(false)
		// Dynamic needs from variables:, no static floor (SPEC §6/§9).
		ps.Variables = s.Variables
		ps.Needs = variableFields(s.Variables)
		ps.Required = append([]string(nil), ps.Needs...)
		ps.OnMissing = s.OnMissing
		if ps.OnMissing == "" {
			ps.OnMissing = "skip"
		}
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
		gateDeliverKeys()
		return ps, problems
	}
	if s.Use == SQLTransformID || s.Use == SQLFilterID {
		ps.IsSQL = true
		if s.Use == SQLTransformID {
			ps.Role = adapters.RoleEnrich
		} else {
			ps.Role = adapters.RoleFilter
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
		gateDeliverKeys()
		gateProvides(false)
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
		ps.Use = "group:" + ps.SourceGroup
		ps.Role = adapters.RoleSource
		ps.Wildcard = true
		gateDeliverKeys()
		gateProvides(false)
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
	// An entity-agnostic manifest (SPEC §6, ADR-033 — the AI steps) takes
	// the pipeline's entity type, so uses:/provides: and its static schemas
	// validate against the registry the records actually belong to. A source
	// has no pipeline type to take.
	isAI := resolved.Manifest.IsAI()
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
		gateDeliverKeys()
	}
	gateProvides(isAI)

	reg, regErr := registry.Load()
	if regErr != nil {
		problems = append(problems, Problem{Step: s.ID, Kind: KindAdapter, Msg: regErr.Error()})
	}

	// Declared AI provides (SPEC §7, ADR-033): the step's effective provides
	// derive from its provides: declaration — names namespaced by pipeline
	// unless already namespaced — and replace the manifest's static shape.
	if s.Provides != nil && isAI && ps.Role != adapters.RoleSource {
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
	if _, ok := ps.Config["provides"]; ok && isAI {
		problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
			Msg: "provides: is a step-level key, not a with: key — move it out of with: (SPEC §9, ADR-033)"})
	}

	// uses: (ADR-004) narrows an AI-backed step's needs-all wildcard to an
	// explicit, plan-time-checkable list. It overrides the manifest's static
	// needs entirely for projection and validation — see planner.Step.Needs's
	// doc and runner.prepare, which project exactly this list once it is set.
	if len(s.Uses) > 0 {
		if ps.Role != adapters.RoleFilter && ps.Role != adapters.RoleCompose {
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
				Msg: fmt.Sprintf("uses: is only valid on filter/compose steps (%s has role %q)", s.Use, ps.Role)})
		}
		ps.Needs = append([]string(nil), s.Uses...)
		ps.Required = append([]string(nil), s.Uses...)
		ps.NeedsAll = false
	}

	// Dynamic needs (SPEC §6, ADR-019): a dynamic filter/compose step with no
	// uses: falls back to needs-all; a dynamic deliver step derives its needs
	// from variables: on top of its static floor.
	dynamic := resolved.Manifest.NeedsDynamic()
	if dynamic && len(s.Uses) == 0 &&
		(ps.Role == adapters.RoleFilter || ps.Role == adapters.RoleCompose) {
		ps.NeedsAll = true
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
		// authoring error surfaces here rather than as a silent mismatch).
		for _, name := range resolved.Manifest.NeedsFields() {
			if err := reg.ValidateName(ps.EntityType, name); err != nil {
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

	// limit is the engine's on a source binding (ADR-047): validated only
	// when the binding declares it, capped by the engine either way.
	if err := resolved.Manifest.ValidateConfig(withoutReservedKeys(resolved, ps.Config)); err != nil {
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
	// cache: 0d on an AI step is the judgment cache switched off (SPEC §7,
	// ADR-039) — the same thing respend: true says.
	if isAI && strings.TrimSpace(s.Cache) == "0d" {
		ps.Respend = true
	}
	// Deferred (ADR-038): adapter config on an AI step; the last-step rule
	// is checked by Build, which knows the position.
	if v, ok := ps.Config["deferred"].(bool); ok && v && isAI {
		ps.Deferred = true
		if engine, _ := ps.Config["engine"].(string); engine == "claude-code" {
			ps.Warnings = append(ps.Warnings,
				"deferred: true has no effect on engine claude-code — it has no batch surface; the step answers synchronously (ADR-038)")
		}
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
	if len(problems) > 0 {
		return &Errors{Problems: problems}
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

// identityPath reports which SPEC §4 identity tiers a field set can derive:
// strong is any non-name-hash tier, weak the name-hash fallback.
func identityPath(entityType string, fields []string) (strong, weak bool) {
	have := map[string]bool{}
	for _, f := range fields {
		have[f] = true
	}
	switch entityType {
	case "person", "":
		strong = have["email"] || have["linkedin_url"] || have["github_username"] || have["twitter_handle"]
		weak = have["full_name"] || have["name"] || (have["first_name"] && have["last_name"])
	case "company":
		strong = have["company_domain"] || have["domain"] || have["website"]
		weak = have["company_name"] || have["name"]
	default:
		// An extensible entity type with no registry vocabulary: nothing to judge.
		return true, true
	}
	return strong, weak
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
	if len(values) == 1 {
		return values[0], fmt.Sprintf("%s → 1 row (scalar): %s", label, shown[0]), nil
	}
	return values, fmt.Sprintf("%s → %d rows (list): %s", label, len(values), strings.Join(shown, ", ")), nil
}

// withoutReservedKeys drops the engine-owned keys a source binding's
// config_schema need not declare (ADR-047: `limit`) before validation. A
// binding that declares the key keeps it, so its own schema and templates
// see it unchanged.
func withoutReservedKeys(resolved *adapters.Resolved, config map[string]any) map[string]any {
	if !resolved.Binding || resolved.Manifest.Role != adapters.RoleSource {
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
