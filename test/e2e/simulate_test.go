package e2e

// The simulation gate (SPEC §8, ADR-028), offline: the campaign-zero shape —
// widened with both AI steps — simulates end-to-end with zero network calls
// and zero credentials, nothing persists in the real ledger, and a
// credentialed adapter with nothing to serve surfaces as a simulation gap.

import (
	"os"
	"path/filepath"
	"testing"
)

const simulateYAML = `name: campaign-zero-sim
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
  - id: fit
    use: ai/filter
    uses: [full_name, company_domain]
    with:
      prompt: Keep everyone who plausibly buys outbound tooling.

  - id: lines
    use: ai/compose
    uses: [full_name]
    with:
      prompt: Write first_line and ps_line.

deliver:
  use: mock/deliver
  with:
    campaign: sim-test
  variables:
    first_name: full_name
    first_line: first_line
  idempotency: email
`

// TestSimulateCampaignZero is M8's simulation acceptance criterion (SPEC §11):
// the campaign-zero pipeline simulates end-to-end with zero network calls —
// no AI key is present, so a network attempt could only fail the run.
func TestSimulateCampaignZero(t *testing.T) {
	h := newHarness(t)
	h.write("contacts.csv", campaignZeroCSV)
	h.write("sim.yaml", simulateYAML)
	deliverLog := filepath.Join(h.work, "delivered.ndjson")

	res := h.runWithEnv([]string{"MOCK_DELIVER_LOG=" + deliverLog}, "", "run", "sim.yaml", "--simulate")
	if res.code != 0 {
		t.Fatalf("simulate exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "SIMULATED", "simulate receipt")
	contains(t, res.stderr, "no network, no spend", "simulate banner")
	// The AI steps answered synthetically (fixture engine, $auto), and the
	// deliver step resolved its variables like a dry run.
	contains(t, res.stderr, "Fixture first line", "simulated compose output")
	contains(t, res.stderr, "resolved variables", "simulate receipt")

	// Nothing sent, nothing persisted: the deliver fixture never ran, and the
	// real ledger gained no run, no identities, no fields.
	if _, err := os.Stat(deliverLog); !os.IsNotExist(err) {
		t.Fatalf("simulate wrote to the deliver log: %v", err)
	}
	if n := h.queryInt(`SELECT count(*) FROM runs`); n != 0 {
		t.Errorf("runs in the real ledger = %d, want 0 (simulation is ephemeral)", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM identities`); n != 0 {
		t.Errorf("identities in the real ledger = %d, want 0", n)
	}
	res = h.mustRun("runs")
	if got := res.stdout + res.stderr; len(nonEmptyLines(got)) > 1 {
		t.Errorf("gtm runs after simulate:\n%s", got)
	}
}

// TestSimulateGapSurfaces: a credentialed process adapter with no fixtures is
// stubbed and surfaced, and its missing credential does not block the
// simulated plan (SPEC §8).
func TestSimulateGapSurfaces(t *testing.T) {
	h := newHarness(t)
	h.write("contacts.csv", "Full Name,Linkedin\nJane Doe,https://www.linkedin.com/in/jane-doe\n")
	h.write("gap.yaml", `name: gap-check
source:
  use: csv/source
  with:
    path: contacts.csv
    columns:
      full_name: Full Name
      linkedin_url: Linkedin
steps:
  - id: enrich
    use: harvest/profile
`)

	// No HARVEST_API_KEY anywhere: a plain run must fail the plan…
	res := h.run("run", "gap.yaml")
	if res.code != 3 {
		t.Fatalf("unsimulated exit = %d, want 3 (missing credential)\nstderr:\n%s", res.code, res.stderr)
	}
	// …while the simulated run proceeds and surfaces the gap.
	res = h.run("run", "gap.yaml", "--simulate")
	if res.code != 0 {
		t.Fatalf("simulate exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "ignoring missing credentials", "simulate plan downgrade")
	contains(t, res.stderr, "simulation gap: enrich (harvest/profile)", "simulate receipt")
	contains(t, res.stderr, "1 record(s) passed through untouched", "simulate receipt")
}

// TestSimulateRejectsFlagCombos: simulate is its own rung on the gate ladder.
func TestSimulateRejectsFlagCombos(t *testing.T) {
	h := newHarness(t)
	h.write("contacts.csv", campaignZeroCSV)
	h.write("sim.yaml", simulateYAML)

	res := h.run("run", "sim.yaml", "--simulate", "--dry-run")
	if res.code != 2 {
		t.Errorf("--simulate --dry-run exit = %d, want 2", res.code)
	}
	res = h.run("run", "sim.yaml", "--simulate", "--resume", "last")
	if res.code != 2 {
		t.Errorf("--simulate --resume exit = %d, want 2", res.code)
	}
}
