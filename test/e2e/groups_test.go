package e2e

// M9 acceptance (SPEC §11, ADR-021), fully offline: the qualify/send
// decomposition — a qualify pipeline fills a group through its terminus,
// exclusion makes judgment memory real (nothing is re-judged), a send
// pipeline consumes the group as a source, records touches under its scope,
// and a suppression window holds re-contacts back, receipted. Plus the plan
// gate on missing groups, the dry-run holds, and the gtme groups verbs.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

const qualifyYAML = `name: q3-qualify
version: 1

source:
  use: csv/source
  with:
    path: contacts.csv
    columns:
      full_name: Full Name
      email: Email
      company_domain: Company Website

steps:
  - id: judge
    use: ai/filter
    uses: [full_name]
    with:
      prompt: Keep only contacts who plausibly buy outbound tooling.
%s
group: q3-qualified
`

// judgeAnswer passes jane and bob, rejects carol.
const judgeAnswer = `[
  {"identity_key":"jane.doe@acme.com","pass":true,"reason":"fits"},
  {"identity_key":"bob@globex.io","pass":true,"reason":"fits"},
  {"identity_key":"carol@initech.dev","pass":false,"reason":"no fit"}
]`

func TestGroupsQualifyJudgeOnceSendSuppress(t *testing.T) {
	h := newHarness(t)
	h.write("contacts.csv", campaignZeroCSV)
	h.write("qualify.yaml", fmt.Sprintf(qualifyYAML, ""))
	deliverLog := filepath.Join(h.work, "delivered.ndjson")

	// 1. Qualify: the terminus captures exactly the completers (passers).
	env := h.fixtureScript("ai.json", judgeAnswer)
	res := h.runWithEnv(env, "", "run", "qualify.yaml")
	if res.code != 0 {
		t.Fatalf("qualify exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `group "q3-qualified": 2 record(s) added`, "qualify receipt")
	if n := h.queryInt(`SELECT count(*) FROM group_events WHERE event = 'added'`); n != 2 {
		t.Fatalf("added events = %d, want 2", n)
	}

	// 2. Snapshot the failers into a rejected group — the after-the-fact
	// grouping ADR-021 promises, via the run layer's records.
	res = h.mustRun("groups", "add", "q3-rejected",
		"--query", `SELECT identity_id FROM run_records WHERE verdicts LIKE '%fail%'`)
	contains(t, res.stderr, "1 added", "snapshot output")

	// 3. Judgment memory: re-running with exclude: on both output groups
	// dispatches nothing to the AI filter — its verdicts are recorded
	// decisions, not per-run rolls of the dice.
	h.write("qualify2.yaml", strings.Replace(fmt.Sprintf(qualifyYAML, "    exclude: [q3-qualified, q3-rejected]\n"),
		"name: q3-qualify", "name: q3-qualify-topup", 1))
	res = h.runWithEnv(h.fixtureScript("ai2.json", judgeAnswer), "", "run", "qualify2.yaml")
	if res.code != 0 {
		t.Fatalf("top-up exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "judge: 3 in, 0 out, 0 cached, 0 filtered, 0 failed, 3 gated", "top-up step line (nothing re-judged)")
	contains(t, res.stderr, "3 gated", "top-up step line")
	if n := h.queryInt(`SELECT count(*) FROM group_events WHERE event = 'added'`); n != 3 {
		t.Errorf("added events after top-up = %d, want 3 (2 qualified + 1 rejected, no re-adds)", n)
	}

	// 4. Send: the group is the source; delivery records touches under the
	// explicit record: scope.
	h.write("send.yaml", `name: q3-send
source:
  group: q3-qualified
steps:
  - id: deliver
    use: mock/deliver
    with:
      campaign: q3
    record: q3-touch
    idempotency: email
`)
	res = h.runWithEnv([]string{"MOCK_DELIVER_LOG=" + deliverLog}, "", "run", "send.yaml")
	if res.code != 0 {
		t.Fatalf("send exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `sourced 2 members of group "q3-qualified"`, "group source")
	if n := h.queryInt(`SELECT count(*) FROM deliveries`); n != 2 {
		t.Fatalf("deliveries = %d, want 2", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM group_events WHERE event = 'touched'`); n != 2 {
		t.Fatalf("touched events = %d, want 2", n)
	}

	// 5. Suppression: a second send under a different idempotency key would
	// re-deliver — the touch window is what holds it back, receipted.
	h.write("send2.yaml", `name: q3-send-again
source:
  group: q3-qualified
steps:
  - id: deliver
    use: mock/deliver
    with:
      campaign: q3
    record: q3-touch
    suppress: { group: q3-touch, within: 30d }
    idempotency: full_name
`)
	res = h.runWithEnv([]string{"MOCK_DELIVER_LOG=" + deliverLog}, "", "run", "send2.yaml")
	if res.code != 0 {
		t.Fatalf("send2 exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "2 record(s) suppressed", "suppression receipt")
	contains(t, res.stderr, `touched in "q3-touch"`, "suppression reasons")
	if n := h.queryInt(`SELECT count(*) FROM deliveries`); n != 2 {
		t.Errorf("deliveries after suppressed send = %d, want 2 (nothing sent)", n)
	}
	if lines := nonEmptyLines(readFile(t, deliverLog)); len(lines) != 2 {
		t.Errorf("deliver log = %d lines, want 2", len(lines))
	}
}

// TestGroupsDryAssertsNothingDurable: a dry run neither touches nor adds —
// the receipt reports what an armed run would have recorded (SPEC §8).
func TestGroupsDryAssertsNothingDurable(t *testing.T) {
	h := newHarness(t)
	h.write("contacts.csv", campaignZeroCSV)
	h.write("qualify.yaml", fmt.Sprintf(qualifyYAML, ""))

	res := h.runWithEnv(h.fixtureScript("ai.json", judgeAnswer), "", "run", "qualify.yaml", "--dry-run")
	if res.code != 0 {
		t.Fatalf("dry exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `group "q3-qualified": 2 record(s) would be added (held back — dry run)`, "dry receipt")
	if n := h.queryInt(`SELECT count(*) FROM group_events`); n != 0 {
		t.Errorf("group events after dry run = %d, want 0", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM groups`); n != 0 {
		t.Errorf("groups created by a dry run = %d, want 0", n)
	}
}

// TestGroupsPlanGate: a referenced group that does not exist fails the plan
// naming the group and the fix; once it exists, the plan shows the gates.
func TestGroupsPlanGate(t *testing.T) {
	h := newHarness(t)
	h.write("contacts.csv", campaignZeroCSV)
	h.write("gated.yaml", `name: gated
source:
  use: csv/source
  with:
    path: contacts.csv
    columns:
      full_name: Full Name
      email: Email
steps:
  - id: judge
    use: ai/filter
    uses: [full_name]
    require: [warm]
    with:
      prompt: judge
`)
	res := h.run("plan", "gated.yaml")
	if res.code != 2 {
		t.Fatalf("plan exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `group "warm" does not exist`, "plan error")
	contains(t, res.stderr, "gtme groups add warm", "plan error names the fix")

	// Mint identities, create the group, and the plan passes showing the gate.
	h.write("mint.yaml", `name: mint
source:
  use: csv/source
  with:
    path: contacts.csv
    columns:
      full_name: Full Name
      email: Email
`)
	h.mustRun("run", "mint.yaml")
	h.mustRun("groups", "add", "warm", "jane.doe@acme.com")
	res = h.mustRun("plan", "gated.yaml")
	contains(t, res.stderr, "require:   members of warm", "plan gates display")
}

// TestGroupsVerbs: list, show, add, remove — membership edits are idempotent
// events with a readable trail.
func TestGroupsVerbs(t *testing.T) {
	h := newHarness(t)
	h.write("contacts.csv", campaignZeroCSV)
	h.write("mint.yaml", `name: mint
source:
  use: csv/source
  with:
    path: contacts.csv
    columns:
      full_name: Full Name
      email: Email
`)
	h.mustRun("run", "mint.yaml")

	res := h.mustRun("groups", "add", "vip", "jane.doe@acme.com", "bob@globex.io")
	contains(t, res.stderr, "2 added", "add output")
	res = h.mustRun("groups", "add", "vip", "jane.doe@acme.com")
	contains(t, res.stderr, "0 added, 1 unchanged", "idempotent re-add")

	res = h.mustRun("groups")
	contains(t, res.stderr, "vip", "groups list")
	contains(t, res.stderr, "2", "groups list member count")

	res = h.mustRun("groups", "show", "vip")
	contains(t, res.stderr, "2 member(s)", "show output")
	contains(t, res.stderr, "person:jane.doe@acme.com", "show members")

	res = h.mustRun("groups", "remove", "vip", "bob@globex.io")
	contains(t, res.stderr, "1 removed", "remove output")
	res = h.mustRun("groups", "show", "vip")
	contains(t, res.stderr, "1 member(s)", "membership after remove")

	res = h.run("groups", "show", "nope")
	if res.code != 2 {
		t.Errorf("show of unknown group exit = %d, want 2", res.code)
	}
}
