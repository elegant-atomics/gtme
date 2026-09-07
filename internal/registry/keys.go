package registry

// Identity-key derivation (SPEC §4, ADR-054): every type's tiers are its
// file's ordered identity list, read here and nowhere else. There is no
// switch over type names.

import (
	"fmt"
	"strings"

	"github.com/gtme-run/gtme/internal/identity"
)

// Candidates returns every key that can be derived from fields for the given
// entity type, strongest first — one per tier that yields a value. The first
// element is the key a new identity should be created with; all of them are
// worth looking up, because the record may already exist under a weaker key.
//
// An empty slice (with a nil error) means the record carries nothing
// identifying; callers should treat that as a failed record.
func (r *Registry) Candidates(entityType string, fields map[string]any) ([]identity.Key, error) {
	t, err := r.Resolve(entityType)
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	var out []identity.Key
	n := len(t.Identity)
	for i, tier := range t.Identity {
		value := t.tierValue(tier, fields)
		if value == "" {
			continue
		}
		out = append(out, identity.Key{EntityType: entityType, Value: value, Strength: identity.Strength(n - i)})
	}
	return out, nil
}

// KeyFor returns the strongest key derivable from fields.
func (r *Registry) KeyFor(entityType string, fields map[string]any) (identity.Key, error) {
	cands, err := r.Candidates(entityType, fields)
	if err != nil {
		return identity.Key{}, err
	}
	if len(cands) == 0 {
		return identity.Key{}, fmt.Errorf("identity: no identity key derivable for %s record", entityType)
	}
	return cands[0], nil
}

// tierValue derives one tier's key from a record, or "".
func (t *Type) tierValue(tier Tier, fields map[string]any) string {
	if tier.Field != "" {
		f, ok := t.byName[tier.Field]
		if !ok {
			return ""
		}
		v := identity.KeyForm(f.Normalization, identity.Str(fields, tier.Field))
		if v == "" {
			return ""
		}
		return tier.Prefix + v
	}
	// A hash tier: the first component is required, the rest contribute
	// what they have (SPEC §4a) — a name salted by the domain when known.
	parts := make([]string, 0, len(tier.Hash))
	for i, c := range tier.Hash {
		v := t.componentValue(c, fields)
		if i == 0 && v == "" {
			return ""
		}
		parts = append(parts, v)
	}
	return identity.NameHash(tier.Prefix, strings.Join(parts, "|"))
}

// componentValue resolves one hash component: the field's rule applied, then
// lowercased with whitespace collapsed (SPEC §4a).
func (t *Type) componentValue(c HashComponent, fields map[string]any) string {
	switch {
	case c.Field != "":
		return identity.NormalizeName(t.ruleValue(c.Field, fields))
	case len(c.Join) > 0:
		parts := make([]string, 0, len(c.Join))
		for _, name := range c.Join {
			v := strings.TrimSpace(identity.Str(fields, name))
			if v == "" {
				return ""
			}
			parts = append(parts, v)
		}
		return identity.NormalizeName(strings.Join(parts, " "))
	default:
		for _, alt := range c.Any {
			if v := t.componentValue(alt, fields); v != "" {
				return v
			}
		}
		return ""
	}
}

// ruleValue applies a field's own normalization rule to its raw value.
func (t *Type) ruleValue(name string, fields map[string]any) string {
	raw := identity.Str(fields, name)
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	f, ok := t.byName[name]
	if !ok {
		return strings.TrimSpace(raw)
	}
	fn, err := ruleFunc(f.Normalization)
	if err != nil {
		return strings.TrimSpace(raw)
	}
	return fn(raw)
}

// Coverage reports which tiers a field set can derive (SPEC §7, §4a check
// (c)): strong is any field tier, weak the hash fallback (its required first
// component). An unknown type covers nothing.
func (r *Registry) Coverage(entityType string, fields []string) (strong, weak bool) {
	t, ok := r.byEntity[entityType]
	if !ok {
		return false, false
	}
	have := map[string]bool{}
	for _, f := range fields {
		have[f] = true
	}
	for _, tier := range t.Identity {
		switch {
		case tier.Field != "":
			if have[tier.Field] {
				strong = true
			}
		case len(tier.Hash) > 0:
			if componentCovered(tier.Hash[0], have) {
				weak = true
			}
		}
	}
	return strong, weak
}

func componentCovered(c HashComponent, have map[string]bool) bool {
	switch {
	case c.Field != "":
		return have[c.Field]
	case len(c.Join) > 0:
		for _, name := range c.Join {
			if !have[name] {
				return false
			}
		}
		return true
	}
	for _, alt := range c.Any {
		if componentCovered(alt, have) {
			return true
		}
	}
	return false
}

// TierFields lists the fields a type's tiers read, in tier order — what the
// plan names when a source covers none of them.
func (t *Type) TierFields() []string {
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, tier := range t.Identity {
		if tier.Field != "" {
			add(tier.Field)
			continue
		}
		for _, c := range tier.Hash {
			for _, name := range c.fields() {
				add(name)
			}
		}
	}
	return out
}

// TierNames renders the tiers for a message: "email, linkedin_url, …, a
// hash of full_name (or first_name+last_name) and company_domain".
func (t *Type) TierNames() []string {
	out := make([]string, 0, len(t.Identity))
	for _, tier := range t.Identity {
		if tier.Field != "" {
			out = append(out, tier.Field)
			continue
		}
		parts := make([]string, 0, len(tier.Hash))
		for _, c := range tier.Hash {
			parts = append(parts, c.String())
		}
		out = append(out, "a hash of "+strings.Join(parts, " and "))
	}
	return out
}

// String renders a component for messages.
func (c HashComponent) String() string {
	switch {
	case c.Field != "":
		return c.Field
	case len(c.Join) > 0:
		return strings.Join(c.Join, "+")
	}
	alts := make([]string, 0, len(c.Any))
	for _, a := range c.Any {
		alts = append(alts, a.String())
	}
	return "(" + strings.Join(alts, " or ") + ")"
}
