package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/elegant-atomics/gtme/internal/identity"
)

func nh(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "nh:" + hex.EncodeToString(sum[:])
}

// The golden keys (SPEC §4): person and company derive from their type files
// exactly as the pre-ADR-054 switch derived them — byte-identical keys, same
// tier order.
func TestKeyForGolden(t *testing.T) {
	r := mustLoad(t)
	cases := []struct {
		name       string
		entityType string
		fields     map[string]any
		want       string
		wantStr    identity.Strength
		wantErr    bool
	}{
		{"person email wins and is lowercased", "person",
			map[string]any{"email": "  Jane.Doe@Example.COM ", "linkedin_url": "https://linkedin.com/in/jane"},
			"jane.doe@example.com", 5, false},
		{"person linkedin when no email", "person",
			map[string]any{"linkedin_url": "https://www.linkedin.com/in/Jane-Doe/?trk=public"},
			"in/jane-doe", 4, false},
		{"reserved handle tiers carry their prefixes", "person",
			map[string]any{"github_username": "@JaneDoe"}, "gh:janedoe", 3, false},
		{"twitter is the weakest handle", "person",
			map[string]any{"twitter_handle": "janedoe"}, "tw:janedoe", 2, false},
		{"empty email string is not a key", "person",
			map[string]any{"email": "   ", "linkedin_url": "linkedin.com/in/x"}, "in/x", 4, false},
		{"malformed email falls through to name hash", "person",
			map[string]any{"email": "not-an-email", "full_name": "Jane Doe", "company_domain": "acme.com"},
			nh("jane doe|acme.com"), 1, false},
		{"name hash normalizes whitespace and domain", "person",
			map[string]any{"full_name": " Jane   DOE ", "company_domain": "https://www.Acme.com/careers"},
			nh("jane doe|acme.com"), 1, false},
		{"name hash from first and last name", "person",
			map[string]any{"first_name": "Jane", "last_name": "Doe"}, nh("jane doe|"), 1, false},
		{"full_name wins over first and last", "person",
			map[string]any{"full_name": "Jane Doe", "first_name": "J", "last_name": "D"}, nh("jane doe|"), 1, false},
		{"a domain without a name is not a person key", "person",
			map[string]any{"company_domain": "acme.com", "first_name": "Jane"}, "", 0, true},
		{"person with nothing identifying", "person",
			map[string]any{"title": "VP Marketing"}, "", 0, true},
		{"company registrable domain from url", "company",
			map[string]any{"company_domain": "https://blog.Acme.co.uk/posts?x=1"}, "acme.co.uk", 2, false},
		{"company domain with port", "company",
			map[string]any{"company_domain": "www.acme.com:8443"}, "acme.com", 2, false},
		{"company name hash when no domain", "company",
			map[string]any{"company_name": "Acme  Inc"}, nh("acme inc"), 1, false},
		{"bare hostname without dot is not a domain", "company",
			map[string]any{"company_domain": "localhost", "company_name": "Acme Inc"}, nh("acme inc"), 1, false},
		{"non-string scalars are usable", "company",
			map[string]any{"company_name": 3600}, nh("3600"), 1, false},
		{"company with nothing identifying", "company",
			map[string]any{"company_employees": 50}, "", 0, true},
		{"post keys on its url and nothing else", "post",
			map[string]any{"url": "HTTPS://www.LinkedIn.com/posts/jane_x-7123/", "text": "hi"},
			"https://www.linkedin.com/posts/jane_x-7123", 1, false},
		{"post without a url is not a post", "post",
			map[string]any{"text": "hi", "platform": "linkedin"}, "", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.KeyFor(tc.entityType, tc.fields)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got key %q", got.Value)
				}
				return
			}
			if err != nil {
				t.Fatalf("KeyFor: %v", err)
			}
			if got.Value != tc.want {
				t.Errorf("key = %q, want %q", got.Value, tc.want)
			}
			if got.Strength != tc.wantStr {
				t.Errorf("strength = %d, want %d", got.Strength, tc.wantStr)
			}
			if got.EntityType != tc.entityType {
				t.Errorf("entity_type = %q, want %q", got.EntityType, tc.entityType)
			}
		})
	}
}

func TestKeyForUnknownEntityType(t *testing.T) {
	r := mustLoad(t)
	if _, err := r.KeyFor("robot", map[string]any{"email": "a@b.com"}); err == nil {
		t.Fatal("want error for unknown entity type")
	}
}

func TestCandidatesOrderedStrongestFirst(t *testing.T) {
	r := mustLoad(t)
	got, err := r.Candidates("person", map[string]any{
		"email":          "Jane@Example.com",
		"linkedin_url":   "https://www.linkedin.com/in/jane-doe/",
		"full_name":      "Jane Doe",
		"company_domain": "acme.com",
	})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	want := []string{"jane@example.com", "in/jane-doe", nh("jane doe|acme.com")}
	if len(got) != len(want) {
		t.Fatalf("got %d candidates, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Value != want[i] {
			t.Errorf("candidate %d = %q, want %q", i, got[i].Value, want[i])
		}
		if i > 0 && got[i-1].Strength < got[i].Strength {
			t.Errorf("candidates not ordered strongest first: %+v", got)
		}
	}
}

func TestCoverage(t *testing.T) {
	r := mustLoad(t)
	cases := []struct {
		entity       string
		fields       []string
		strong, weak bool
	}{
		{"person", []string{"email", "title"}, true, false},
		{"person", []string{"full_name"}, false, true},
		{"person", []string{"first_name", "last_name"}, false, true},
		{"person", []string{"first_name"}, false, false},
		{"person", []string{"company_domain"}, false, false},
		{"company", []string{"company_name"}, false, true},
		{"company", []string{"company_domain"}, true, false},
		{"post", []string{"text", "platform"}, false, false},
		{"post", []string{"url"}, true, false},
		{"martian", []string{"email"}, false, false},
	}
	for _, c := range cases {
		strong, weak := r.Coverage(c.entity, c.fields)
		if strong != c.strong || weak != c.weak {
			t.Errorf("Coverage(%s, %v) = %v,%v want %v,%v", c.entity, c.fields, strong, weak, c.strong, c.weak)
		}
	}
}

func TestTypeKindsAndReferences(t *testing.T) {
	r := mustLoad(t)
	person, err := r.Resolve("person")
	if err != nil {
		t.Fatal(err)
	}
	if person.Kind != KindSubject || person.IsSignal() {
		t.Errorf("person kind = %q", person.Kind)
	}
	refs := person.References()
	if len(refs) != 1 || refs[0].Name != "company_domain" {
		t.Fatalf("person references = %+v, want company_domain only", refs)
	}
	ref := refs[0].Reference
	if ref.Type != "company" || ref.Relation != "works_at" || len(ref.Fields) != 2 {
		t.Errorf("company_domain reference = %+v", ref)
	}
	post, err := r.Resolve("post")
	if err != nil {
		t.Fatal(err)
	}
	if !post.IsSignal() {
		t.Error("post is a signal")
	}
}

// The full §4 person tier ordering: email > public slug > gh: > tw: > nh:.
func TestPersonTierOrdering(t *testing.T) {
	r := mustLoad(t)
	fields := map[string]any{
		"email":           "jane@acme.com",
		"linkedin_url":    "https://www.linkedin.com/in/jane-doe",
		"github_username": "janedoe",
		"twitter_handle":  "@janedoe",
		"full_name":       "Jane Doe",
		"company_domain":  "acme.com",
	}
	cands, err := r.Candidates("person", fields)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"jane@acme.com", "in/jane-doe", "gh:janedoe", "tw:janedoe"}
	if len(cands) != 5 {
		t.Fatalf("want 5 candidates, got %d: %+v", len(cands), cands)
	}
	for i, w := range want {
		if cands[i].Value != w {
			t.Errorf("candidate %d = %q, want %q", i, cands[i].Value, w)
		}
	}
	for i := 1; i < len(cands); i++ {
		if cands[i-1].Strength <= cands[i].Strength {
			t.Errorf("candidates not strongest-first at %d: %v then %v", i, cands[i-1].Strength, cands[i].Strength)
		}
	}
	// An internal-form URL contributes no slug candidate: the record falls
	// through to the handle tiers.
	fields["linkedin_url"] = "https://www.linkedin.com/in/ACwAAAbQ2xKB9abcDEF"
	delete(fields, "email")
	cands, err = r.Candidates("person", fields)
	if err != nil {
		t.Fatal(err)
	}
	if cands[0].Value != "gh:janedoe" {
		t.Errorf("internal-form URL should fall through to gh: tier, got %q", cands[0].Value)
	}
}
