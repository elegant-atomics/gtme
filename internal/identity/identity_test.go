package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func nh(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "nh:" + hex.EncodeToString(sum[:])
}

func TestKeyFor(t *testing.T) {
	cases := []struct {
		name       string
		entityType string
		fields     map[string]any
		want       string
		wantStr    Strength
		wantErr    bool
	}{
		{
			name:       "person email wins and is lowercased",
			entityType: Person,
			fields:     map[string]any{"email": "  Jane.Doe@Example.COM ", "linkedin_url": "https://linkedin.com/in/jane"},
			want:       "jane.doe@example.com",
			wantStr:    StrengthEmail,
		},
		{
			name:       "person linkedin when no email",
			entityType: Person,
			fields:     map[string]any{"linkedin_url": "https://www.linkedin.com/in/Jane-Doe/?trk=public"},
			want:       "in/jane-doe",
			wantStr:    StrengthSlug,
		},
		{
			name:       "linkedin without scheme or host",
			entityType: Person,
			fields:     map[string]any{"linkedin_url": "in/Jane-Doe"},
			want:       "in/jane-doe",
			wantStr:    StrengthSlug,
		},
		{
			name:       "linkedin with locale subdomain and fragment",
			entityType: Person,
			fields:     map[string]any{"linkedin_url": "HTTP://de.linkedin.com/in/jane-doe#about"},
			want:       "in/jane-doe",
			wantStr:    StrengthSlug,
		},
		{
			name:       "linkedin percent escapes are decoded",
			entityType: Person,
			fields:     map[string]any{"linkedin_url": "https://linkedin.com/in/jos%C3%A9-p"},
			want:       "in/josé-p",
			wantStr:    StrengthSlug,
		},
		{
			name:       "empty email string is not a key",
			entityType: Person,
			fields:     map[string]any{"email": "   ", "linkedin_url": "linkedin.com/in/x"},
			want:       "in/x",
			wantStr:    StrengthSlug,
		},
		{
			name:       "malformed email falls through to name hash",
			entityType: Person,
			fields:     map[string]any{"email": "not-an-email", "full_name": "Jane Doe", "company_domain": "acme.com"},
			want:       nh("jane doe|acme.com"),
			wantStr:    StrengthNameHash,
		},
		{
			name:       "name hash normalizes whitespace and domain",
			entityType: Person,
			fields:     map[string]any{"name": " Jane   DOE ", "company_domain": "https://www.Acme.com/careers"},
			want:       nh("jane doe|acme.com"),
			wantStr:    StrengthNameHash,
		},
		{
			name:       "name hash from first and last name",
			entityType: Person,
			fields:     map[string]any{"first_name": "Jane", "last_name": "Doe"},
			want:       nh("jane doe|"),
			wantStr:    StrengthNameHash,
		},
		{
			name:       "person with nothing identifying",
			entityType: Person,
			fields:     map[string]any{"title": "VP Marketing"},
			wantErr:    true,
		},
		{
			name:       "company registrable domain from url",
			entityType: Company,
			fields:     map[string]any{"domain": "https://blog.Acme.co.uk/posts?x=1"},
			want:       "acme.co.uk",
			wantStr:    StrengthDomain,
		},
		{
			name:       "company domain from website with port",
			entityType: Company,
			fields:     map[string]any{"website": "www.acme.com:8443"},
			want:       "acme.com",
			wantStr:    StrengthDomain,
		},
		{
			name:       "company name hash when no domain",
			entityType: Company,
			fields:     map[string]any{"name": "Acme  Inc"},
			want:       nh("acme inc"),
			wantStr:    StrengthNameHash,
		},
		{
			name:       "bare hostname without dot is not a domain",
			entityType: Company,
			fields:     map[string]any{"domain": "localhost", "name": "Acme Inc"},
			want:       nh("acme inc"),
			wantStr:    StrengthNameHash,
		},
		{
			name:       "company with nothing identifying",
			entityType: Company,
			fields:     map[string]any{"employees": 50},
			wantErr:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := KeyFor(tc.entityType, tc.fields)
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
	if _, err := KeyFor("robot", map[string]any{"email": "a@b.com"}); err == nil {
		t.Fatal("want error for unknown entity type")
	}
}

func TestCandidatesOrderedStrongestFirst(t *testing.T) {
	got, err := Candidates(Person, map[string]any{
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

func TestNonStringScalarsAreUsable(t *testing.T) {
	// CSV and JSON sources hand us numbers now and then.
	got, err := KeyFor(Company, map[string]any{"name": 3600})
	if err != nil {
		t.Fatalf("KeyFor: %v", err)
	}
	if got.Value != nh("3600") {
		t.Errorf("key = %q, want %q", got.Value, nh("3600"))
	}
}
