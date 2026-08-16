// Package planner resolves a pipeline into an executable plan and validates the
// contracts between its steps before anything is spent (SPEC §7).
package planner

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/trevorfox/gtm/internal/adapters"
	"github.com/trevorfox/gtm/internal/pipeline"
	"github.com/trevorfox/gtm/internal/registry"
	"github.com/trevorfox/gtm/internal/secrets"
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

	Credentials map[string]string
	// MissingOptional are declared-optional credentials that did not resolve;
	// the plan reports them as warnings, not errors.
	MissingOptional []string
	CostEstimate    *float64

	IsSource  bool
	IsDeliver bool
}

// Plan is a validated, executable pipeline.
type Plan struct {
	Pipeline  *pipeline.Pipeline
	Steps     []Step
	Available []string
	Wildcard  bool
}

// Source is the source step.
func (p *Plan) Source() *Step { return &p.Steps[0] }

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
// spends nothing.
func Build(p *pipeline.Pipeline) (*Plan, error) {
	plan := &Plan{Pipeline: p}
	var problems []Problem

	available := map[string]bool{}
	steps := p.AllSteps()
	nSteps := len(steps)

	for i, s := range steps {
		isSource := i == 0
		isDeliver := p.Deliver != nil && i == nSteps-1

		ps, stepProblems := ResolveStep(s, isSource, isDeliver)
		problems = append(problems, stepProblems...)

		// Contract walk: every required need must already be available.
		if ps.Manifest != nil && !isSource {
			var missing []string
			for _, f := range ps.Required {
				if !available[f] {
					missing = append(missing, f)
				}
			}
			if len(missing) > 0 && !plan.Wildcard {
				problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
					Msg: fmt.Sprintf("needs %s, which no earlier step provides (available: %s)",
						strings.Join(missing, ", "), describe(available))})
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
		// A source with an exact (probed, closed) schema is checked for an
		// identity-key path (SPEC §7, ADR-018): no derivable tier is an error,
		// only the name-hash fallback is a note. Judged here and only here —
		// downstream sufficiency is each following step's own needs check.
		if isSource && ps.Manifest != nil && !ps.Wildcard && len(ps.Provides) > 0 {
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

		plan.Steps = append(plan.Steps, ps)
	}

	plan.Available = keys(available)
	if len(problems) > 0 {
		return plan, &Errors{Problems: problems}
	}
	return plan, nil
}

// ResolveStep resolves one step's adapter, config, credentials, cache window and
// schemas.
func ResolveStep(s pipeline.Step, isSource, isDeliver bool) (Step, []Problem) {
	var problems []Problem
	ps := Step{
		ID:        s.ID,
		Use:       s.Use,
		Config:    s.With,
		When:      s.When,
		WhenStep:  s.WhenStep(),
		IsSource:  isSource,
		IsDeliver: isDeliver,
		BatchSize: DefaultBatchSize,
	}
	if ps.Config == nil {
		ps.Config = map[string]any{}
	}

	resolved, err := adapters.Resolve(s.Use)
	if err != nil {
		return ps, append(problems, Problem{Step: s.ID, Kind: KindAdapter, Msg: err.Error()})
	}
	ps.Adapter = resolved
	ps.Manifest = resolved.Manifest
	ps.Role = resolved.Manifest.Role
	ps.EntityType = resolved.EntityType(ps.Config)
	ps.Needs = resolved.Manifest.NeedsFields()
	ps.Required = resolved.Manifest.RequiredNeeds()
	ps.CostEstimate = resolved.Manifest.CostEstimate
	ps.Batch = resolved.Manifest.Batch
	ps.NeedsAll = len(ps.Needs) == 0 && adapters.Wildcard(resolved.Manifest.Needs)

	reg, regErr := registry.Load()
	if regErr != nil {
		problems = append(problems, Problem{Step: s.ID, Kind: KindAdapter, Msg: regErr.Error()})
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
	if len(s.Variables) > 0 {
		if !isDeliver {
			// pipeline.Parse already rejects this; belt and braces for callers
			// constructing pipelines programmatically.
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig,
				Msg: "variables: is only valid on the deliver step (ADR-018)"})
		} else if !dynamic {
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
	if isDeliver {
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
			if registry.IsNamespaced(name) {
				ps.Notes = append(ps.Notes,
					fmt.Sprintf("needs vendor-namespaced field %q — this pipeline is coupled to that vendor", name))
			}
		}
		// Manifest static schemas get the same check (an external adapter's
		// authoring error surfaces here rather than as a silent mismatch).
		for _, name := range resolved.Manifest.NeedsFields() {
			if err := reg.ValidateName(ps.EntityType, name); err != nil {
				problems = append(problems, Problem{Step: s.ID, Kind: KindAdapter,
					Msg: fmt.Sprintf("manifest needs: %v", err)})
			}
		}
		for _, name := range resolved.Manifest.ProvidesFields() {
			if err := reg.ValidateName(ps.EntityType, name); err != nil {
				problems = append(problems, Problem{Step: s.ID, Kind: KindAdapter,
					Msg: fmt.Sprintf("manifest provides: %v", err)})
			}
		}
	}

	// Role must match position: a source at the head, a deliver at the tail.
	switch {
	case isSource && ps.Role != adapters.RoleSource:
		problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
			Msg: fmt.Sprintf("%s has role %q but is used as the source", s.Use, ps.Role)})
	case !isSource && ps.Role == adapters.RoleSource:
		problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
			Msg: fmt.Sprintf("%s is a source adapter and can only be the pipeline source", s.Use)})
	case isDeliver && ps.Role != adapters.RoleDeliver:
		problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
			Msg: fmt.Sprintf("%s has role %q but is used as the deliver step", s.Use, ps.Role)})
	case !isDeliver && ps.Role == adapters.RoleDeliver:
		problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
			Msg: fmt.Sprintf("%s is a deliver adapter; put it under deliver:", s.Use)})
	}

	if err := resolved.Manifest.ValidateConfig(ps.Config); err != nil {
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

	// Cache window: step override, else the manifest's freshness_days.
	if s.Cache != "" {
		d, err := pipeline.ParseCache(s.Cache)
		if err != nil {
			problems = append(problems, Problem{Step: s.ID, Kind: KindConfig, Msg: err.Error()})
		}
		ps.Cache = d
	} else if days := resolved.Manifest.FreshnessDays; days > 0 {
		ps.Cache = time.Duration(days) * 24 * time.Hour
	}

	if isSource && len(ps.Required) > 0 {
		problems = append(problems, Problem{Step: s.ID, Kind: KindContract,
			Msg: fmt.Sprintf("a source cannot require input fields (%s)", strings.Join(ps.Required, ", "))})
	}

	// Provides: a config-specific probe wins over the static manifest schema.
	ps.ProvidesSchema = resolved.Manifest.Provides
	if probed, err := resolved.ProbeSchema(ps.Config); err != nil {
		problems = append(problems, Problem{Step: s.ID, Kind: KindConfig, Msg: err.Error()})
	} else if len(probed) > 0 {
		ps.ProvidesSchema = probed
	}
	ps.Provides = schemaProperties(ps.ProvidesSchema)
	ps.Wildcard = adapters.Wildcard(ps.ProvidesSchema)

	// Credentials must be resolvable before we start (SPEC §7.3).
	creds, missing := secrets.Resolve(resolved.Manifest.Credentials)
	ps.Credentials = creds
	for _, name := range missing {
		problems = append(problems, Problem{Step: s.ID, Kind: KindCredential,
			Msg: fmt.Sprintf("missing credential %s (set it in the environment or run `gtm secret set %s`)", name, name)})
	}
	optional, missingOptional := secrets.Resolve(resolved.Manifest.CredentialsOptional)
	for k, v := range optional {
		ps.Credentials[k] = v
	}
	ps.MissingOptional = missingOptional

	if isDeliver {
		ps.Idempotency = s.Idempotency
	}
	return ps, problems
}

// PrevState is the run_records state a record must be in to be eligible for
// step i (SPEC §7): the id of the previous step, or 'sourced' for the first.
func (p *Plan) PrevState(i int) string {
	if i <= 1 {
		return "sourced"
	}
	return p.Steps[i-1].ID
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
