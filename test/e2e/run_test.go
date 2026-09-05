package e2e

import (
	"regexp"
	"strings"
	"testing"
)

const peopleCSV = `email,full_name,company_domain,linkedin_url,title
Jane.Doe@Acme.com,Jane Doe,acme.com,https://www.linkedin.com/in/jane-doe/,VP Marketing
bob@globex.io,Bob Stone,https://www.globex.io,linkedin.com/in/bob-stone,Head of Growth
carol@initech.dev,Carol Ray,initech.dev,,Director of Demand Gen
`

const csvToMockYAML = `name: csv-to-mock
version: 1

source:
  use: csv/source
  with:
    path: people.csv

steps:
  - id: mock
    use: mock-enrich-py
    cache: 30d
`

// TestRunSourceThroughExternalAdapter is the M2 acceptance test: a two-step
// pipeline whose second step is an external Python adapter, then a second run
// that must be a complete cache hit.
func TestRunSourceThroughExternalAdapter(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("pipeline.yaml", csvToMockYAML)

	first := h.mustRun("run", "pipeline.yaml")
	contains(t, first.stderr, "source: sourced 3 records", "first run stderr")
	contains(t, first.stderr, "mock: 3 in, 3 out", "first run stderr")
	if first.stdout != "" {
		t.Errorf("gtme run must keep stdout data-only, got:\n%s", first.stdout)
	}

	// Identities: three people, and the three companies their domains imply.
	if n := h.queryInt(`SELECT count(*) FROM identities WHERE entity_type = 'person'`); n != 3 {
		t.Errorf("person identities = %d, want 3", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM identities WHERE entity_type = 'company'`); n != 3 {
		t.Errorf("company identities = %d, want 3", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM relations WHERE relation = 'works_at'`); n != 3 {
		t.Errorf("works_at relations = %d, want 3", n)
	}

	// Keys are canonical: lowercased email, normalized domain.
	keys := h.queryStrings(`SELECT identity_key FROM identities WHERE entity_type = 'person' ORDER BY identity_key`)
	want := []string{"bob@globex.io", "carol@initech.dev", "jane.doe@acme.com"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("person keys = %v, want %v", keys, want)
	}

	// Enriched fields carry provenance: the external adapter's id and the run.
	if n := h.queryInt(
		`SELECT count(*) FROM field_values WHERE field = 'mock.score' AND source = 'mock-enrich-py@1' AND run_id IS NOT NULL`); n != 3 {
		t.Errorf("mock.score values with provenance = %d, want 3", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE source = 'csv/source@1' AND field = 'title'`); n != 3 {
		t.Errorf("sourced title values = %d, want 3", n)
	}

	// Every record finished the last step.
	states := h.queryStrings(`SELECT DISTINCT state FROM run_records`)
	if len(states) != 1 || states[0] != "mock" {
		t.Errorf("run record states = %v, want [mock]", states)
	}
	if n := h.queryInt(`SELECT count(*) FROM runs WHERE status = 'done'`); n != 1 {
		t.Errorf("done runs = %d, want 1", n)
	}
	// The adapter's COST message was recorded even though it costs nothing.
	if n := h.queryInt(`SELECT count(*) FROM costs WHERE provider = 'mock'`); n != 3 {
		t.Errorf("cost rows = %d, want 3", n)
	}

	// Second run: same source, but the ledger already knows every provided field,
	// so the adapter must not be called at all.
	second := h.mustRun("run", "pipeline.yaml")
	contains(t, second.stderr, "mock: 3 in, 0 out, 3 cached", "second run stderr")
	contains(t, second.stderr, "avoided via cache", "second run receipt")

	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field = 'mock.score'`); n != 3 {
		t.Errorf("mock.score rows after second run = %d, want 3 (no adapter call)", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE event = 'skipped_cache' AND step_id = 'mock'`); n != 3 {
		t.Errorf("skipped_cache events = %d, want 3", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM costs`); n != 3 {
		t.Errorf("cost rows after second run = %d, want 3 (cache spends nothing)", n)
	}

	// A third run with a shorter cache window than the data's age still hits,
	// but with cache off the adapter is called again.
	h.write("pipeline-nocache.yaml", strings.Replace(csvToMockYAML, "    cache: 30d\n", "    cache: 0d\n", 1))
	third := h.mustRun("run", "pipeline-nocache.yaml")
	contains(t, third.stderr, "mock: 3 in, 3 out", "third run stderr")
	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field = 'mock.score'`); n != 6 {
		t.Errorf("mock.score rows after cache-off run = %d, want 6 (append-only)", n)
	}
}

// TestRunRejectsUnknownAdapterAndBadYAML covers the operator's most common
// mistakes.
func TestRunRejectsUnknownAdapterAndBadYAML(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)

	h.write("unknown.yaml", `name: x
source:
  use: nope/nothing
  with:
    path: people.csv
`)
	res := h.run("run", "unknown.yaml")
	if res.code != 2 {
		t.Errorf("exit = %d, want 2 for an unknown adapter\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "unknown adapter", "stderr")

	h.write("waterfall.yaml", `name: x
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: emails
    waterfall:
      - use: a/b
`)
	res = h.run("run", "waterfall.yaml")
	if res.code != 2 {
		t.Errorf("exit = %d, want 2 for reserved waterfall syntax", res.code)
	}
	contains(t, res.stderr, "not implemented in v0", "stderr")

	h.write("typo.yaml", `name: x
source:
  use: csv/source
  wth:
    path: people.csv
`)
	res = h.run("run", "typo.yaml")
	if res.code != 2 {
		t.Errorf("exit = %d, want 2 for an unknown YAML field", res.code)
	}
}

// TestPlanFailsWhenSourceHasNoIdentityPath: a source whose columns can derive
// no §4 identity at all is caught at plan time (SPEC §7, ADR-018), before a
// single row is read — the run never starts.
func TestPlanFailsWhenSourceHasNoIdentityPath(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", "title,city\nVP Marketing,Austin\nHead of Growth,Boston\n")
	h.write("pipeline.yaml", `name: unkeyable
source:
  use: csv/source
  with:
    path: people.csv
`)
	res := h.run("plan", "pipeline.yaml")
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "no identity-key path", "stderr")
	res = h.run("run", "pipeline.yaml")
	if res.code != 2 {
		t.Errorf("run exit = %d, want 2 (same plan gate)", res.code)
	}
	if n := h.queryInt(`SELECT count(*) FROM identities`); n != 0 {
		t.Errorf("identities = %d, want 0", n)
	}
}

// TestRunDropsRecordThatCannotBeKeyed proves a single record with nothing
// identifying is dropped without taking the run down — the plan passed (an
// email column exists), but this row's cell is empty.
func TestRunDropsRecordThatCannotBeKeyed(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", "email,title\njane@acme.com,VP Marketing\n,Head of Growth\n")
	h.write("pipeline.yaml", `name: half-keyable
source:
  use: csv/source
  with:
    path: people.csv
`)
	res := h.mustRun("run", "pipeline.yaml")
	contains(t, res.stderr, "dropped a record", "stderr")
	if n := h.queryInt(`SELECT count(*) FROM identities WHERE entity_type = 'person'`); n != 1 {
		t.Errorf("person identities = %d, want 1 (the keyable row)", n)
	}

	// SPEC §5: the drop is recorded, not just printed. A failed step event with
	// the reason keeps the run explainable after the terminal is gone (#30).
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE step_id = 'source' AND event = 'failed'`); n != 1 {
		t.Errorf("failed step events for source = %d, want 1", n)
	}
	details := h.queryStrings(`SELECT detail FROM step_events WHERE step_id = 'source' AND event = 'failed'`)
	if len(details) != 1 || !strings.Contains(details[0], "reason") {
		t.Errorf("failed event detail = %v, want a reason", details)
	}

	// And the read-back receipt reports it, matching what the live run printed:
	// claimed -, done 1, cached -, failed 1.
	readback := h.mustRun("runs", "last")
	if !regexp.MustCompile(`(?m)^source\s+-\s+1\s+-\s+1\s`).MatchString(readback.stderr) {
		t.Errorf("runs last does not report failed = 1 for source:\n%s", readback.stderr)
	}
}
