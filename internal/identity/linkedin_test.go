package identity

import "testing"

// The public/internal/sales-nav shapes SPEC §4 (ADR-020) names, table-driven
// per milestone M7.
func TestClassifyLinkedIn(t *testing.T) {
	cases := []struct {
		in   string
		want LinkedInShape
	}{
		{"https://www.linkedin.com/in/jane-doe", LinkedInPublic},
		{"https://linkedin.com/in/Jane-Doe/?trk=x", LinkedInPublic},
		{"linkedin.com/pub/jane-doe/1/2/3", LinkedInPublic},
		{"https://www.linkedin.com/company/acme-corp", LinkedInPublic},
		{"https://www.linkedin.com/in/ACwAAAbQ2xKB9abcDEF", LinkedInInternal},
		{"https://www.linkedin.com/in/acoaaAbQ2xKB9abcDEF", LinkedInInternal},
		{"https://www.linkedin.com/profile/view?id=12345", LinkedInInternal},
		{"https://www.linkedin.com/talent/profile/xyz", LinkedInInternal},
		{"https://www.linkedin.com/sales/lead/ACwAAAbQ2xKB9,NAME_SEARCH", LinkedInSalesNav},
		{"https://www.linkedin.com/sales/people/ACwAAAbQ2xKB9", LinkedInSalesNav},
		// A short vanity slug that merely starts with the token letters stays public.
		{"https://www.linkedin.com/in/acwa", LinkedInPublic},
		{"", LinkedInNone},
		{"https://www.linkedin.com/in/", LinkedInNone},
		{"not a url at all", LinkedInNone},
	}
	for _, c := range cases {
		if got := ClassifyLinkedIn(c.in); got != c.want {
			t.Errorf("ClassifyLinkedIn(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestNormalizeLinkedInPublicOnly(t *testing.T) {
	if got := NormalizeLinkedIn("https://www.linkedin.com/in/Jane-Doe/?trk=x"); got != "in/jane-doe" {
		t.Errorf("public slug: got %q", got)
	}
	// Internal and sales-nav shapes are never key material (SPEC §4, ADR-020).
	for _, in := range []string{
		"https://www.linkedin.com/in/ACwAAAbQ2xKB9abcDEF",
		"https://www.linkedin.com/sales/lead/ACwAAAbQ2xKB9,NAME",
	} {
		if got := NormalizeLinkedIn(in); got != "" {
			t.Errorf("NormalizeLinkedIn(%q) = %q, want \"\"", in, got)
		}
	}
	if got := NormalizeLinkedInURL("linkedin.com/in/Jane-Doe/"); got != "https://www.linkedin.com/in/jane-doe" {
		t.Errorf("NormalizeLinkedInURL: got %q", got)
	}
}

func TestNormalizeHandle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"janedoe", "janedoe"},
		{"@JaneDoe", "janedoe"},
		{"https://github.com/JaneDoe", "janedoe"},
		{"https://www.twitter.com/janedoe/", "janedoe"},
		{"x.com/JaneDoe?ref=1", "janedoe"},
		{"", ""},
		{"two words", ""},
	}
	for _, c := range cases {
		if got := NormalizeHandle(c.in); got != c.want {
			t.Errorf("NormalizeHandle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The full §4 person tier ordering: email > public slug > gh: > tw: > nh:.
func TestPersonTierOrdering(t *testing.T) {
	fields := map[string]any{
		"email":           "jane@acme.com",
		"linkedin_url":    "https://www.linkedin.com/in/jane-doe",
		"github_username": "janedoe",
		"twitter_handle":  "@janedoe",
		"full_name":       "Jane Doe",
		"company_domain":  "acme.com",
	}
	cands, err := Candidates(Person, fields)
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
	cands, err = Candidates(Person, fields)
	if err != nil {
		t.Fatal(err)
	}
	if cands[0].Value != "gh:janedoe" {
		t.Errorf("internal-form URL should fall through to gh: tier, got %q", cands[0].Value)
	}
}
