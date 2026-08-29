package e2e

// M16 acceptance (SPEC §11, ADR-039): the judgment cache. A re-run with an
// unchanged prompt and inputs dispatches nothing (every record skipped with
// reason same_judgment, verdicts re-applied); changing the prompt re-judges
// all; changing one input re-judges one; cache: 1d past the window
// re-judges; respend: true re-judges; provenance carries the signature; the
// AI respend warning is gone; --simulate cache-skips; a deferred step
// cache-checks before submitting.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cachedJudgeYAML = `name: cached-judge
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: judge
    use: ai/filter
    uses: [title]
    provides: [rationale]
    with:
      prompt: %s
  - id: brief
    use: ai/compose
    when: judge.passed
    uses: [full_name]
    provides: [line]
    with:
      prompt: Write one line.
group: judged
`

const cachedJudgeAnswer = `[
  {"identity_key":"jane.doe@acme.com","pass":true,"reason":"fits","cached-judge.rationale":"buys"},
  {"identity_key":"bob@globex.io","pass":true,"reason":"fits","cached-judge.rationale":"buys"},
  {"identity_key":"carol@initech.dev","pass":false,"reason":"no","cached-judge.rationale":"does not"}
]`

func TestJudgmentCacheReusesTheSameAnswer(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("judge.yaml", strings.Replace(cachedJudgeYAML, "%s", "Keep decision makers.", 1))
	run := func(name string, answers ...string) (result, int) {
		t.Helper()
		log := filepath.Join(h.work, name+".log")
		env := h.fixtureScript(name+".json", answers...)
		env = append(env, "GTME_AI_FIXTURE_LOG="+log, "GTME_CONCURRENCY=1")
		res := h.runWithEnv(env, "", "run", "judge.yaml")
		if res.code != 0 {
			t.Fatalf("%s: exit = %d\nstderr:\n%s", name, res.code, res.stderr)
		}
		return res, countLines(t, log)
	}

	// Run 1 judges and composes for real: two model calls (one per AI step).
	res, calls := run("first", cachedJudgeAnswer, "$auto")
	contains(t, res.stderr, "judge: 3 in, 2 out, 0 cached, 1 filtered", "first run")
	contains(t, res.stderr, `group "judged": 2 record(s) added`, "first terminus")
	if calls != 2 {
		t.Fatalf("model calls = %d, want 2", calls)
	}
	// Provenance carries the signature; gtme show --provenance shows it.
	src := h.queryStrings(`SELECT DISTINCT source FROM field_values WHERE field = 'cached-judge.rationale'`)
	if len(src) != 1 || !strings.HasPrefix(src[0], "ai/filter @ fixture#") || len(src[0]) != len("ai/filter @ fixture#")+12 {
		t.Errorf("provenance = %v, want ai/filter @ <model>#<12-hex signature>", src)
	}
	show := h.mustRun("show", "jane.doe@acme.com", "--provenance")
	contains(t, show.stdout, "ai/filter @ fixture#", "show --provenance")
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE event = 'done' AND step_id = 'judge'
		AND json_extract(detail, '$.signature') IS NOT NULL AND json_extract(detail, '$.input') IS NOT NULL`); n != 3 {
		t.Errorf("done events carrying the cache keys = %d, want 3 (the failed judgment too)", n)
	}
	if strings.Contains(h.mustRun("plan", "judge.yaml").stderr, "respend:") {
		t.Errorf("AI steps must not warn about respend any more")
	}

	// Run 2: same question, same facts — nothing dispatched; verdicts
	// re-applied, so gating and the terminus behave as before.
	res, calls = run("second", cachedJudgeAnswer, "$auto")
	contains(t, res.stderr, "judge: 0 in, 0 out, 3 cached, 1 filtered", "judge cached, verdicts re-applied")
	contains(t, res.stderr, "brief: 0 in, 0 out, 2 cached", "compose cached")
	if calls != 0 {
		t.Fatalf("model calls on an unchanged re-run = %d, want 0", calls)
	}
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE event = 'skipped_cache' AND detail LIKE '%same_judgment%'`); n != 5 {
		t.Errorf("same_judgment skips = %d, want 5 (3 judged + 2 composed)", n)
	}
	last := h.queryStrings(`SELECT id FROM runs ORDER BY started_at DESC LIMIT 1`)[0]
	if n := h.queryInt(`SELECT count(*) FROM run_records WHERE run_id = '` + last + `' AND verdicts LIKE '%"judge":"fail"%'`); n != 1 {
		t.Errorf("re-applied fail verdicts = %d, want 1", n)
	}
	// --simulate of the judged pipeline cache-skips exactly the same way —
	// the signature names the model the armed run uses, so the same
	// operator env sees the same cache.
	sim := h.runWithEnv(h.fixtureScript("sim.json", "$auto"), "", "run", "judge.yaml", "--simulate")
	if sim.code != 0 {
		t.Fatalf("simulate exit = %d\nstderr:\n%s", sim.code, sim.stderr)
	}
	contains(t, sim.stderr, "judge: 0 in, 0 out, 3 cached, 1 filtered", "simulate cache-skips")

	// Run 3: a changed prompt is a different question — everyone re-judged.
	h.write("judge.yaml", strings.Replace(cachedJudgeYAML, "%s", "Keep decision makers who own budget.", 1))
	res, calls = run("third", cachedJudgeAnswer, "$auto")
	contains(t, res.stderr, "judge: 3 in, 2 out, 0 cached, 1 filtered", "prompt change re-judges all")
	contains(t, res.stderr, "brief: 0 in, 0 out, 2 cached", "the compose's question and facts are unchanged — still cached")
	if calls != 1 {
		t.Errorf("model calls after a prompt change = %d, want 1 (only the judge)", calls)
	}

	// Run 4: one record's input changes — only that record is re-judged.
	h.write("people.csv", strings.Replace(peopleCSV, "Head of Growth", "CFO", 1))
	res, calls = run("fourth", `[{"identity_key":"bob@globex.io","pass":true,"reason":"fits","cached-judge.rationale":"buys"}]`)
	contains(t, res.stderr, "judge: 1 in, 1 out, 2 cached, 1 filtered", "one changed input re-judges one")
	if calls != 1 {
		t.Errorf("model calls after one input change = %d, want 1", calls)
	}

	// Run 5: cache: 1d with the judgments aged past the window — re-judged.
	h.write("judge.yaml", strings.Replace(strings.Replace(cachedJudgeYAML, "%s", "Keep decision makers who own budget.", 1),
		"    uses: [title]\n", "    uses: [title]\n    cache: 1d\n", 1))
	l := h.open()
	if _, err := l.DB().ExecContext(context.Background(), `UPDATE step_events SET created_at = '2020-01-01T00:00:00.000Z'`); err != nil {
		t.Fatal(err)
	}
	l.Close()
	res, calls = run("fifth", cachedJudgeAnswer, "$auto")
	contains(t, res.stderr, "judge: 3 in, 2 out, 0 cached, 1 filtered", "aged past the window re-judges")

	// Run 6: respend: true re-judges regardless.
	h.write("judge.yaml", strings.Replace(strings.Replace(cachedJudgeYAML, "%s", "Keep decision makers who own budget.", 1),
		"    uses: [title]\n", "    uses: [title]\n    respend: true\n", 1))
	res, calls = run("sixth", cachedJudgeAnswer, "$auto")
	contains(t, res.stderr, "judge: 3 in, 2 out, 0 cached, 1 filtered", "respend: true re-judges")
	if calls < 1 {
		t.Errorf("respend: true must call the model")
	}
	_ = os.Remove
}

// TestDeferredStepCacheChecksBeforeSubmitting: a deferred step reuses
// judgments before it would submit — a re-run after a collection submits
// only what changed (here: nothing).
func TestDeferredStepCacheChecksBeforeSubmitting(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("judge.yaml", `name: judge-deferred-cached
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: judge
    use: ai/filter
    uses: [title]
    with:
      prompt: Keep decision makers.
      deferred: true
group: judged
`)
	env := h.fixtureScript("ai.json", "$auto")
	env = append(env, "GTME_AI_FIXTURE_DEFER=1", "GTME_CONCURRENCY=1")
	// Submit, then collect.
	h.runWithEnv(env, "", "run", "judge.yaml")
	res := h.runWithEnv(env, "", "run", "judge.yaml")
	contains(t, res.stderr, "judge: 3 in, 3 out", "collected")
	// A fresh run: everything is cached — nothing submitted, run done.
	res = h.runWithEnv(env, "", "run", "judge.yaml")
	if res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "judge: 0 in, 0 out, 3 cached", "cache-check before submit")
	if strings.Contains(res.stderr, "in flight") {
		t.Errorf("nothing should be submitted:\n%s", res.stderr)
	}
	if n := h.queryInt(`SELECT count(DISTINCT json_extract(detail, '$.token')) FROM step_events WHERE event = 'pending'`); n != 1 {
		t.Errorf("distinct batch tokens = %d, want 1", n)
	}
}
