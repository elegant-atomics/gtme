package instantly

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/trevorfox/gtm/internal/adapters"
	"github.com/trevorfox/gtm/internal/adapters/adaptertest"
	"github.com/trevorfox/gtm/internal/httpx"
	"github.com/trevorfox/gtm/internal/protocol"
)

const campaignID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

func routes(t *testing.T) map[string]adaptertest.Response {
	t.Helper()
	return map[string]adaptertest.Response{
		"GET /api/v2/campaigns": {Body: adaptertest.Fixture(t, "campaigns.json")},
		"POST /api/v2/leads":    {Body: adaptertest.Fixture(t, "lead.json")},
	}
}

func lead(key string, fields map[string]any) []protocol.Message {
	return []protocol.Message{adaptertest.Record(key, fields)}
}

func TestAddsLeadWithComposedLines(t *testing.T) {
	ResetCampaignCache()
	stub := &adaptertest.Stub{Routes: routes(t)}
	msgs, err := adaptertest.Run(t, &Adapter{HTTP: stub}, adaptertest.Input{
		Config: map[string]any{
			"campaign": "Q3 VP Marketing", "base_url": "https://instantly.test",
			// The egress mapping (ADR-018): first-class targets map into the
			// lead body, everything else becomes a custom variable. Injected
			// by the runner from the step-level variables: key.
			"variables": map[string]any{
				"first_name":      "first_name",
				"last_name":       "last_name",
				"company_name":    "company_name",
				"personalization": "first_line",
				"ps_line":         "ps_line",
				"title":           "title",
			},
		},
		Env: map[string]string{"INSTANTLY_API_KEY": "secret"},
		Records: lead("jane.doe@acme.com", map[string]any{
			"email":        "Jane.Doe@Acme.com",
			"first_name":   "Jane",
			"last_name":    "Doe",
			"company_name": "Acme Inc",
			"title":        "VP Marketing",
			"first_line":   "Saw your post on killing three channels.",
			"ps_line":      "PS: the CAC math checks out.",
		}),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Bearer auth, and the campaign name was resolved to an id before any lead.
	if got := stub.Calls[0].Header.Get("Authorization"); got != "Bearer secret" {
		t.Errorf("Authorization = %q", got)
	}
	if !strings.Contains(stub.Calls[0].URL, "/api/v2/campaigns") {
		t.Errorf("first call should resolve the campaign, got %s", stub.Calls[0].URL)
	}

	var body leadRequest
	if err := json.Unmarshal([]byte(stub.Calls[1].Body), &body); err != nil {
		t.Fatalf("lead body: %v\n%s", err, stub.Calls[1].Body)
	}
	if body.Campaign != campaignID {
		t.Errorf("campaign = %q, want the resolved id %q", body.Campaign, campaignID)
	}
	if body.Email != "jane.doe@acme.com" {
		t.Errorf("email = %q, want it lowercased", body.Email)
	}
	if body.FirstName != "Jane" || body.CompanyName != "Acme Inc" {
		t.Errorf("lead = %+v", body)
	}
	if body.Personalization != "Saw your post on killing three channels." {
		t.Errorf("personalization = %q", body.Personalization)
	}
	if body.CustomVariables["ps_line"] != "PS: the CAC math checks out." ||
		body.CustomVariables["title"] != "VP Marketing" {
		t.Errorf("custom variables = %v", body.CustomVariables)
	}
	if !body.SkipIfInCampaign {
		t.Error("skip_if_in_campaign should default to true — Instantly's own duplicate guard")
	}

	// The adapter acknowledges the delivery so the runner can record it.
	records := adaptertest.Records(msgs)
	if len(records) != 1 || records[0].Key.IdentityKey != "jane.doe@acme.com" {
		t.Errorf("records = %+v", records)
	}
	if len(adaptertest.Costs(msgs)) != 1 {
		t.Errorf("expected a COST message per lead")
	}
}

func TestResolvesCampaignOncePerInvocation(t *testing.T) {
	ResetCampaignCache()
	stub := &adaptertest.Stub{Routes: routes(t)}
	_, err := adaptertest.Run(t, &Adapter{HTTP: stub}, adaptertest.Input{
		Config: map[string]any{"campaign": "Q3 VP Marketing", "base_url": "https://instantly.test"},
		Env:    map[string]string{"INSTANTLY_API_KEY": "secret"},
		Records: []protocol.Message{
			adaptertest.Record("a@x.com", map[string]any{"email": "a@x.com", "first_name": "A"}),
			adaptertest.Record("b@x.com", map[string]any{"email": "b@x.com", "first_name": "B"}),
			adaptertest.Record("c@x.com", map[string]any{"email": "c@x.com", "first_name": "C"}),
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := stub.CallsTo("/api/v2/campaigns"); n != 1 {
		t.Errorf("campaign lookups = %d, want 1 for the whole batch", n)
	}
	if n := stub.CallsTo("/api/v2/leads"); n != 3 {
		t.Errorf("lead calls = %d, want 3", n)
	}
}

// TestResolvesCampaignOncePerProcess: the worker pool opens several adapter
// sessions per run, but the name resolves once (SPEC §10.6) — found by
// campaign zero's first armed run, which resolved once per session.
func TestResolvesCampaignOncePerProcess(t *testing.T) {
	ResetCampaignCache()
	stub := &adaptertest.Stub{Routes: routes(t)}
	for i := 0; i < 3; i++ { // three sessions, as three worker chunks would be
		_, err := adaptertest.Run(t, &Adapter{HTTP: stub}, adaptertest.Input{
			Config:  map[string]any{"campaign": "Q3 VP Marketing", "base_url": "https://instantly.test"},
			Env:     map[string]string{"INSTANTLY_API_KEY": "secret"},
			Records: lead("a@x.com", map[string]any{"email": "a@x.com"}),
		})
		if err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
	if n := stub.CallsTo("/api/v2/campaigns"); n != 1 {
		t.Errorf("campaign lookups = %d across 3 sessions, want 1", n)
	}
}

func TestCampaignIDPassesThroughWithoutALookup(t *testing.T) {
	stub := &adaptertest.Stub{Routes: routes(t)}
	_, err := adaptertest.Run(t, &Adapter{HTTP: stub}, adaptertest.Input{
		Config:  map[string]any{"campaign": campaignID, "base_url": "https://instantly.test"},
		Env:     map[string]string{"INSTANTLY_API_KEY": "secret"},
		Records: lead("a@x.com", map[string]any{"email": "a@x.com", "first_name": "A"}),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := stub.CallsTo("/api/v2/campaigns"); n != 0 {
		t.Errorf("campaign lookups = %d, want 0 when an id was given", n)
	}
}

// TestUnknownCampaignStopsBeforeDelivering is the important safety case: never
// invent a campaign, never deliver into the wrong one.
func TestUnknownCampaignStopsBeforeDelivering(t *testing.T) {
	ResetCampaignCache()
	stub := &adaptertest.Stub{Routes: routes(t)}
	_, err := adaptertest.Run(t, &Adapter{HTTP: stub}, adaptertest.Input{
		Config:  map[string]any{"campaign": "Campaign That Does Not Exist", "base_url": "https://instantly.test"},
		Env:     map[string]string{"INSTANTLY_API_KEY": "secret"},
		Records: lead("a@x.com", map[string]any{"email": "a@x.com", "first_name": "A"}),
	})
	if err == nil {
		t.Fatal("want an error for an unknown campaign")
	}
	if !strings.Contains(err.Error(), "no campaign named") {
		t.Errorf("error = %v", err)
	}
	if n := stub.CallsTo("/api/v2/leads"); n != 0 {
		t.Errorf("lead calls = %d, want 0 — nothing may be delivered before the campaign resolves", n)
	}
}

func TestCampaignMatchIsCaseInsensitive(t *testing.T) {
	ResetCampaignCache()
	stub := &adaptertest.Stub{Routes: routes(t)}
	_, err := adaptertest.Run(t, &Adapter{HTTP: stub}, adaptertest.Input{
		Config:  map[string]any{"campaign": "q3 vp marketing", "base_url": "https://instantly.test"},
		Env:     map[string]string{"INSTANTLY_API_KEY": "secret"},
		Records: lead("a@x.com", map[string]any{"email": "a@x.com", "first_name": "A"}),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestErrorsAreClassified(t *testing.T) {
	old := httpx.RetryBase
	httpx.RetryBase = 0
	defer func() { httpx.RetryBase = old }()

	r := routes(t)
	r["POST /api/v2/leads"] = adaptertest.Response{Status: 429, Body: `{"error":"slow down"}`}
	_, err := adaptertest.Run(t, &Adapter{HTTP: &adaptertest.Stub{Routes: r}}, adaptertest.Input{
		Config:  map[string]any{"campaign": campaignID, "base_url": "https://instantly.test"},
		Env:     map[string]string{"INSTANTLY_API_KEY": "secret"},
		Records: lead("a@x.com", map[string]any{"email": "a@x.com", "first_name": "A"}),
	})
	if err == nil {
		t.Fatal("want an error")
	}
	if code := httpx.ExitCodeFor(err); code != 4 {
		t.Errorf("exit code = %d, want 4 (rate limited)", code)
	}

	_, err = adaptertest.Run(t, &Adapter{HTTP: &adaptertest.Stub{Routes: routes(t)}}, adaptertest.Input{
		Config: map[string]any{"campaign": "x"},
	})
	if err == nil {
		t.Fatal("want an error without INSTANTLY_API_KEY")
	}
	if code := httpx.ExitCodeFor(err); code != 3 {
		t.Errorf("exit code = %d, want 3 (auth)", code)
	}
}

func TestConfigRequiresACampaign(t *testing.T) {
	if _, err := parseConfig(map[string]any{}); err == nil {
		t.Fatal("want an error without a campaign")
	}
	cfg, err := parseConfig(map[string]any{"campaign": "x",
		"variables": map[string]any{"ps_line": "ps_line", "personalization": "first_line"}})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if len(cfg.Variables) != 2 || cfg.Variables["personalization"] != "first_line" {
		t.Errorf("variables = %v", cfg.Variables)
	}
	if !cfg.SkipIfInCampaign {
		t.Error("skip_if_in_campaign should default to true")
	}
	if _, err := parseConfig(map[string]any{"campaign": "x",
		"variables": map[string]any{"ps_line": ""}}); err == nil {
		t.Error("want an error for a variables: entry with no field name")
	}
}

func TestLooksLikeID(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{campaignID, true},
		{"Q3 VP Marketing", false},
		{"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeez", false},
		{"", false},
	} {
		if got := looksLikeID(tc.in); got != tc.want {
			t.Errorf("looksLikeID(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestManifestContract(t *testing.T) {
	resolved, err := adapters.Resolve(ID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Manifest.Role != adapters.RoleDeliver {
		t.Errorf("role = %q", resolved.Manifest.Role)
	}
	// Dynamic needs with an email floor (SPEC §6, §10.6): everything else the
	// adapter sends derives from the step's variables: mapping.
	if !resolved.Manifest.NeedsDynamic() {
		t.Error("instantly must declare dynamic needs (ADR-019)")
	}
	if got := resolved.Manifest.RequiredNeeds(); strings.Join(got, ",") != "email" {
		t.Errorf("required needs (the static floor) = %v, want just email", got)
	}
}
