package e2e

// M13 acceptance (SPEC §11, ADR-031): deliver adapters are ordinary steps: —
// any number, any position. A mid-list and a final deliver step dry-run to
// resolved variables with zero deliveries writes; armed, both deliver and a
// re-run delivers nothing twice on either; a record failing between the two
// delivers to the first only and misses the terminus, while a record
// suppressed at the final deliver step completes and joins it; the deliver-only
// keys are role-gated at plan time; a top-level deliver: block fails validation.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const twoSendsCSV = "full_name,email\n" +
	"Jane Doe,jane@acme.com\n" +
	"Bob Stone,bob@globex.io\n" +
	"Carol Chen,carol@initech.dev\n"

// twoSendsYAML delivers mid-pipeline (send-a), filters bob out, delivers again
// (send-b), and ends in a membership terminus. The two deliver steps hit
// different targets, so each keeps its own deliveries idempotency scope (§8).
const twoSendsYAML = `name: two-sends
version: 1

source:
  use: csv/source
  with:
    path: contacts.csv

steps:
  - id: send-a
    use: mock/deliver
    with:
      campaign: mid-pipeline
    variables:
      first_name: full_name
    idempotency: email

  - id: gate
    use: sql/filter
    with:
      query: >
        SELECT i.id AS identity_id,
               CASE WHEN f.value = '"bob@globex.io"' THEN 0 ELSE 1 END AS pass,
               'bob is filtered between the sends' AS reason
        FROM identities i
        JOIN current_fields f ON f.identity_id = i.id AND f.field = 'email'
        WHERE i.entity_type = 'person'

  - id: send-b
    use: csv/deliver
    with:
      path: %s
    variables:
      contact_email: email
    idempotency: email

group: finished
`

func TestDeliverStepsMidAndFinal(t *testing.T) {
	h := newHarness(t)
	h.write("contacts.csv", twoSendsCSV)
	outCSV := filepath.Join(h.work, "send-b.csv")
	h.write("two-sends.yaml", strings.Replace(twoSendsYAML, "%s", outCSV, 1))
	deliverLog := filepath.Join(h.work, "delivered.ndjson")
	env := []string{"MOCK_DELIVER_LOG=" + deliverLog}

	// Plan calls out the full send surface — every deliver step, target and
	// touch scope, in one place (SPEC §7, ADR-031).
	res := h.mustRun("plan", "two-sends.yaml")
	contains(t, res.stderr, "send surface: 2 deliver step(s) (ADR-031)", "plan output")
	contains(t, res.stderr, "send-a → mock/deliver (touch scope: two-sends)", "plan output")
	contains(t, res.stderr, "send-b → csv/deliver (touch scope: two-sends)", "plan output")

	// Dry run: resolved variables for BOTH deliver steps, zero deliveries.
	res = h.runWithEnv(env, "", "run", "two-sends.yaml", "--dry-run")
	if res.code != 0 {
		t.Fatalf("dry run exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "send-a: resolved variables for 3 record(s)", "dry receipt")
	contains(t, res.stderr, "send-b: resolved variables for 2 record(s)", "dry receipt")
	contains(t, res.stderr, `first_name: "Jane Doe"`, "dry receipt")
	contains(t, res.stderr, `contact_email: "jane@acme.com"`, "dry receipt")
	if n := h.queryInt(`SELECT count(*) FROM deliveries`); n != 0 {
		t.Fatalf("deliveries after dry run = %d, want 0", n)
	}
	if _, err := os.Stat(deliverLog); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote to the deliver log: %v", err)
	}

	// Armed: both steps deliver — each against its own target scope.
	res = h.runWithEnv(env, "", "run", "two-sends.yaml")
	if res.code != 0 {
		t.Fatalf("armed run exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if n := len(nonEmptyLines(readFile(t, deliverLog))); n != 3 {
		t.Fatalf("send-a delivered %d record(s), want 3", n)
	}
	if n := len(nonEmptyLines(readFile(t, outCSV))); n != 3 { // header + jane + carol
		t.Fatalf("send-b csv lines = %d, want 3:\n%s", n, readFile(t, outCSV))
	}
	if n := h.queryInt(`SELECT count(*) FROM deliveries WHERE target = 'mock/deliver'`); n != 3 {
		t.Errorf("mock/deliver deliveries = %d, want 3", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM deliveries WHERE target = 'csv/deliver'`); n != 2 {
		t.Errorf("csv/deliver deliveries = %d, want 2", n)
	}
	// Bob failed between the two: delivered at send-a only, absent from the
	// terminus — it captures completers, not sends (SPEC §8, ADR-031).
	if n := h.queryInt(`SELECT count(*) FROM group_members gm
		JOIN groups g ON g.id = gm.group_id WHERE g.name = 'finished'`); n != 2 {
		t.Errorf("group finished members = %d, want 2", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM group_members gm
		JOIN groups g ON g.id = gm.group_id
		JOIN identities i ON i.id = gm.identity_id
		WHERE g.name = 'finished' AND i.identity_key = 'bob@globex.io'`); n != 0 {
		t.Errorf("bob joined the terminus despite failing between the sends")
	}
	// Both deliver steps share the default touch scope (the pipeline name).
	if n := h.queryInt(`SELECT count(*) FROM group_events ge
		JOIN groups g ON g.id = ge.group_id
		WHERE g.name = 'two-sends' AND ge.event = 'touched'`); n != 5 {
		t.Errorf("touched events in the default scope = %d, want 5 (3 at send-a + 2 at send-b)", n)
	}

	// Re-run: nothing delivers twice on either target.
	res = h.runWithEnv(env, "", "run", "two-sends.yaml")
	if res.code != 0 {
		t.Fatalf("re-run exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if n := h.queryInt(`SELECT count(*) FROM deliveries`); n != 5 {
		t.Errorf("deliveries after re-run = %d, want 5 (idempotency must hold per target)", n)
	}
	if n := len(nonEmptyLines(readFile(t, deliverLog))); n != 3 {
		t.Errorf("deliver log after re-run = %d lines, want 3", n)
	}
	if n := len(nonEmptyLines(readFile(t, outCSV))); n != 3 {
		t.Errorf("send-b csv after re-run = %d lines, want 3", n)
	}

	// A record suppressed at the final deliver step completes the run and
	// joins the terminus (SPEC §8, ADR-031): the fail verdict withholds this
	// step's send; the record still advances. Idempotency is keyed off email
	// so the §8 floor misses and the suppression window is what holds.
	h.write("resend.yaml", `name: resend
source:
  use: csv/source
  with:
    path: contacts.csv
steps:
  - id: send-again
    use: mock/deliver
    with:
      campaign: resend
    variables:
      first_name: full_name
    suppress: { group: two-sends, within: 30d }
    idempotency: full_name
group: resent
`)
	res = h.runWithEnv(env, "", "run", "resend.yaml")
	if res.code != 0 {
		t.Fatalf("resend exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "3 record(s) suppressed", "resend receipt")
	if n := h.queryInt(`SELECT count(*) FROM deliveries WHERE target = 'mock/deliver'`); n != 3 {
		t.Errorf("suppression did not hold: mock/deliver deliveries = %d, want 3", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM group_members gm
		JOIN groups g ON g.id = gm.group_id WHERE g.name = 'resent'`); n != 3 {
		t.Errorf("group resent members = %d, want 3 (suppressed records complete and join)", n)
	}
}

// TestDeliverKeysRoleGated: the deliver-only keys are rejected at plan time on
// any step whose role is not deliver, naming the step and the key (SPEC §9,
// ADR-031 — the uses: pattern, second instance).
func TestDeliverKeysRoleGated(t *testing.T) {
	h := newHarness(t)
	h.write("contacts.csv", twoSendsCSV)
	h.write("bad-variables.yaml", `name: bad-variables
source:
  use: csv/source
  with:
    path: contacts.csv
steps:
  - id: judge
    use: ai/filter
    uses: [full_name]
    variables:
      first_name: full_name
    with:
      prompt: Keep everyone.
`)
	res := h.run("plan", "bad-variables.yaml")
	if res.code != 2 {
		t.Fatalf("plan exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `step "judge"`, "plan error names the step")
	contains(t, res.stderr, "variables: is only valid on deliver steps", "plan error names the key")

	h.write("bad-record.yaml", `name: bad-record
source:
  use: csv/source
  with:
    path: contacts.csv
steps:
  - id: score
    use: mock-enrich-py
    record: touched-scope
    idempotency: email
`)
	res = h.run("plan", "bad-record.yaml")
	if res.code != 2 {
		t.Fatalf("plan exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `step "score"`, "plan error names the step")
	contains(t, res.stderr, "record: is only valid on deliver steps", "plan error names the key")
	contains(t, res.stderr, "idempotency: is only valid on deliver steps", "plan error names the key")
}

// TestTopLevelDeliverBlockRejected: the pre-ADR-031 shape fails validation,
// and the error names the fix.
func TestTopLevelDeliverBlockRejected(t *testing.T) {
	h := newHarness(t)
	h.write("contacts.csv", twoSendsCSV)
	h.write("old-shape.yaml", `name: old-shape
source:
  use: csv/source
  with:
    path: contacts.csv
deliver:
  use: mock/deliver
  idempotency: email
`)
	res := h.run("plan", "old-shape.yaml")
	if res.code != 2 {
		t.Fatalf("plan exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "deliver adapters are ordinary steps: entries", "validation error names the fix")
}
