package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSpecExamplePipelinePlans is the offline half of the M6 acceptance test: the
// SPEC §9 pipeline, with the real paid adapters, resolves and validates end to
// end. Actually running it spends money and mails people, so that stays a human
// gate (SPEC §12) — see `make live`.
func TestSpecExamplePipelinePlans(t *testing.T) {
	h := newHarness(t)

	raw, err := os.ReadFile(filepath.Join(repoRoot(), "examples", "apollo-to-instantly.yaml"))
	if err != nil {
		t.Fatalf("reading the example pipeline: %v", err)
	}
	h.write("apollo-to-instantly.yaml", string(raw))

	// Without credentials the plan fails as an auth error and names every missing key.
	res := h.run("plan", "apollo-to-instantly.yaml")
	if res.code != 3 {
		t.Fatalf("exit = %d, want 3 (missing credentials)\nstderr:\n%s", res.code, res.stderr)
	}
	for _, key := range []string{"APOLLO_API_KEY", "HARVEST_API_KEY", "INSTANTLY_API_KEY"} {
		contains(t, res.stderr, "missing credential "+key, "plan stderr")
	}

	// With them set (values are never used at plan time), the plan resolves.
	env := []string{
		"APOLLO_API_KEY=plan-only",
		"HARVEST_API_KEY=plan-only",
		"INSTANTLY_API_KEY=plan-only",
		"ANTHROPIC_API_KEY=plan-only",
	}
	res = h.runWithEnv(env, "", "plan", "apollo-to-instantly.yaml")
	if res.code != 0 {
		t.Fatalf("exit = %d with credentials set\nstderr:\n%s", res.code, res.stderr)
	}

	for _, want := range []string{
		"1. source [source] — apollo/search@2",
		"2. icp-filter [filter] — ai/filter@1",
		"3. reveal [enrich] — apollo/enrich@1",
		"4. linkedin [enrich] — harvest/profile@1",
		"5. personalize [compose] — ai/compose@1",
		"6. send [deliver] — instantly/add-to-campaign@1",
		"send surface: 1 deliver step(s) (ADR-031)",
		"send → instantly/add-to-campaign (touch scope: apollo-to-instantly)",
		"requires:  any of linkedin_url | linkedin_internal_url | linkedin_sales_nav_url",
		"variables: first_line ← first_line, ps_line ← ps_line",
		"on_missing: skip",
		"cache:     30d",
		"est/record: $0.0120",
		"idempotency: email",
		"plan ok — nothing has been spent",
	} {
		contains(t, res.stderr, want, "plan output")
	}

	// The contract really is satisfied: the reveal step provides the
	// linkedin_url that harvest requires (ADR-043 — masked search no longer
	// carries it), and compose provides the lines instantly sends.
	if strings.Contains(res.stderr, "plan problems") {
		t.Errorf("plan reported problems:\n%s", res.stderr)
	}
	contains(t, res.stderr, "first_line", "available fields")

	// And planning still spends nothing and mints no run.
	if n := h.queryInt(`SELECT count(*) FROM runs`); n != 0 {
		t.Errorf("runs = %d, want 0", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM costs`); n != 0 {
		t.Errorf("cost rows = %d, want 0", n)
	}
}

// TestRunsAndSecretCommands covers the M7 verbs.
func TestRunsAndSecretCommands(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("pipeline.yaml", csvToMockYAML)

	empty := h.mustRun("runs")
	contains(t, empty.stderr, "no runs yet", "runs stderr")

	h.mustRun("run", "pipeline.yaml")

	listed := h.mustRun("runs")
	contains(t, listed.stderr, "csv-to-mock", "runs list")
	contains(t, listed.stderr, "done", "runs list")

	receipt := h.mustRun("runs", "last")
	contains(t, receipt.stderr, "pipeline: csv-to-mock", "receipt")
	contains(t, receipt.stderr, "records: 3", "receipt")
	contains(t, receipt.stderr, "mock", "receipt")
	contains(t, receipt.stderr, "gtme freeze", "receipt should say how to rebuild the pipeline")

	if res := h.run("runs", "01ZZZZZZZZZZZZZZZZZZZZZZZZ"); res.code != 2 {
		t.Errorf("unknown run exit = %d, want 2", res.code)
	}

	// Secrets: piped in, never echoed back.
	set := h.runWithEnv(nil, "s3cr3t-value\n", "secret", "set", "FIXTURE_API_KEY")
	if set.code != 0 {
		t.Fatalf("secret set exit = %d\nstderr:\n%s", set.code, set.stderr)
	}
	contains(t, set.stderr, "stored FIXTURE_API_KEY", "secret set")
	if strings.Contains(set.stderr, "s3cr3t-value") {
		t.Error("the value must never be echoed")
	}

	list := h.mustRun("secret", "list")
	contains(t, list.stderr, "FIXTURE_API_KEY", "secret list")
	if strings.Contains(list.stderr, "s3cr3t-value") {
		t.Error("secret list must print names only")
	}

	// The secrets file is private and the stored credential satisfies a plan.
	info, err := os.Stat(filepath.Join(h.home, ".gtme", "secrets"))
	if err != nil {
		t.Fatalf("stat secrets: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("secrets file mode = %o, want 600", perm)
	}

	h.writeAdapter("needs-key", needsKeyManifest, echoAdapterScript)
	h.write("keyed.yaml", `name: keyed
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: keyed
    use: needs-key
`)
	if res := h.run("plan", "keyed.yaml"); res.code != 0 {
		t.Errorf("plan exit = %d — a secret in ~/.gtme/secrets should satisfy a credential\nstderr:\n%s",
			res.code, res.stderr)
	}
}

// TestDemoPipelinePlans guards the zero-key demo's second rung. README.md
// offers `gtme plan examples/demo.yaml`, and nothing tested it, so the file
// drifted out of contract unnoticed: the Apollo search returns masked fields
// only (ADR-043), and the later steps wanted full_name and email. --simulate
// kept working the whole time, which is why it went unseen.
func TestDemoPipelinePlans(t *testing.T) {
	h := newHarness(t)

	raw, err := os.ReadFile(filepath.Join(repoRoot(), "examples", "demo.yaml"))
	if err != nil {
		t.Fatalf("reading the demo pipeline: %v", err)
	}
	h.write("demo.yaml", string(raw))

	env := []string{
		"APOLLO_API_KEY=plan-only",
		"ANTHROPIC_API_KEY=plan-only",
		"ATTIO_API_KEY=plan-only",
	}
	res := h.runWithEnv(env, "", "plan", "demo.yaml")
	if res.code != 0 {
		t.Fatalf("exit = %d, want 0 — README.md offers this command\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "plan ok — nothing has been spent", "demo plan")

	// The reveal sits past the filter, so the per-credit call is only made for
	// records that survived it (ADR-043).
	fit := strings.Index(res.stderr, "[filter] — ai/filter")
	reveal := strings.Index(res.stderr, "[enrich] — apollo/enrich")
	if fit < 0 || reveal < 0 {
		t.Fatalf("demo should filter before it reveals:\n%s", res.stderr)
	}
	if reveal < fit {
		t.Errorf("apollo/enrich runs before the filter — ADR-043 pays only past it")
	}
}
