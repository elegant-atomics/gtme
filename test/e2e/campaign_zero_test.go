package e2e

// The campaign-zero shape (VALIDATION.md, ADR-019) exercised fully offline
// against the mock/deliver fixture: identity, both edge mappings (columns: in,
// variables: out), dynamic needs, plan coherence, per-record on_missing
// verdicts, the dry/armed gate, and idempotent delivery — milestone M7's e2e
// acceptance criteria.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const campaignZeroCSV = "Full Name,Email,Company Website,Favorite Color\n" +
	"Jane Doe,Jane.Doe@Acme.com,https://www.acme.com,teal\n" +
	"Bob Stone,bob@globex.io,globex.io,\n" +
	",carol@initech.dev,initech.dev,mauve\n" // no name: on_missing must hold this one back

const campaignZeroYAML = `name: campaign-zero
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
  - id: deliver
    use: mock/deliver
    with:
      campaign: campaign-zero-test
    variables:
      first_name: full_name
    idempotency: email
`

func TestCampaignZeroDryThenArmedThenRerun(t *testing.T) {
	h := newHarness(t)
	h.write("contacts.csv", campaignZeroCSV)
	h.write("campaign-zero.yaml", campaignZeroYAML)
	deliverLog := filepath.Join(h.work, "delivered.ndjson")
	env := []string{"MOCK_DELIVER_LOG=" + deliverLog}

	// Plan: the edge mappings are visible, and nothing is spent.
	res := h.mustRun("plan", "campaign-zero.yaml")
	contains(t, res.stderr, "variables: first_name ← full_name", "plan output")
	contains(t, res.stderr, "on_missing: skip", "plan output")

	// Dry run: variables resolve into the receipt, nothing sends, nothing is
	// recorded as delivered (SPEC §8) — the armed run must not be fooled.
	res = h.runWithEnv(env, "", "run", "campaign-zero.yaml", "--dry-run")
	if res.code != 0 {
		t.Fatalf("dry run exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "dry run — nothing sent", "dry-run receipt")
	contains(t, res.stderr, `first_name: "Jane Doe"`, "dry-run resolved variables")
	contains(t, res.stderr, `first_name: "Bob Stone"`, "dry-run resolved variables")
	// The nameless record is held back by on_missing (default skip), with the
	// missing field named in the receipt.
	contains(t, res.stderr, "held back by on_missing", "dry-run receipt")
	contains(t, res.stderr, "missing full_name", "dry-run receipt")
	if _, err := os.Stat(deliverLog); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote to the deliver log: %v", err)
	}
	if n := h.queryInt(`SELECT count(*) FROM deliveries`); n != 0 {
		t.Fatalf("deliveries after dry run = %d, want 0", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE event = 'dry_run'`); n != 2 {
		t.Errorf("dry_run events = %d, want 2", n)
	}
	// The resolved variables are reconstructable from the ledger afterwards
	// (the Report story): they live in the dry_run events' detail.
	details := h.queryStrings(`SELECT detail FROM step_events WHERE event = 'dry_run' ORDER BY id`)
	if len(details) != 2 || !strings.Contains(strings.Join(details, "\n"), "Jane Doe") {
		t.Errorf("dry_run event details = %v", details)
	}

	// Armed: same command without the flag. Two deliveries, mapped variables in
	// the target's own vocabulary, the held-back record still held back.
	res = h.runWithEnv(env, "", "run", "campaign-zero.yaml")
	if res.code != 0 {
		t.Fatalf("armed run exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	raw, err := os.ReadFile(deliverLog)
	if err != nil {
		t.Fatalf("deliver log: %v", err)
	}
	lines := nonEmptyLines(string(raw))
	if len(lines) != 2 {
		t.Fatalf("delivered = %d, want 2:\n%s", len(lines), raw)
	}
	contains(t, lines[0]+lines[1], `"first_name": "Jane Doe"`, "deliver log variables")
	if n := h.queryInt(`SELECT count(*) FROM deliveries WHERE target = 'mock/deliver'`); n != 2 {
		t.Errorf("deliveries = %d, want 2", n)
	}
	// The email idempotency key is canonicalized: the CSV's mixed-case cell
	// keyed as lowercase.
	if n := h.queryInt(`SELECT count(*) FROM deliveries WHERE idempotency = 'jane.doe@acme.com'`); n != 1 {
		t.Errorf("canonical idempotency key missing")
	}

	// Re-run, unchanged: zero new deliveries — the simplest Top-up story, with
	// no cache layer involved at all (VALIDATION.md campaign zero, step 4).
	res = h.runWithEnv(env, "", "run", "campaign-zero.yaml")
	if res.code != 0 {
		t.Fatalf("re-run exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if n := h.queryInt(`SELECT count(*) FROM deliveries`); n != 2 {
		t.Errorf("deliveries after re-run = %d, want 2 (idempotency must hold)", n)
	}
	if lines := nonEmptyLines(readFile(t, deliverLog)); len(lines) != 2 {
		t.Errorf("deliver log after re-run = %d lines, want 2", len(lines))
	}

	// The ingress edge did its whole job: canonical values normalized, the
	// unmapped leftover kept under its csv.* name (SPEC §10.1).
	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field = 'email' AND value = '"jane.doe@acme.com"'`); n == 0 {
		t.Error("email was not normalized at ingress")
	}
	if n := h.queryInt(`SELECT count(DISTINCT identity_id) FROM field_values WHERE field = 'csv.favorite_color'`); n != 2 {
		t.Errorf("identities with csv.favorite_color = %d, want 2 (kept, namespaced)", n)
	}
}

func TestCampaignZeroOnMissingFail(t *testing.T) {
	h := newHarness(t)
	h.write("contacts.csv", campaignZeroCSV)
	h.write("campaign-zero.yaml", strings.Replace(campaignZeroYAML,
		"    idempotency: email", "    on_missing: fail\n    idempotency: email", 1))
	deliverLog := filepath.Join(h.work, "delivered.ndjson")

	res := h.runWithEnv([]string{"MOCK_DELIVER_LOG=" + deliverLog}, "", "run", "campaign-zero.yaml")
	if res.code != 0 {
		t.Fatalf("run exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	// fail marks the incomplete record failed (SPEC §8); the complete ones send.
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE event = 'failed' AND detail LIKE '%missing full_name%'`); n != 1 {
		t.Errorf("failed events for the incomplete record = %d, want 1", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM deliveries`); n != 2 {
		t.Errorf("deliveries = %d, want 2", n)
	}
}

// Guard (VALIDATION.md campaign zero, step 1): a CSV with no email column
// fails at plan — via the deliver adapter's static floor, before any row is
// read or anything sends.
func TestCampaignZeroGuardMissingEmail(t *testing.T) {
	h := newHarness(t)
	h.write("contacts.csv", "Full Name,Company Website\nJane Doe,acme.com\n")
	h.write("campaign-zero.yaml", campaignZeroYAML)

	res := h.run("plan", "campaign-zero.yaml")
	if res.code == 0 {
		t.Fatalf("plan passed without an email column\nstderr:\n%s", res.stderr)
	}
	// The columns: mapping names a header the file does not have — caught at
	// probe time (SPEC §7). Fix the mapping and the floor check fires instead.
	contains(t, res.stderr, `no CSV column named "Email"`, "plan stderr")

	h.write("campaign-zero.yaml", strings.Replace(campaignZeroYAML,
		"      email: Email\n", "", 1))
	res = h.run("plan", "campaign-zero.yaml")
	if res.code != 2 {
		t.Fatalf("plan exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "needs email", "plan stderr")
	if n := h.queryInt(`SELECT count(*) FROM costs`); n != 0 {
		t.Errorf("plan wrote %d cost rows, want 0", n)
	}

	// Without the deliver step, the same names-only CSV is a legitimate
	// pipeline (an enrichment starting point): the plan passes, noting —
	// never blocking on — the weak identity tier (SPEC §7).
	h.write("names-only.yaml", `name: names-only
source:
  use: csv/source
  with:
    path: contacts.csv
    columns:
      full_name: Full Name
      company_domain: Company Website
`)
	res = h.mustRun("plan", "names-only.yaml")
	contains(t, res.stderr, "name-hash fallback", "plan stderr")
}

// A variables: entry referencing a field nothing provides is a plan error
// naming the step and the field (SPEC §7, ADR-019); a near-miss of a canonical
// name gets a suggestion (SPEC §4a).
func TestCampaignZeroVariablesPlanErrors(t *testing.T) {
	h := newHarness(t)
	h.write("contacts.csv", campaignZeroCSV)
	h.write("campaign-zero.yaml", strings.Replace(campaignZeroYAML,
		"    first_name: full_name", "    first_name: headline", 1))

	res := h.run("plan", "campaign-zero.yaml")
	if res.code != 2 {
		t.Fatalf("plan exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `step "deliver"`, "plan stderr")
	contains(t, res.stderr, "headline", "plan stderr")

	h.write("campaign-zero.yaml", strings.Replace(campaignZeroYAML,
		"    first_name: full_name", "    first_name: full_nmae", 1))
	res = h.run("plan", "campaign-zero.yaml")
	if res.code != 2 {
		t.Fatalf("plan exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `did you mean "full_name"`, "plan stderr")
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(raw)
}
