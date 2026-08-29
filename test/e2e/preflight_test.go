package e2e

// M17 acceptance (SPEC §11, ADR-040): a fixture deliver adapter declaring
// preflights answers ok, blocked, and inconclusive in turn — ok delivers
// with the checks on the receipt; blocked under --dry-run reports and writes
// nothing, and armed fails the step with zero deliveries, zero record
// sessions, records at the previous state, run failed, and --resume after
// the fix delivers; inconclusive delivers with a warning; a non-preflighting
// adapter is never asked; two deliver steps preflight each.

import (
	"path/filepath"
	"strings"
	"testing"
)

const preflightYAML = `name: preflight
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: send
    use: mock/preflight
    with:
      campaign: q3
    variables:
      name: full_name
    idempotency: email
`

func TestDeliverPreflightGatesTheSend(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("p.yaml", preflightYAML)
	log := filepath.Join(h.work, "delivered.ndjson")
	env := func(mode string) []string {
		return []string{"MOCK_PREFLIGHT=" + mode, "MOCK_DELIVER_LOG=" + log, "GTME_CONCURRENCY=1"}
	}

	// Blocked, dry: the check is reported, nothing is written, the run is done.
	res := h.runWithEnv(env("blocked"), "", "run", "p.yaml", "--dry-run")
	if res.code != 0 {
		t.Fatalf("dry exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "send: preflight BLOCKED — fixture: template step 2 does not reference {{body_step_2}}", "dry receipt")
	contains(t, res.stderr, "✗ campaign active", "dry receipt checks")
	if n := h.queryInt(`SELECT count(*) FROM deliveries`); n != 0 {
		t.Errorf("deliveries after a dry run = %d", n)
	}

	// Blocked, armed: the step fails before a record moves — zero
	// deliveries, zero record sessions, records at the previous state.
	res = h.runWithEnv(env("blocked"), "", "run", "p.yaml")
	if res.code == 0 {
		t.Fatalf("a blocked preflight must fail the run\nstderr:\n%s", res.stderr)
	}
	contains(t, res.stderr, "preflight blocked — fixture: template step 2 does not reference {{body_step_2}} (nothing was sent", "error")
	if n := h.queryInt(`SELECT count(*) FROM deliveries`); n != 0 {
		t.Errorf("deliveries after a blocked run = %d", n)
	}
	if lines := countLines(t, log); lines != 0 {
		t.Errorf("the adapter received %d record(s) in a blocked run", lines)
	}
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE step_id = 'send' AND event = 'claimed'`); n != 0 {
		t.Errorf("record sessions opened = %d, want 0", n)
	}
	runID := h.queryStrings(`SELECT id FROM runs WHERE status = 'failed' ORDER BY started_at DESC LIMIT 1`)[0]
	if n := h.queryInt(`SELECT count(*) FROM run_records WHERE run_id = '` + runID + `' AND state = 'sourced'`); n != 3 {
		t.Errorf("records at the previous state = %d, want 3", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE step_id = 'send' AND event = 'preflight' AND detail LIKE '%blocked%'`); n < 1 {
		t.Errorf("the preflight is recorded as a step event")
	}

	// Fix the target (flip the fixture) and resume: preflight runs again,
	// the records deliver.
	res = h.runWithEnv(env("ok"), "", "run", "p.yaml", "--resume", runID)
	if res.code != 0 {
		t.Fatalf("resume exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "send: preflight ok — 2 check(s) (✓ campaign active, ✓ template references variables)", "armed receipt")
	contains(t, res.stderr, "send: 3 in, 3 out", "delivered after the fix")
	if n := h.queryInt(`SELECT count(*) FROM deliveries`); n != 3 {
		t.Errorf("deliveries = %d, want 3", n)
	}

	// Inconclusive: proceeds with a warning.
	h.write("people2.csv", "email,full_name\nnew@example.com,New Person\n")
	h.write("p2.yaml", strings.Replace(preflightYAML, "people.csv", "people2.csv", 1))
	res = h.runWithEnv(env("inconclusive"), "", "run", "p2.yaml")
	if res.code != 0 {
		t.Fatalf("inconclusive exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "send [warn]: preflight inconclusive — fixture: campaign could not be read; proceeding", "warning")
	contains(t, res.stderr, "send: preflight inconclusive — fixture: campaign could not be read (proceeded)", "receipt")
	contains(t, res.stderr, "send: 1 in, 1 out", "delivered")

	// Silent: an adapter that answers nothing is inconclusive.
	h.write("people3.csv", "email,full_name\nthird@example.com,Third Person\n")
	h.write("p3.yaml", strings.Replace(preflightYAML, "people.csv", "people3.csv", 1))
	res = h.runWithEnv(env("silent"), "", "run", "p3.yaml")
	if res.code != 0 {
		t.Fatalf("silent exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "preflight inconclusive — the adapter reported no preflight", "silent → inconclusive")

	// preflight: false skips it.
	h.write("people4.csv", "email,full_name\nfourth@example.com,Fourth Person\n")
	h.write("p4.yaml", strings.Replace(strings.Replace(preflightYAML, "people.csv", "people4.csv", 1), "      campaign: q3\n", "      campaign: q3\n      preflight: false\n", 1))
	res = h.runWithEnv(env("blocked"), "", "run", "p4.yaml")
	if res.code != 0 || strings.Contains(res.stderr, ": preflight") {
		t.Errorf("preflight: false must skip the check\nexit %d\n%s", res.code, res.stderr)
	}
	last := h.queryStrings(`SELECT id FROM runs ORDER BY started_at DESC LIMIT 1`)[0]
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE run_id = '` + last + `' AND event = 'preflight'`); n != 0 {
		t.Errorf("preflight events with preflight: false = %d", n)
	}
}

func TestPreflightOnlyWhereDeclared(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	log := filepath.Join(h.work, "delivered.ndjson")
	// Two deliver steps: mock/deliver never preflights; mock/preflight does.
	h.write("both.yaml", `name: both
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: crm
    use: mock/deliver
    with:
      campaign: crm
    idempotency: email
  - id: send
    use: mock/preflight
    with:
      campaign: q3
    idempotency: email
`)
	res := h.runWithEnv([]string{"MOCK_PREFLIGHT=ok", "MOCK_DELIVER_LOG=" + log}, "", "run", "both.yaml")
	if res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if strings.Contains(res.stderr, "crm: preflight") {
		t.Errorf("a non-preflighting adapter must never be asked:\n%s", res.stderr)
	}
	contains(t, res.stderr, "send: preflight ok — 2 check(s)", "the declaring step preflights")
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE event = 'preflight'`); n != 1 {
		t.Errorf("preflight events = %d, want 1", n)
	}
}
