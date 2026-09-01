package binding

import (
	"testing"

	"github.com/elegant-atomics/gtme/internal/adapters/adaptertest"
	"github.com/elegant-atomics/gtme/internal/protocol"
)

// A per-record enrich binding whose rate is the operator's, not the
// author's (ADR-046): amount_usd templates from config.
const templatedRateBinding = `
id: vendor/lookup
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
      default: "https://api.vendor.test"
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

func runTemplated(t *testing.T, config map[string]any) []protocol.Message {
	t.Helper()
	b, err := Parse([]byte(templatedRateBinding))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	stub := &adaptertest.Stub{Routes: map[string]adaptertest.Response{
		"GET /lookup": {Status: 200, Body: `{"person":{"title":"VP Marketing"}}`},
	}}
	msgs, err := adaptertest.Run(t, &Engine{B: b, HTTP: stub}, adaptertest.Input{
		Config:  config,
		Records: []protocol.Message{adaptertest.Record("jane@acme.com", map[string]any{"email": "jane@acme.com"})},
	})
	if err != nil {
		t.Fatalf("engine: %v\nlogs:\n%s", err, adaptertest.Logs(msgs))
	}
	return msgs
}

func TestTemplatedRateRunsAtTheOperatorsFigure(t *testing.T) {
	msgs := runTemplated(t, map[string]any{"cost_per_record_usd": 0.03})
	costs := adaptertest.Costs(msgs)
	if len(costs) != 1 || costs[0].Amount() != 0.03 {
		t.Fatalf("costs = %#v, want one $0.03 row", costs)
	}
	if costs[0].CostBasis() != protocol.BasisEstimated {
		t.Errorf("a rate multiplied out is estimated, got %q", costs[0].Basis)
	}
}

func TestUnsetRateCostsZero(t *testing.T) {
	msgs := runTemplated(t, map[string]any{})
	costs := adaptertest.Costs(msgs)
	if len(costs) != 1 || costs[0].Amount() != 0 {
		t.Fatalf("costs = %#v, want one $0 row", costs)
	}
	if costs[0].CostBasis() != protocol.BasisEstimated {
		t.Errorf("an unset rate is a $0 guess, got %q", costs[0].Basis)
	}
}

// The manifest bridge exposes the rate to the planner: resolved from a step's
// config it is the estimate; unresolved it is unset, never $0.
func TestManifestRateResolvesFromConfig(t *testing.T) {
	b, err := Parse([]byte(templatedRateBinding))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m, err := b.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if m.CostEstimate != nil {
		t.Errorf("a templated rate must not bridge as a static cost_estimate_usd, got %v", *m.CostEstimate)
	}
	if m.CostRate == nil {
		t.Fatal("a templated rate must bridge as a CostRate resolver")
	}
	if v, ok := m.CostRate(map[string]any{"cost_per_record_usd": 0.03}); !ok || v != 0.03 {
		t.Errorf("CostRate(set) = %v, %v; want 0.03, true", v, ok)
	}
	if _, ok := m.CostRate(map[string]any{}); ok {
		t.Error("CostRate(unset) resolved; want unset")
	}
}

// A static amount keeps its old bridge, and a number is still a number.
func TestStaticRateBridgesAsBefore(t *testing.T) {
	doc := []byte(`
id: vendor/static
version: 1
role: enrich
entity_type: person
needs: { type: object, properties: { email: { type: string } } }
provides: { type: object, additionalProperties: false, properties: { title: { type: string } } }
request: { method: GET, url: "https://api.vendor.test/x" }
extract: { records: person, fields: { title: title } }
cost: { per: record, amount_usd: 0.5 }
`)
	b, err := Parse(doc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m, err := b.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if m.CostEstimate == nil || *m.CostEstimate != 0.5 || m.CostRate != nil {
		t.Errorf("static rate bridged as estimate=%v rate set=%v", m.CostEstimate, m.CostRate != nil)
	}
}
