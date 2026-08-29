// Package adapters loads adapter manifests and launches adapter sessions.
// Built-in adapters are compiled into the binary; external ones are executables
// discovered on disk. Both are driven through the same protocol boundary
// (SPEC §5, §6).
package adapters

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// Roles an adapter can play (SPEC §6).
const (
	RoleSource  = "source"
	RoleFilter  = "filter"
	RoleEnrich  = "enrich"
	RoleVerify  = "verify"
	RoleCompose = "compose"
	RoleDeliver = "deliver"
)

// Manifest is an adapter's contract.
type Manifest struct {
	ID          string          `json:"id"`
	Version     int             `json:"version"`
	Role        string          `json:"role"`
	EntityType  string          `json:"entity_type"`
	Needs       json.RawMessage `json:"needs,omitempty"`
	Provides    json.RawMessage `json:"provides,omitempty"`
	Credentials []string        `json:"credentials,omitempty"`
	// CredentialsOptional are injected when present but never block a plan: an
	// adapter that can work several ways (an AI step with a local engine, say)
	// should not demand a key it may not use.
	CredentialsOptional []string        `json:"credentials_optional,omitempty"`
	ConfigSchema        json.RawMessage `json:"config_schema,omitempty"`
	FreshnessDays       int             `json:"freshness_days,omitempty"`
	EmitsKeyFields      []string        `json:"emits_key_fields,omitempty"`
	CostEstimate        *float64        `json:"cost_estimate_usd,omitempty"`
	// Batch marks adapters the runner must feed in batches (AI steps), one
	// invocation per batch.
	Batch bool `json:"batch,omitempty"`
	// KeepPayloads / PayloadTTLDays declare ADR-030 retention for raw
	// responses this adapter attaches to its RECORDs (SPEC §5, §6): keep
	// defaults to true, TTL to 90 days; a step config may override keep.
	KeepPayloads   *bool `json:"keep_payloads,omitempty"`
	PayloadTTLDays *int  `json:"payload_ttl_days,omitempty"`

	needs, provides, config *jsonschema.Schema
}

// Source is the provenance string written to field_values.source.
func (m *Manifest) Source() string { return fmt.Sprintf("%s@%d", m.ID, m.Version) }

// AIPrefix names the operation-named AI steps (ADR-026): the adapters whose
// judgment comes from a model rather than a provider.
const AIPrefix = "ai/"

// ProvidesConfigKey is the OPEN config key the runner injects an AI step's
// derived provides schema under (SPEC §7, ADR-033) — the `variables` pattern,
// second instance: never authored inside with:.
const ProvidesConfigKey = "provides"

// IsAI reports an AI step (ADR-026). AI manifests are entity-agnostic
// (SPEC §10.3, ADR-033): the step's entity type is the pipeline's, and they
// are the adapters that derive their provides from a step-level declaration.
func (m *Manifest) IsAI() bool { return strings.HasPrefix(m.ID, AIPrefix) }

// DefaultPayloadTTLDays is ADR-030's default retention window.
const DefaultPayloadTTLDays = 90

// PayloadRetention resolves the ADR-030 declaration against a step's config:
// whether payloads this adapter attaches are kept, and for how many days
// (0 = no expiry).
func (m *Manifest) PayloadRetention(config map[string]any) (keep bool, ttlDays int) {
	keep = m.KeepPayloads == nil || *m.KeepPayloads
	if v, ok := config["keep_payloads"].(bool); ok {
		keep = v
	}
	ttlDays = DefaultPayloadTTLDays
	if m.PayloadTTLDays != nil {
		ttlDays = *m.PayloadTTLDays
	}
	return keep, ttlDays
}

// ParseManifest decodes and validates a manifest document.
func ParseManifest(raw []byte) (*Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("adapters: parsing manifest: %w", err)
	}
	if err := m.compile(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) compile() error {
	if m.ID == "" {
		return fmt.Errorf("adapters: manifest has no id")
	}
	if m.Version <= 0 {
		return fmt.Errorf("adapters: %s: version must be >= 1", m.ID)
	}
	switch m.Role {
	case RoleSource, RoleFilter, RoleEnrich, RoleVerify, RoleCompose, RoleDeliver:
	default:
		return fmt.Errorf("adapters: %s: unknown role %q", m.ID, m.Role)
	}
	if m.EntityType == "" {
		return fmt.Errorf("adapters: %s: entity_type is required", m.ID)
	}
	var err error
	// The bare string "dynamic" declares fully config-derived needs (SPEC §6,
	// ADR-019) and is not a schema; the object form {"dynamic": true, ...}
	// compiles as an ordinary schema (its floor), the extra keyword ignored.
	if !m.needsIsBareDynamic() {
		if m.needs, err = compileSchema(m.ID+"/needs", m.Needs); err != nil {
			return err
		}
	}
	if m.provides, err = compileSchema(m.ID+"/provides", m.Provides); err != nil {
		return err
	}
	if m.config, err = compileSchema(m.ID+"/config", m.ConfigSchema); err != nil {
		return err
	}
	return nil
}

// CompileSchema compiles a JSON Schema document for validation — the same
// compiler manifests use, so a config-derived provides schema (SPEC §7)
// validates exactly as a static one.
func CompileSchema(name string, raw json.RawMessage) (*jsonschema.Schema, error) {
	return compileSchema(name, raw)
}

// NormalizeForSchema round-trips a value through JSON so a validator sees the
// same types it would see on the wire (int → float64, structs → objects).
func NormalizeForSchema(v any) any { return normalizeForSchema(v) }

func compileSchema(name string, raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(name, strings.NewReader(string(raw))); err != nil {
		return nil, fmt.Errorf("adapters: %s: invalid schema: %w", name, err)
	}
	s, err := c.Compile(name)
	if err != nil {
		return nil, fmt.Errorf("adapters: %s: invalid schema: %w", name, err)
	}
	return s, nil
}

// NeedsFields lists every field the adapter can consume, sorted — the union of
// the top-level schema's properties and every anyOf branch's (SPEC §7 one-of
// needs project the union of their branches).
func (m *Manifest) NeedsFields() []string {
	seen := map[string]bool{}
	for _, f := range schemaProperties(m.needsObject()) {
		seen[f] = true
	}
	for _, branch := range m.NeedsAnyOf() {
		for _, f := range schemaProperties(branch) {
			seen[f] = true
		}
	}
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// RequiredNeeds lists the fields that must always be present for the adapter
// to run: the top-level required list (for a dynamic manifest, its static
// floor). One-of alternatives live in NeedsBranches, not here.
func (m *Manifest) RequiredNeeds() []string { return schemaRequired(m.needsObject()) }

// NeedsDynamic reports whether the manifest declares dynamic needs (SPEC §6,
// ADR-019): the bare string "dynamic", or an object with "dynamic": true.
func (m *Manifest) NeedsDynamic() bool {
	if m.needsIsBareDynamic() {
		return true
	}
	var doc struct {
		Dynamic bool `json:"dynamic"`
	}
	if len(m.Needs) == 0 || json.Unmarshal(m.Needs, &doc) != nil {
		return false
	}
	return doc.Dynamic
}

// NeedsBranches returns the one-of alternatives (SPEC §7): the required list
// of each top-level anyOf branch. Empty when the needs schema has no anyOf.
func (m *Manifest) NeedsBranches() [][]string {
	branches := m.NeedsAnyOf()
	if len(branches) == 0 {
		return nil
	}
	out := make([][]string, 0, len(branches))
	for _, b := range branches {
		out = append(out, schemaRequired(b))
	}
	return out
}

// NeedsAnyOf returns the raw top-level anyOf branches of the needs schema.
func (m *Manifest) NeedsAnyOf() []json.RawMessage {
	var doc struct {
		AnyOf []json.RawMessage `json:"anyOf"`
	}
	raw := m.needsObject()
	if len(raw) == 0 || json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	return doc.AnyOf
}

// needsObject returns the needs schema when it is an object, nil for the bare
// "dynamic" string form.
func (m *Manifest) needsObject() json.RawMessage {
	if m.needsIsBareDynamic() {
		return nil
	}
	return m.Needs
}

func (m *Manifest) needsIsBareDynamic() bool {
	return strings.TrimSpace(string(m.Needs)) == `"dynamic"`
}

// ProvidesFields lists every field the adapter can produce, sorted.
func (m *Manifest) ProvidesFields() []string { return schemaProperties(m.Provides) }

// ValidateConfig checks a step's resolved config against config_schema.
func (m *Manifest) ValidateConfig(config map[string]any) error {
	if m.config == nil {
		return nil
	}
	if config == nil {
		config = map[string]any{}
	}
	if err := m.config.Validate(normalizeForSchema(config)); err != nil {
		return fmt.Errorf("adapters: %s: invalid config: %w", m.ID, err)
	}
	return nil
}

// ValidateNeeds checks a projection against the adapter's needs schema.
func (m *Manifest) ValidateNeeds(fields map[string]any) error {
	if m.needs == nil {
		return nil
	}
	if err := m.needs.Validate(normalizeForSchema(fields)); err != nil {
		return fmt.Errorf("needs not satisfied: %w", err)
	}
	return nil
}

// ValidateProvides checks adapter output before it reaches the ledger (SPEC §5).
func (m *Manifest) ValidateProvides(fields map[string]any) error {
	if m.provides == nil {
		return nil
	}
	if err := m.provides.Validate(normalizeForSchema(fields)); err != nil {
		return fmt.Errorf("output does not match provides: %w", err)
	}
	return nil
}

// normalizeForSchema round-trips a value through JSON so the validator sees the
// same types it would see on the wire (int → float64, structs → objects).
func normalizeForSchema(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return v
	}
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

func schemaRequired(raw json.RawMessage) []string {
	var doc struct {
		Required []string `json:"required"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	out := append([]string(nil), doc.Required...)
	sort.Strings(out)
	return out
}
