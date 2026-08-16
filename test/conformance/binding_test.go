package conformance

// The binding conformance kit (SPEC §10a, ADR-022): every shipped binding
// parses against spec/binding-schema.json, and the engine run against the
// binding's own conformance fixtures produces the expected canonical,
// registry-valid records — fixture payloads in, canonical records out. The
// same fixtures serve `--simulate` (SPEC §8), which is why keeping them good
// matters twice.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/trevorfox/gtm/internal/adapters"
	"github.com/trevorfox/gtm/internal/adapters/adaptertest"
	_ "github.com/trevorfox/gtm/internal/adapters/all"
	"github.com/trevorfox/gtm/internal/binding"
	"github.com/trevorfox/gtm/internal/protocol"
	"github.com/trevorfox/gtm/internal/registry"
)

// loadShipped loads one embedded binding and its fixtures.
func loadShipped(t *testing.T, name string) (*binding.Binding, *binding.FixtureSet) {
	t.Helper()
	dir, err := binding.ShippedFS(name)
	if err != nil {
		t.Fatalf("shipped binding %s: %v", name, err)
	}
	b, fixtures, err := binding.LoadFS(dir)
	if err != nil {
		t.Fatalf("loading %s: %v", name, err)
	}
	return b, fixtures
}

// TestShippedBindingsParse: every binding under spec/bindings/ conforms to the
// schema, bridges onto a valid §6 manifest, and names only registry-valid
// fields in its extraction (§4a enforcement extended to bindings).
func TestShippedBindingsParse(t *testing.T) {
	names := binding.Shipped()
	if len(names) < 4 {
		t.Fatalf("shipped bindings = %v, want at least the three ports plus attio", names)
	}
	reg, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		b, fixtures := loadShipped(t, name)
		if _, err := b.Manifest(); err != nil {
			t.Errorf("%s: manifest bridge: %v", name, err)
		}
		for field := range b.Extract.Fields {
			if err := reg.ValidateName(b.EntityType, field); err != nil {
				t.Errorf("%s: extract field: %v", name, err)
			}
		}
		if fixtures == nil {
			t.Errorf("%s: ships no conformance fixtures — a simulation gap by construction (SPEC §8)", name)
		}
	}
}

// TestAttioBuiltinResolves: attio/assert is registered as a built-in binding
// (SPEC §11 M8: the first net-new integration is pure YAML).
func TestAttioBuiltinResolves(t *testing.T) {
	res, err := adapters.Resolve("attio/assert")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Binding || !res.HasFixtures {
		t.Errorf("attio/assert: Binding=%v HasFixtures=%v, want true/true", res.Binding, res.HasFixtures)
	}
	if res.Manifest.Role != adapters.RoleDeliver {
		t.Errorf("role = %q", res.Manifest.Role)
	}
}

// checkRegistryValid asserts a record's canonical fields satisfy the registry
// (§4a layer 2, applied to engine output).
func checkRegistryValid(t *testing.T, entityType string, fields map[string]any) {
	t.Helper()
	reg, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range fields {
		if err := reg.CheckValue(entityType, name, normalizeJSON(v)); err != nil {
			t.Errorf("registry: %v", err)
		}
	}
}

func normalizeJSON(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return v
	}
	return out
}

func TestApolloBindingConformance(t *testing.T) {
	b, fixtures := loadShipped(t, "apollo-search")
	eng := &binding.Engine{B: b, HTTP: fixtures.Doer()}

	msgs, err := adaptertest.Run(t, eng, adaptertest.Input{
		Config: map[string]any{"query": "vp marketing"},
		Env:    map[string]string{"APOLLO_API_KEY": "k"},
	})
	if err != nil {
		t.Fatalf("engine: %v\nlogs:\n%s", err, adaptertest.Logs(msgs))
	}
	records := adaptertest.Records(msgs)
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2\nlogs:\n%s", len(records), adaptertest.Logs(msgs))
	}

	jane := records[0].Fields
	wantJane := map[string]any{
		"apollo.id": "5f3b0c1a2d", "first_name": "Jane", "last_name": "Doe",
		"full_name": "Jane Doe", "title": "VP Marketing", "seniority": "vp",
		"email": "jane.doe@acme.com", "email_status": "verified",
		"linkedin_url": "https://www.linkedin.com/in/jane-doe",
		"city":         "Austin", "state": "Texas", "country": "United States",
		"company_name": "Acme Inc", "company_website": "http://www.acme.com",
		"company_linkedin_url": "https://www.linkedin.com/company/acme",
		"company_industry":     "software", "company_domain": "acme.com",
		"company_employees": float64(120),
	}
	if !reflect.DeepEqual(normalizeJSON(jane), normalizeJSON(wantJane)) {
		t.Errorf("jane = %#v\nwant   %#v", normalizeJSON(jane), normalizeJSON(wantJane))
	}
	checkRegistryValid(t, "person", jane)

	// Bob's email is Apollo's locked-email placeholder: a sentinel absent value
	// that must never reach the ledger (it would poison the identity key).
	bob := records[1].Fields
	if _, has := bob["email"]; has {
		t.Errorf("bob's locked-email placeholder leaked through: %v", bob["email"])
	}
	if bob["email_status"] != "locked" || bob["company_domain"] != "globex.io" {
		t.Errorf("bob = %#v", bob)
	}
	checkRegistryValid(t, "person", bob)

	costs := adaptertest.Costs(msgs)
	if len(costs) != 1 || costs[0].Provider != "apollo" || costs[0].Amount() != 0 {
		t.Errorf("costs = %#v", costs)
	}
}

func TestHarvestBindingConformance(t *testing.T) {
	b, fixtures := loadShipped(t, "harvest-profile")
	eng := &binding.Engine{B: b, HTTP: fixtures.Doer()}

	// Looked up by internal-form URL: the resolved public linkedin_url must be
	// emitted (ADR-020's recovery path, skip_if_input lets it through).
	msgs, err := adaptertest.Run(t, eng, adaptertest.Input{
		Config: map[string]any{},
		Records: []protocol.Message{adaptertest.Record("in/acwaa123",
			map[string]any{"linkedin_internal_url": "https://www.linkedin.com/in/ACwAA123"})},
		Env: map[string]string{"HARVEST_API_KEY": "k"},
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	records := adaptertest.Records(msgs)
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1\nlogs:\n%s", len(records), adaptertest.Logs(msgs))
	}
	got := records[0].Fields
	want := map[string]any{
		"linkedin_url":    "https://www.linkedin.com/in/jane-doe",
		"headline":        "VP Marketing at Acme — demand gen, lifecycle, and the occasional spreadsheet",
		"about":           "I run marketing at Acme.",
		"location":        "Austin, Texas, United States",
		"current_role":    "VP Marketing",
		"current_company": "Acme Inc",
		"follower_count":  float64(4210),
	}
	if !reflect.DeepEqual(normalizeJSON(got), normalizeJSON(want)) {
		t.Errorf("fields = %#v\nwant    %#v", normalizeJSON(got), normalizeJSON(want))
	}
	if _, has := got["open_to_work"]; has {
		t.Error("open_to_work false should be absent (sentinel)")
	}
	checkRegistryValid(t, "person", got)

	costs := adaptertest.Costs(msgs)
	if len(costs) != 1 || costs[0].Amount() != 0.012 || costs[0].Provider != "harvest" {
		t.Errorf("costs = %#v", costs)
	}

	// Looked up by public URL: the already-known linkedin_url is not re-emitted
	// (skip_if_input).
	eng2 := &binding.Engine{B: b, HTTP: fixtures.Doer()}
	msgs, err = adaptertest.Run(t, eng2, adaptertest.Input{
		Config: map[string]any{},
		Records: []protocol.Message{adaptertest.Record("in/jane-doe",
			map[string]any{"linkedin_url": "https://www.linkedin.com/in/jane-doe"})},
		Env: map[string]string{"HARVEST_API_KEY": "k"},
	})
	if err != nil {
		t.Fatal(err)
	}
	records = adaptertest.Records(msgs)
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	if _, has := records[0].Fields["linkedin_url"]; has {
		t.Error("linkedin_url re-emitted despite skip_if_input")
	}
}

func TestInstantlyBindingConformance(t *testing.T) {
	b, fixtures := loadShipped(t, "instantly-add-to-campaign")
	_ = fixtures
	stub := &adaptertest.Stub{Routes: map[string]adaptertest.Response{
		"POST /api/v2/leads": {Body: `{"id":"lead_1","email":"jane.doe@acme.com"}`},
	}}
	eng := &binding.Engine{B: b, HTTP: stub}

	msgs, err := adaptertest.Run(t, eng, adaptertest.Input{
		Config: map[string]any{
			"campaign": "7d467891-4257-4a62-a8b2-08d3837f5714",
			"variables": map[string]any{
				"first_name": "full_name",
				"first_line": "first_line",
			},
		},
		Records: []protocol.Message{adaptertest.Record("jane.doe@acme.com", map[string]any{
			"email": "jane.doe@acme.com", "full_name": "Jane Doe", "first_line": "Hello Jane",
		})},
		Env: map[string]string{"INSTANTLY_API_KEY": "k"},
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	records := adaptertest.Records(msgs)
	if len(records) != 1 || len(records[0].Fields) != 0 {
		t.Fatalf("want one empty acknowledgement RECORD, got %#v", records)
	}

	if len(stub.Calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(stub.Calls))
	}
	call := stub.Calls[0]
	if call.Header.Get("Authorization") != "Bearer k" {
		t.Errorf("auth header = %q", call.Header.Get("Authorization"))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(call.Body), &body); err != nil {
		t.Fatal(err)
	}
	// First-class routing is declarative: first_name was consumed by the body
	// template, first_line rode the $variables splice into custom_variables.
	want := map[string]any{
		"campaign":            "7d467891-4257-4a62-a8b2-08d3837f5714",
		"email":               "jane.doe@acme.com",
		"skip_if_in_campaign": true,
		"first_name":          "Jane Doe",
		"custom_variables":    map[string]any{"first_line": "Hello Jane"},
	}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("request body = %#v\nwant %#v", body, want)
	}
}

func TestAttioBindingConformance(t *testing.T) {
	b, _ := loadShipped(t, "attio-assert")
	stub := &adaptertest.Stub{Routes: map[string]adaptertest.Response{
		"PUT /v2/objects/people/records": {Body: `{"data":{"id":{"record_id":"rec_1"}}}`},
	}}
	eng := &binding.Engine{B: b, HTTP: stub}

	msgs, err := adaptertest.Run(t, eng, adaptertest.Input{
		Config: map[string]any{
			"variables": map[string]any{"name": "full_name"},
		},
		Records: []protocol.Message{adaptertest.Record("jane.doe@acme.com", map[string]any{
			"email": "jane.doe@acme.com", "full_name": "Jane Doe",
		})},
		Env: map[string]string{"ATTIO_API_KEY": "k"},
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if records := adaptertest.Records(msgs); len(records) != 1 {
		t.Fatalf("records = %#v", records)
	}
	call := stub.Calls[0]
	if got := call.URL; !contains1(got, "matching_attribute=email_addresses") {
		t.Errorf("url = %q, want matching_attribute query", got)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(call.Body), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"data": map[string]any{"values": map[string]any{
		"email_addresses": []any{"jane.doe@acme.com"},
		"name":            "Jane Doe",
	}}}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("request body = %#v\nwant %#v", body, want)
	}
}

// TestSimulateGap: a binding without fixtures, asked to simulate, surfaces the
// gap instead of touching the network or silently passing (SPEC §8).
func TestSimulateGap(t *testing.T) {
	b, _ := loadShipped(t, "harvest-profile")
	eng := &binding.Engine{B: b, Fixtures: nil, HTTP: nil} // no fixtures, no network

	msgs, err := adaptertest.Run(t, eng, adaptertest.Input{
		Config: map[string]any{},
		Records: []protocol.Message{adaptertest.Record("in/jane-doe",
			map[string]any{"linkedin_url": "https://www.linkedin.com/in/jane-doe"})},
		Env: map[string]string{binding.SimulateEnv: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if records := adaptertest.Records(msgs); len(records) != 0 {
		t.Errorf("a fixtureless simulation produced records: %#v", records)
	}
	if logs := adaptertest.Logs(msgs); !contains1(logs, "simulation gap") {
		t.Errorf("logs = %q, want a simulation-gap warning", logs)
	}
}

// TestSimulateServesFixtures: with fixtures present and GTM_SIMULATE set, the
// engine answers from fixtures without any HTTP seam at all.
func TestSimulateServesFixtures(t *testing.T) {
	b, fixtures := loadShipped(t, "harvest-profile")
	eng := &binding.Engine{B: b, Fixtures: fixtures, HTTP: nil}

	msgs, err := adaptertest.Run(t, eng, adaptertest.Input{
		Config: map[string]any{},
		Records: []protocol.Message{adaptertest.Record("in/acwaa123",
			map[string]any{"linkedin_internal_url": "https://www.linkedin.com/in/ACwAA123"})},
		Env: map[string]string{binding.SimulateEnv: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	records := adaptertest.Records(msgs)
	if len(records) != 1 || records[0].Fields["headline"] == "" {
		t.Fatalf("simulated records = %#v", records)
	}
}

func contains1(haystack, needle string) bool { return strings.Contains(haystack, needle) }
