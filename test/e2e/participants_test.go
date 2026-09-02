package e2e

// M24 acceptance (SPEC §11, ADR-048/049/050): people and agents are
// adapters. A human/* or agent/* step opens no session — with no terminal
// every record waits in the ledger under a runner-owned token, `gtme answer`
// records a validated judgment, and the next `gtme run` collects it as it
// collects a deferred batch. What the answer lands as is the point: the
// participant in provenance, the referent pointing at the value reviewed, and
// the participant's own cost under the run.

import (
	"strings"
	"testing"
)

// reviewYAML: a compose writes the draft, a person grades it. `of:` names
// what the review is about, `provides:` is the menu, and no `uses:` is
// needed — the referent alone is the surface.
const reviewYAML = `name: review
version: 1

source:
  use: csv/source
  with:
    path: people.csv

steps:
  - id: draft
    use: ai/compose
    uses: [full_name]
    provides: [first_line]
    with:
      prompt: Write one opening line.

  - id: grade
    use: human/review
    of: review.first_line
    provides:
      grade:
        enum: [A, B, C, D, F]
`

// A declared provides: lands namespaced (ADR-033), so the model answers with
// the namespaced field — and the review's of: names the same one.
const draftAnswer = `[
  {"identity_key":"jane.doe@acme.com","review.first_line":"Hi Jane, saw the Acme launch."},
  {"identity_key":"bob@globex.io","review.first_line":"Hi Bob, Globex is hiring fast."},
  {"identity_key":"carol@initech.dev","review.first_line":"Hi Carol, demand gen at Initech."}
]`

const agentDraftAnswer = `[
  {"identity_key":"jane.doe@acme.com","agent.first_line":"Hi Jane, saw the Acme launch."},
  {"identity_key":"bob@globex.io","agent.first_line":"Hi Bob, Globex is hiring fast."},
  {"identity_key":"carol@initech.dev","agent.first_line":"Hi Carol, demand gen at Initech."}
]`

// TestHumanReviewPendsAnswersAndCollects is the milestone's spine: no TTY
// pends, `gtme answer` validates and records, `gtme run` collects, and the
// judgment cache remembers the person's answer exactly as it remembers a
// model's.
func TestHumanReviewPendsAnswersAndCollects(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("review.yaml", reviewYAML)

	// A review never gates, so the plan says so rather than leaving the
	// operator to infer it from a missing verdict.
	plan := h.mustRun("plan", "review.yaml")
	contains(t, plan.stderr, "human/review", "plan names the participant")

	// No TTY: every record ends pending, and the receipt names the verb
	// that answers rather than a provider that will call back.
	env := h.fixtureScript("draft.json", draftAnswer)
	res := h.runWithEnv(env, "", "run", "review.yaml")
	if res.code != 0 {
		t.Fatalf("run exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "awaiting human/review", "receipt names the participant")
	contains(t, res.stderr, "gtme answer review", "receipt names the verb")
	if got := h.queryStrings(`SELECT status FROM runs ORDER BY started_at DESC LIMIT 1`); len(got) != 1 || got[0] != "pending" {
		t.Fatalf("run status = %v, want [pending]", got)
	}
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE step_id = 'grade' AND event = 'pending'`); n != 3 {
		t.Errorf("pending events = %d, want 3", n)
	}

	// The pending surface is what an agent reads before it answers: the
	// rendered record on stderr, the same thing as NDJSON on stdout.
	pend := h.mustRun("show", "--run", "last", "--pending")
	contains(t, pend.stderr, "3 awaiting human/review", "pending listing")
	contains(t, pend.stderr, "Hi Jane, saw the Acme launch.", "pending listing renders the referent")
	contains(t, pend.stdout, `"step":"grade"`, "pending NDJSON")
	contains(t, pend.stdout, `"role":"review"`, "pending NDJSON names the role")

	// A value outside the enum is refused naming what is allowed.
	bad := h.run("answer", "review.yaml", "jane.doe@acme.com", "--set", "grade=Z")
	if bad.code != 2 {
		t.Fatalf("bad grade exit = %d, want 2\nstderr:\n%s", bad.code, bad.stderr)
	}
	for _, want := range []string{"A", "B", "C", "D", "F"} {
		contains(t, bad.stderr, want, "the refusal names the allowed values")
	}

	// A record that is not pending under the step is refused too.
	notPending := h.run("answer", "review.yaml", "grade", "nobody@example.com", "--set", "grade=B")
	if notPending.code != 2 {
		t.Errorf("unknown record exit = %d, want 2\nstderr:\n%s", notPending.code, notPending.stderr)
	}

	// A valid answer records, and says the run collects it next.
	ok := h.mustRun("answer", "review.yaml", "jane.doe@acme.com", "--set", "grade=B", "--note", "too generic")
	contains(t, ok.stderr, "answered by human/", "answer receipt")
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE event = 'answered'`); n != 1 {
		t.Fatalf("answered events = %d, want 1", n)
	}
	// `gtme answer` writes an answer and nothing else: no field lands until
	// the run collects it.
	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field LIKE '%grade%'`); n != 0 {
		t.Errorf("grade values before collection = %d, want 0", n)
	}

	// Collect: the next run resumes the pending run rather than sourcing
	// anew, and the answered record completes the step.
	env = h.fixtureScript("draft2.json", draftAnswer)
	res = h.runWithEnv(env, "", "run", "review.yaml")
	if res.code != 0 {
		t.Fatalf("collect exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field = 'review.grade'`); n != 1 {
		t.Fatalf("grade values after collection = %d, want 1", n)
	}

	// Provenance: the participant in the model's place, and a referent
	// pointing at the very first_line row the person graded.
	src := h.queryStrings(`SELECT source FROM field_values WHERE field = 'review.grade'`)
	if len(src) != 1 || !strings.HasPrefix(src[0], "human/review @ ") {
		t.Fatalf("grade source = %v, want a human/review participant", src)
	}
	if !strings.Contains(src[0], "#") {
		t.Errorf("grade source %q carries no judgment signature", src[0])
	}
	ref := h.queryStrings(`SELECT COALESCE(fv.referent, '') FROM field_values fv WHERE fv.field = 'review.grade'`)
	if len(ref) != 1 || ref[0] == "" {
		t.Fatalf("grade referent = %v, want the reviewed row's id", ref)
	}
	about := h.queryStrings(`SELECT field FROM field_values WHERE id = ?`, ref[0])
	if len(about) != 1 || about[0] != "review.first_line" {
		t.Errorf("referent points at %v, want review.first_line", about)
	}

	// The note rides the answer and shows up in provenance, never in a key.
	show := h.mustRun("show", "jane.doe@acme.com", "--provenance")
	contains(t, show.stdout, `"note": "too generic"`, "provenance note")
	contains(t, show.stdout, `"referent"`, "provenance referent")

	// The other two are still pending: an unanswered record never advances.
	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field = 'review.grade'`); n != 1 {
		t.Errorf("unanswered records must not land a grade: %d values", n)
	}
}

// TestHumanJudgmentIsCached: a person's answer is remembered like a model's —
// an unchanged draft is not re-asked, a rewritten one is.
func TestHumanJudgmentIsCached(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("review.yaml", reviewYAML)

	// Pend, answer everyone, collect: only a run with nothing left pending
	// finishes, so the next one is a fresh pass over the same drafts.
	env := h.fixtureScript("d1.json", draftAnswer)
	h.runWithEnv(env, "", "run", "review.yaml")
	for _, key := range []string{"jane.doe@acme.com", "bob@globex.io", "carol@initech.dev"} {
		h.mustRun("answer", "review.yaml", key, "--set", "grade=B")
	}
	env = h.fixtureScript("d2.json", draftAnswer)
	res := h.runWithEnv(env, "", "run", "review.yaml")
	if res.code != 0 {
		t.Fatalf("collect exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field = 'review.grade'`); n != 3 {
		t.Fatalf("grades after collection = %d, want 3", n)
	}
	pended := h.queryInt(`SELECT count(*) FROM step_events WHERE step_id = 'grade' AND event = 'pending'`)

	// Same draft, same question: the person's answer is remembered exactly
	// as a model's would be, so nobody is asked again.
	env = h.fixtureScript("d3.json", draftAnswer)
	res = h.runWithEnv(env, "", "run", "review.yaml")
	if res.code != 0 {
		t.Fatalf("cached exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "grade: 0 in, 0 out, 3 cached", "an unchanged draft is not re-asked")
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE step_id = 'grade' AND event = 'pending'`); n != pended {
		t.Errorf("pending events = %d, want %d (an unchanged draft must not re-pend)", n, pended)
	}

	// A rewritten draft is a different question, so it comes back. The
	// compose step remembers its own answer too, so the rewrite has to be a
	// new question for it as well — a changed prompt.
	h.write("review.yaml", strings.Replace(reviewYAML,
		"prompt: Write one opening line.", "prompt: Write one warmer opening line.", 1))
	rewritten := `[
  {"identity_key":"jane.doe@acme.com","review.first_line":"Jane — congratulations on the raise."},
  {"identity_key":"bob@globex.io","review.first_line":"Hi Bob, Globex is hiring fast."},
  {"identity_key":"carol@initech.dev","review.first_line":"Hi Carol, demand gen at Initech."}
]`
	env = h.fixtureScript("d4.json", rewritten)
	res = h.runWithEnv(env, "", "run", "review.yaml")
	contains(t, res.stderr, "1 awaiting human/review", "a rewritten draft re-pends")
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE step_id = 'grade' AND event = 'pending'`); n != pended+1 {
		t.Errorf("pending events = %d, want %d (only the rewritten draft re-pends)", n, pended+1)
	}
}

// TestHumanFilterGatesOnTheAnswer: a filter answered pass=false freezes the
// record; pass=true advances it. The verdict is a person's, the mechanism is
// the runner's.
func TestHumanFilterGatesOnTheAnswer(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("gate.yaml", `name: gate
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: vet
    use: human/filter
    uses: [title]
  - id: park
    use: group/deliver
    when: vet.passed
    with:
      group: vetted
`)
	res := h.mustRun("run", "gate.yaml")
	contains(t, res.stderr, "awaiting human/filter", "receipt names the participant")

	h.mustRun("answer", "gate.yaml", "vet", "jane.doe@acme.com", "--set", "pass=true", "--set", "reason=owns budget")
	h.mustRun("answer", "gate.yaml", "vet", "bob@globex.io", "--set", "pass=false", "--set", "reason=not a buyer")

	// A filter takes only pass/reason: anything else is refused.
	bad := h.run("answer", "gate.yaml", "vet", "carol@initech.dev", "--set", "grade=A")
	if bad.code != 2 {
		t.Errorf("a filter must refuse an undeclared field, exit = %d\nstderr:\n%s", bad.code, bad.stderr)
	}

	res = h.mustRun("run", "gate.yaml")
	if n := h.queryInt(`SELECT count(*) FROM run_records WHERE verdicts LIKE '%"vet":"pass"%'`); n != 1 {
		t.Errorf("passing verdicts = %d, want 1", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM run_records WHERE verdicts LIKE '%"vet":"fail"%'`); n != 1 {
		t.Errorf("failing verdicts = %d, want 1", n)
	}
	show := h.mustRun("groups", "show", "vetted")
	contains(t, show.stderr, "1 member(s)", "only the passer advances")
	if strings.Contains(show.stderr, "bob@globex.io") {
		t.Errorf("a failed filter must freeze the record:\n%s", show.stderr)
	}
}

// TestAgentReviewRecordsItsOwnCost: an agent answers under agent/*, names
// itself with --as, and reports what it spent — measured, because the agent
// read the figure off its own vendor rather than multiplying a rate.
func TestAgentReviewRecordsItsOwnCost(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("agent.yaml", `name: agent
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: draft
    use: ai/compose
    uses: [full_name]
    provides: [first_line]
    with:
      prompt: Write one opening line.
  - id: grade
    use: agent/review
    of: agent.first_line
    provides:
      grade:
        enum: [A, B, C, D, F]
`)
	env := h.fixtureScript("draft.json", agentDraftAnswer)
	res := h.runWithEnv(env, "", "run", "agent.yaml")
	if res.code != 0 {
		t.Fatalf("run exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "awaiting agent/review", "receipt names the agent adapter")

	h.mustRun("answer", "agent.yaml", "jane.doe@acme.com", "--set", "grade=A",
		"--as", "claude-code", "--cost", "0.01", "--measured")

	env = h.fixtureScript("draft2.json", agentDraftAnswer)
	h.runWithEnv(env, "", "run", "agent.yaml")

	src := h.queryStrings(`SELECT source FROM field_values WHERE field = 'agent.grade'`)
	if len(src) != 1 || !strings.HasPrefix(src[0], "agent/review @ claude-code#") {
		t.Fatalf("grade source = %v, want agent/review @ claude-code#<sig>", src)
	}
	if n := h.queryInt(`SELECT count(*) FROM costs WHERE basis = 'measured' AND amount_usd = 0.01`); n != 1 {
		t.Errorf("measured cost rows = %d, want 1", n)
	}
	// The event names who answered with its kind prefix; provenance puts the
	// adapter before the @ and the bare name after it (ADR-049 (6) and (8)).
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE event = 'answered' AND detail LIKE '%"participant":"agent/claude-code"%'`); n != 1 {
		t.Errorf("agent/claude-code answered events = %d, want 1", n)
	}

	// --measured says how to read --cost, so it needs one.
	bare := h.run("answer", "agent.yaml", "bob@globex.io", "--set", "grade=B", "--measured")
	if bare.code != 2 {
		t.Errorf("--measured without --cost exit = %d, want 2\nstderr:\n%s", bare.code, bare.stderr)
	}
}

// TestParticipantPlanRules: a review never gates, a deliver step after a
// person carries the cron note, and --simulate rehearses nothing a person
// would have decided.
func TestParticipantPlanRules(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)

	// `when: <review>.passed` is a category error: a review labels, it does
	// not gate.
	h.write("gates.yaml", `name: gates
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: grade
    use: human/review
    of: full_name
    provides:
      grade:
        enum: [A, B]
  - id: park
    use: group/deliver
    when: grade.passed
    with:
      group: graded
`)
	res := h.run("plan", "gates.yaml")
	if res.code != 2 {
		t.Fatalf("when: <review>.passed exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "grade", "the refusal names the step")

	// A deliver step after a person: one note, because under cron the
	// pipeline waits for its person and sources nothing new.
	h.write("cron.yaml", `name: cron
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: vet
    use: human/filter
    uses: [title]
  - id: park
    use: group/deliver
    when: vet.passed
    with:
      group: vetted
`)
	plan := h.mustRun("plan", "cron.yaml")
	contains(t, plan.stderr, "under cron this pipeline waits for a person", "the cron note")

	// --simulate: a person is a simulation gap, not a rehearsal.
	sim := h.mustRun("run", "cron.yaml", "--simulate")
	contains(t, sim.stderr, "simulation gap", "a human step is a gap under --simulate")
}
