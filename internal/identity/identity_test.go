package identity

import "testing"

// The rules (SPEC §4a): each exists exactly once, here, shared by key
// derivation and ingress normalization. Which rules a type keys on is its
// type file's business (internal/registry); this file tests the rules.

func TestNormalizeURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"HTTPS://WWW.LinkedIn.com/posts/jane_x-7123/", "https://www.linkedin.com/posts/jane_x-7123"},
		{" https://x.com/jane/status/1?s=20#top ", "https://x.com/jane/status/1?s=20"},
		{"https://example.com/Path/With/Case", "https://example.com/Path/With/Case"},
		{"https://example.com", "https://example.com"},
		{"https://example.com/", "https://example.com"},
		{"example.com/no-scheme", ""},
		{"ftp://example.com/x", ""},
		{"", ""},
		{"not a url", ""},
	}
	for _, c := range cases {
		if got := NormalizeURL(c.in); got != c.want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// A stored canonical value is a fixed point of its rule.
	for _, c := range cases {
		if c.want == "" {
			continue
		}
		if got := NormalizeURL(c.want); got != c.want {
			t.Errorf("NormalizeURL is not idempotent on %q: got %q", c.want, got)
		}
	}
}

func TestKeyFormPerRule(t *testing.T) {
	cases := []struct{ rule, in, want string }{
		{"email", " Jane.Doe@Example.COM ", "jane.doe@example.com"},
		{"email", "not-an-email", ""},
		{"domain", "https://blog.Acme.co.uk/posts?x=1", "acme.co.uk"},
		{"domain", "www.acme.com:8443", "acme.com"},
		{"domain", "localhost", ""},
		// linkedin_url keys on the public slug, not the stored URL (SPEC §4).
		{"linkedin_url", "https://www.linkedin.com/in/Jane-Doe/?trk=public", "in/jane-doe"},
		{"linkedin_url", "in/Jane-Doe", "in/jane-doe"},
		{"linkedin_url", "HTTP://de.linkedin.com/in/jane-doe#about", "in/jane-doe"},
		{"linkedin_url", "https://linkedin.com/in/jos%C3%A9-p", "in/josé-p"},
		{"linkedin_url", "https://www.linkedin.com/sales/lead/ACwAAAbQxKB9,NAME", ""},
		{"handle", "@JaneDoe", "janedoe"},
		{"handle", "https://github.com/JaneDoe/", "janedoe"},
		{"url", "https://X.com/jane/status/1/", "https://x.com/jane/status/1"},
		{"trim", "anything", ""}, // not a key rule
	}
	for _, c := range cases {
		if got := KeyForm(c.rule, c.in); got != c.want {
			t.Errorf("KeyForm(%s, %q) = %q, want %q", c.rule, c.in, got, c.want)
		}
	}
}

func TestNormalizeName(t *testing.T) {
	if got := NormalizeName("  Jane   DOE "); got != "jane doe" {
		t.Errorf("NormalizeName = %q", got)
	}
}

func TestStrFormatsScalars(t *testing.T) {
	// CSV and JSON sources hand us numbers now and then.
	if got := Str(map[string]any{"n": 3600}, "n"); got != "3600" {
		t.Errorf("Str = %q", got)
	}
	if got := Str(map[string]any{"n": nil}, "n"); got != "" {
		t.Errorf("Str(nil) = %q", got)
	}
}
