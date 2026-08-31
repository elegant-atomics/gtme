package e2e

// The M21 acceptance (SPEC §11, ADR-044): delivery dedupe scopes to the
// resolved idempotency_scope config value — the same record delivered
// through one adapter into two scopes lands two rows; re-running either
// scope adds nothing; an unscoped adapter keeps '' semantics.

import (
	"path/filepath"
	"testing"
)

const scopeCSV = "Full Name,Email\nJane Doe,jane.doe@acme.com\n"

func scopePipeline(name, out string) string {
	return `name: ` + name + `
source:
  use: csv/source
  with:
    path: contacts.csv
    columns:
      full_name: Full Name
      email: Email
steps:
  - id: out
    use: csv/deliver
    with:
      path: ` + out + `
    variables:
      contact_email: email
    idempotency: email
`
}

func TestDeliveryDedupeScopesToTheCampaign(t *testing.T) {
	h := newHarness(t)
	h.write("contacts.csv", scopeCSV)
	a := filepath.Join(h.work, "a.csv")
	b := filepath.Join(h.work, "b.csv")
	pa := h.write("pa.yaml", scopePipeline("scope-a", a))
	pb := h.write("pb.yaml", scopePipeline("scope-b", b))

	// The same record into two scopes of one adapter: two deliveries rows,
	// one per resolved path (csv/deliver declares idempotency_scope: path).
	for _, p := range []string{pa, pb} {
		res := h.run("run", p)
		if res.code != 0 {
			t.Fatalf("run %s exit = %d\n%s", p, res.code, res.stderr)
		}
	}
	if n := h.queryInt(`SELECT count(*) FROM deliveries WHERE target = 'csv/deliver'`); n != 2 {
		t.Fatalf("deliveries = %d, want 2 (one per scope)", n)
	}
	if n := h.queryInt(`SELECT count(DISTINCT scope) FROM deliveries WHERE target = 'csv/deliver'`); n != 2 {
		t.Errorf("distinct scopes = %d, want 2", n)
	}

	// Re-running either scope adds nothing: same campaign, same record.
	for _, p := range []string{pa, pb} {
		res := h.run("run", p)
		if res.code != 0 {
			t.Fatalf("re-run %s exit = %d\n%s", p, res.code, res.stderr)
		}
		contains(t, res.stderr, "1 cached", "re-run receipt")
	}
	if n := h.queryInt(`SELECT count(*) FROM deliveries WHERE target = 'csv/deliver'`); n != 2 {
		t.Errorf("deliveries after re-runs = %d, want 2", n)
	}
	// Each artifact holds header + exactly one row: the vendor-side view of
	// "same scope never double-adds".
	for _, f := range []string{a, b} {
		if n := len(nonEmptyLines(readFile(t, f))); n != 2 {
			t.Errorf("%s lines = %d, want 2", f, n)
		}
	}

	// An unscoped deliver adapter (mock/deliver declares no idempotency_scope)
	// keeps '' scope semantics.
	if n := h.queryInt(`SELECT count(*) FROM deliveries WHERE scope = ''`); n != 0 {
		t.Errorf("csv/deliver rows with empty scope = %d, want 0", n)
	}
}
