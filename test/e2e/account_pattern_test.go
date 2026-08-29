package e2e

// M14 capstone (SPEC §11): the account pattern — four pipelines chained
// through groups — runs end to end offline with zero network calls, from
// shipped atoms only: a company-entity AI judgment with declared provides
// (ADR-033), a fan-out source parameterised by a config query (ADR-037), a
// cross-type gate over relations and membership (sql/filter), handoffs as
// deliveries (ADR-032), a cross-record fan-in (sql/transform), an
// entity-agnostic compose on a group source, a bounded consumer, and a
// fixture send.

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAccountPatternRunsOfflineEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.write("companies.csv", companiesCSV)
	h.write("people.csv", peopleCSV)
	deliverLog := filepath.Join(h.work, "delivered.ndjson")

	// 1. Qualify accounts (company entity): the judgment lands namespaced,
	//    passers join the terminus group.
	h.write("qualify.yaml", `name: accounts-qualify
source:
  use: csv/source
  with:
    path: companies.csv
    entity_type: company
steps:
  - id: judge
    use: ai/filter
    uses: [company_name]
    provides:
      tier: {enum: [a, b]}
      rationale: {}
    with:
      prompt: Tier the account.
group: qualified-accounts
`)
	env := h.fixtureScript("qualify.json", `[
  {"identity_key":"acme.com","pass":true,"reason":"fits","accounts-qualify.tier":"a","accounts-qualify.rationale":"anvils"},
  {"identity_key":"globex.io","pass":true,"reason":"fits","accounts-qualify.tier":"b","accounts-qualify.rationale":"growing"}
]`)
	res := h.runWithEnv(env, "", "run", "qualify.yaml")
	if res.code != 0 {
		t.Fatalf("qualify exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `group "qualified-accounts": 2 record(s) added`, "qualify receipt")
	if n := h.queryInt(`SELECT count(*) FROM current_values WHERE field = 'accounts-qualify.tier'`); n != 2 {
		t.Fatalf("tier judgments = %d, want 2", n)
	}
	// The same pipeline simulates with no fixture script at all: the fixture
	// engine synthesizes a schema-valid answer for the declared shape.
	sim := h.run("run", "qualify.yaml", "--simulate")
	if sim.code != 0 {
		t.Fatalf("simulate exit = %d\nstderr:\n%s", sim.code, sim.stderr)
	}
	contains(t, sim.stderr, "judge: 2 in, 2 out", "simulated judgment")

	// 2. Select people: the source is parameterised from the ledger (fan-out
	//    at the pipeline boundary), the gate is cross-type (people whose
	//    company is a qualified account, via works_at), the handoff is a
	//    delivery to the next stage.
	h.write("select.yaml", `name: people-select
source:
  use: csv/source
  with:
    path: {query: "SELECT 'people.csv'"}
steps:
  - id: at-qualified
    use: sql/filter
    with:
      query: >
        SELECT r.from_id AS identity_id
        FROM relations r
        JOIN group_membership gm ON gm.identity_id = r.to_id AND gm.group_name = 'qualified-accounts'
        WHERE r.relation = 'works_at'
  - id: select
    use: group/deliver
    with:
      group: selected-people
    variables:
      name: full_name
    idempotency: email
`)
	plan := h.mustRun("plan", "select.yaml")
	contains(t, plan.stderr, `with.path ← {query: SELECT 'people.csv'} → 1 row (scalar): people.csv`, "fan-out config value")
	contains(t, plan.stderr, "cross-record: this query reads relations and group_membership", "the gate is cross-record")
	res = h.mustRun("run", "select.yaml")
	contains(t, res.stderr, "at-qualified: 3 in, 2 out, 0 cached, 1 filtered", "carol's company is not qualified")
	contains(t, res.stderr, `select: 2 record(s) handed off to group "selected-people"`, "handoff receipt")

	// 3. Brief each account (group source, entity-blind): fan-in over the
	//    selected people, an AI compose with declared provides, a handoff.
	h.write("brief.yaml", `name: account-brief
source:
  group: qualified-accounts
steps:
  - id: headcount
    use: sql/transform
    with:
      provides: [acct.selected_people]
      query: >
        SELECT r.to_id AS identity_id, count(*) AS "acct.selected_people"
        FROM relations r
        JOIN group_membership gm ON gm.identity_id = r.from_id AND gm.group_name = 'selected-people'
        WHERE r.relation = 'works_at'
        GROUP BY r.to_id
  - id: brief
    use: ai/compose
    uses: [company_name, acct.selected_people]
    provides: [brief]
    with:
      prompt: Write a one-line account brief from the headcount.
  - id: handoff
    use: group/deliver
    with:
      group: briefed
    variables:
      brief: account-brief.brief
`)
	env = h.fixtureScript("brief.json", "$auto")
	res = h.runWithEnv(env, "", "run", "brief.yaml")
	if res.code != 0 {
		t.Fatalf("brief exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "headcount: 2 in, 2 out", "fan-in ran over both accounts")
	contains(t, res.stderr, `handoff: 2 record(s) handed off to group "briefed"`, "brief handoff")
	if n := h.queryInt(`SELECT count(*) FROM current_values v JOIN identities i ON i.id = v.identity_id
		WHERE v.field = 'acct.selected_people' AND v.value = 1 AND i.entity_type = 'company'`); n != 2 {
		t.Errorf("fan-in counts on company identities = %d, want 2", n)
	}
	briefs := h.queryStrings(`SELECT value FROM current_values WHERE field = 'account-brief.brief' ORDER BY value`)
	if len(briefs) != 2 || !strings.HasPrefix(briefs[0], "Fixture brief for ") {
		t.Errorf("briefs = %v", briefs)
	}

	// 4. Outreach: a bounded consumer of the selected people, its campaign
	//    name computed from the ledger, delivered to a fixture target with
	//    the terminus recording who was contacted.
	h.write("outreach.yaml", `name: outreach
source:
  group: selected-people
  limit: 10
steps:
  - id: send
    use: mock/deliver
    with:
      campaign: {query: "SELECT 'q3-' || count(*) FROM group_membership WHERE group_name = 'briefed'"}
    variables:
      name: full_name
    idempotency: email
group: contacted
`)
	dry := h.runWithEnv([]string{"MOCK_DELIVER_LOG=" + deliverLog}, "", "run", "outreach.yaml", "--dry-run")
	if dry.code != 0 {
		t.Fatalf("dry exit = %d\nstderr:\n%s", dry.code, dry.stderr)
	}
	contains(t, dry.stderr, "send: resolved variables for 2 record(s)", "dry receipt")
	if lines := countLines(t, deliverLog); lines != 0 {
		t.Fatalf("a dry run sent %d record(s)", lines)
	}
	res = h.runWithEnv([]string{"MOCK_DELIVER_LOG=" + deliverLog}, "", "run", "outreach.yaml")
	if res.code != 0 {
		t.Fatalf("outreach exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `source: sourced 2 members of group "selected-people" (limit 10, oldest first)`, "bounded consumer")
	contains(t, res.stderr, `group "contacted": 2 record(s) added`, "terminus")
	if lines := countLines(t, deliverLog); lines != 2 {
		t.Errorf("delivered = %d, want 2", lines)
	}
	if n := h.queryInt(`SELECT count(*) FROM runs WHERE pipeline = 'outreach' AND config_json LIKE '%"campaign":"q3-2"%'`); n != 2 {
		t.Errorf("both outreach runs (dry, armed) must record the resolved campaign name; got %d", n)
	}

	// The whole pattern left one delivery per person per target, every one
	// accepted, and re-running the consumer contacts nobody twice.
	if n := h.queryInt(`SELECT count(*) FROM deliveries WHERE status = 'accepted'`); n != 6 {
		t.Errorf("accepted deliveries = %d, want 6 (2 selected + 2 briefed + 2 sent)", n)
	}
	again := h.runWithEnv([]string{"MOCK_DELIVER_LOG=" + deliverLog}, "", "run", "outreach.yaml")
	contains(t, again.stderr, "send: 0 in, 0 out, 2 cached", "nothing twice")
	groups := h.mustRun("groups")
	for _, g := range []string{"qualified-accounts", "selected-people", "briefed", "contacted"} {
		contains(t, groups.stderr, g, "groups list")
	}
}
