// Package registry loads the type files (SPEC §4a, ADR-017, ADR-054): a type
// is a file — its kind, the ordered identity tiers §4 reads, the canonical
// vocabulary that makes needs/provides string matching meaningful, and the
// references its fields declare. The embedded files live in spec/fields/;
// more are discovered in place (~/.gtme/types/, an installed binding's
// types/). This package is the one place their rules are interpreted.
//
// Enforcement layers (SPEC §4a): layer 1 (names in manifests and step config
// must be canonical or vendor-namespaced) is ValidateName; layer 2 (canonical
// values match their declared type, domain and normalized form) is CheckValue;
// layer 3 (the adapter conformance kit) lives in the test suite and consumes
// the same registry through this package. Key derivation (SPEC §4) is
// Candidates, read from each type's identity list.
package registry

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/elegant-atomics/gtme/internal/identity"
	"github.com/elegant-atomics/gtme/spec"
)

// Type kinds (SPEC §4a, ADR-054).
const (
	KindSubject = "subject"
	KindSignal  = "signal"
)

// Field is one registry entry (spec/schemas/field-registry.schema.json).
type Field struct {
	Name          string     `json:"name"`
	Tier          string     `json:"tier"` // identity | core
	Type          string     `json:"type"` // string | integer | number | boolean | array
	Format        string     `json:"format,omitempty"`
	ItemsType     string     `json:"items_type,omitempty"`
	Normalization string     `json:"normalization"`
	Enum          []string   `json:"enum,omitempty"`
	Reserved      bool       `json:"reserved,omitempty"`
	Reference     *Reference `json:"reference,omitempty"`
	Description   string     `json:"description"`
	Example       any        `json:"example"`
}

// Reference is a field's declared reference (SPEC §4a, ADR-054): a record
// carrying the field also names an identity of Type, keyed and populated from
// Fields carried under the same names, and the runner writes Relation from
// the record to it.
type Reference struct {
	Type     string   `json:"type"`
	Relation string   `json:"relation"`
	Fields   []string `json:"fields"`
}

// Tier is one identity tier (SPEC §4): a field whose normalization is a
// public-identifier rule, or a hash of components.
type Tier struct {
	Field  string          `json:"field,omitempty"`
	Hash   []HashComponent `json:"hash,omitempty"`
	Prefix string          `json:"prefix,omitempty"`
}

// HashComponent is one component of a hash tier: a field name, the first of
// several alternatives that yields a value, or fields joined with a space.
type HashComponent struct {
	Field string
	Any   []HashComponent
	Join  []string
}

// UnmarshalJSON accepts the three schema forms.
func (c *HashComponent) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		*c = HashComponent{Field: s}
		return nil
	}
	var doc struct {
		Any  []HashComponent `json:"any"`
		Join []string        `json:"join"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	switch {
	case len(doc.Any) > 0 && len(doc.Join) == 0:
		*c = HashComponent{Any: doc.Any}
	case len(doc.Join) > 0 && len(doc.Any) == 0:
		*c = HashComponent{Join: doc.Join}
	default:
		return fmt.Errorf("a hash component is a field name, {any: […]} or {join: […]}")
	}
	return nil
}

// fields lists every registry field a component reads.
func (c HashComponent) fields() []string {
	switch {
	case c.Field != "":
		return []string{c.Field}
	case len(c.Join) > 0:
		return append([]string(nil), c.Join...)
	}
	var out []string
	for _, alt := range c.Any {
		out = append(out, alt.fields()...)
	}
	return out
}

// Type is one loaded type file: the whole definition of an entity type.
type Type struct {
	EntityType  string  `json:"entity_type"`
	Version     int     `json:"version"`
	Kind        string  `json:"kind"`
	Description string  `json:"description"`
	Identity    []Tier  `json:"identity"`
	Fields      []Field `json:"fields"`

	// Path names where the file was read from — "embedded", or the file's
	// path on disk (SPEC §4a: a conflict names both paths).
	Path string `json:"-"`
	// Origin classifies the source: "embedded", "home" (~/.gtme/types) or
	// "binding" (shipped beside a binding.yaml).
	Origin string `json:"-"`
	// Hash is the file content's sha256, for the same-name rule.
	Hash string `json:"-"`

	byName map[string]Field
}

// Lookup finds a canonical field by name.
func (t *Type) Lookup(name string) (Field, bool) {
	f, ok := t.byName[name]
	return f, ok
}

// Names lists the canonical names, sorted.
func (t *Type) Names() []string {
	out := make([]string, 0, len(t.byName))
	for k := range t.byName {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// References lists the fields that declare a reference, in file order.
func (t *Type) References() []Field {
	var out []Field
	for _, f := range t.Fields {
		if f.Reference != nil {
			out = append(out, f)
		}
	}
	return out
}

// IsSignal reports a signal type (SPEC §4a): found and traversed from, never
// delivered to.
func (t *Type) IsSignal() bool { return t.Kind == KindSignal }

// Registry is the loaded set of types.
type Registry struct {
	byEntity map[string]*Type
	// problems records names that cannot resolve to exactly one type file —
	// the same name with different content across sources, or a file that
	// does not validate — keyed by name, each a message naming the paths.
	// A problem surfaces where the type is referenced (plan, verify), never
	// as a load failure that would take every verb down.
	problems map[string]string
}

var (
	loadOnce sync.Once
	loaded   *Registry
	loadErr  error
)

// Load returns the registry: the embedded types plus those discovered on this
// machine (SPEC §4a), parsed once per process.
func Load() (*Registry, error) {
	loadOnce.Do(func() {
		loaded, loadErr = load(true)
	})
	return loaded, loadErr
}

// LoadEmbedded returns the embedded types only — what a build ships, with
// nothing discovered. Tests and the schema check use it.
func LoadEmbedded() (*Registry, error) {
	return load(false)
}

// Reset forgets the loaded registry so the next Load discovers again. Tests
// only: discovery reads the environment, which a test may change.
func Reset() {
	loadOnce = sync.Once{}
	loaded, loadErr = nil, nil
}

// typeSchema validates a type file (spec/schemas/field-registry.schema.json).
var typeSchema = func() *jsonschema.Schema {
	c := jsonschema.NewCompiler()
	if err := c.AddResource("field-registry.schema.json", strings.NewReader(string(spec.TypeSchema))); err != nil {
		panic(fmt.Sprintf("registry: schema resource: %v", err))
	}
	s, err := c.Compile("field-registry.schema.json")
	if err != nil {
		panic(fmt.Sprintf("registry: compiling spec/schemas/field-registry.schema.json: %v", err))
	}
	return s
}()

func load(discover bool) (*Registry, error) {
	r := &Registry{byEntity: map[string]*Type{}, problems: map[string]string{}}
	entries, err := spec.Fields.ReadDir("fields")
	if err != nil {
		return nil, fmt.Errorf("registry: reading embedded spec/fields: %w", err)
	}
	for _, e := range entries {
		raw, err := spec.Fields.ReadFile("fields/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("registry: reading %s: %w", e.Name(), err)
		}
		t, err := ParseType(raw, "embedded", "embedded")
		if err != nil {
			// An embedded file that does not parse is a build defect, not a
			// runtime condition.
			return nil, fmt.Errorf("registry: %s: %w", e.Name(), err)
		}
		if t.EntityType != strings.TrimSuffix(e.Name(), ".json") {
			return nil, fmt.Errorf("registry: %s declares entity_type %q; the file is named after the type", e.Name(), t.EntityType)
		}
		r.byEntity[t.EntityType] = t
	}
	if discover {
		r.discover()
	}
	return r, nil
}

// ParseType decodes and validates one type file. path and origin label where
// it came from.
func ParseType(raw []byte, path, origin string) (*Type, error) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing: %w", err)
	}
	if err := typeSchema.Validate(doc); err != nil {
		return nil, fmt.Errorf("does not validate against spec/schemas/field-registry.schema.json: %w", err)
	}
	var t Type
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("parsing: %w", err)
	}
	t.Path, t.Origin, t.Hash = path, origin, contentHash(raw)
	t.byName = map[string]Field{}
	for _, fld := range t.Fields {
		if strings.Contains(fld.Name, ".") {
			return nil, fmt.Errorf("%q: a canonical name must not contain a dot", fld.Name)
		}
		if _, err := ruleFunc(fld.Normalization); err != nil {
			return nil, fmt.Errorf("%q: %w", fld.Name, err)
		}
		if _, dup := t.byName[fld.Name]; dup {
			return nil, fmt.Errorf("%q is declared twice", fld.Name)
		}
		t.byName[fld.Name] = fld
	}
	if err := t.checkIdentity(); err != nil {
		return nil, err
	}
	for _, fld := range t.Fields {
		if ref := fld.Reference; ref != nil {
			for _, name := range ref.Fields {
				if _, ok := t.byName[name]; !ok {
					return nil, fmt.Errorf("%q: reference field %q is not a field of this type", fld.Name, name)
				}
			}
		}
	}
	return &t, nil
}

// checkIdentity enforces what the schema cannot say about the identity list
// (SPEC §4a): a field tier names a field of this file under a key rule; a
// hash tier names fields of this file; a signal declares no hash tier.
func (t *Type) checkIdentity() error {
	for i, tier := range t.Identity {
		switch {
		case tier.Field != "":
			f, ok := t.byName[tier.Field]
			if !ok {
				return fmt.Errorf("identity tier %d names %q, which is not a field of this type", i+1, tier.Field)
			}
			if !identity.KeyRules[f.Normalization] {
				return fmt.Errorf("identity tier %d: %q normalizes with %s, which is not a public-identifier rule (email, domain, linkedin_url, handle, url — SPEC §4a)", i+1, tier.Field, f.Normalization)
			}
		default:
			if t.Kind == KindSignal {
				return fmt.Errorf("identity tier %d: a signal type declares no hash tier — a %s without a public identifier is not one the ledger can hold (SPEC §4)", i+1, t.EntityType)
			}
			for _, c := range tier.Hash {
				for _, name := range c.fields() {
					if _, ok := t.byName[name]; !ok {
						return fmt.Errorf("identity tier %d names %q, which is not a field of this type", i+1, name)
					}
				}
			}
		}
	}
	return nil
}

// Resolve finds a type by name: exactly one file (SPEC §4a check (a)). The
// error names the problem — an unknown type, or a name two sources define
// differently, with both paths.
func (r *Registry) Resolve(entityType string) (*Type, error) {
	if msg, bad := r.problems[entityType]; bad {
		return nil, fmt.Errorf("type %q: %s", entityType, msg)
	}
	t, ok := r.byEntity[entityType]
	if !ok {
		return nil, fmt.Errorf("type %q has no type file — this build embeds %s; a binding ships its own as types/%s.json, or place one in ~/.gtme/types/ (SPEC §4a)",
			entityType, strings.Join(r.Types(), ", "), entityType)
	}
	return t, nil
}

// Types lists the resolvable type names, sorted.
func (r *Registry) Types() []string {
	out := make([]string, 0, len(r.byEntity))
	for k := range r.byEntity {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Lookup finds a canonical field for an entity type.
func (r *Registry) Lookup(entityType, name string) (Field, bool) {
	t, ok := r.byEntity[entityType]
	if !ok {
		return Field{}, false
	}
	return t.Lookup(name)
}

// Known reports whether a type file exists for this entity type. An entity
// type with no file has no canonical vocabulary, so name validation does not
// apply to it — the empty pipeline type of an untyped legacy group (SPEC §9).
func (r *Registry) Known(entityType string) bool {
	_, ok := r.byEntity[entityType]
	return ok
}

// Names lists the canonical names for an entity type, sorted.
func (r *Registry) Names(entityType string) []string {
	t, ok := r.byEntity[entityType]
	if !ok {
		return nil
	}
	return t.Names()
}

// IsNamespaced reports whether a field name is vendor-namespaced
// (<vendor>.<field>, SPEC §4a tier 3).
func IsNamespaced(name string) bool {
	i := strings.Index(name, ".")
	return i > 0 && i < len(name)-1
}

// ValidateName is enforcement layer 1: a field name crossing an adapter
// boundary must be canonical for the entity type or vendor-namespaced. The
// error names the nearest canonical field when one is close enough to be the
// likely intent.
func (r *Registry) ValidateName(entityType, name string) error {
	if !r.Known(entityType) || IsNamespaced(name) {
		return nil
	}
	if _, ok := r.Lookup(entityType, name); ok {
		return nil
	}
	if s := r.Suggest(entityType, name); s != "" {
		return fmt.Errorf("%q is not a canonical %s field (did you mean %q?) — use a canonical name or namespace it as <vendor>.%s", name, entityType, s, name)
	}
	return fmt.Errorf("%q is not a canonical %s field — use a canonical name (see spec/fields/%s.json) or namespace it as <vendor>.%s", name, entityType, entityType, name)
}

// Suggest returns the canonical name within a small edit distance of name, or
// "" when nothing is close. Used for plan-time near-miss suggestions (SPEC §7)
// — suggested, never silently applied.
func (r *Registry) Suggest(entityType, name string) string {
	best, bestDist := "", 3 // suggest only within edit distance 2
	for _, cand := range r.Names(entityType) {
		if d := editDistance(strings.ToLower(name), cand); d < bestDist {
			best, bestDist = cand, d
		}
	}
	return best
}

// NormalizeValue applies a canonical field's rule to an incoming value
// (ingress, SPEC §10.1). It returns the normalized value, or an error when the
// value is invalid for the field (wrong type, outside the enum domain, or
// rejected by the rule — e.g. a non-public URL under linkedin_url).
// Non-canonical (namespaced or unknown-entity) fields pass through untouched.
func (r *Registry) NormalizeValue(entityType, name string, v any) (any, error) {
	f, ok := r.Lookup(entityType, name)
	if !ok {
		return v, nil
	}
	return normalize(f, v)
}

// CheckValue is enforcement layer 2 (runtime): a canonical value already in
// flight must match its declared type and domain and be a fixed point of its
// rule. Unlike NormalizeValue it never rewrites — a non-normalized value is an
// error, because the providing adapter was required to normalize at its own
// boundary (SPEC §4a).
func (r *Registry) CheckValue(entityType, name string, v any) error {
	f, ok := r.Lookup(entityType, name)
	if !ok {
		return nil
	}
	got, err := normalize(f, v)
	if err != nil {
		return err
	}
	if s, ok := v.(string); ok {
		if ns, _ := got.(string); ns != s {
			return fmt.Errorf("field %q: value %q is not in normalized form (rule %s wants %q)", name, s, f.Normalization, ns)
		}
	}
	return nil
}

func normalize(f Field, v any) (any, error) {
	switch f.Type {
	case "string":
		s, ok := v.(string)
		if !ok {
			return v, fmt.Errorf("field %q: expected a string, got %T", f.Name, v)
		}
		fn, err := ruleFunc(f.Normalization)
		if err != nil {
			return v, err
		}
		out := fn(s)
		if out == "" {
			return v, fmt.Errorf("field %q: %q is not a valid value (rule %s)", f.Name, s, f.Normalization)
		}
		if len(f.Enum) > 0 && !contains(f.Enum, out) {
			return v, fmt.Errorf("field %q: %q is outside the canonical domain %v", f.Name, out, f.Enum)
		}
		return out, nil
	case "integer":
		switch n := v.(type) {
		case int:
			return n, nil
		case float64:
			if n != float64(int64(n)) {
				return v, fmt.Errorf("field %q: expected an integer, got %v", f.Name, v)
			}
			return n, nil
		default:
			return v, fmt.Errorf("field %q: expected an integer, got %T", f.Name, v)
		}
	case "number":
		switch v.(type) {
		case int, float64:
			return v, nil
		default:
			return v, fmt.Errorf("field %q: expected a number, got %T", f.Name, v)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return v, fmt.Errorf("field %q: expected a boolean, got %T", f.Name, v)
		}
		return v, nil
	case "array":
		list, ok := v.([]any)
		if !ok {
			return v, fmt.Errorf("field %q: expected an array, got %T", f.Name, v)
		}
		if f.ItemsType == "string" {
			for i, item := range list {
				if _, ok := item.(string); !ok {
					return v, fmt.Errorf("field %q: element %d: expected a string, got %T", f.Name, i, item)
				}
			}
		}
		return v, nil
	default:
		return v, nil
	}
}

// ApplyRule runs one named normalization rule on a string value — the binding
// engine's extraction `transform:` hook (SPEC §10a), which is restricted to
// exactly these registry rules. An empty result means the value was invalid
// for the rule (dropped by the caller, mirroring §10.1 ingress semantics).
func ApplyRule(id, value string) (string, error) {
	fn, err := ruleFunc(id)
	if err != nil {
		return "", err
	}
	return fn(value), nil
}

// ruleFunc maps a rule id to its single implementation (SPEC §4a: each rule
// exists exactly once, shared with identity-key derivation).
func ruleFunc(id string) (func(string) string, error) {
	switch id {
	case "none":
		return func(s string) string { return s }, nil
	case "trim":
		return strings.TrimSpace, nil
	case "lower":
		return func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }, nil
	case "email":
		return identity.NormalizeEmail, nil
	case "domain":
		return identity.NormalizeDomain, nil
	case "linkedin_url":
		return identity.NormalizeLinkedInURL, nil
	case "handle":
		return identity.NormalizeHandle, nil
	case "url":
		return identity.NormalizeURL, nil
	default:
		return nil, fmt.Errorf("unknown normalization rule %q", id)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// editDistance is a plain Levenshtein distance, sized for field names.
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
