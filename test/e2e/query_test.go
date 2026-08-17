package e2e

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestQueryReadsTheLedgerAndSavesSegments is the M5 acceptance test for
// `gtme query`: rows on stdout, a saved segment that replays, and no way to write.
func TestQueryReadsTheLedgerAndSavesSegments(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("pipeline.yaml", csvToMockYAML)
	h.mustRun("run", "pipeline.yaml")

	const sql = `SELECT i.identity_key, fv.field, fv.value
	             FROM identities i JOIN field_values fv ON fv.identity_id = i.id
	             WHERE fv.field = 'mock.score' ORDER BY i.identity_key`

	res := h.mustRun("query", sql)
	lines := nonEmptyLines(res.stdout)
	if len(lines) != 3 {
		t.Fatalf("stdout had %d rows, want 3:\n%s", len(lines), res.stdout)
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatalf("rows must be NDJSON: %v\n%s", err, lines[0])
	}
	if row["identity_key"] != "bob@globex.io" || row["field"] != "mock.score" {
		t.Errorf("first row = %v", row)
	}
	contains(t, res.stderr, "3 rows", "stderr")

	// Save it as a segment, then replay it by name.
	saved := h.mustRun("query", "--save", "scored", sql)
	contains(t, saved.stderr, `saved segment "scored"`, "stderr")

	replayed := h.mustRun("query", "--name", "scored")
	if len(nonEmptyLines(replayed.stdout)) != 3 {
		t.Errorf("replaying the segment returned:\n%s", replayed.stdout)
	}

	listed := h.mustRun("query", "--list")
	contains(t, listed.stderr, "scored", "segment list")

	// Table and CSV formats are for humans and spreadsheets.
	table := h.mustRun("query", "--format", "table", "--limit", "1", sql)
	contains(t, table.stdout, "identity_key", "table header")
	if got := len(nonEmptyLines(table.stdout)); got != 2 {
		t.Errorf("table rows = %d, want header + 1", got)
	}
	csvOut := h.mustRun("query", "--format", "csv", sql)
	if !strings.HasPrefix(csvOut.stdout, "identity_key,field,value") {
		t.Errorf("csv output = %q", csvOut.stdout)
	}
}

// TestQueryRefusesToWrite keeps `gtme query` read-only in both layers.
func TestQueryRefusesToWrite(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("pipeline.yaml", csvToMockYAML)
	h.mustRun("run", "pipeline.yaml")

	for _, tc := range []struct{ sql, want string }{
		{`DELETE FROM identities`, "read-only"},
		{`UPDATE field_values SET value = '"tampered"'`, "read-only"},
		{`DROP TABLE runs`, "read-only"},
		{`INSERT INTO identities (id, entity_type, identity_key, created_at) VALUES ('x','person','y','z')`, "read-only"},
		// A second statement smuggled behind a SELECT gets its own message.
		{`SELECT 1; DELETE FROM identities`, "one statement at a time"},
	} {
		res := h.run("query", tc.sql)
		if res.code != 2 {
			t.Errorf("exit = %d for %q, want 2", res.code, tc.sql)
		}
		contains(t, res.stderr, tc.want, "stderr for "+tc.sql)
	}
	if n := h.queryInt(`SELECT count(*) FROM identities WHERE entity_type = 'person'`); n != 3 {
		t.Errorf("identities = %d, want 3 — a write got through", n)
	}

	// A real SQL error is a validation error, not a crash.
	res := h.run("query", "SELECT nope FROM nowhere")
	if res.code != 2 {
		t.Errorf("exit = %d for a bad table, want 2", res.code)
	}
	contains(t, res.stderr, "query failed", "stderr")
}

// TestQueryUnknownSegment fails helpfully.
func TestQueryUnknownSegment(t *testing.T) {
	h := newHarness(t)
	res := h.run("query", "--name", "nope")
	if res.code != 2 {
		t.Errorf("exit = %d, want 2", res.code)
	}
	contains(t, res.stderr, `no saved segment named "nope"`, "stderr")
}
