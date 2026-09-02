package e2e

// M14 step 1 acceptance (SPEC §11, ADR-033): an AI step declares its output
// fields. The planner derives the step's provides from the declaration,
// namespaced by pipeline; the adapter generates the prompt shape from the
// schema and validates the model's answer against it (an enum violation is
// retried, then fails the batch — never stored); a filter emits VERDICT and
// RECORD; AI manifests are entity-agnostic, so a company pipeline plans and
// validates against the company registry.

import (
	"strings"
	"testing"
)

const qualifyPipeline = `name: qualify
version: 1

source:
  use: csv/source
  with:
    path: people.csv

steps:
  - id: judge
    use: ai/filter
    uses: [title]
    provides:
      state: {enum: [now, later]}
      rationale: {}
    with:
      prompt: Decide when to work each contact.

  - id: brief
    use: ai/compose
    when: judge.passed
    uses: [qualify.state, qualify.rationale]
    provides: [subject]
    with:
      prompt: Write a subject line from the judgment.
`

const judgeBadEnum = `[
  {"identity_key": "jane.doe@acme.com", "pass": true, "reason": "fits", "qualify.state": "never", "qualify.rationale": "r1"},
  {"identity_key": "bob@globex.io", "pass": true, "reason": "fits", "qualify.state": "later", "qualify.rationale": "r2"},
  {"identity_key": "carol@initech.dev", "pass": false, "reason": "no", "qualify.state": "later", "qualify.rationale": "r3"}
]`

const judgeGood = `[
  {"identity_key": "jane.doe@acme.com", "pass": true, "reason": "fits", "qualify.state": "now", "qualify.rationale": "owns budget"},
  {"identity_key": "bob@globex.io", "pass": true, "reason": "fits", "qualify.state": "later", "qualify.rationale": "hiring soon"},
  {"identity_key": "carol@initech.dev", "pass": false, "reason": "no", "qualify.state": "later", "qualify.rationale": "not a buyer"}
]`

// TestAIDeclaredProvidesStoreNamespacedAndRejectEnum: `provides: {state:
// {enum: [now, later]}, rationale: {}}` on an ai/filter stores
// `<pipeline>.state` for every judged record (pass or fail) and rejects a
// value outside the enum; a downstream step consumes the namespaced fields.
func TestAIDeclaredProvidesStoreNamespacedAndRejectEnum(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("pipeline.yaml", qualifyPipeline)

	// Plan: the derived, namespaced provides flow into the available set, the
	// downstream uses: validates against them, and the coincidence with the
	// canonical person field `state` is called out — not silently shadowed.
	plan := h.mustRun("plan", "pipeline.yaml")
	for _, want := range []string{
		"2. judge [filter] — ai/filter@1",
		"provides:  qualify.rationale, qualify.state",
		"projects:  qualify.state, qualify.rationale",
		"provides:  qualify.subject",
		`needs this pipeline's own judgment field "qualify.state" (declared by an earlier AI step, ADR-033)`,
		`provides: "state" lands as "qualify.state" (per-campaign, ADR-033); the canonical person field "state" is untouched — add canonical: true to write it instead`,
	} {
		contains(t, plan.stderr, want, "plan output")
	}

	// An enum violation twice over fails the batch: nothing is stored.
	env := h.fixtureScript("bad.json", judgeBadEnum, judgeBadEnum)
	env = append(env, "GTME_CONCURRENCY=1")
	res := h.runWithEnv(env, "", "run", "pipeline.yaml")
	if res.code == 0 {
		t.Fatalf("expected a non-zero exit when the enum violation survives the retry\nstderr:\n%s", res.stderr)
	}
	contains(t, res.stderr, `qualify.state must be one of now, later (got "never")`, "stderr names the violation")
	contains(t, res.stderr, "still invalid after one retry", "stderr")
	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field LIKE 'qualify.%'`); n != 0 {
		t.Fatalf("a value outside the enum reached the ledger: %d rows", n)
	}

	// One violation, then a valid answer: retried and stored. The compose step
	// answers with the fixture engine's synthesized value for the declared field.
	env = h.fixtureScript("good.json", judgeBadEnum, judgeGood, "$auto")
	env = append(env, "GTME_CONCURRENCY=1")
	res = h.runWithEnv(env, "", "run", "pipeline.yaml")
	if res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "invalid model output", "the enum violation is retried")
	contains(t, res.stderr, "judge: 3 in, 2 out, 0 cached, 1 filtered, 0 failed", "filter tally")
	contains(t, res.stderr, "brief: 2 in, 2 out", "compose tally")

	// VERDICT and RECORD from one call (SPEC §5): every judged record — the
	// failing one included — carries the namespaced judgment with ai/filter
	// provenance, and the verdicts still gate advancement.
	states := h.queryStrings(`SELECT value FROM field_values WHERE field = 'qualify.state' AND source LIKE 'ai/filter @ fixture#%' ORDER BY value`)
	if strings.Join(states, ",") != `"later","later","now"` {
		t.Errorf("qualify.state values = %v", states)
	}
	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field = 'qualify.rationale'`); n != 3 {
		t.Errorf("qualify.rationale rows = %d, want 3 (the failing record's reasoning is stored too)", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field = 'state'`); n != 0 {
		t.Errorf("the canonical person field `state` was written %d time(s); judgments must stay per-campaign", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM run_records WHERE state = 'sourced'
		AND run_id = (SELECT id FROM runs WHERE status = 'done' ORDER BY rowid DESC LIMIT 1)`); n != 1 {
		t.Errorf("frozen records = %d, want 1 (the fail verdict stops advancement)", n)
	}
	subjects := h.queryStrings(`SELECT value FROM field_values WHERE field = 'qualify.subject' AND source LIKE 'ai/compose @ fixture#%' ORDER BY value`)
	if len(subjects) != 2 || !strings.Contains(subjects[0], "Fixture subject for") {
		t.Errorf("qualify.subject values = %v", subjects)
	}
	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field IN ('first_line', 'ps_line')`); n != 0 {
		t.Errorf("a compose declaring provides must not emit its default shape: %d rows", n)
	}

	// Under --simulate the fixture engine synthesizes a schema-valid answer
	// for the declared shape, so the whole pipeline runs offline.
	sim := h.run("run", "pipeline.yaml", "--simulate")
	if sim.code != 0 {
		t.Fatalf("simulate exit = %d\nstderr:\n%s", sim.code, sim.stderr)
	}
	contains(t, sim.stderr, "judge: 3 in, 3 out", "simulated filter")
	contains(t, sim.stderr, "brief: 3 in, 3 out", "simulated compose")
}

const companiesCSV = `company_domain,company_name
acme.com,Acme Inc
globex.io,Globex
`

// TestAICompanyPipelinePlansAgainstCompanyRegistry: AI manifests are
// entity-agnostic (SPEC §10.3) — inside a company pipeline an AI step plans
// as company, validates uses: against the company registry, and its declared
// outputs land on company identities.
func TestAICompanyPipelinePlansAgainstCompanyRegistry(t *testing.T) {
	h := newHarness(t)
	h.write("companies.csv", companiesCSV)
	pipeline := func(uses string, extra string) string {
		return `name: accounts
source:
  use: csv/source
  with:
    path: companies.csv
    entity_type: company
steps:
  - id: judge
    use: ai/filter
    uses: [` + uses + `]
    provides:
      tier: {enum: [a, b]}
    with:
      prompt: Tier the account.
` + extra
	}

	h.write("ok.yaml", pipeline("company_name", ""))
	plan := h.mustRun("plan", "ok.yaml")
	contains(t, plan.stderr, "2. judge [filter] — ai/filter@1\n     entity:    company", "the AI step plans as the pipeline's entity")
	contains(t, plan.stderr, "provides:  accounts.tier", "plan output")

	// A person field in uses: is a plan error against the company registry —
	// the wrong-registry validation ADR-033 names.
	h.write("wrong.yaml", pipeline("title", ""))
	res := h.run("plan", "wrong.yaml")
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `"title" is not a canonical company field`, "stderr")

	// A compose declaring nothing keeps its person-vocabulary default, which a
	// company pipeline cannot accept; the error points at the fix.
	h.write("compose.yaml", pipeline("company_name", `  - id: brief
    use: ai/compose
    with:
      prompt: Write.
`))
	res = h.run("plan", "compose.yaml")
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `manifest provides: "first_line" is not a canonical company field`, "stderr")
	contains(t, res.stderr, "declare provides: on this step (ADR-033)", "stderr")

	// Declared outputs land on the company identities, registry-checked as company.
	env := h.fixtureScript("ai.json", "$auto")
	res = h.runWithEnv(env, "", "run", "ok.yaml")
	if res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	n := h.queryInt(`SELECT count(*) FROM field_values fv JOIN identities i ON i.id = fv.identity_id
		WHERE fv.field = 'accounts.tier' AND fv.value = '"a"' AND i.entity_type = 'company'`)
	if n != 2 {
		t.Errorf("accounts.tier on company identities = %d, want 2", n)
	}
}

// TestCanonicalProvidesLandGlobally: `canonical: true` (SPEC §7) lands a
// declared field on the canonical field of that name — global, so a deliver
// step's variables: reach it unchanged — beside namespaced siblings; the
// claim is registry-checked at plan, type and domain included.
func TestCanonicalProvidesLandGlobally(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	pipeline := func(decl string) string {
		return `name: outreach
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: write
    use: ai/compose
    uses: [title]
    provides:
` + decl + `    with:
      prompt: Write.
`
	}
	h.write("ok.yaml", pipeline(`      first_line: {canonical: true, type: string}
      subject: {}
`))
	plan := h.mustRun("plan", "ok.yaml")
	contains(t, plan.stderr, "provides:  first_line, outreach.subject", "plan output")
	if strings.Contains(plan.stderr, "note:      provides:") {
		t.Errorf("a canonical declaration is not a coincidence to note:\n%s", plan.stderr)
	}

	env := h.fixtureScript("ai.json", "$auto")
	res := h.runWithEnv(env, "", "run", "ok.yaml")
	if res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field = 'first_line' AND source LIKE 'ai/compose @ fixture#%'`); n != 3 {
		t.Errorf("canonical first_line rows = %d, want 3", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field IN ('outreach.first_line', 'ps_line')`); n != 0 {
		t.Errorf("first_line must land canonically and ps_line not at all: %d stray rows", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field = 'outreach.subject'`); n != 3 {
		t.Errorf("outreach.subject rows = %d, want 3", n)
	}

	for _, tc := range []struct{ name, decl, want string }{
		{"not canonical", "      opener: {canonical: true}\n",
			`provides: "opener" is marked canonical but is not a canonical person field`},
		{"near miss", "      first_lines: {canonical: true}\n", `did you mean "first_line"?`},
		{"type mismatch", "      follower_count: {canonical: true, type: string}\n",
			`provides: "follower_count" declares type string but the canonical person field is integer`},
		{"enum on non-string", "      follower_count: {canonical: true, enum: [a]}\n",
			`declares an enum but the canonical person field is integer, not string`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := h.write(strings.ReplaceAll(tc.name, " ", "-")+".yaml", pipeline(tc.decl))
			res := h.run("plan", path)
			if res.code != 2 {
				t.Fatalf("exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
			}
			contains(t, res.stderr, tc.want, "stderr")
		})
	}
}

// TestProvidesIsRejectedOffAISteps: the key is role-gated like uses: —
// anywhere but an AI filter/compose step it fails plan naming step and key.
func TestProvidesIsRejectedOffAISteps(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	base := `name: gated
source:
  use: csv/source
  with:
    path: people.csv
steps:
`
	for _, tc := range []struct{ name, steps, want string }{
		{"enrich adapter", `  - id: score
    use: mock-enrich-py
    provides: [note]
`, `step "score": provides: is only valid on filter/compose/review steps (mock-enrich-py has role "enrich")`},
		{"sql filter", `  - id: keep
    use: sql/filter
    provides: [note]
    with:
      query: SELECT id AS identity_id FROM identities
`, `step "keep": provides: is only valid on participant steps (ai/*, human/*, agent/*)`},
		{"inside with", `  - id: judge
    use: ai/filter
    with:
      prompt: Judge.
      provides: {state: {}}
`, `step "judge": provides: is a step-level key, not a with: key`},
		{"reserved name", `  - id: judge
    use: ai/filter
    provides: [pass, identity_key]
    with:
      prompt: Judge.
`, `step "judge": provides: "identity_key" is reserved by the AI output shape`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := h.write(strings.ReplaceAll(tc.name, " ", "-")+".yaml", base+tc.steps)
			res := h.run("plan", path)
			if res.code != 2 {
				t.Fatalf("exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
			}
			contains(t, res.stderr, tc.want, "stderr")
		})
	}
}

// anyEntityManifest is an external enrich adapter declaring entity_type "*"
// (SPEC §6): its steps take the pipeline's entity type.
const anyEntityManifest = `{
  "id": "any-entity",
  "version": 1,
  "role": "enrich",
  "entity_type": "*",
  "needs": {"type":"object","additionalProperties":true},
  "provides": {"type":"object","additionalProperties":false,"properties":{"headline":{"type":"string"}}}
}`

// TestEntityAgnosticManifestTakesThePipelinesType: "*" is not an AI-only
// trick — any adapter may declare it. Its static provides validate against
// the pipeline's registry at plan (headline is a person field, so a company
// pipeline rejects it), and a source may not declare it at all.
func TestEntityAgnosticManifestTakesThePipelinesType(t *testing.T) {
	h := newHarness(t)
	h.writeAdapter("any-entity", anyEntityManifest, echoAdapterScript)
	h.write("people.csv", peopleCSV)
	h.write("companies.csv", companiesCSV)

	h.write("person.yaml", `name: p
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: any
    use: any-entity
`)
	plan := h.mustRun("plan", "person.yaml")
	contains(t, plan.stderr, "2. any [enrich] — any-entity@1 (external:", "plan output")
	contains(t, plan.stderr, "entity:    person", "the step takes the pipeline's entity type")

	h.write("company.yaml", `name: c
source:
  use: csv/source
  with:
    path: companies.csv
    entity_type: company
steps:
  - id: any
    use: any-entity
`)
	res := h.run("plan", "company.yaml")
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `manifest provides: "headline" is not a canonical company field`, "static provides validate against the pipeline's type")

	h.writeAdapter("any-source", strings.NewReplacer(`"any-entity"`, `"any-source"`, `"enrich"`, `"source"`).Replace(anyEntityManifest), echoAdapterScript)
	h.write("source.yaml", `name: s
source:
  use: any-source
`)
	res = h.run("plan", "source.yaml")
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `any-source declares entity_type "*" and cannot be the source`, "stderr")
}
