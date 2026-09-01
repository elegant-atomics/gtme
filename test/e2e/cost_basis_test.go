package e2e

// The M23 acceptance (SPEC §11, ADR-046, ADR-047), offline: a fixture
// binding with a templated rate runs at the operator's figure and its cost
// rows say `estimated`; the same binding with no rate set plans as `unset`
// and runs at $0; a fixture adapter emitting vendor-reported cost lands
// `measured` and a mixed run prints the split total; a strict source
// binding that does not declare `limit` accepts `limit: 1` and stops
// paginating after one record.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBindingYAML installs an inline binding into the harness home.
func (h *harness) writeBindingYAML(id, doc string) {
	h.t.Helper()
	dir := filepath.Join(h.home, ".gtme", "adapters", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "binding.yaml"), []byte(doc), 0o644); err != nil {
		h.t.Fatalf("write binding: %v", err)
	}
}

// A per-record enrich binding whose price is the operator's plan, not the
// author's guess (ADR-046): amount_usd templates from config.
const ratedLookupBinding = `id: rated/lookup
version: 1
role: enrich
entity_type: person
needs:
  type: object
  required: [email]
  properties:
    email: { type: string }
provides:
  type: object
  additionalProperties: false
  properties:
    title: { type: string }
config_schema:
  type: object
  additionalProperties: false
  properties:
    cost_per_record_usd:
      type: number
      minimum: 0
      description: What one lookup costs you on your plan
    base_url:
      type: string
request:
  method: GET
  url: "{{config.base_url}}/lookup"
  query:
    email: "{{record.email}}"
extract:
  records: person
  fields:
    title: title
cost:
  per: record
  amount_usd: "{{config.cost_per_record_usd}}"
`

// A process adapter that reads its cost back from the vendor (a fixture
// standing in for response cost metadata) and says so.
const measuredManifest = `{
  "id": "measured-enrich",
  "version": 1,
  "role": "enrich",
  "entity_type": "person",
  "needs": {"type":"object","required":["email"],"properties":{"email":{"type":"string"}}},
  "provides": {"type":"object","additionalProperties":false,"properties":{"headline":{"type":"string"}}}
}`

const measuredScript = `#!/usr/bin/env python3
import json, sys
PROVIDES = {"type":"object","additionalProperties":False,"properties":{"headline":{"type":"string"}}}
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    if msg.get("type") == "OPEN":
        print(json.dumps({"type":"SCHEMA","provides":PROVIDES}), flush=True)
    elif msg.get("type") == "RECORD":
        print(json.dumps({"type":"RECORD","key":msg["key"],"fields":{"headline":"fixture"}}), flush=True)
        print(json.dumps({"type":"COST","key":msg["key"],"provider":"vendor","amount_usd":0.02,"basis":"measured","detail":{"reported":True}}), flush=True)
    elif msg.get("type") == "END":
        break
print(json.dumps({"type":"END"}), flush=True)
`

const oneContactCSV = "email,full_name\njane@acme.com,Jane Doe\n"

func lookupServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"person":{"title":"VP Marketing"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ratedPipeline(srvURL, rate string) string {
	return `name: rated
source:
  use: csv/source
  with:
    path: contacts.csv
steps:
  - id: lookup
    use: rated/lookup
    cache: 0d
    with:
      base_url: ` + jsonString(srvURL) + `
` + rate
}

func TestTemplatedRateRunsAtTheOperatorsFigure(t *testing.T) {
	srv := lookupServer(t)
	h := newHarness(t)
	h.writeBindingYAML("rated-lookup", ratedLookupBinding)
	h.write("contacts.csv", oneContactCSV)
	p := h.write("p.yaml", ratedPipeline(srv.URL, "      cost_per_record_usd: 0.03\n"))

	plan := h.mustRun("plan", p)
	contains(t, plan.stderr, "est/record: $0.0300", "plan with a rate set")

	run := h.mustRun("run", p)
	contains(t, run.stderr, "total: $0.0300 (estimated) spent", "receipt")
	if got := h.queryStrings(`SELECT basis || ':' || CAST(amount_usd AS TEXT) FROM costs WHERE step_id = 'lookup'`); len(got) != 1 || got[0] != "estimated:0.03" {
		t.Errorf("cost rows = %v, want [estimated:0.03]", got)
	}
	runs := h.mustRun("runs", "last")
	contains(t, runs.stderr, "total: $0.0300 (estimated)", "gtme runs receipt")
}

func TestUnsetRatePlansUnsetAndRunsAtZero(t *testing.T) {
	srv := lookupServer(t)
	h := newHarness(t)
	h.writeBindingYAML("rated-lookup", ratedLookupBinding)
	h.write("contacts.csv", oneContactCSV)
	p := h.write("p.yaml", ratedPipeline(srv.URL, ""))

	plan := h.mustRun("plan", p)
	contains(t, plan.stderr, "est/record: unset", "plan with no rate")
	if res := plan.stderr; strings.Contains(res, "est/record: $0.0000") {
		t.Errorf("an unset rate must never plan as $0.0000:\n%s", res)
	}

	run := h.mustRun("run", p)
	contains(t, run.stderr, "total: $0 (estimated) spent", "receipt")
	if got := h.queryStrings(`SELECT basis || ':' || CAST(amount_usd AS TEXT) FROM costs WHERE step_id = 'lookup'`); len(got) != 1 || got[0] != "estimated:0.0" {
		t.Errorf("cost rows = %v, want [estimated:0.0]", got)
	}
}

func TestMeasuredCostLandsAndMixedRunSplitsTheTotal(t *testing.T) {
	srv := lookupServer(t)
	h := newHarness(t)
	h.writeBindingYAML("rated-lookup", ratedLookupBinding)
	h.writeAdapter("measured-enrich", measuredManifest, measuredScript)
	h.write("contacts.csv", oneContactCSV)
	p := h.write("p.yaml", ratedPipeline(srv.URL, "      cost_per_record_usd: 0.03\n")+`  - id: measured
    use: measured-enrich
    cache: 0d
`)

	run := h.mustRun("run", p)
	contains(t, run.stderr, "total: $0.0500 ($0.0200 measured + $0.0300 estimated) spent", "mixed receipt")
	if got := h.queryStrings(`SELECT basis FROM costs WHERE step_id = 'measured'`); len(got) != 1 || got[0] != "measured" {
		t.Errorf("measured step's cost basis = %v, want [measured]", got)
	}
	runs := h.mustRun("runs", "last")
	contains(t, runs.stderr, "total: $0.0500 ($0.0200 measured + $0.0300 estimated)", "gtme runs mixed receipt")

	// A purely measured run prints bare.
	h2 := newHarness(t)
	h2.writeAdapter("measured-enrich", measuredManifest, measuredScript)
	h2.write("contacts.csv", oneContactCSV)
	p2 := h2.write("p.yaml", `name: measured-only
source:
  use: csv/source
  with:
    path: contacts.csv
steps:
  - id: measured
    use: measured-enrich
`)
	run2 := h2.mustRun("run", p2)
	contains(t, run2.stderr, "total: $0.0200 spent", "measured receipt")
	if strings.Contains(run2.stderr, "estimated") {
		t.Errorf("a purely measured total must print bare:\n%s", run2.stderr)
	}
}

// A strict source binding (additionalProperties: false, no limit declared):
// limit is the engine's, accepted anyway, and pagination stops at the cap.
const strictSourceBinding = `id: strict/people
version: 1
role: source
entity_type: person
provides:
  type: object
  additionalProperties: false
  properties:
    email: { type: string }
    full_name: { type: string }
config_schema:
  type: object
  additionalProperties: false
  required: [base_url]
  properties:
    base_url:
      type: string
request:
  method: GET
  url: "{{config.base_url}}/people"
pagination:
  strategy: page
  param: page
  termination:
    empty_records: true
extract:
  records: people
  fields:
    email: email
    full_name: name
`

func TestUndeclaredLimitIsEngineOwned(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		page := r.URL.Query().Get("page")
		if page == "3" {
			fmt.Fprint(w, `{"people":[]}`)
			return
		}
		fmt.Fprintf(w, `{"people":[{"email":"p%s-a@acme.com","name":"A %s"},{"email":"p%s-b@acme.com","name":"B %s"}]}`, page, page, page, page)
	}))
	defer srv.Close()

	h := newHarness(t)
	h.writeBindingYAML("strict-people", strictSourceBinding)
	p := h.write("p.yaml", `name: strict
source:
  use: strict/people
  with:
    base_url: `+jsonString(srv.URL)+`
    limit: 1
steps: []
`)
	plan := h.run("plan", p)
	if plan.code != 0 {
		t.Fatalf("plan exit = %d — limit is a reserved engine key (ADR-047)\n%s", plan.code, plan.stderr)
	}
	run := h.mustRun("run", p)
	contains(t, run.stderr, "source: sourced 1 records", "run receipt")
	if requests != 1 {
		t.Errorf("requests = %d, want 1 — the engine stops paginating at the cap", requests)
	}
	if n := h.queryInt(`SELECT count(*) FROM identities WHERE entity_type = 'person'`); n != 1 {
		t.Errorf("person identities = %d, want 1", n)
	}

	// A key the binding does not declare and the engine does not own is
	// still a config error: the reservation is for limit alone.
	bad := h.write("bad.yaml", `name: strict
source:
  use: strict/people
  with:
    base_url: `+jsonString(srv.URL)+`
    limits: 1
steps: []
`)
	if res := h.run("plan", bad); res.code != 2 {
		t.Errorf("plan with an unknown key exit = %d, want 2\n%s", res.code, res.stderr)
	}
}
