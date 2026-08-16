package apollo

import (
	"strings"
	"testing"

	"github.com/trevorfox/gtm/internal/adapters"
	"github.com/trevorfox/gtm/internal/adapters/adaptertest"
	"github.com/trevorfox/gtm/internal/httpx"
	"github.com/trevorfox/gtm/internal/protocol"
)

func TestSearchMapsPeopleAndCompanies(t *testing.T) {
	stub := &adaptertest.Stub{Routes: map[string]adaptertest.Response{
		"POST /api/v1/mixed_people/search": {Body: adaptertest.Fixture(t, "search.json")},
	}}
	a := &Adapter{HTTP: stub}

	msgs, err := adaptertest.Run(t, a, adaptertest.Input{
		Config: map[string]any{"query": "vp marketing", "base_url": "https://apollo.test", "limit": float64(10)},
		Env:    map[string]string{"APOLLO_API_KEY": "secret"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The API key rides in the header Apollo documents, and the body carries the search.
	if got := stub.Calls[0].Header.Get("X-Api-Key"); got != "secret" {
		t.Errorf("X-Api-Key = %q", got)
	}
	if !strings.Contains(stub.Calls[0].Body, `"q_keywords":"vp marketing"`) {
		t.Errorf("request body = %s", stub.Calls[0].Body)
	}

	records := adaptertest.Records(msgs)
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	jane := records[0].Fields
	for field, want := range map[string]any{
		"email":      "jane.doe@acme.com",
		"first_name": "Jane",
		"full_name":  "Jane Doe",
		"title":      "VP Marketing",
		"seniority":  "vp",
		// Normalized at the adapter's own boundary (SPEC §4a): canonical
		// public-URL form, whatever scheme/host variant Apollo returned.
		"linkedin_url":      "https://www.linkedin.com/in/jane-doe",
		"company_name":      "Acme Inc",
		"company_domain":    "acme.com",
		"company_employees": float64(120),
		"city":              "Austin",
	} {
		if got := jane[field]; got != want {
			t.Errorf("%s = %#v, want %#v", field, got, want)
		}
	}
	if records[0].Key != nil {
		t.Error("a source must not invent identity keys")
	}

	// Apollo's locked-email placeholder must never reach the ledger — it would
	// become an identity key.
	bob := records[1].Fields
	if _, ok := bob["email"]; ok {
		t.Errorf("locked email leaked: %#v", bob["email"])
	}
	if bob["email_status"] != "locked" {
		t.Errorf("email_status = %#v", bob["email_status"])
	}
	if bob["company_domain"] != "globex.io" {
		t.Errorf("company_domain = %#v", bob["company_domain"])
	}

	// Output validates against the manifest contract.
	m, err := adapters.ParseManifest(manifestJSON)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	for i, rec := range records {
		if err := m.ValidateProvides(rec.Fields); err != nil {
			t.Errorf("record %d does not match provides: %v", i, err)
		}
	}
	if len(adaptertest.Costs(msgs)) != 1 {
		t.Error("expected one step-level COST message")
	}
}

func TestSearchHonoursLimitAndPaginates(t *testing.T) {
	page := adaptertest.Fixture(t, "search.json")
	// Pretend there are more pages so the adapter would keep going if not limited.
	page = strings.Replace(page, `"total_pages": 1`, `"total_pages": 5`, 1)
	stub := &adaptertest.Stub{Routes: map[string]adaptertest.Response{
		"POST /api/v1/mixed_people/search": {Body: page},
	}}
	a := &Adapter{HTTP: stub}

	msgs, err := adaptertest.Run(t, a, adaptertest.Input{
		Config: map[string]any{"titles": []any{"vp marketing"}, "base_url": "https://apollo.test", "limit": float64(1)},
		Env:    map[string]string{"APOLLO_API_KEY": "secret"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := len(adaptertest.Records(msgs)); n != 1 {
		t.Errorf("records = %d, want 1 (limit)", n)
	}
	if n := len(stub.Calls); n != 1 {
		t.Errorf("HTTP calls = %d, want 1 — the limit should stop paging", n)
	}
}

func TestSearchRequiresCredentialsAndFilters(t *testing.T) {
	a := &Adapter{HTTP: &adaptertest.Stub{}}

	_, err := adaptertest.Run(t, a, adaptertest.Input{
		Config: map[string]any{"query": "x"},
	})
	if err == nil {
		t.Fatal("want an error without APOLLO_API_KEY")
	}
	if code := httpx.ExitCodeFor(err); code != 3 {
		t.Errorf("exit code = %d, want 3 (auth)", code)
	}

	_, err = adaptertest.Run(t, a, adaptertest.Input{
		Config: map[string]any{},
		Env:    map[string]string{"APOLLO_API_KEY": "secret"},
	})
	if err == nil {
		t.Fatal("want an error when no search filter is given")
	}
}

func TestSearchClassifiesProviderErrors(t *testing.T) {
	old := httpx.RetryBase
	httpx.RetryBase = 0
	defer func() { httpx.RetryBase = old }()

	cases := map[string]struct {
		status   int
		wantCode int
	}{
		"rejected key": {401, 3},
		"rate limited": {429, 4},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stub := &adaptertest.Stub{Routes: map[string]adaptertest.Response{
				"POST /api/v1/mixed_people/search": {Status: tc.status, Body: `{"error":"nope"}`},
			}}
			a := &Adapter{HTTP: stub}
			_, err := adaptertest.Run(t, a, adaptertest.Input{
				Config: map[string]any{"query": "x", "base_url": "https://apollo.test"},
				Env:    map[string]string{"APOLLO_API_KEY": "secret"},
			})
			if err == nil {
				t.Fatal("want an error")
			}
			if code := httpx.ExitCodeFor(err); code != tc.wantCode {
				t.Errorf("exit code = %d, want %d", code, tc.wantCode)
			}
		})
	}
}

func TestManifestIsRegistered(t *testing.T) {
	resolved, err := adapters.Resolve(ID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Manifest.Role != adapters.RoleSource {
		t.Errorf("role = %q", resolved.Manifest.Role)
	}
	if got := resolved.Manifest.Credentials; len(got) != 1 || got[0] != "APOLLO_API_KEY" {
		t.Errorf("credentials = %v", got)
	}
	// A source must not require input fields.
	if len(resolved.Manifest.RequiredNeeds()) != 0 {
		t.Errorf("required needs = %v", resolved.Manifest.RequiredNeeds())
	}
}

func TestSchemaIsFirstMessage(t *testing.T) {
	stub := &adaptertest.Stub{Routes: map[string]adaptertest.Response{
		"POST /api/v1/mixed_people/search": {Body: `{"people":[],"pagination":{"total_pages":1}}`},
	}}
	msgs, err := adaptertest.Run(t, &Adapter{HTTP: stub}, adaptertest.Input{
		Config: map[string]any{"query": "x", "base_url": "https://apollo.test"},
		Env:    map[string]string{"APOLLO_API_KEY": "secret"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(msgs) == 0 || msgs[0].Type != protocol.TypeSchema {
		t.Fatalf("first message = %+v, want SCHEMA", msgs)
	}
	if !strings.Contains(string(msgs[0].Provides), "company_domain") {
		t.Errorf("schema = %s", msgs[0].Provides)
	}
}
