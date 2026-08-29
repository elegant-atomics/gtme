// Package pipeline parses and validates pipeline.yaml (SPEC §9).
package pipeline

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Pipeline is a frozen, recurring workflow. Deliver adapters are ordinary
// Steps entries (ADR-031) — any number, any position; there is no top-level
// deliver block.
type Pipeline struct {
	Name    string `yaml:"name" json:"name"`
	Version int    `yaml:"version" json:"version"`

	Source *Step  `yaml:"source" json:"source,omitempty"`
	Steps  []Step `yaml:"steps,omitempty" json:"steps,omitempty"`

	// Group is the membership terminus (SPEC §8/§9, ADR-021): records that
	// complete the run's final step are `added` to this group, created on
	// demand. Valid with or without deliver steps; it captures completers,
	// not sends (ADR-031).
	Group string `yaml:"group,omitempty" json:"group,omitempty"`

	// Waterfall is reserved syntax: accepted by the parser and rejected with
	// "not implemented in v0" so v1 can add it without a format break (SPEC §9).
	// It is omitted when writing YAML back out, so a frozen pipeline does not
	// advertise a key that does not work yet.
	Waterfall any `yaml:"waterfall,omitempty" json:"-"`
}

// Step is one stage of a pipeline.
type Step struct {
	ID   string         `yaml:"id" json:"id,omitempty"`
	Use  string         `yaml:"use" json:"use"`
	With map[string]any `yaml:"with,omitempty" json:"with,omitempty"`
	// When gates a step; v0 supports only "<step_id>.passed".
	When  string `yaml:"when,omitempty" json:"when,omitempty"`
	Cache string `yaml:"cache,omitempty" json:"cache,omitempty"`
	// Idempotency names the field whose value keys a delivery. Valid only on
	// deliver-role steps; the planner (which knows adapter roles, unlike this
	// package) enforces that, as it does for every deliver-only key (ADR-031).
	Idempotency string `yaml:"idempotency,omitempty" json:"idempotency,omitempty"`
	// Uses declares an AI-backed step's dynamic needs (SPEC §7, §9,
	// DECISIONS.md ADR-004): a static manifest needs schema cannot enumerate
	// fields a free-text prompt references, so the step declares them here.
	// The planner treats Uses exactly as needs.required for plan-time
	// validation and runtime projection. Valid only on filter/compose steps;
	// the planner (which knows adapter roles, unlike this package) enforces
	// that.
	Uses []string `yaml:"uses,omitempty" json:"uses,omitempty"`
	// Provides declares an AI-backed step's output fields (SPEC §7, §9,
	// DECISIONS.md ADR-033): a list of field names, or a map of field name →
	// {type?, enum?, canonical?}. The planner derives the step's effective
	// provides from it (namespaced by pipeline unless already namespaced or
	// marked canonical) and the runtime
	// validates the model's output against the derived schema. Valid only on
	// AI-backed filter/compose steps; the planner enforces that, as with Uses.
	// Kept as decoded YAML here; ProvidesFields is the parsed form.
	Provides any `yaml:"provides,omitempty" json:"provides,omitempty"`
	// Variables is a deliver step's egress mapping (SPEC §9, ADR-018/019):
	// target merge-field name → canonical or namespaced ledger field. Its
	// values are the step's dynamic needs. Valid only on deliver steps.
	Variables map[string]string `yaml:"variables,omitempty" json:"variables,omitempty"`
	// OnMissing is a deliver step's per-record completeness policy (SPEC §8):
	// skip (default) or fail when a variables: target does not resolve. Skip
	// withholds this step's send but the record advances; fail freezes it
	// (ADR-031).
	OnMissing string `yaml:"on_missing,omitempty" json:"on_missing,omitempty"`

	// Group is group-as-source (SPEC §9, ADR-021): valid only on the source
	// step, mutually exclusive with use:. Members are projected from the
	// ledger like any record.
	Group string `yaml:"group,omitempty" json:"group,omitempty"`
	// Limit caps a group source (SPEC §9, ADR-032): at most N current
	// members, oldest-added first — the budget for "work thirty today".
	// Valid only on a group source.
	Limit int `yaml:"limit,omitempty" json:"limit,omitempty"`
	// Require / Exclude are membership gates (SPEC §7, ADR-021): process only
	// current members of every Require group; skip current members of any
	// Exclude group. Valid on interior steps and deliver, not the source.
	Require []string `yaml:"require,omitempty" json:"require,omitempty"`
	Exclude []string `yaml:"exclude,omitempty" json:"exclude,omitempty"`
	// Record is a deliver step's touch scope (SPEC §8, ADR-021): successful
	// deliveries append `touched` events to this group. Defaults to the
	// pipeline name per deliver step (steps sharing the default share the
	// scope, ADR-031); created on demand.
	Record string `yaml:"record,omitempty" json:"record,omitempty"`
	// Suppress skips records touched in a group within a window (SPEC §8,
	// ADR-021). Deliver steps only; the suppressed record advances (ADR-031).
	Suppress *Suppress `yaml:"suppress,omitempty" json:"suppress,omitempty"`

	Waterfall any `yaml:"waterfall,omitempty" json:"-"`
}

// Suppress is a deliver step's contact-policy window (SPEC §8, ADR-021).
type Suppress struct {
	Group  string `yaml:"group" json:"group"`
	Within string `yaml:"within" json:"within"`
}

// DefaultSourceID names the implicit source step.
const DefaultSourceID = "source"

// Load reads and validates a pipeline file.
func Load(path string) (*Pipeline, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pipeline: reading %s: %w", path, err)
	}
	p, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// Parse decodes and validates pipeline YAML.
func Parse(raw []byte) (*Pipeline, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var p Pipeline
	if err := dec.Decode(&p); err != nil {
		if strings.Contains(err.Error(), "field deliver not found in type pipeline.Pipeline") {
			return nil, fmt.Errorf("pipeline: the top-level deliver: block was removed (ADR-031) — deliver adapters are ordinary steps: entries; move the block into steps: (%w)", err)
		}
		return nil, fmt.Errorf("pipeline: %w", err)
	}
	if err := p.normalize(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Marshal renders a pipeline back to YAML (gtme freeze).
func Marshal(p *Pipeline) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(p); err != nil {
		return nil, fmt.Errorf("pipeline: encoding: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("pipeline: encoding: %w", err)
	}
	return buf.Bytes(), nil
}

var whenPattern = regexp.MustCompile(`^([A-Za-z0-9_.\-]+)\.passed$`)

func (p *Pipeline) normalize() error {
	if p.Waterfall != nil {
		return fmt.Errorf("pipeline: waterfall: not implemented in v0")
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("pipeline: name is required")
	}
	if p.Version == 0 {
		p.Version = 1
	}
	if p.Version != 1 {
		return fmt.Errorf("pipeline: unsupported version %d (v0 understands version 1)", p.Version)
	}
	if p.Source == nil {
		return fmt.Errorf("pipeline: source is required")
	}

	seen := map[string]bool{}
	claim := func(s *Step, fallback string) error {
		if s.Waterfall != nil {
			return fmt.Errorf("pipeline: %s: waterfall: not implemented in v0", fallback)
		}
		isGroupSource := s == p.Source && strings.TrimSpace(s.Group) != ""
		if isGroupSource && strings.TrimSpace(s.Use) != "" {
			return fmt.Errorf("pipeline: %s: group: and use: are mutually exclusive on the source (SPEC §9)", fallback)
		}
		if strings.TrimSpace(s.Use) == "" && !isGroupSource {
			return fmt.Errorf("pipeline: %s: use is required", fallback)
		}
		if s.ID == "" {
			s.ID = fallback
		}
		if seen[s.ID] {
			return fmt.Errorf("pipeline: duplicate step id %q", s.ID)
		}
		seen[s.ID] = true
		if s.Cache != "" {
			if _, err := ParseCache(s.Cache); err != nil {
				return fmt.Errorf("pipeline: %s: %w", s.ID, err)
			}
		}
		return nil
	}

	if err := claim(p.Source, DefaultSourceID); err != nil {
		return err
	}
	if p.Source.When != "" {
		return fmt.Errorf("pipeline: source: when is not allowed on the source step")
	}
	for i := range p.Steps {
		if err := claim(&p.Steps[i], fmt.Sprintf("step-%d", i+1)); err != nil {
			return err
		}
	}

	// Value shapes for the deliver-only keys are checked here; WHICH steps may
	// carry them is a role question this package cannot answer — the planner
	// role-gates variables:/on_missing:/idempotency:/record:/suppress: to
	// deliver steps (SPEC §9, ADR-031), the way it gates uses:.
	wellFormed := func(s *Step) error {
		switch s.OnMissing {
		case "", "skip", "fail":
		default:
			return fmt.Errorf("pipeline: %s: on_missing must be \"skip\" or \"fail\" (got %q)", s.ID, s.OnMissing)
		}
		for target, field := range s.Variables {
			if strings.TrimSpace(target) == "" || strings.TrimSpace(field) == "" {
				return fmt.Errorf("pipeline: %s: variables: entries need a non-empty target and field", s.ID)
			}
		}
		if s.Suppress != nil {
			if strings.TrimSpace(s.Suppress.Group) == "" {
				return fmt.Errorf("pipeline: %s: suppress: needs a group", s.ID)
			}
			if _, err := ParseCache(s.Suppress.Within); err != nil {
				return fmt.Errorf("pipeline: %s: suppress.within: %w", s.ID, err)
			}
		}
		if _, err := s.ProvidesFields(); err != nil {
			return fmt.Errorf("pipeline: %s: %w", s.ID, err)
		}
		return nil
	}
	if err := wellFormed(p.Source); err != nil {
		return err
	}
	for i := range p.Steps {
		if err := wellFormed(&p.Steps[i]); err != nil {
			return err
		}
	}

	// Group keys (SPEC §9, ADR-021): group: only on the source (as a source)
	// or top-level (as the terminus); require:/exclude: never on the source.
	for i := range p.Steps {
		if strings.TrimSpace(p.Steps[i].Group) != "" {
			return fmt.Errorf("pipeline: %s: group: is only valid on the source step (as a source) or at the top level (as the terminus)", p.Steps[i].ID)
		}
	}
	if len(p.Source.Require) > 0 || len(p.Source.Exclude) > 0 {
		return fmt.Errorf("pipeline: %s: require:/exclude: are not valid on the source step (SPEC §9)", p.Source.ID)
	}
	// limit: (ADR-032) bounds a group source and nothing else.
	for _, s := range p.AllSteps() {
		if s.Limit == 0 {
			continue
		}
		if s.Limit < 0 {
			return fmt.Errorf("pipeline: %s: limit must be >= 1 (got %d)", s.ID, s.Limit)
		}
		if strings.TrimSpace(s.Group) == "" || s.ID != p.Source.ID {
			return fmt.Errorf("pipeline: %s: limit: is only valid on a group source (SPEC §9, ADR-032)", s.ID)
		}
	}
	for _, s := range p.AllSteps() {
		for _, g := range append(append([]string{}, s.Require...), s.Exclude...) {
			if strings.TrimSpace(g) == "" {
				return fmt.Errorf("pipeline: %s: require:/exclude: entries must be non-empty group names", s.ID)
			}
		}
	}

	// when: may only reference an earlier step, and only with .passed (v0).
	priors := map[string]bool{p.Source.ID: true}
	for i := range p.Steps {
		s := &p.Steps[i]
		if s.When != "" {
			m := whenPattern.FindStringSubmatch(strings.TrimSpace(s.When))
			if m == nil {
				return fmt.Errorf("pipeline: %s: when must look like \"<step_id>.passed\" (got %q)", s.ID, s.When)
			}
			if !priors[m[1]] {
				return fmt.Errorf("pipeline: %s: when references unknown or later step %q", s.ID, m[1])
			}
		}
		priors[s.ID] = true
	}
	return nil
}

// ProvidesField is one declared output field of an AI-backed step (ADR-033).
type ProvidesField struct {
	Name string
	// Type is a JSON-Schema primitive type when declared, else "".
	Type string
	// Enum is the value domain when declared; a value outside it is a
	// validation failure at run time, never stored (SPEC §7).
	Enum []string
	// Canonical marks the declared name as a canonical field of the
	// pipeline's entity type (SPEC §7): the output lands there, global,
	// instead of namespaced by pipeline. The planner validates the claim.
	Canonical bool
}

// providesTypes are the JSON-Schema types a declared field may carry.
var providesTypes = map[string]bool{
	"string": true, "integer": true, "number": true, "boolean": true, "array": true,
}

// ProvidesFields parses a step's provides: declaration (SPEC §9, ADR-033) into
// its fields, in declaration order for the list form and sorted by name for
// the map form. Nil for a step declaring nothing. Shape errors name the field
// and what was wrong with it; WHICH steps may declare provides is the
// planner's role question.
func (s Step) ProvidesFields() ([]ProvidesField, error) {
	if s.Provides == nil {
		return nil, nil
	}
	switch v := s.Provides.(type) {
	case []any:
		if len(v) == 0 {
			return nil, fmt.Errorf("provides: must name at least one field")
		}
		out := make([]ProvidesField, 0, len(v))
		seen := map[string]bool{}
		for _, item := range v {
			name, ok := item.(string)
			if !ok || strings.TrimSpace(name) == "" {
				return nil, fmt.Errorf("provides: list entries must be non-empty field names (got %#v)", item)
			}
			name = strings.TrimSpace(name)
			if seen[name] {
				return nil, fmt.Errorf("provides: %q is declared twice", name)
			}
			seen[name] = true
			out = append(out, ProvidesField{Name: name})
		}
		return out, nil
	case map[string]any:
		if len(v) == 0 {
			return nil, fmt.Errorf("provides: must name at least one field")
		}
		names := make([]string, 0, len(v))
		for name := range v {
			names = append(names, name)
		}
		sort.Strings(names)
		out := make([]ProvidesField, 0, len(names))
		for _, name := range names {
			if strings.TrimSpace(name) == "" {
				return nil, fmt.Errorf("provides: field names must be non-empty")
			}
			f, err := parseProvidesField(strings.TrimSpace(name), v[name])
			if err != nil {
				return nil, err
			}
			out = append(out, f)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("provides: must be a list of field names or a map of field name → {type, enum} (got %T)", s.Provides)
	}
}

// parseProvidesField reads one map-form entry: null or {} declares only the
// name; type and enum are the two keywords SPEC §7 admits.
func parseProvidesField(name string, raw any) (ProvidesField, error) {
	f := ProvidesField{Name: name}
	if raw == nil {
		return f, nil
	}
	spec, ok := raw.(map[string]any)
	if !ok {
		return f, fmt.Errorf("provides: %s: must be null, {}, or a map with type/enum (got %T)", name, raw)
	}
	for k, v := range spec {
		switch k {
		case "type":
			t, ok := v.(string)
			if !ok || !providesTypes[t] {
				return f, fmt.Errorf("provides: %s: type must be one of string, integer, number, boolean, array (got %#v)", name, v)
			}
			f.Type = t
		case "enum":
			list, ok := v.([]any)
			if !ok || len(list) == 0 {
				return f, fmt.Errorf("provides: %s: enum must be a non-empty list of strings", name)
			}
			seen := map[string]bool{}
			for _, item := range list {
				str, ok := item.(string)
				if !ok || strings.TrimSpace(str) == "" {
					return f, fmt.Errorf("provides: %s: enum values must be non-empty strings (got %#v)", name, item)
				}
				if seen[str] {
					return f, fmt.Errorf("provides: %s: enum value %q repeats", name, str)
				}
				seen[str] = true
				f.Enum = append(f.Enum, str)
			}
		case "canonical":
			b, ok := v.(bool)
			if !ok {
				return f, fmt.Errorf("provides: %s: canonical must be true or false (got %#v)", name, v)
			}
			f.Canonical = b
		default:
			return f, fmt.Errorf("provides: %s: unknown keyword %q (a declared field may carry type, enum and canonical, SPEC §7)", name, k)
		}
	}
	if f.Canonical && strings.Contains(name, ".") {
		return f, fmt.Errorf("provides: %s: a canonical name must not contain a dot (SPEC §4a) — drop canonical: true or the namespace", name)
	}
	if len(f.Enum) > 0 && f.Type != "" && f.Type != "string" {
		return f, fmt.Errorf("provides: %s: an enum is a string domain; type %q contradicts it", name, f.Type)
	}
	return f, nil
}

// WhenStep returns the step id a gate depends on, or "" when ungated.
func (s Step) WhenStep() string {
	m := whenPattern.FindStringSubmatch(strings.TrimSpace(s.When))
	if m == nil {
		return ""
	}
	return m[1]
}

var cachePattern = regexp.MustCompile(`^([0-9]+)d$`)

// ParseCache reads a cache window. v0 accepts only "Nd" (SPEC §9).
func ParseCache(s string) (time.Duration, error) {
	m := cachePattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("cache must look like \"30d\" (got %q)", s)
	}
	days, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("cache must look like \"30d\" (got %q)", s)
	}
	return time.Duration(days) * 24 * time.Hour, nil
}

// FormatCache renders a window as "Nd".
func FormatCache(d time.Duration) string {
	return strconv.Itoa(int(d/(24*time.Hour))) + "d"
}

// AllSteps returns the source and every step in execution order.
func (p *Pipeline) AllSteps() []Step {
	out := make([]Step, 0, len(p.Steps)+1)
	if p.Source != nil {
		out = append(out, *p.Source)
	}
	out = append(out, p.Steps...)
	return out
}
