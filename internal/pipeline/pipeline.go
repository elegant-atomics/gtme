// Package pipeline parses and validates pipeline.yaml (SPEC §9).
package pipeline

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Pipeline is a frozen, recurring workflow.
type Pipeline struct {
	Name    string `yaml:"name" json:"name"`
	Version int    `yaml:"version" json:"version"`

	Source  *Step  `yaml:"source" json:"source,omitempty"`
	Steps   []Step `yaml:"steps,omitempty" json:"steps,omitempty"`
	Deliver *Step  `yaml:"deliver,omitempty" json:"deliver,omitempty"`

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
	// Idempotency names the field whose value keys a delivery (deliver steps).
	Idempotency string `yaml:"idempotency,omitempty" json:"idempotency,omitempty"`
	// Uses declares an AI-backed step's dynamic needs (SPEC §7, §9,
	// DECISIONS.md ADR-004): a static manifest needs schema cannot enumerate
	// fields a free-text prompt references, so the step declares them here.
	// The planner treats Uses exactly as needs.required for plan-time
	// validation and runtime projection. Valid only on filter/compose steps;
	// the planner (which knows adapter roles, unlike this package) enforces
	// that.
	Uses []string `yaml:"uses,omitempty" json:"uses,omitempty"`

	Waterfall any `yaml:"waterfall,omitempty" json:"-"`
}

// DefaultSourceID and DefaultDeliverID name the implicit steps.
const (
	DefaultSourceID  = "source"
	DefaultDeliverID = "deliver"
)

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
		return nil, fmt.Errorf("pipeline: %w", err)
	}
	if err := p.normalize(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Marshal renders a pipeline back to YAML (gtm freeze).
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
		if strings.TrimSpace(s.Use) == "" {
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
	if p.Deliver != nil {
		if err := claim(p.Deliver, DefaultDeliverID); err != nil {
			return err
		}
	}

	// when: may only reference an earlier step, and only with .passed (v0).
	priors := map[string]bool{p.Source.ID: true}
	check := func(s *Step) error {
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
		return nil
	}
	for i := range p.Steps {
		if err := check(&p.Steps[i]); err != nil {
			return err
		}
	}
	if p.Deliver != nil {
		if err := check(p.Deliver); err != nil {
			return err
		}
	}
	return nil
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

// AllSteps returns source, steps and deliver in execution order.
func (p *Pipeline) AllSteps() []Step {
	out := make([]Step, 0, len(p.Steps)+2)
	if p.Source != nil {
		out = append(out, *p.Source)
	}
	out = append(out, p.Steps...)
	if p.Deliver != nil {
		out = append(out, *p.Deliver)
	}
	return out
}
