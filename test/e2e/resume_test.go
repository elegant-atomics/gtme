package e2e

import (
	"strings"
	"testing"
)

// TestResumeAfterAdapterFailure is the M4 acceptance test for resume: an adapter
// dies mid-step, the records it already answered stay done, and --resume finishes
// the rest without paying for the ones that are finished.
func TestResumeAfterAdapterFailure(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)

	// Concurrency 1 puts all three records in one session, so the induced failure
	// on the second record leaves a partially finished step — the interesting case.
	h.write("failing.yaml", `name: resumable
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: mock
    use: mock-enrich-py
    cache: 30d
    with:
      fail_on: bob@globex.io
`)

	first := h.runWithEnv([]string{"GTME_CONCURRENCY=1"}, "", "run", "failing.yaml")
	if first.code == 0 {
		t.Fatalf("expected a non-zero exit when the adapter dies\nstderr:\n%s", first.stderr)
	}
	contains(t, first.stderr, "induced failure", "stderr")

	if n := h.queryInt(`SELECT count(*) FROM runs WHERE status = 'failed'`); n != 1 {
		t.Errorf("failed runs = %d, want 1", n)
	}
	// The record answered before the crash is kept: the ledger is append-only.
	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field = 'mock.score'`); n != 1 {
		t.Fatalf("mock.score rows = %d, want 1 (partial output survives)", n)
	}
	done := h.queryStrings(`SELECT identity_key FROM identities i
	  JOIN run_records rr ON rr.identity_id = i.id WHERE rr.state = 'mock'`)
	if len(done) != 1 || done[0] != "jane.doe@acme.com" {
		t.Fatalf("records past the step = %v, want [jane.doe@acme.com]", done)
	}
	if n := h.queryInt(
		`SELECT count(*) FROM step_events WHERE step_id='mock' AND event='failed' AND identity_id IS NOT NULL`); n != 2 {
		t.Errorf("per-record failed events = %d, want 2 (the killed record and the one never reached)", n)
	}
	if n := h.queryInt(
		`SELECT count(*) FROM step_events WHERE step_id='mock' AND event='failed' AND identity_id IS NULL`); n != 1 {
		t.Errorf("step-level failed events = %d, want 1 (the adapter's own death)", n)
	}

	runID := h.queryStrings(`SELECT id FROM runs`)[0]

	// Resume the same run with the failure removed.
	h.write("fixed.yaml", `name: resumable
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: mock
    use: mock-enrich-py
    cache: 30d
`)
	second := h.mustRun("run", "fixed.yaml", "--resume", runID)
	contains(t, second.stderr, "resuming run "+runID, "stderr")
	contains(t, second.stderr, "already sourced (3 records)", "stderr")
	contains(t, second.stderr, "mock: 2 in, 2 out", "stderr")

	// Exactly the two unfinished records were processed: no duplicate work for the
	// one that was already done.
	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field = 'mock.score'`); n != 3 {
		t.Errorf("mock.score rows = %d, want 3 (2 new, 1 untouched)", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM run_records WHERE state = 'mock'`); n != 3 {
		t.Errorf("records past the step = %d, want 3", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM runs`); n != 1 {
		t.Errorf("runs = %d, want 1 — resume must not mint a new run", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM runs WHERE status = 'done'`); n != 1 {
		t.Errorf("done runs = %d, want 1", n)
	}
	// The source was not re-drained, so no second round of csv/source writes.
	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE source = 'csv/source@1' AND field = 'email'`); n != 3 {
		t.Errorf("sourced email rows = %d, want 3 (source ran once)", n)
	}
}

// TestResumeLastAndUnknownRun covers the operator-facing edges of --resume.
func TestResumeLastAndUnknownRun(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("pipeline.yaml", csvToMockYAML)

	res := h.run("run", "pipeline.yaml", "--resume", "last")
	if res.code != 2 {
		t.Errorf("exit = %d, want 2 when there is nothing to resume", res.code)
	}
	contains(t, res.stderr, "no runs to resume", "stderr")

	h.mustRun("run", "pipeline.yaml")

	res = h.run("run", "pipeline.yaml", "--resume", "01ZZZZZZZZZZZZZZZZZZZZZZZZ")
	if res.code != 2 {
		t.Errorf("exit = %d, want 2 for an unknown run", res.code)
	}
	contains(t, res.stderr, "unknown run", "stderr")

	// Resuming a finished run is a no-op that reports itself as such.
	res = h.mustRun("run", "pipeline.yaml", "--resume", "last")
	contains(t, res.stderr, "already sourced", "stderr")
	if strings.Contains(res.stderr, "mock: 3 in") {
		t.Error("resuming a finished run must not redo its work")
	}
}
