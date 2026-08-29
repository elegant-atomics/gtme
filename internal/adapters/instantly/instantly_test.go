package instantly

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/adapters/adaptertest"
	"github.com/elegant-atomics/gtme/internal/httpx"
	"github.com/elegant-atomics/gtme/internal/protocol"
)

const campaignID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

const leadID = "99999999-8888-7777-6666-555555555555"

func routes(t *testing.T) map[string]adaptertest.Response {
	t.Helper()
	return map[string]adaptertest.Response{
		"GET /api/v2/campaigns":               {Body: adaptertest.Fixture(t, "campaigns.json")},
		"POST /api/v2/leads":                  {Body: adaptertest.Fixture(t, "lead.json")},
		"GET /api/v2/leads/" + leadID:         {Body: adaptertest.Fixture(t, "lead-read.json")},
		"GET /api/v2/campaigns/" + campaignID: {Body: adaptertest.Fixture(t, "campaign.json")},
	}
}

// preflights returns the PREFLIGHT messages.
func preflights(msgs []protocol.Message) []protocol.Message {
	var out []protocol.Message
	for _, m := range msgs {
		if m.Type == protocol.TypePreflight {
			out = append(out, m)
		}
	}
	return out
}

// attests returns the ATTEST messages.
func attests(msgs []protocol.Message) []protocol.Message {
	var out []protocol.Message
	for _, m := range msgs {
		if m.Type == protocol.TypeAttest {
			out = append(out, m)
		}
	}
	return out
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
	if n := stub.CallsTo("POST https://instantly.test/api/v2/leads"); n != 3 {
		t.Errorf("lead creates = %d, want 3", n)
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

// TestAttestsThreeWays is ADR-036 at the adapter: after the create, the lead
// is re-read and compared field by field — confirmed when everything sent is
// stored, contradicted when a stored value disagrees, inconclusive when the
// re-read fails or the shape carries no readable value.
func TestAttestsThreeWays(t *testing.T) {
	ResetCampaignCache()
	input := func() adaptertest.Input {
		return adaptertest.Input{
			Config: map[string]any{
				"campaign": campaignID, "base_url": "https://instantly.test",
				"variables": map[string]any{"first_name": "first_name", "personalization": "first_line", "ps_line": "ps_line", "title": "title"},
			},
			Env: map[string]string{"INSTANTLY_API_KEY": "secret"},
			Records: lead("jane.doe@acme.com", map[string]any{
				"email": "Jane.Doe@Acme.com", "first_name": "Jane", "title": "VP Marketing",
				"first_line": "Saw your post on killing three channels.", "ps_line": "PS: the CAC math checks out.",
			}),
		}
	}

	// Confirmed: the fixture re-read matches everything sent.
	stub := &adaptertest.Stub{Routes: routes(t)}
	msgs, err := adaptertest.Run(t, &Adapter{HTTP: stub}, input())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stub.CallsTo("GET https://instantly.test/api/v2/leads/"+leadID) != 1 {
		t.Errorf("the lead must be re-read once; calls = %+v", stub.Calls)
	}
	got := attests(msgs)
	if len(got) != 1 || got[0].Status != protocol.AttestConfirmed || got[0].Key.IdentityKey != "jane.doe@acme.com" {
		t.Fatalf("attest = %+v, want confirmed", got)
	}
	// Order: the acknowledgement RECORD precedes the ATTEST (SPEC §5).
	var order []string
	for _, m := range msgs {
		if m.Type == protocol.TypeRecord || m.Type == protocol.TypeAttest {
			order = append(order, m.Type)
		}
	}
	if strings.Join(order, ",") != "RECORD,ATTEST" {
		t.Errorf("order = %v", order)
	}

	// Contradicted: the stored personalization is not what was sent.
	r := routes(t)
	r["GET /api/v2/leads/"+leadID] = adaptertest.Response{Body: strings.Replace(
		adaptertest.Fixture(t, "lead-read.json"), "Saw your post on killing three channels.", "", 1)}
	msgs, err = adaptertest.Run(t, &Adapter{HTTP: &adaptertest.Stub{Routes: r}}, input())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got = attests(msgs)
	if len(got) != 1 || got[0].Status != protocol.AttestContradicted ||
		!strings.Contains(got[0].Reason, `personalization: sent "Saw your post on killing three channels.", stored ""`) {
		t.Errorf("attest = %+v, want contradicted naming the field", got)
	}

	// Inconclusive: the re-read fails (no route → 404).
	r = routes(t)
	delete(r, "GET /api/v2/leads/"+leadID)
	msgs, err = adaptertest.Run(t, &Adapter{HTTP: &adaptertest.Stub{Routes: r}}, input())
	if err != nil {
		t.Fatalf("Run: %v (a failed re-read must not fail the delivery)", err)
	}
	got = attests(msgs)
	if len(got) != 1 || got[0].Status != protocol.AttestInconclusive || !strings.Contains(got[0].Reason, "re-read failed") {
		t.Errorf("attest = %+v, want inconclusive", got)
	}

	// Inconclusive: an unrecognised shape — no custom variables readable.
	r = routes(t)
	r["GET /api/v2/leads/"+leadID] = adaptertest.Response{Body: `{"id":"` + leadID + `","email":"jane.doe@acme.com","first_name":"Jane","personalization":"Saw your post on killing three channels."}`}
	msgs, err = adaptertest.Run(t, &Adapter{HTTP: &adaptertest.Stub{Routes: r}}, input())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got = attests(msgs)
	if len(got) != 1 || got[0].Status != protocol.AttestInconclusive || !strings.Contains(got[0].Reason, "custom variable ps_line, custom variable title") {
		t.Errorf("attest = %+v, want inconclusive naming the unreadable variables", got)
	}

	// Inconclusive, not contradicted: a first-class field the response omits
	// entirely is unreadable — absence is not disagreement (ADR-036).
	r = routes(t)
	r["GET /api/v2/leads/"+leadID] = adaptertest.Response{Body: strings.Replace(
		adaptertest.Fixture(t, "lead-read.json"), `"personalization": "Saw your post on killing three channels.",`, "", 1)}
	msgs, err = adaptertest.Run(t, &Adapter{HTTP: &adaptertest.Stub{Routes: r}}, input())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got = attests(msgs)
	if len(got) != 1 || got[0].Status != protocol.AttestInconclusive || !strings.Contains(got[0].Reason, "no readable value for personalization") {
		t.Errorf("attest = %+v, want inconclusive for an omitted field", got)
	}
}

func TestManifestDeclaresAttestation(t *testing.T) {
	resolved, err := adapters.Resolve(ID)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Manifest.Attests {
		t.Error("instantly/add-to-campaign is the first attesting adapter (ADR-036)")
	}
}

// TestPreflightChecksTheLiveCampaign is ADR-040 at the adapter: a preflight
// session sends nothing and answers from the campaign read — ok when the
// campaign is active, the sequence long enough, every variable referenced
// and every variant carrying it; blocked, naming the check, otherwise;
// inconclusive when the campaign cannot be read or the shape is unreadable.
func TestPreflightChecksTheLiveCampaign(t *testing.T) {
	ResetCampaignCache()
	run := func(r map[string]adaptertest.Response, variables map[string]any) (*adaptertest.Stub, []protocol.Message) {
		t.Helper()
		stub := &adaptertest.Stub{Routes: r}
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		go func() {
			w := protocol.NewWriter(inW)
			w.Write(protocol.Message{Type: protocol.TypeOpen, StepID: "send", RunID: "run1", Preflight: true,
				Config: map[string]any{"campaign": campaignID, "base_url": "https://instantly.test", "variables": variables}})
			w.Write(protocol.End())
			inW.Close()
		}()
		go func() {
			outW.CloseWithError((&Adapter{HTTP: stub}).Run(context.Background(),
				adapters.Ports{In: inR, Out: outW, Log: io.Discard, Env: map[string]string{"INSTANTLY_API_KEY": "secret"}}))
		}()
		var msgs []protocol.Message
		rd := protocol.NewReader(outR)
		for {
			m, err := rd.Next()
			if err != nil {
				break
			}
			msgs = append(msgs, m)
		}
		return stub, msgs
	}
	vars := map[string]any{"first_name": "first_name", "personalization": "first_line",
		"body_step_1": "outreach.body_1", "body_step_2": "outreach.body_2", "body_step_3": "outreach.body_3",
		"ps_line": "ps_line", "title": "title"}

	// ok: the fixture campaign references everything, three steps, and its
	// only A/B step carries body_step_3 in both variants.
	stub, msgs := run(routes(t), vars)
	got := preflights(msgs)
	if len(got) != 1 || got[0].Status != protocol.PreflightOK {
		t.Fatalf("preflight = %+v, want ok", got)
	}
	if stub.CallsTo("POST") != 0 {
		t.Errorf("a preflight session must send nothing; calls = %+v", stub.Calls)
	}
	if len(got[0].Checks) < 6 {
		t.Errorf("checks = %+v, want status, step count, and one per merge variable", got[0].Checks)
	}
	for _, m := range msgs {
		if m.Type == protocol.TypeRecord || m.Type == protocol.TypeAttest || m.Type == protocol.TypeCost {
			t.Errorf("preflight session emitted %s", m.Type)
		}
	}

	campaign := adaptertest.Fixture(t, "campaign.json")
	cases := []struct {
		name, body string
		vars       map[string]any
		status     string
		reason     string
	}{
		{"paused", strings.Replace(campaign, `"status": 1`, `"status": 2`, 1), vars, protocol.PreflightBlocked, "campaign is not Active"},
		{"too few steps", campaign, map[string]any{"body_step_4": "outreach.body_4"}, protocol.PreflightBlocked, "the copy assumes 4 step(s) but the sequence has 3"},
		{"unreferenced variable", campaign, map[string]any{"opener": "first_line"}, protocol.PreflightBlocked, "no sequence step references {{opener}}"},
		{"unfilled variant", strings.Replace(campaign, `{{body_step_3}} — {{title}}`, `{{title}}`, 1), vars, protocol.PreflightBlocked, "step 3 has a variant without {{body_step_3}}"},
		{"unreadable shape", `{"id":"` + campaignID + `","name":"Q3 VP Marketing"}`, vars, protocol.PreflightInconclusive, "no readable status, sequence"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := routes(t)
			r["GET /api/v2/campaigns/"+campaignID] = adaptertest.Response{Body: tc.body}
			_, msgs := run(r, tc.vars)
			got := preflights(msgs)
			if len(got) != 1 || got[0].Status != tc.status || !strings.Contains(got[0].Reason, tc.reason) {
				t.Errorf("preflight = %+v, want %s naming %q", got, tc.status, tc.reason)
			}
		})
	}

	// Unreadable target: the read fails → inconclusive, not blocked.
	r := routes(t)
	r["GET /api/v2/campaigns/"+campaignID] = adaptertest.Response{Status: 503, Body: `{"error":"down"}`}
	_, msgs = run(r, vars)
	got = preflights(msgs)
	if len(got) != 1 || got[0].Status != protocol.PreflightInconclusive || !strings.Contains(got[0].Reason, "campaign could not be read") {
		t.Errorf("preflight = %+v, want inconclusive", got)
	}
}
