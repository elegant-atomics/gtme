// Package registry loads and enforces the canonical field registry (SPEC §4a,
// ADR-017): the shared vocabulary that makes needs/provides string matching
// meaningful. The registry files themselves live in spec/fields/ and are
// embedded; this package is the one place their rules are interpreted.
//
// Enforcement layers (SPEC §4a): layer 1 (names in manifests and step config
// must be canonical or vendor-namespaced) is ValidateName; layer 2 (canonical
// values match their declared type, domain and normalized form) is CheckValue;
// layer 3 (the adapter conformance kit) lives in the test suite and consumes
// the same registry through this package.
package registry

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/trevorfox/gtm/internal/identity"
	"github.com/trevorfox/gtm/spec"
)

// Field is one registry entry (spec/schemas/field-registry.schema.json).
type Field struct {
	Name          string   `json:"name"`
	Tier          string   `json:"tier"` // identity | core
	Type          string   `json:"type"` // string | integer | number | boolean | array
	Format        string   `json:"format,omitempty"`
	ItemsType     string   `json:"items_type,omitempty"`
	Normalization string   `json:"normalization"`
	Enum          []string `json:"enum,omitempty"`
	Reserved      bool     `json:"reserved,omitempty"`
	Description   string   `json:"description"`
	Example       any      `json:"example"`
}

type file struct {
	EntityType  string  `json:"entity_type"`
	Version     int     `json:"version"`
	Description string  `json:"description"`
	Fields      []Field `json:"fields"`
}

// Registry is the loaded vocabulary, all entity types.
type Registry struct {
	byEntity map[string]map[string]Field
}

var (
	loadOnce sync.Once
	loaded   *Registry
	loadErr  error
)

// Load returns the embedded registry, parsed once per process.
func Load() (*Registry, error) {
	loadOnce.Do(func() {
		loaded, loadErr = parse()
	})
	return loaded, loadErr
}

func parse() (*Registry, error) {
	entries, err := spec.Fields.ReadDir("fields")
	if err != nil {
		return nil, fmt.Errorf("registry: reading embedded spec/fields: %w", err)
	}
	r := &Registry{byEntity: map[string]map[string]Field{}}
	for _, e := range entries {
		raw, err := spec.Fields.ReadFile("fields/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("registry: reading %s: %w", e.Name(), err)
		}
		var f file
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			return nil, fmt.Errorf("registry: parsing %s: %w", e.Name(), err)
		}
		if f.EntityType == "" {
			return nil, fmt.Errorf("registry: %s: entity_type is required", e.Name())
		}
		m := map[string]Field{}
		for _, fld := range f.Fields {
			if strings.Contains(fld.Name, ".") {
				return nil, fmt.Errorf("registry: %s: %q: a canonical name must not contain a dot", e.Name(), fld.Name)
			}
			if _, err := ruleFunc(fld.Normalization); err != nil {
				return nil, fmt.Errorf("registry: %s: %q: %w", e.Name(), fld.Name, err)
			}
			m[fld.Name] = fld
		}
		r.byEntity[f.EntityType] = m
	}
	return r, nil
}

// Lookup finds a canonical field for an entity type.
func (r *Registry) Lookup(entityType, name string) (Field, bool) {
	f, ok := r.byEntity[entityType][name]
	return f, ok
}

// Known reports whether a vocabulary exists for this entity type at all. An
// entity type with no registry file has no canonical vocabulary yet, so name
// validation does not apply to it (the type is extensible per SPEC §3).
func (r *Registry) Known(entityType string) bool {
	_, ok := r.byEntity[entityType]
	return ok
}

// Names lists the canonical names for an entity type, sorted.
func (r *Registry) Names(entityType string) []string {
	m := r.byEntity[entityType]
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
