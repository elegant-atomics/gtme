package e2e

import (
	"encoding/json"
	"testing"
)

// TestShowIdentityPrintsProjection is the M3 acceptance test for `gtm show`
// (SPEC §8, ADR-006): the current-value projection for one identity, with
// --fields narrowing it and --provenance adding source/confidence/run.
func TestShowIdentityPrintsProjection(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("pipeline.yaml", csvToMockYAML)
	h.mustRun("run", "pipeline.yaml")

	res := h.mustRun("show", "jane.doe@acme.com")
	var out map[string]any
	if err := json.Unmarshal([]byte(res.stdout), &out); err != nil {
		t.Fatalf("show output must be JSON: %v\n%s", err, res.stdout)
	}
	if out["entity_type"] != "person" || out["identity_key"] != "jane.doe@acme.com" {
		t.Fatalf("show = %+v", out)
	}
	fields, ok := out["fields"].(map[string]any)
	if !ok {
		t.Fatalf("fields was not an object: %+v", out["fields"])
	}
	if fields["full_name"] != "Jane Doe" || fields["mock_score"] == nil {
		t.Errorf("fields = %+v, want full_name (sourced) and mock_score (enriched)", fields)
	}

	// --fields narrows the projection.
	narrow := h.mustRun("show", "jane.doe@acme.com", "--fields", "mock_score")
	var narrowed map[string]any
	if err := json.Unmarshal([]byte(narrow.stdout), &narrowed); err != nil {
		t.Fatalf("show --fields output must be JSON: %v", err)
	}
	nf := narrowed["fields"].(map[string]any)
	if len(nf) != 1 || nf["mock_score"] == nil {
		t.Errorf("narrowed fields = %+v, want exactly {mock_score}", nf)
	}

	// --provenance turns each value into {value, source, confidence, run_id, created_at}.
	prov := h.mustRun("show", "jane.doe@acme.com", "--fields", "mock_score", "--provenance")
	var provenanced map[string]any
	if err := json.Unmarshal([]byte(prov.stdout), &provenanced); err != nil {
		t.Fatalf("show --provenance output must be JSON: %v", err)
	}
	pf := provenanced["fields"].(map[string]any)["mock_score"].(map[string]any)
	for _, key := range []string{"value", "source", "confidence", "run_id", "created_at"} {
		if _, ok := pf[key]; !ok {
			t.Errorf("--provenance field missing %q: %+v", key, pf)
		}
	}
	if src, _ := pf["source"].(string); src == "" {
		t.Errorf("provenance source was empty: %+v", pf)
	}
}

// TestShowUnknownIdentity fails helpfully rather than printing nothing.
func TestShowUnknownIdentity(t *testing.T) {
	h := newHarness(t)
	res := h.run("show", "nobody@nowhere.example")
	if res.code != 2 {
		t.Errorf("exit = %d, want 2", res.code)
	}
	contains(t, res.stderr, "no identity known by key", "stderr")
}

// TestShowRunListsRecords is --run's half of the acceptance test: every
// record the run touched, NDJSON, with --limit capping the count.
func TestShowRunListsRecords(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("pipeline.yaml", csvToMockYAML)
	h.mustRun("run", "pipeline.yaml")

	res := h.mustRun("show", "--run", "last")
	lines := nonEmptyLines(res.stdout)
	if len(lines) != 3 {
		t.Fatalf("--run last printed %d records, want 3:\n%s", len(lines), res.stdout)
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatalf("each --run line must be JSON: %v", err)
	}
	for _, key := range []string{"entity_type", "identity_key", "state", "fields"} {
		if _, ok := row[key]; !ok {
			t.Errorf("--run record missing %q: %+v", key, row)
		}
	}

	limited := h.mustRun("show", "--run", "last", "--limit", "1")
	if got := len(nonEmptyLines(limited.stdout)); got != 1 {
		t.Errorf("--limit 1 printed %d records", got)
	}
}

// TestShowNeverWrites keeps `gtm show` read-only (SPEC §8, ADR-006): running
// it must not change the ledger's row counts.
func TestShowNeverWrites(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("pipeline.yaml", csvToMockYAML)
	h.mustRun("run", "pipeline.yaml")

	before := h.queryInt(`SELECT count(*) FROM field_values`)
	h.mustRun("show", "jane.doe@acme.com", "--provenance")
	h.mustRun("show", "--run", "last")
	after := h.queryInt(`SELECT count(*) FROM field_values`)
	if before != after {
		t.Errorf("field_values went from %d to %d rows — gtm show wrote to the ledger", before, after)
	}
}
