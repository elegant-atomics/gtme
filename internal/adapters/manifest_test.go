package adapters

import (
	"strings"
	"testing"
)

const harvestManifest = `{
  "id": "harvest/profile",
  "version": 1,
  "role": "enrich",
  "entity_type": "person",
  "needs": {
    "type": "object",
    "required": ["linkedin_url"],
    "properties": {"linkedin_url": {"type": "string"}}
  },
  "provides": {
    "type": "object",
    "additionalProperties": false,
    "properties": {
      "headline": {"type": "string"},
      "recent_posts": {"type": "array", "items": {"type": "string"}}
    }
  },
  "credentials": ["HARVEST_API_KEY"],
  "config_schema": {"type": "object", "additionalProperties": false, "properties": {"depth": {"type": "integer"}}},
  "freshness_days": 30,
  "cost_estimate_usd": 0.012
}`

func TestParseManifest(t *testing.T) {
	m, err := ParseManifest([]byte(harvestManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Source() != "harvest/profile@1" {
		t.Errorf("Source() = %q", m.Source())
	}
	if got := m.NeedsFields(); len(got) != 1 || got[0] != "linkedin_url" {
		t.Errorf("NeedsFields = %v", got)
	}
	if got := m.RequiredNeeds(); len(got) != 1 || got[0] != "linkedin_url" {
		t.Errorf("RequiredNeeds = %v", got)
	}
	if got := m.ProvidesFields(); strings.Join(got, ",") != "headline,recent_posts" {
		t.Errorf("ProvidesFields = %v", got)
	}
	if m.CostEstimate == nil || *m.CostEstimate != 0.012 {
		t.Errorf("CostEstimate = %v", m.CostEstimate)
	}
	if Wildcard(m.Provides) {
		t.Error("a closed provides schema is not a wildcard")
	}
}

func TestManifestValidation(t *testing.T) {
	m, err := ParseManifest([]byte(harvestManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}

	if err := m.ValidateNeeds(map[string]any{"linkedin_url": "in/jane"}); err != nil {
		t.Errorf("ValidateNeeds: %v", err)
	}
	if err := m.ValidateNeeds(map[string]any{}); err == nil {
		t.Error("a missing required need must fail")
	}
	if err := m.ValidateNeeds(map[string]any{"linkedin_url": 42}); err == nil {
		t.Error("a wrongly typed need must fail")
	}

	if err := m.ValidateProvides(map[string]any{"headline": "VP", "recent_posts": []string{"a"}}); err != nil {
		t.Errorf("ValidateProvides: %v", err)
	}
	if err := m.ValidateProvides(map[string]any{"headline": 3}); err == nil {
		t.Error("output with the wrong type must not reach the ledger")
	}
	if err := m.ValidateProvides(map[string]any{"invented": "x"}); err == nil {
		t.Error("a closed provides schema must reject undeclared fields")
	}

	if err := m.ValidateConfig(map[string]any{"depth": 2}); err != nil {
		t.Errorf("ValidateConfig: %v", err)
	}
	if err := m.ValidateConfig(map[string]any{"dpth": 2}); err == nil {
		t.Error("a config typo must fail")
	}
	if err := m.ValidateConfig(nil); err != nil {
		t.Errorf("nil config with no required keys should validate: %v", err)
	}
}

func TestParseManifestRejectsBadDocuments(t *testing.T) {
	cases := map[string]string{
		"no id":            `{"version":1,"role":"enrich","entity_type":"person"}`,
		"bad role":         `{"id":"a/b","version":1,"role":"transmogrify","entity_type":"person"}`,
		"no entity type":   `{"id":"a/b","version":1,"role":"enrich"}`,
		"zero version":     `{"id":"a/b","version":0,"role":"enrich","entity_type":"person"}`,
		"unknown field":    `{"id":"a/b","version":1,"role":"enrich","entity_type":"person","provdes":{}}`,
		"invalid schema":   `{"id":"a/b","version":1,"role":"enrich","entity_type":"person","needs":{"type":123}}`,
		"malformed json":   `{"id":`,
		"provides not obj": `{"id":"a/b","version":1,"role":"enrich","entity_type":"person","provides":"nope"}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseManifest([]byte(doc)); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestWildcardProvides(t *testing.T) {
	m, err := ParseManifest([]byte(`{"id":"csv/x","version":1,"role":"source","entity_type":"person",
	  "provides":{"type":"object","additionalProperties":true}}`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if !Wildcard(m.Provides) {
		t.Error("additionalProperties:true means the adapter may provide anything")
	}
	if err := m.ValidateProvides(map[string]any{"anything": "goes"}); err != nil {
		t.Errorf("ValidateProvides: %v", err)
	}
}

func TestResolveUnknownAdapter(t *testing.T) {
	t.Setenv("GTME_ADAPTER_PATH", t.TempDir())
	t.Setenv("GTME_HOME", t.TempDir())
	_, err := Resolve("nope/nothing")
	if err == nil {
		t.Fatal("want an error")
	}
	if !IsNotFound(err) {
		t.Errorf("IsNotFound = false for %v", err)
	}
	if !strings.Contains(err.Error(), "looked for") {
		t.Errorf("the error should say where it looked: %v", err)
	}
}
