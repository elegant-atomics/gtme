package harvest

import (
	"strings"
	"testing"

	"github.com/trevorfox/gtm/internal/adapters"
	"github.com/trevorfox/gtm/internal/adapters/adaptertest"
	"github.com/trevorfox/gtm/internal/httpx"
	"github.com/trevorfox/gtm/internal/protocol"
)

func routes(t *testing.T) map[string]adaptertest.Response {
	t.Helper()
	return map[string]adaptertest.Response{
		// The posts route is listed first so substring matching does not send
		// profile-posts requests to the profile route.
		"GET /linkedin/profile-posts": {Body: adaptertest.Fixture(t, "posts.json")},
		"GET /linkedin/profile":       {Body: adaptertest.Fixture(t, "profile.json")},
	}
}

// one builds a single-record input.
func one(key string, fields map[string]any) []protocol.Message {
	return []protocol.Message{adaptertest.Record(key, fields)}
}

func TestEnrichOneProfile(t *testing.T) {
	stub := &adaptertest.Stub{Routes: routes(t)}
	a := &Adapter{HTTP: stub}

	msgs, err := adaptertest.Run(t, a, adaptertest.Input{
		Config:  map[string]any{"base_url": "https://harvest.test", "posts_limit": float64(3)},
		Env:     map[string]string{"HARVEST_API_KEY": "secret"},
		Records: one("jane.doe@acme.com", map[string]any{"linkedin_url": "in/jane-doe"}),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The ledger stores a slug; Harvest is asked for a full URL, with the key in
	// the header it documents.
	if got := stub.Calls[0].Header.Get("X-API-Key"); got != "secret" {
		t.Errorf("X-API-Key = %q", got)
	}
	if !strings.Contains(stub.Calls[0].URL, "url=https%3A%2F%2Fwww.linkedin.com%2Fin%2Fjane-doe") {
		t.Errorf("profile URL = %s", stub.Calls[0].URL)
	}

	records := adaptertest.Records(msgs)
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	fields := records[0].Fields
	if headline, _ := fields["headline"].(string); !strings.HasPrefix(headline, "VP Marketing at Acme") {
		t.Errorf("headline = %#v", fields["headline"])
	}
	if fields["current_role"] != "VP Marketing" || fields["current_company"] != "Acme Inc" {
		t.Errorf("current role/company = %#v / %#v", fields["current_role"], fields["current_company"])
	}
	if fields["location"] != "Austin, Texas, United States" {
		t.Errorf("location = %#v", fields["location"])
	}
	if fields["follower_count"] != float64(4210) {
		t.Errorf("follower_count = %#v", fields["follower_count"])
	}
	if _, ok := fields["open_to_work"]; ok {
		t.Error("open_to_work should be omitted when false, not stored as a false")
	}

	history, ok := fields["role_history"].([]any)
	if !ok || len(history) != 2 {
		t.Fatalf("role_history = %#v", fields["role_history"])
	}
	if history[0] != "VP Marketing at Acme Inc (2022–present)" {
		t.Errorf("current role line = %#v", history[0])
	}
	if history[1] != "Director of Demand Gen at Initech (2019–2022)" {
		t.Errorf("previous role line = %#v", history[1])
	}

	posts, ok := fields["recent_posts"].([]any)
	if !ok {
		t.Fatalf("recent_posts = %#v", fields["recent_posts"])
	}
	if len(posts) != 3 {
		t.Errorf("recent_posts = %d, want 3 (empty post skipped, fourth over the limit)", len(posts))
	}
	if first, _ := posts[0].(string); !strings.Contains(first, "cut our CAC in half") {
		t.Errorf("first post = %#v", posts[0])
	}

	// Output matches the contract, and the call is priced for the receipt.
	m, err := adapters.ParseManifest(manifestJSON)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if err := m.ValidateProvides(fields); err != nil {
		t.Errorf("output does not match provides: %v", err)
	}
	costs := adaptertest.Costs(msgs)
	if len(costs) != 1 || costs[0].Amount() != 0.012 {
		t.Fatalf("costs = %+v", costs)
	}
	if costs[0].Key == nil || costs[0].Key.IdentityKey != "jane.doe@acme.com" {
		t.Errorf("cost should be attributed to the record: %+v", costs[0].Key)
	}
}

func TestPostsLimitZeroSkipsTheSecondCall(t *testing.T) {
	stub := &adaptertest.Stub{Routes: routes(t)}
	msgs, err := adaptertest.Run(t, &Adapter{HTTP: stub}, adaptertest.Input{
		Config:  map[string]any{"base_url": "https://harvest.test", "posts_limit": float64(0)},
		Env:     map[string]string{"HARVEST_API_KEY": "secret"},
		Records: one("jane.doe@acme.com", map[string]any{"linkedin_url": "https://www.linkedin.com/in/jane-doe"}),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := stub.CallsTo("/linkedin/profile-posts"); n != 0 {
		t.Errorf("posts calls = %d, want 0 when posts_limit is 0", n)
	}
	if _, ok := adaptertest.Records(msgs)[0].Fields["recent_posts"]; ok {
		t.Error("recent_posts should be absent")
	}
}

func TestPostsFailureKeepsTheProfile(t *testing.T) {
	old := httpx.RetryBase
	httpx.RetryBase = 0
	defer func() { httpx.RetryBase = old }()

	r := routes(t)
	r["GET /linkedin/profile-posts"] = adaptertest.Response{Status: 500, Body: `{"error":"boom"}`}
	msgs, err := adaptertest.Run(t, &Adapter{HTTP: &adaptertest.Stub{Routes: r}}, adaptertest.Input{
		Config:  map[string]any{"base_url": "https://harvest.test"},
		Env:     map[string]string{"HARVEST_API_KEY": "secret"},
		Records: one("jane.doe@acme.com", map[string]any{"linkedin_url": "in/jane-doe"}),
	})
	if err != nil {
		t.Fatalf("a failed posts call must not fail the record: %v", err)
	}
	records := adaptertest.Records(msgs)
	if len(records) != 1 || records[0].Fields["headline"] == nil {
		t.Fatalf("expected the profile anyway, got %+v", records)
	}
	if !strings.Contains(adaptertest.Logs(msgs), "posts for jane.doe@acme.com") {
		t.Errorf("expected a warning, got: %s", adaptertest.Logs(msgs))
	}
}

func TestRecordWithoutLinkedInIsSkipped(t *testing.T) {
	stub := &adaptertest.Stub{Routes: routes(t)}
	msgs, err := adaptertest.Run(t, &Adapter{HTTP: stub}, adaptertest.Input{
		Config:  map[string]any{"base_url": "https://harvest.test"},
		Env:     map[string]string{"HARVEST_API_KEY": "secret"},
		Records: one("nolink@acme.com", map[string]any{}),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(adaptertest.Records(msgs)) != 0 {
		t.Error("no record should be emitted without a linkedin_url")
	}
	if len(stub.Calls) != 0 {
		t.Error("no HTTP call should be made without a linkedin_url — that would spend a credit for nothing")
	}
	if !strings.Contains(adaptertest.Logs(msgs), "no linkedin_url") {
		t.Errorf("logs = %s", adaptertest.Logs(msgs))
	}
}

func TestProfileErrorsAreClassified(t *testing.T) {
	old := httpx.RetryBase
	httpx.RetryBase = 0
	defer func() { httpx.RetryBase = old }()

	for name, tc := range map[string]struct {
		status   int
		wantCode int
	}{
		"rejected key": {403, 3},
		"rate limited": {429, 4},
		"server error": {503, 1},
	} {
		t.Run(name, func(t *testing.T) {
			stub := &adaptertest.Stub{Routes: map[string]adaptertest.Response{
				"GET /linkedin/profile": {Status: tc.status, Body: `{"error":"nope"}`},
			}}
			_, err := adaptertest.Run(t, &Adapter{HTTP: stub}, adaptertest.Input{
				Config:  map[string]any{"base_url": "https://harvest.test"},
				Env:     map[string]string{"HARVEST_API_KEY": "secret"},
				Records: one("jane@acme.com", map[string]any{"linkedin_url": "in/jane"}),
			})
			if err == nil {
				t.Fatal("want an error")
			}
			if code := httpx.ExitCodeFor(err); code != tc.wantCode {
				t.Errorf("exit code = %d, want %d (%v)", code, tc.wantCode, err)
			}
		})
	}
}

func TestMissingCredentialIsAnAuthError(t *testing.T) {
	_, err := adaptertest.Run(t, &Adapter{HTTP: &adaptertest.Stub{}}, adaptertest.Input{
		Config: map[string]any{},
	})
	if err == nil {
		t.Fatal("want an error without HARVEST_API_KEY")
	}
	if code := httpx.ExitCodeFor(err); code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
}

func TestManifestContract(t *testing.T) {
	resolved, err := adapters.Resolve(ID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Manifest.Role != adapters.RoleEnrich {
		t.Errorf("role = %q", resolved.Manifest.Role)
	}
	if got := resolved.Manifest.RequiredNeeds(); len(got) != 1 || got[0] != "linkedin_url" {
		t.Errorf("required needs = %v", got)
	}
	if resolved.Manifest.FreshnessDays != 30 {
		t.Errorf("freshness_days = %d, want 30", resolved.Manifest.FreshnessDays)
	}
	if resolved.Manifest.CostEstimate == nil {
		t.Error("a paid adapter should publish a cost estimate so the receipt can show savings")
	}
}
