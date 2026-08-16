// Package binding implements the declarative binding tier (SPEC §10a,
// ADR-022): a tier-1 adapter is a YAML document validated against
// spec/binding-schema.json and interpreted deterministically by one generic
// HTTP execution engine. All judgment is frozen at authoring time — the
// engine templates requests, paginates, extracts canonical fields through
// registry normalization rules, and maps errors; it never evaluates logic.
package binding

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"gopkg.in/yaml.v3"

	"github.com/trevorfox/gtm/internal/adapters"
	"github.com/trevorfox/gtm/spec"
)

// Binding is one parsed, schema-valid binding document.
type Binding struct {
	ID                  string          `json:"id"`
	Version             int             `json:"version"`
	Role                string          `json:"role"`
	EntityType          string          `json:"entity_type"`
	Needs               json.RawMessage `json:"needs,omitempty"`
	Provides            json.RawMessage `json:"provides,omitempty"`
	ConfigSchema        json.RawMessage `json:"config_schema,omitempty"`
	FreshnessDays       int             `json:"freshness_days,omitempty"`
	CostEstimate        *float64        `json:"cost_estimate_usd,omitempty"`
	Credentials         []string        `json:"credentials,omitempty"`
	CredentialsOptional []string        `json:"credentials_optional,omitempty"`
	KeepPayloads        *bool           `json:"keep_payloads,omitempty"`
	PayloadTTLDays      *int            `json:"payload_ttl_days,omitempty"`

	Auth        *Auth                `json:"auth,omitempty"`
	Request     Request              `json:"request"`
	Pagination  *Pagination          `json:"pagination,omitempty"`
	Extract     Extract              `json:"extract"`
	Errors      map[string]ErrorRule `json:"errors,omitempty"`
	Idempotency string               `json:"idempotency,omitempty"`
	Cost        *Cost                `json:"cost,omitempty"`
	Retry       *Retry               `json:"retry,omitempty"`
	Session     *Session             `json:"session,omitempty"`
}

// Auth is primitive 1: where the credential goes.
type Auth struct {
	Type   string `json:"type"` // header|query|basic|bearer
	Name   string `json:"name,omitempty"`
	Env    string `json:"env"`
	Prefix string `json:"prefix,omitempty"`
}

// Request is primitive 2: the templated request.
type Request struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Query   map[string]string `json:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    any               `json:"body,omitempty"`
}

// Pagination is primitive 3.
type Pagination struct {
	Strategy    string       `json:"strategy"` // page|cursor|offset
	Param       string       `json:"param,omitempty"`
	In          string       `json:"in,omitempty"` // query|body (default query)
	CursorPath  string       `json:"cursor_path,omitempty"`
	PageSize    any          `json:"page_size,omitempty"` // int or template
	SizeParam   string       `json:"size_param,omitempty"`
	Termination *Termination `json:"termination,omitempty"`
	Max         int          `json:"max,omitempty"`
}

// Termination says when pagination stops (beyond the always-on empty page).
type Termination struct {
	EmptyRecords   bool   `json:"empty_records,omitempty"`
	ShortPage      bool   `json:"short_page,omitempty"`
	TotalPagesPath string `json:"total_pages_path,omitempty"`
}

// Extract is primitive 4: response → canonical records.
type Extract struct {
	Records RecordsPath          `json:"records"`
	Fields  map[string]FieldRule `json:"fields"`
}

// RecordsPath is a dotted path to the record array, or alternatives.
type RecordsPath []string

// UnmarshalJSON accepts a string or a list of strings.
func (r *RecordsPath) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		*r = RecordsPath{s}
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("records must be a path or a list of paths")
	}
	*r = RecordsPath(list)
	return nil
}

// FieldRule is one field's extraction: a path (or alternatives), an optional
// registry transform, sentinel absent values, and the skip_if_input dedupe.
type FieldRule struct {
	Paths       []string `json:"paths,omitempty"`
	Transform   string   `json:"transform,omitempty"`
	Absent      []any    `json:"absent,omitempty"`
	SkipIfInput bool     `json:"skip_if_input,omitempty"`
}

// UnmarshalJSON accepts the bare-path string form.
func (f *FieldRule) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		*f = FieldRule{Paths: []string{s}}
		return nil
	}
	var doc struct {
		Path        string   `json:"path"`
		Paths       []string `json:"paths"`
		Transform   string   `json:"transform"`
		Absent      []any    `json:"absent"`
		SkipIfInput bool     `json:"skip_if_input"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	paths := doc.Paths
	if doc.Path != "" {
		paths = append([]string{doc.Path}, paths...)
	}
	*f = FieldRule{Paths: paths, Transform: doc.Transform, Absent: doc.Absent, SkipIfInput: doc.SkipIfInput}
	return nil
}

// ErrorRule is primitive 5: one status (or class) → verdict.
type ErrorRule struct {
	Verdict string `json:"verdict"` // fail_record|fail_run|retry|skip
	Reason  string `json:"reason,omitempty"`
}

// Cost is primitive 7.
type Cost struct {
	Per       string  `json:"per,omitempty"` // record|request|unit
	AmountUSD float64 `json:"amount_usd,omitempty"`
	UnitPath  string  `json:"unit_path,omitempty"`
}

// Retry is primitive 8.
type Retry struct {
	MaxAttempts    int      `json:"max_attempts,omitempty"`
	BackoffSeconds float64  `json:"backoff_seconds,omitempty"`
	RatePerHour    int      `json:"rate_per_hour,omitempty"`
	Windows        []string `json:"windows,omitempty"`
}

// Session is the optional pagination-consistency session declaration.
type Session struct {
	Param string `json:"param,omitempty"`
	In    string `json:"in,omitempty"` // header|query|body
}

// compiledSchema is the binding-schema validator, compiled once.
var compiledSchema = func() *jsonschema.Schema {
	c := jsonschema.NewCompiler()
	if err := c.AddResource("binding-schema.json", strings.NewReader(string(spec.BindingSchema))); err != nil {
		panic(fmt.Sprintf("binding: schema resource: %v", err))
	}
	s, err := c.Compile("binding-schema.json")
	if err != nil {
		panic(fmt.Sprintf("binding: compiling spec/binding-schema.json: %v", err))
	}
	return s
}()

// Parse decodes a binding YAML document, validates it against
// spec/binding-schema.json, and checks the constraints the schema cannot say.
func Parse(rawYAML []byte) (*Binding, error) {
	var doc any
	if err := yaml.Unmarshal(rawYAML, &doc); err != nil {
		return nil, fmt.Errorf("binding: parsing YAML: %w", err)
	}
	// Round-trip through JSON so the validator and the struct decoder see the
	// exact types the schema speaks (yaml ints → float64 etc.).
	rawJSON, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("binding: %w", err)
	}
	var normalized any
	if err := json.Unmarshal(rawJSON, &normalized); err != nil {
		return nil, fmt.Errorf("binding: %w", err)
	}
	if err := compiledSchema.Validate(normalized); err != nil {
		return nil, fmt.Errorf("binding: document does not conform to spec/binding-schema.json: %w", err)
	}

	var b Binding
	if err := json.Unmarshal(rawJSON, &b); err != nil {
		return nil, fmt.Errorf("binding: %w", err)
	}
	if err := b.check(); err != nil {
		return nil, err
	}
	return &b, nil
}

// check enforces what the JSON schema cannot express.
func (b *Binding) check() error {
	if b.Role == adapters.RoleDeliver && b.Idempotency == "" {
		return fmt.Errorf("binding: %s: a deliver binding must declare idempotency: native | ledger", b.ID)
	}
	if b.Role != adapters.RoleDeliver && (len(b.Extract.Records) == 0 || len(b.Extract.Fields) == 0) {
		return fmt.Errorf("binding: %s: a %s binding must declare extract.records and extract.fields", b.ID, b.Role)
	}
	if b.Retry != nil && (len(b.Retry.Windows) > 0 || b.Retry.RatePerHour > 0) {
		// No silent caps: a policy the engine does not enforce yet must refuse to
		// load rather than pretend.
		return fmt.Errorf("binding: %s: retry windows / rate_per_hour are declared but not yet enforced by this engine build", b.ID)
	}
	for name, rule := range b.Extract.Fields {
		if len(rule.Paths) == 0 {
			return fmt.Errorf("binding: %s: extract field %q has no path", b.ID, name)
		}
	}
	if b.Pagination != nil && b.Pagination.Strategy == "cursor" && b.Pagination.CursorPath == "" {
		return fmt.Errorf("binding: %s: cursor pagination needs cursor_path", b.ID)
	}
	return nil
}

// Manifest bridges the binding onto the §6 manifest surface, so the planner
// and runner treat both adapter tiers identically (SPEC §10a).
func (b *Binding) Manifest() (*adapters.Manifest, error) {
	doc := map[string]any{
		"id":          b.ID,
		"version":     b.Version,
		"role":        b.Role,
		"entity_type": b.EntityType,
	}
	if len(b.Needs) > 0 {
		doc["needs"] = json.RawMessage(b.Needs)
	}
	if len(b.Provides) > 0 {
		doc["provides"] = json.RawMessage(b.Provides)
	}
	if len(b.ConfigSchema) > 0 {
		doc["config_schema"] = json.RawMessage(b.ConfigSchema)
	}
	if b.FreshnessDays > 0 {
		doc["freshness_days"] = b.FreshnessDays
	}
	if b.CostEstimate != nil {
		doc["cost_estimate_usd"] = *b.CostEstimate
	} else if b.Cost != nil && b.Cost.Per == "record" {
		doc["cost_estimate_usd"] = b.Cost.AmountUSD
	}
	if len(b.Credentials) > 0 {
		doc["credentials"] = b.Credentials
	}
	if len(b.CredentialsOptional) > 0 {
		doc["credentials_optional"] = b.CredentialsOptional
	}
	if b.KeepPayloads != nil {
		doc["keep_payloads"] = *b.KeepPayloads
	}
	if b.PayloadTTLDays != nil {
		doc["payload_ttl_days"] = *b.PayloadTTLDays
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("binding: %s: %w", b.ID, err)
	}
	return adapters.ParseManifest(raw)
}

// Provider is the COST provider name: the id's vendor prefix.
func (b *Binding) Provider() string {
	if i := strings.IndexByte(b.ID, '/'); i > 0 {
		return b.ID[:i]
	}
	return b.ID
}
