package e2e

// M15 acceptance (SPEC §11, ADR-038): a deferred ai/filter as the last step
// ends the run `pending`; a plain `gtme run` of the pipeline collects rather
// than re-submitting; still-processing leaves it pending; the next run
// collects — verdicts, COST, terminus, `done`; the run after that sources
// fresh; the rules and warnings around it.

import (
	"strings"
	"testing"
)

const deferredYAML = `name: judge-deferred
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: judge
    use: ai/filter
    uses: [title]
    exclude: [judged]
    with:
      prompt: Keep decision makers.
      deferred: true
group: judged
`

func TestDeferredStepEndsTheRunPendingAndRunCollects(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("judge.yaml", deferredYAML)
	h.write("mint.yaml", "name: mint\nsource:\n  use: csv/source\n  with:\n    path: people.csv\n")
	h.mustRun("run", "mint.yaml")
	h.mustRun("groups", "add", "judged", "carol@initech.dev") // judgment memory: carol is already judged

	// The script: submit consumes nothing; the first collect is still
	// processing; the second answers one record at a time.
	env := h.fixtureScript("ai.json", "$pending", "$auto")
	env = append(env, "GTME_AI_FIXTURE_DEFER=1", "GTME_CONCURRENCY=1")

	plan := h.mustRun("plan", "judge.yaml")
	contains(t, plan.stderr, "deferred:  the run ends in flight here; the next `gtme run` of this pipeline collects", "plan output")
	if strings.Contains(plan.stderr, "respend:") {
		t.Errorf("AI steps do not warn about respend (ADR-039):\n%s", plan.stderr)
	}

	// 1. Submit: the run ends pending — zero verdicts, zero cost, the token
	//    and the collection verb on the receipt.
	res := h.runWithEnv(env, "", "run", "judge.yaml")
	if res.code != 0 {
		t.Fatalf("submit exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "judge: 2 in, 0 out, 0 cached, 0 filtered, 0 failed, 1 gated, 2 in flight", "step line")
	contains(t, res.stderr, "pending — ended with a step in flight; the next `gtme run` of this pipeline collects", "receipt title")
	contains(t, res.stderr, "judge: 2 record(s) in flight (fixture-batch-1); the next `gtme run` of this pipeline collects, or `gtme run --resume", "receipt")
	runID := h.queryStrings(`SELECT id FROM runs WHERE pipeline = 'judge-deferred'`)[0]
	if s := h.queryStrings(`SELECT status FROM runs WHERE id = '` + runID + `'`); s[0] != "pending" {
		t.Fatalf("status = %s, want pending", s[0])
	}
	if n := h.queryInt(`SELECT count(*) FROM run_records WHERE run_id = '` + runID + `' AND verdicts != '{}'`); n != 0 {
		t.Errorf("verdicts before collection = %d", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM costs`); n != 0 {
		t.Errorf("cost before collection = %d rows", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE event = 'pending' AND detail LIKE '%fixture-batch-1%'`); n != 2 {
		t.Errorf("pending events = %d, want 2", n)
	}
	list := h.mustRun("runs")
	for _, line := range strings.Split(list.stderr, "\n") {
		if strings.Contains(line, "judge-deferred") {
			if !strings.Contains(line, "pending") || !strings.HasSuffix(strings.TrimRight(line, " "), " 2") {
				t.Errorf("runs list should show pending with 2 in flight: %q", line)
			}
		}
	}

	// 2. A plain run collects — still processing — and submits nothing new.
	res = h.runWithEnv(env, "", "run", "judge.yaml")
	if res.code != 0 {
		t.Fatalf("collect-1 exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "collecting run "+runID+" — the latest run of \"judge-deferred\" ended with a step in flight", "collect-first")
	contains(t, res.stderr, "judge: 2 in, 0 out, 0 cached, 0 filtered, 0 failed, 1 gated, 2 in flight", "still in flight")
	if n := h.queryInt(`SELECT count(*) FROM runs WHERE pipeline = 'judge-deferred'`); n != 1 {
		t.Fatalf("runs = %d, want 1 — collecting must not start a run", n)
	}
	if n := h.queryInt(`SELECT count(DISTINCT json_extract(detail, '$.token')) FROM step_events WHERE event = 'pending'`); n != 1 {
		t.Errorf("distinct tokens = %d, want 1 — zero new submits", n)
	}
	if s := h.queryStrings(`SELECT status FROM runs WHERE id = '` + runID + `'`); s[0] != "pending" {
		t.Errorf("status = %s, want pending", s[0])
	}

	// 3. The next run collects: verdicts and COST land under the same run,
	//    the terminus asserts, the run finishes done.
	res = h.runWithEnv(env, "", "run", "judge.yaml")
	if res.code != 0 {
		t.Fatalf("collect-2 exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "judge: 2 in, 2 out, 0 cached, 0 filtered, 0 failed", "collected")
	contains(t, res.stderr, `group "judged": 2 record(s) added`, "terminus")
	if s := h.queryStrings(`SELECT status FROM runs WHERE id = '` + runID + `'`); s[0] != "done" {
		t.Errorf("status = %s, want done", s[0])
	}
	if n := h.queryInt(`SELECT count(*) FROM run_records WHERE run_id = '` + runID + `' AND verdicts LIKE '%"judge":"pass"%'`); n != 2 {
		t.Errorf("pass verdicts = %d, want 2", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM costs WHERE run_id = '` + runID + `' AND step_id = 'judge'`); n != 1 {
		t.Errorf("COST rows under the run = %d, want 1", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE event = 'collected'`); n != 2 {
		t.Errorf("collected events = %d, want 2", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM runs WHERE pipeline = 'judge-deferred'`); n != 1 {
		t.Errorf("runs = %d, want 1", n)
	}

	// 4. Now a run sources fresh — a new run, and judgment memory keeps the
	//    two judged records out (exclude: judged), so nothing is submitted.
	res = h.runWithEnv(env, "", "run", "judge.yaml")
	if res.code != 0 {
		t.Fatalf("fresh exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if strings.Contains(res.stderr, "collecting run") {
		t.Errorf("a done pipeline must not be collected:\n%s", res.stderr)
	}
	if n := h.queryInt(`SELECT count(*) FROM runs WHERE pipeline = 'judge-deferred'`); n != 2 {
		t.Errorf("runs = %d, want 2", n)
	}
	contains(t, res.stderr, "judge: 0 in, 0 out, 0 cached, 0 filtered, 0 failed, 3 gated", "judgment memory")
	if s := h.queryStrings(`SELECT status FROM runs WHERE pipeline = 'judge-deferred' ORDER BY started_at DESC LIMIT 1`); s[0] != "done" {
		t.Errorf("fresh run status = %s, want done", s[0])
	}

	// --simulate answers synchronously and says so (a variant with no
	// judgment memory, so the step actually runs).
	h.write("judge-sim.yaml", strings.Replace(strings.Replace(deferredYAML, "    exclude: [judged]\n", "", 1), "name: judge-deferred", "name: judge-sim", 1))
	sim := h.run("run", "judge-sim.yaml", "--simulate")
	if sim.code != 0 {
		t.Fatalf("simulate exit = %d\nstderr:\n%s", sim.code, sim.stderr)
	}
	contains(t, sim.stderr, "has no batch surface — answering synchronously", "simulate note")
	if strings.Contains(sim.stderr, "in flight") {
		t.Errorf("a simulated run never ends in flight:\n%s", sim.stderr)
	}
}

func TestDeferredRulesAndRespendWarning(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)

	// A deferred step anywhere but last fails plan naming the fix.
	h.write("mid.yaml", `name: mid
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: judge
    use: ai/filter
    with:
      prompt: Judge.
      deferred: true
  - id: send
    use: mock/deliver
    with:
      campaign: q3
    idempotency: email
`)
	res := h.run("plan", "mid.yaml")
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `step "judge": deferred: true is valid only on the pipeline's last step — land this step's output in a group`, "stderr")

	// --dry-run on a deferred pipeline warns.
	h.write("cc.yaml", `name: cc
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: judge
    use: ai/filter
    with:
      prompt: Judge.
      deferred: true
`)
	plan := h.mustRun("plan", "cc.yaml")
	// AI steps no longer warn about respend: the judgment cache remembers
	// by default (ADR-039).
	if strings.Contains(plan.stderr, "respend:") {
		t.Errorf("an AI step must not warn about respend:\n%s", plan.stderr)
	}
	// engine: retired with the shell-out (ADR-050): the plan names the
	// replacement rather than quietly ignoring the key.
	h.write("engine.yaml", `name: engine
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: judge
    use: ai/filter
    with:
      prompt: Judge.
      engine: claude-code
`)
	eng := h.run("plan", "engine.yaml")
	if eng.code != 2 {
		t.Fatalf("engine: exit = %d, want 2\nstderr:\n%s", eng.code, eng.stderr)
	}
	contains(t, eng.stderr, "engine: is not a key (ADR-050)", "engine refusal")
	contains(t, eng.stderr, "make this an agent/* step", "engine refusal names agent/*")
	env := h.fixtureScript("ai.json", "$auto")
	dry := h.runWithEnv(env, "", "run", "cc.yaml", "--dry-run")
	if dry.code != 0 {
		t.Fatalf("dry exit = %d\nstderr:\n%s", dry.code, dry.stderr)
	}
	contains(t, dry.stderr, "warning: --dry-run has nothing to hold back here — a deferred pipeline carries no deliver step", "dry-run warning")

	// A credentialed enrich with no freshness window warns the same way;
	// cache: silences it.
	h.writeAdapter("paid-enrich", `{
  "id": "paid-enrich",
  "version": 1,
  "role": "enrich",
  "entity_type": "person",
  "credentials": ["PAID_KEY"],
  "needs": {"type":"object","additionalProperties":true},
  "provides": {"type":"object","additionalProperties":false,"properties":{"headline":{"type":"string"}}}
}`, echoAdapterScript)
	h.write("paid.yaml", `name: paid
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: look
    use: paid-enrich
`)
	plan = h.runWithEnv([]string{"PAID_KEY=x"}, "", "plan", "paid.yaml")
	if plan.code != 0 {
		t.Fatalf("plan exit = %d\nstderr:\n%s", plan.code, plan.stderr)
	}
	contains(t, plan.stderr, "warning:   respend: this paid step has no freshness window", "enrich respend warning")
	h.write("paid-cached.yaml", `name: paid-cached
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: look
    use: paid-enrich
    cache: 30d
`)
	plan = h.runWithEnv([]string{"PAID_KEY=x"}, "", "plan", "paid-cached.yaml")
	if strings.Contains(plan.stderr, "respend:") {
		t.Errorf("cache: must silence the warning:\n%s", plan.stderr)
	}
}
