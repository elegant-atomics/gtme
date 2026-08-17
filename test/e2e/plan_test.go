package e2e

import (
	"strings"
	"testing"
)

// TestPlanPrintsTheResolvedPlan covers the happy path: no execution, no spend.
func TestPlanPrintsTheResolvedPlan(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("pipeline.yaml", csvToMockYAML)

	res := h.mustRun("plan", "pipeline.yaml")
	for _, want := range []string{
		"pipeline csv-to-mock",
		"1. source [source] — csv/source@1",
		"2. mock [enrich] — mock-enrich-py@1 (external:",
		"projects:  email, full_name",
		"provides:  mock.note, mock.score",
		"cache:     30d",
		"est/record: $0.0000",
		"plan ok — nothing has been spent",
	} {
		contains(t, res.stderr, want, "plan output")
	}
	if res.stdout != "" {
		t.Errorf("plan writes to stderr; stdout should stay empty, got:\n%s", res.stdout)
	}

	// Planning must not touch the ledger's run tables.
	if n := h.queryInt(`SELECT count(*) FROM runs`); n != 0 {
		t.Errorf("runs = %d, want 0 — plan does not execute", n)
	}
}

// TestPlanFailsOnUnsatisfiableNeeds is the M4 acceptance test for contracts: the
// error names the step and the missing field, before anything is spent.
func TestPlanFailsOnUnsatisfiableNeeds(t *testing.T) {
	h := newHarness(t)
	h.writeAdapter("needs-linkedin", needsLinkedInManifest, echoAdapterScript)
	// The CSV has no linkedin_url column, so the pipeline cannot work.
	h.write("people.csv", "email,full_name\njane@acme.com,Jane Doe\n")
	h.write("pipeline.yaml", `name: broken
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: profile
    use: needs-linkedin
`)

	res := h.run("plan", "pipeline.yaml")
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `step "profile"`, "stderr")
	contains(t, res.stderr, "needs linkedin_url", "stderr")
	contains(t, res.stderr, "available: email, full_name", "stderr")

	// gtme run must refuse for the same reason, without minting a run.
	res = h.run("run", "pipeline.yaml")
	if res.code != 2 {
		t.Errorf("run exit = %d, want 2", res.code)
	}
	if n := h.queryInt(`SELECT count(*) FROM runs`); n != 0 {
		t.Errorf("runs = %d, want 0", n)
	}
}

// TestPlanFailsOnUnsatisfiableUses is TestPlanFailsOnUnsatisfiableNeeds' twin
// for AI steps (SPEC §7, §9, DECISIONS.md ADR-004): a uses: field with no
// upstream provider is a plan error naming the step and field, exactly like
// needs.required, even though ai/filter's static manifest needs is a
// needs-all wildcard that alone could never catch this.
func TestPlanFailsOnUnsatisfiableUses(t *testing.T) {
	h := newHarness(t)
	// The CSV has no linkedin_url column, so recent_posts can never exist.
	h.write("people.csv", peopleCSV)
	h.write("pipeline.yaml", `name: broken-uses
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: icp-filter
    use: ai/filter
    uses: [recent_posts]
    with:
      prompt: "keep everyone"
`)

	res := h.run("plan", "pipeline.yaml")
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `step "icp-filter"`, "stderr")
	contains(t, res.stderr, "needs recent_posts", "stderr")

	res = h.run("run", "pipeline.yaml")
	if res.code != 2 {
		t.Errorf("run exit = %d, want 2", res.code)
	}
	if n := h.queryInt(`SELECT count(*) FROM runs`); n != 0 {
		t.Errorf("runs = %d, want 0", n)
	}
}

// TestPlanRejectsUsesOnNonAIStep keeps uses: from being config nobody reads:
// declaring it on a step whose adapter is not filter/compose is a plan error,
// not a silently ignored key.
func TestPlanRejectsUsesOnNonAIStep(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("pipeline.yaml", `name: uses-on-enrich
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: score
    use: mock-enrich-py
    uses: [full_name]
`)

	res := h.run("plan", "pipeline.yaml")
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "uses: is only valid on filter/compose steps", "stderr")
}

// TestPlanFailsOnUnsatisfiableOneOfNeeds: a one-of needs step (SPEC §7,
// harvest/profile's shape) plans when any single branch is available and fails
// naming every branch and what each is missing.
func TestPlanFailsOnUnsatisfiableOneOfNeeds(t *testing.T) {
	h := newHarness(t)
	// No LinkedIn field of any shape.
	h.write("people.csv", "email,full_name\njane@acme.com,Jane Doe\n")
	h.write("pipeline.yaml", `name: no-shape
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: linkedin
    use: harvest/profile
`)
	res := h.runWithEnv([]string{"HARVEST_API_KEY=plan-only"}, "", "plan", "pipeline.yaml")
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `step "linkedin"`, "stderr")
	contains(t, res.stderr, "needs at least one of", "stderr")
	for _, branch := range []string{"linkedin_url", "linkedin_internal_url", "linkedin_sales_nav_url"} {
		contains(t, res.stderr, branch, "stderr")
	}

	// Any single shape satisfies the step — here the Sales Navigator URL.
	h.write("people.csv", "email,full_name,linkedin_sales_nav_url\njane@acme.com,Jane Doe,https://www.linkedin.com/sales/lead/ACwAAAbQ2xKB9\n")
	res = h.runWithEnv([]string{"HARVEST_API_KEY=plan-only"}, "", "plan", "pipeline.yaml")
	if res.code != 0 {
		t.Fatalf("exit = %d with a sales-nav column\nstderr:\n%s", res.code, res.stderr)
	}
}

// TestPlanNotes covers the non-blocking observations SPEC §7 and §4a require:
// a near-miss column is SUGGESTED (never silently mapped), and a namespaced
// field in a step's needs makes the vendor coupling visible.
func TestPlanNotes(t *testing.T) {
	h := newHarness(t)
	// "E-mail" normalizes to e_mail — not canonical, one edit from "email".
	h.write("people.csv", "E-mail,full_name,Favorite Color\njane@acme.com,Jane Doe,teal\n")
	h.write("pipeline.yaml", `name: notes
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: icp-filter
    use: ai/filter
    uses: [full_name, csv.favorite_color]
    with:
      prompt: "keep people whose favorite color suggests taste"
`)
	res := h.mustRun("plan", "pipeline.yaml")
	contains(t, res.stderr, `column "e_mail" looks like canonical "email"`, "plan output")
	contains(t, res.stderr, `vendor-namespaced field "csv.favorite_color"`, "plan output")
	// Suggested is not applied: the column stays namespaced.
	contains(t, res.stderr, "csv.e_mail", "plan output")
}

// TestPlanFailsOnMissingCredential is the auth-error path: exit 3, and the fix is
// named in the message.
func TestPlanFailsOnMissingCredential(t *testing.T) {
	h := newHarness(t)
	h.writeAdapter("needs-key", needsKeyManifest, echoAdapterScript)
	h.write("people.csv", peopleCSV)
	h.write("pipeline.yaml", `name: needs-a-key
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: keyed
    use: needs-key
`)

	res := h.run("plan", "pipeline.yaml")
	if res.code != 3 {
		t.Fatalf("exit = %d, want 3 for a missing credential\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "missing credential FIXTURE_API_KEY", "stderr")
	contains(t, res.stderr, "gtme secret set FIXTURE_API_KEY", "stderr")

	// With the credential in the environment, the same plan is fine.
	res = h.runWithEnv([]string{"FIXTURE_API_KEY=abc123"}, "", "plan", "pipeline.yaml")
	if res.code != 0 {
		t.Fatalf("exit = %d with the credential set\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "creds:     FIXTURE_API_KEY (resolved)", "plan output")
	if strings.Contains(res.stderr, "abc123") {
		t.Error("the plan must never print a credential value")
	}
}

// TestPlanReportsSeveralProblemsAtOnce so the operator fixes them in one pass.
func TestPlanReportsSeveralProblemsAtOnce(t *testing.T) {
	h := newHarness(t)
	h.writeAdapter("needs-key", needsKeyManifest, echoAdapterScript)
	h.writeAdapter("needs-linkedin", needsLinkedInManifest, echoAdapterScript)
	h.write("people.csv", "email,full_name\njane@acme.com,Jane Doe\n")
	h.write("pipeline.yaml", `name: two-problems
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: profile
    use: needs-linkedin
  - id: keyed
    use: needs-key
`)
	res := h.run("plan", "pipeline.yaml")
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2 (a contract problem outranks a credential one)", res.code)
	}
	contains(t, res.stderr, "2 plan problems", "stderr")
	contains(t, res.stderr, "needs linkedin_url", "stderr")
	contains(t, res.stderr, "FIXTURE_API_KEY", "stderr")
}

const needsKeyManifest = `{
  "id": "needs-key",
  "version": 1,
  "role": "enrich",
  "entity_type": "person",
  "needs": {"type":"object","properties":{"email":{"type":"string"}}},
  "provides": {"type":"object","additionalProperties":false,"properties":{"headline":{"type":"string"}}},
  "credentials": ["FIXTURE_API_KEY"],
  "freshness_days": 30
}`
