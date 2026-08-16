package registry

import (
	"strings"
	"testing"
)

func mustLoad(t *testing.T) *Registry {
	t.Helper()
	r, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return r
}

func TestLoadKnowsSeededEntityTypes(t *testing.T) {
	r := mustLoad(t)
	for _, et := range []string{"person", "company"} {
		if !r.Known(et) {
			t.Errorf("registry does not know entity type %q", et)
		}
	}
	if r.Known("martian") {
		t.Error("registry claims to know an unseeded entity type")
	}
	// The identity tier is mandatory (SPEC §4a): spot-check the ADR-017 set.
	for _, name := range []string{"email", "linkedin_url", "first_name", "last_name"} {
		f, ok := r.Lookup("person", name)
		if !ok || f.Tier != "identity" {
			t.Errorf("person %q: want identity tier, got %+v (ok=%v)", name, f, ok)
		}
	}
}

func TestValidateName(t *testing.T) {
	r := mustLoad(t)
	cases := []struct {
		entity, name string
		wantErr      bool
		wantHint     string
	}{
		{"person", "email", false, ""},
		{"person", "apollo.id", false, ""},                        // namespaced, tier 3
		{"person", "csv.favorite_color", false, ""},               // namespaced, tier 3
		{"person", "full_nmae", true, `did you mean "full_name"`}, // near-miss suggested
		{"person", "shoe_size", true, "not a canonical person field"},
		{"martian", "anything_goes", false, ""}, // no vocabulary for the type yet
	}
	for _, c := range cases {
		err := r.ValidateName(c.entity, c.name)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateName(%s, %s): err=%v, wantErr=%v", c.entity, c.name, err, c.wantErr)
			continue
		}
		if err != nil && c.wantHint != "" && !strings.Contains(err.Error(), c.wantHint) {
			t.Errorf("ValidateName(%s, %s): error %q missing %q", c.entity, c.name, err, c.wantHint)
		}
	}
}

func TestNormalizeValueAppliesRules(t *testing.T) {
	r := mustLoad(t)
	got, err := r.NormalizeValue("person", "email", "Jane.Doe@Acme.com ")
	if err != nil || got != "jane.doe@acme.com" {
		t.Errorf("email: got %v, %v", got, err)
	}
	got, err = r.NormalizeValue("person", "company_domain", "https://www.Acme.com/about")
	if err != nil || got != "acme.com" {
		t.Errorf("company_domain: got %v, %v", got, err)
	}
	got, err = r.NormalizeValue("person", "linkedin_url", "linkedin.com/in/Jane-Doe/")
	if err != nil || got != "https://www.linkedin.com/in/jane-doe" {
		t.Errorf("linkedin_url: got %v, %v", got, err)
	}
	// A non-public shape is an INVALID value for linkedin_url (ADR-020), not a
	// silently reshaped one.
	if _, err := r.NormalizeValue("person", "linkedin_url", "https://www.linkedin.com/sales/lead/ACwAAAbQxKB9,NAME"); err == nil {
		t.Error("linkedin_url accepted a Sales Navigator URL")
	}
	// Namespaced and unknown-entity values pass through untouched.
	got, err = r.NormalizeValue("person", "csv.notes", "  as-is  ")
	if err != nil || got != "  as-is  " {
		t.Errorf("namespaced passthrough: got %v, %v", got, err)
	}
}

func TestNormalizeValueChecksTypes(t *testing.T) {
	r := mustLoad(t)
	if _, err := r.NormalizeValue("person", "company_employees", "50-200"); err == nil {
		t.Error("company_employees accepted a range string (SPEC §4a: integer, never a range)")
	}
	if _, err := r.NormalizeValue("person", "company_employees", float64(120)); err != nil {
		t.Errorf("company_employees rejected an integer: %v", err)
	}
	if _, err := r.NormalizeValue("person", "recent_posts", []any{"a post", 7}); err == nil {
		t.Error("recent_posts accepted a non-string element")
	}
	if _, err := r.NormalizeValue("person", "open_to_work", "yes"); err == nil {
		t.Error("open_to_work accepted a string")
	}
}

func TestCheckValueDemandsNormalizedForm(t *testing.T) {
	r := mustLoad(t)
	if err := r.CheckValue("person", "email", "jane@acme.com"); err != nil {
		t.Errorf("normalized email rejected: %v", err)
	}
	if err := r.CheckValue("person", "email", "Jane@Acme.com"); err == nil {
		t.Error("non-normalized email passed CheckValue (layer 2 must demand fixed points)")
	}
}

func TestSuggestOnlyNearMisses(t *testing.T) {
	r := mustLoad(t)
	if s := r.Suggest("person", "e-mail"); s != "email" {
		t.Errorf("Suggest(e-mail) = %q, want email", s)
	}
	if s := r.Suggest("person", "favorite_color"); s != "" {
		t.Errorf("Suggest(favorite_color) = %q, want none", s)
	}
}
