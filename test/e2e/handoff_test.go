package e2e

// M14 step 3 acceptance (SPEC §11, ADR-032): the handoff to the next stage
// is a delivery. A pipeline with two group/deliver steps dry-runs to two
// rendered receipts with zero group events and, armed, routes passers and
// failers to different groups; a re-run hands off nothing twice; a consumer
// pipeline pulls from the group with judgment memory; a group source with
// limit: 2 sources the two oldest members; plan warns when a handoff and a
// network send share a pipeline; `groups remove --note` records the reason.

import (
	"strings"
	"testing"
)

// handoffYAML: everyone is handed to `intake` before judgment; passers are
// then handed to `stage-2`. With `when:` gating only on .passed (and a
// filter's fail freezing the record, SPEC §7), the failers' route is the
// consumer pipeline below: intake minus stage-2.
const handoffYAML = `name: triage
version: 1

source:
  use: csv/source
  with:
    path: people.csv

steps:
  - id: intake
    use: group/deliver
    with:
      group: intake
    idempotency: email

  - id: judge
    use: ai/filter
    uses: [title]
    with:
      prompt: Keep decision makers.

  - id: handoff
    use: group/deliver
    when: judge.passed
    with:
      group: stage-2
    variables:
      name: full_name
      title: title
    idempotency: email
`

const triageAnswer = `[
  {"identity_key":"jane.doe@acme.com","pass":true,"reason":"owns budget"},
  {"identity_key":"bob@globex.io","pass":true,"reason":"owns budget"},
  {"identity_key":"carol@initech.dev","pass":false,"reason":"not a buyer"}
]`

func TestGroupDeliverHandsOffAsADelivery(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("triage.yaml", handoffYAML)

	// Plan: both handoffs on the send surface, no adapter, no warning (no
	// network send shares the pipeline).
	plan := h.mustRun("plan", "triage.yaml")
	for _, want := range []string{
		"2. intake [deliver] — group/deliver\n",
		`handoff:   → group "intake" (created on demand)`,
		`variables: name ← full_name, title ← title`,
		"send surface: 2 deliver step(s)",
		`intake → group "intake" (handoff, no network) (touch scope: triage)`,
		`handoff → group "stage-2" (handoff, no network) (touch scope: triage)`,
	} {
		contains(t, plan.stderr, want, "plan output")
	}
	if strings.Contains(plan.stderr, "one commit point") {
		t.Errorf("no network send here; the warning must not fire:\n%s", plan.stderr)
	}

	// Dry run: two rendered receipts — resolved variables per record — and
	// nothing durable: no group events, no deliveries, no groups even.
	env := h.fixtureScript("dry.json", triageAnswer)
	res := h.runWithEnv(env, "", "run", "triage.yaml", "--dry-run")
	if res.code != 0 {
		t.Fatalf("dry exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `intake: 3 record(s) would be handed off to group "intake" (held back — dry run)`, "dry receipt")
	contains(t, res.stderr, `handoff: 2 record(s) would be handed off to group "stage-2" (held back — dry run)`, "dry receipt")
	contains(t, res.stderr, "handoff: resolved variables for 2 record(s)", "dry receipt renders the review artifact")
	contains(t, res.stderr, `name: "Jane Doe"`, "resolved variable")
	for _, q := range []string{`SELECT count(*) FROM group_events`, `SELECT count(*) FROM deliveries`, `SELECT count(*) FROM groups`} {
		if n := h.queryInt(q); n != 0 {
			t.Errorf("%s = %d after a dry run, want 0", q, n)
		}
	}

	// Armed: everyone reaches intake, passers reach stage-2; the handoff is
	// a delivery keyed per group.
	env = h.fixtureScript("armed.json", triageAnswer)
	res = h.runWithEnv(env, "", "run", "triage.yaml")
	if res.code != 0 {
		t.Fatalf("armed exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `intake: 3 record(s) handed off to group "intake"`, "armed receipt")
	contains(t, res.stderr, `handoff: 2 record(s) handed off to group "stage-2"`, "armed receipt")
	if n := h.queryInt(`SELECT count(*) FROM deliveries WHERE target = 'group:intake'`); n != 3 {
		t.Errorf("intake deliveries = %d, want 3", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM deliveries WHERE target = 'group:stage-2'`); n != 2 {
		t.Errorf("stage-2 deliveries = %d, want 2", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM group_events WHERE event = 'added' AND detail LIKE '%"handoff":true%'`); n != 5 {
		t.Errorf("handoff added events = %d, want 5", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM group_events WHERE event = 'touched'`); n != 5 {
		t.Errorf("touched events (record: scope) = %d, want 5", n)
	}
	show := h.mustRun("groups", "show", "stage-2")
	contains(t, show.stderr, "2 member(s)", "stage-2 membership")
	if strings.Contains(show.stderr, "carol") {
		t.Errorf("the failer must not reach stage-2:\n%s", show.stderr)
	}

	// Re-run: delivery idempotency — nothing is handed off twice.
	env = h.fixtureScript("again.json", triageAnswer)
	res = h.runWithEnv(env, "", "run", "triage.yaml")
	if res.code != 0 {
		t.Fatalf("re-run exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "intake: 0 in, 0 out, 3 cached", "already delivered")
	if n := h.queryInt(`SELECT count(*) FROM group_events WHERE event = 'added'`); n != 5 {
		t.Errorf("added events after re-run = %d, want 5 (nothing twice)", n)
	}

	// The failers' route: a consumer pulls intake minus stage-2 into `held`
	// — judgment memory (exclude:) does the routing, no sql/filter needed.
	h.write("hold.yaml", `name: hold
source:
  group: intake
steps:
  - id: park
    use: group/deliver
    exclude: [stage-2]
    with:
      group: held
`)
	res = h.mustRun("run", "hold.yaml")
	contains(t, res.stderr, `park: 1 record(s) handed off to group "held"`, "consumer receipt")
	show = h.mustRun("groups", "show", "held")
	contains(t, show.stderr, "person:carol@initech.dev", "the failer is held")

	// Reject with a reason: groups remove --note lands in the event detail.
	res = h.mustRun("groups", "remove", "held", "carol@initech.dev", "--note", "wrong segment")
	contains(t, res.stderr, "1 removed", "remove output")
	show = h.mustRun("groups", "show", "held")
	contains(t, show.stderr, "0 member(s)", "membership after remove")
	contains(t, show.stderr, `"note":"wrong segment"`, "the reason is on the event")
	res = h.run("groups", "add", "held", "carol@initech.dev", "--note", "x")
	if res.code != 2 {
		t.Errorf("--note on add exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
}

// TestGroupSourceLimitServesOldestFirst: `limit: 2` sources the two oldest
// members in group_events insertion order — the budget for "work N today".
func TestGroupSourceLimitServesOldestFirst(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("mint.yaml", `name: mint
source:
  use: csv/source
  with:
    path: people.csv
`)
	h.mustRun("run", "mint.yaml")
	// Added one at a time, so insertion order is carol, jane, bob — not
	// key order, not source order.
	for _, key := range []string{"carol@initech.dev", "jane.doe@acme.com", "bob@globex.io"} {
		h.mustRun("groups", "add", "todo", key)
	}

	h.write("work.yaml", `name: work
source:
  group: todo
  limit: 2
steps:
  - id: park
    use: group/deliver
    with:
      group: worked
`)
	plan := h.mustRun("plan", "work.yaml")
	contains(t, plan.stderr, "limit:     2 member(s), oldest-added first", "plan output")
	res := h.mustRun("run", "work.yaml")
	contains(t, res.stderr, `source: sourced 2 members of group "todo" (limit 2, oldest first)`, "source line")
	worked := h.queryStrings(`SELECT i.identity_key FROM group_members m JOIN identities i ON i.id = m.identity_id
		JOIN groups g ON g.id = m.group_id WHERE g.name = 'worked' ORDER BY i.identity_key`)
	if strings.Join(worked, ",") != "carol@initech.dev,jane.doe@acme.com" {
		t.Errorf("worked = %v, want the two oldest (carol, jane)", worked)
	}

	// limit: anywhere but a group source fails validation.
	h.write("bad.yaml", `name: bad
source:
  use: csv/source
  with:
    path: people.csv
  limit: 2
`)
	res = h.run("plan", "bad.yaml")
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "limit: is only valid on a group source", "stderr")
}

// TestOneCommitPointWarning: a handoff and a network-side deliver in one
// pipeline plans, but warns — arming would approve both at once (ADR-031).
func TestOneCommitPointWarning(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("both.yaml", `name: both
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: stage
    use: group/deliver
    with:
      group: stage-2
  - id: send
    use: mock/deliver
    with:
      campaign: q3
    idempotency: email
`)
	plan := h.mustRun("plan", "both.yaml")
	contains(t, plan.stderr, `warning: one commit point (ADR-032): this pipeline both hands off — stage (→ group "stage-2") — and sends — send (→ mock/deliver).`, "plan warning")
	contains(t, plan.stderr, "approving the handoff approves the send", "plan warning")

	// group/deliver config is exactly with.group; the deliver-only keys are
	// its to use, and the other roles' keys are rejected as anywhere else.
	h.write("cfg.yaml", `name: cfg
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: stage
    use: group/deliver
    uses: [title]
    provides: [x]
    with:
      campaign: nope
`)
	res := h.run("plan", "cfg.yaml")
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	for _, want := range []string{
		"group/deliver needs with.group",
		`group/deliver takes only with.group (got "campaign")`,
		`uses: is only valid on filter/compose/review steps (group/deliver has role "deliver")`,
		`provides: is only valid on filter/compose/review steps (group/deliver has role "deliver")`,
	} {
		contains(t, res.stderr, want, "stderr")
	}
}
