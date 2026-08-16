package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// aiPipeline is the shape of the pipeline in SPEC §9, with fixture adapters
// standing in for the paid ones: source → AI filter → AI compose → deliver.
const aiPipeline = `name: ai-and-deliver
version: 1

source:
  use: csv/source
  with:
    path: people.csv

steps:
  - id: icp-filter
    use: ai/filter
    with:
      prompt: Keep only contacts who own outbound tooling decisions.
      batch_size: 25

  - id: personalize
    use: ai/compose
    when: icp-filter.passed
    with:
      prompt: Write first_line and ps_line from what you know.
      batch_size: 25

deliver:
  use: mock/deliver
  with:
    campaign: Q3 VP Marketing
  idempotency: email
`

// fixtureScript writes an AI engine script and returns the env entries that
// select it.
func (h *harness) fixtureScript(name string, responses ...string) []string {
	h.t.Helper()
	body := "[\n"
	for i, r := range responses {
		if i > 0 {
			body += ",\n"
		}
		body += "  " + jsonString(r)
	}
	body += "\n]\n"
	path := h.write(name, body)
	return []string{"GTM_AI_ENGINE=fixture", "GTM_AI_FIXTURE=" + path}
}

func jsonString(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + replacer.Replace(s) + `"`
}

// TestAIStepsFilterComposeAndDeliver is the M5 acceptance test: verdicts gate the
// downstream step, malformed AI output retries once and then succeeds, and a
// second run delivers nothing twice.
func TestAIStepsFilterComposeAndDeliver(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("pipeline.yaml", aiPipeline)
	deliverLog := filepath.Join(h.work, "delivered.ndjson")

	// The filter's first answer is garbage, so the adapter must retry once. Its
	// second answer keeps two of three records. The compose step then answers with
	// the fixture engine's synthesized (schema-valid) reply.
	filterAnswer := `[
	  {"identity_key": "jane.doe@acme.com", "pass": true, "reason": "owns tooling"},
	  {"identity_key": "bob@globex.io", "pass": true, "reason": "owns tooling"},
	  {"identity_key": "carol@initech.dev", "pass": false, "reason": "not a decision maker"}
	]`
	env := h.fixtureScript("ai.json", "I'd be happy to help! Here are the results:", filterAnswer, "$auto")
	env = append(env, "GTM_CONCURRENCY=1", "MOCK_DELIVER_LOG="+deliverLog)

	first := h.runWithEnv(env, "", "run", "pipeline.yaml")
	if first.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", first.code, first.stderr)
	}
	contains(t, first.stderr, "invalid model output", "stderr should report the retry")
	contains(t, first.stderr, "icp-filter: 3 in, 2 out, 0 cached, 1 filtered", "filter tally")
	contains(t, first.stderr, "personalize: 2 in, 2 out", "compose tally")
	contains(t, first.stderr, "deliver: 2 in, 2 out", "deliver tally")

	// The filtered-out record is recorded as a fail verdict and stops advancing.
	verdicts := h.queryStrings(`SELECT verdicts FROM run_records ORDER BY identity_id`)
	fails := 0
	for _, v := range verdicts {
		if strings.Contains(v, `"fail"`) {
			fails++
		}
	}
	if fails != 1 {
		t.Errorf("fail verdicts = %d, want 1 (%v)", fails, verdicts)
	}
	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field = 'first_line'`); n != 2 {
		t.Errorf("first_line values = %d, want 2 (the filtered record must not be composed)", n)
	}
	// ai/* provenance carries the engine's model identifier (SPEC §10a,
	// ADR-026) — under the fixture engine, that identifier is "fixture",
	// which is also what marks the judgment as synthetic.
	if n := h.queryInt(
		`SELECT count(*) FROM field_values WHERE field = 'ps_line' AND source = 'ai/compose @ fixture'`); n != 2 {
		t.Errorf("ps_line values = %d, want 2", n)
	}

	// Deliveries: two records, keyed by email (the configured idempotency field).
	if n := h.queryInt(`SELECT count(*) FROM deliveries WHERE target = 'mock/deliver'`); n != 2 {
		t.Errorf("deliveries = %d, want 2", n)
	}
	keys := h.queryStrings(`SELECT idempotency FROM deliveries ORDER BY idempotency`)
	if strings.Join(keys, ",") != "bob@globex.io,jane.doe@acme.com" {
		t.Errorf("idempotency keys = %v", keys)
	}
	if lines := countLines(t, deliverLog); lines != 2 {
		t.Fatalf("the fixture target received %d records, want 2", lines)
	}

	// Second run: same pipeline, same records. Nothing may be delivered twice.
	env2 := h.fixtureScript("ai2.json", filterAnswer, "$auto")
	env2 = append(env2, "GTM_CONCURRENCY=1", "MOCK_DELIVER_LOG="+deliverLog)
	second := h.runWithEnv(env2, "", "run", "pipeline.yaml")
	if second.code != 0 {
		t.Fatalf("second run exit = %d\nstderr:\n%s", second.code, second.stderr)
	}
	contains(t, second.stderr, "deliver: 0 in, 0 out, 2 cached", "deliver should skip both")

	if n := h.queryInt(`SELECT count(*) FROM deliveries`); n != 2 {
		t.Errorf("deliveries after the second run = %d, want 2", n)
	}
	if lines := countLines(t, deliverLog); lines != 2 {
		t.Errorf("the fixture target received %d records in total, want 2 — a duplicate was delivered", lines)
	}
	if n := h.queryInt(
		`SELECT count(*) FROM step_events WHERE step_id = 'deliver' AND event = 'skipped_cache'`); n != 2 {
		t.Errorf("already_delivered skips = %d, want 2", n)
	}
}

// TestAIStepFailsAfterRetry proves the batch fails loudly when the model cannot
// produce valid output twice, and that the run is marked failed.
func TestAIStepFailsAfterRetry(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("pipeline.yaml", `name: bad-ai
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: icp-filter
    use: ai/filter
    with:
      prompt: Keep the good ones.
`)
	env := h.fixtureScript("ai.json", "nope", "still not json")
	env = append(env, "GTM_CONCURRENCY=1")

	res := h.runWithEnv(env, "", "run", "pipeline.yaml")
	if res.code == 0 {
		t.Fatalf("expected a non-zero exit\nstderr:\n%s", res.stderr)
	}
	contains(t, res.stderr, "still invalid after one retry", "stderr")
	if n := h.queryInt(`SELECT count(*) FROM runs WHERE status = 'failed'`); n != 1 {
		t.Errorf("failed runs = %d, want 1", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM run_records WHERE state = 'icp-filter'`); n != 0 {
		t.Errorf("records past the filter = %d, want 0", n)
	}
}

// TestAIPlanShowsBatchingAndCredentialWarning covers what the operator sees before
// spending anything.
func TestAIPlanShowsBatchingAndCredentialWarning(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("pipeline.yaml", aiPipeline)

	res := h.mustRun("plan", "pipeline.yaml")
	contains(t, res.stderr, "batch:     25 records per invocation", "plan output")
	contains(t, res.stderr, "projects:  (every field known about the record)", "plan output")
	contains(t, res.stderr, "optional credential ANTHROPIC_API_KEY is not set", "plan output")
	contains(t, res.stderr, "idempotency: email", "plan output")
	contains(t, res.stderr, "when:      icp-filter.passed", "plan output")
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("reading %s: %v", path, err)
	}
	return len(nonEmptyLines(string(raw)))
}
