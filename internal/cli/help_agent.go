package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/ledger"
)

// cmdHelpAgent emits the full CLI + adapter surface as one compact,
// machine-readable document (SPEC §8, DECISIONS.md ADR-007), regenerated from
// the live verb table below and the installed adapter registry — never
// hand-maintained by a human. Acceptance criterion (SPEC §8): this document
// alone must be enough for an agent to author a pipeline.yaml that passes
// `gtme plan`, so every field an agent would need to make that decision
// (needs/provides/config_schema/credentials) is here, not just names.
func cmdHelpAgent(env Env) error {
	doc := agentDoc{
		Verbs:        agentVerbs,
		Adapters:     agentAdapters(),
		SQLSteps:     agentSQLSteps,
		Examples:     agentExamples,
		Ledger:       agentLedger(),
		Bindings:     agentBindingsPointer,
		Participants: agentParticipantDoc,
	}
	enc := json.NewEncoder(env.Stdout)
	if err := enc.Encode(doc); err != nil {
		return fail(ExitOther, "writing surface doc: %v", err)
	}
	return nil
}

type agentDoc struct {
	Verbs    []agentVerb    `json:"verbs"`
	Adapters []agentAdapter `json:"adapters"`
	// SQLSteps are the runner-owned steps (SPEC §10a) `gtme plan` resolves
	// beside the adapters above; they carry no manifest, so the listing is
	// hand-shaped here (the round-trip agent found them only through the
	// ledger notes; both surfaces now name them).
	SQLSteps []agentVerb    `json:"sql_steps"`
	Examples []agentExample `json:"examples"`
	// Ledger is the public read surface (SPEC §3) and the canonical query
	// shapes (ADR-037), so an agent can write a sql/* step or a {query:}
	// value without reading spec/ledger.sql.
	Ledger agentLedgerDoc `json:"ledger"`
	// Bindings is the one pointer SPEC §8 allows this document to carry about
	// the binding contract (ADR-041): the contract itself lives in
	// `gtme help --bindings`, so the pipeline document stays short.
	Bindings agentPointer `json:"bindings"`
	// Participants is the answer rhythm and its consequences (ADR-049) —
	// the one part of the surface an agent drives rather than declares.
	Participants agentParticipants `json:"participants"`
}

// agentParticipants documents how an agent answers a step addressed to it.
type agentParticipants struct {
	Note     string   `json:"note"`
	Rhythm   []string `json:"rhythm"`
	KnowAlso []string `json:"know_also"`
}

// agentParticipantDoc is the rhythm an agent follows when a pipeline hands it
// a judgment, and the nuances each of which someone will otherwise trip on
// (ADR-049's consequences).
var agentParticipantDoc = agentParticipants{
	Note: "A human/* or agent/* step is a participant step: it opens no adapter session, and its answer is yours to record. Use agent/* for a step you answer yourself and human/* for one a person answers; the pipeline file says whose work it is, and `gtme runs` says who is awaited. Under `--simulate` a participant step is a simulation gap — there is no prompt to script and no person to rehearse.",
	Rhythm: []string{
		"1. `gtme run pipeline.yaml` — the step pends every record it reaches and the run ends `pending`; the receipt names the count, the participant and this verb.",
		"2. `gtme show --run last --pending [STEP]` — read the waiting records and the surface each is shown. stdout is NDJSON: identity_key, surface, outputs.",
		"3. `gtme answer last [STEP] <identity-key> --set field=value` — once per record, validated on the spot. Add `--as <name>` under an agent/* step so the ledger names you, `--cost` if you spent anything, `--note` for why.",
		"4. `gtme run pipeline.yaml` again — collection reads your answers instead of opening a session, and the records continue to the next stage. Unanswered records stay pending and the run stays pending.",
	},
	KnowAlso: []string{
		"A cron pipeline with a participant step stalls until it is answered: a pending run is resumed, not re-sourced, so nothing new is picked up meanwhile. The pattern that avoids it is two pipelines — the reviewing one a person runs, ending in a group:; the cron one sourcing from that group (see the `human-review-then-cron` example).",
		"Ctrl-C during an in-run walk leaves the rest pending; the run ends pending, not failed.",
		"Answers are idempotent per (run, step, record): answering again before collection replaces the earlier answer, and the latest one wins.",
		"A judgment is remembered like a model's: an unchanged value is not asked again, whoever answers next. `cache: 0d` or `respend: true` ask again on purpose.",
		"A review labels one value and never gates — `when: <review>.passed` fails plan. Use a filter to gate.",
		"`of:` names the value a review or an edit is about; its current value is part of the cache key, so a rewritten draft comes back and an unchanged one does not.",
	},
}

// agentPointer names another surface and what it is for.
type agentPointer struct {
	See  string `json:"see"`
	Does string `json:"does"`
}

var agentBindingsPointer = agentPointer{
	See:  "gtme help --bindings",
	Does: "when the adapters listed here do not cover an API you need: the binding contract — the schema a binding.yaml validates against, the discovery path (~/.gtme/adapters/<id, slashes → dashes>/binding.yaml), one reference binding verbatim, and the fixtures expectation. A binding is declarative YAML the engine interprets; `gtme plan` resolves it like a built-in. This document carries none of that contract.",
}

type agentVerb struct {
	Usage string `json:"usage"`
	Does  string `json:"does"`
}

// agentVerbs is the entire v0 verb set (SPEC §8, ADR-005) — kept next to
// usage() deliberately, so a change to one is a visible diff away from the
// other, even though nothing enforces they stay in sync mechanically.
var agentVerbs = []agentVerb{
	{"gtme init", "create ~/.gtme and the ledger"},
	{"gtme secret set KEY [VALUE]", "store a credential in ~/.gtme/secrets (VALUE omitted = prompt, no echo)"},
	{"gtme plan pipeline.yaml", "resolve adapters, validate every step's needs/uses and credentials, print the plan — no network, no spend"},
	{"gtme run pipeline.yaml [--resume RUN_ID] [--dry-run] [--simulate]", "execute a pipeline; --resume continues a run that stopped partway; --dry-run holds deliver steps back and receipts their resolved variables instead of sending; --simulate executes everything offline from fixtures (no network, no spend, nothing persists)"},
	{"gtme query \"SQL\" [--save NAME] [--name NAME] [--list] [--format ndjson|table|csv] [--limit N]", "read-only SQL against the ledger; --save stores it as a named segment"},
	{"gtme show <identity-key> [--fields a,b] [--provenance]", "print the current-value projection for one identity"},
	{"gtme show --run RUN_ID|last [--fields a,b] [--provenance] [--limit N]", "list the records a run touched"},
	{"gtme show --run RUN_ID|last --pending [STEP]", "the records awaiting a participant with the surface each is shown, as text on stderr and NDJSON on stdout — what an agent reads before it answers (ADR-049)"},
	{"gtme answer [RUN_ID|last|PIPELINE] [STEP] [IDENTITY_KEY] [--set field=value ...] [--as NAME] [--cost USD [--measured]] [--note TEXT]", "record a participant's answer for one pending human/* or agent/* step: a filter takes pass=true|false and reason, a compose or review its declared fields; the value is validated against the step's declared outputs and refused naming them otherwise. Writes an `answered` event and nothing else — it never sends. STEP may be omitted when one step is pending; with no identity key and a terminal it walks the pending records. --as names the participant (default the OS user; the human/ or agent/ prefix follows the adapter), --cost records what the participant spent (estimated unless --measured), --note is free text kept with the answer (never part of a cache key)"},
	{"gtme runs [RUN_ID|last]", "list runs, or print one run's receipt (records/cost per step)"},
	{"gtme freeze [RUN_ID|last] [--bundle DIR]", "print the pipeline.yaml that produced a run, reconstructed from its stored config; --bundle assembles a portable campaign bundle instead (pipeline + referenced bindings with fixtures + registry slice + hash manifest), which `gtme run` accepts wherever it accepts a pipeline path (ADR-029)"},
	{"gtme groups [show NAME | add NAME KEY...|--from-segment NAME|--query SQL | remove NAME KEY... [--note TEXT]]", "list groups with derived character (members, added/removed/touched tallies), inspect one, or hand-edit membership; snapshots evaluate a segment or SQL into extensional membership with provenance (ADR-021); --note records a removal's reason (ADR-032)"},
	{"gtme vacuum", "evict expired payloads from the ADR-030 cache tier — and nothing else; facts are append-only forever (SPEC §8)"},
	{"gtme adapters", "list installed adapters with their source and pin (.source.json)"},
	{"gtme adapters search TEXT", "search the bindings registry index by id, vendor, description and role (GTME_REGISTRY overrides the index URL)"},
	{"gtme adapters add github.com/<owner>/<repo>/<path>[@ref]", "fetch a binding at a pinned ref, verify it (schema + fixtures offline; nothing installs unverified), install it under ~/.gtme/adapters/ with .source.json beside it"},
	{"gtme adapters verify ID", "validate an installed binding against the schema, run its fixtures offline, print the hosts it will call and the credentials it will demand"},
	{"gtme adapters update ID [@ref]", "re-fetch at a newer ref — the only thing that moves a pin"},
	{"gtme help --agent", "print this document"},
	{"gtme help --bindings", "print the binding contract — schema, discovery path, a reference binding, the fixtures expectation — for authoring an adapter this binary does not ship (see `bindings` below)"},
}

// agentSQLSteps describe the deterministic transform floor (ADR-027): no
// adapter, no wire protocol — one read-only, timeboxed SELECT per step,
// scoped to the run's eligible records, its output registry-checked and
// provenance-stamped like adapter output. The query shapes below (ledger
// section) are written for these.
var agentSQLSteps = []agentVerb{
	{"use: sql/filter — with: {query: \"SELECT identity_id, pass[, reason] FROM ...\"}", "runner-owned filter: rows decide pass/fail per eligible record (a missing row, or pass=0, fails it with the reason recorded); read-only against the §3 surface, $0"},
	{"use: sql/transform — with: {query: \"SELECT identity_id, <expr> AS \\\"ns.field\\\" ...\"}", "runner-owned derivation: result columns append like adapter output (registry-checked, provenance `sql/transform @ <query-hash>`); declare provides: [ns.field]; cross-record aggregates and fan-in live here"},
}

type agentAdapter struct {
	ID                  string          `json:"id"`
	Version             int             `json:"version"`
	Role                string          `json:"role"`
	EntityType          string          `json:"entity_type"`
	Needs               json.RawMessage `json:"needs,omitempty"`
	Provides            json.RawMessage `json:"provides,omitempty"`
	Credentials         []string        `json:"credentials,omitempty"`
	CredentialsOptional []string        `json:"credentials_optional,omitempty"`
	ConfigSchema        json.RawMessage `json:"config_schema,omitempty"`
	FreshnessDays       int             `json:"freshness_days,omitempty"`
	CostEstimateUSD     *float64        `json:"cost_estimate_usd,omitempty"`
	Attests             bool            `json:"attests,omitempty"`
}

// agentAdapters reads the live registry (built-ins + anything on
// GTME_ADAPTER_PATH / ~/.gtme/adapters), so this document always matches what
// `gtme plan` will actually resolve.
func agentAdapters() []agentAdapter {
	installed := adapters.Installed()
	out := make([]agentAdapter, 0, len(installed))
	for _, m := range installed {
		out = append(out, agentAdapter{
			ID:                  m.ID,
			Version:             m.Version,
			Role:                m.Role,
			EntityType:          m.EntityType,
			Needs:               m.Needs,
			Provides:            m.Provides,
			Credentials:         m.Credentials,
			CredentialsOptional: m.CredentialsOptional,
			ConfigSchema:        m.ConfigSchema,
			FreshnessDays:       m.FreshnessDays,
			CostEstimateUSD:     m.CostEstimate,
			Attests:             m.Attests,
		})
	}
	return out
}

type agentExample struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Yaml        string `json:"yaml"`
}

// agentExamples are the 3 canonical examples SPEC §8 requires: minimal and
// offline, the full real-provider funnel with uses: on a filter step, and a
// CSV-sourced compose-only funnel with uses: on a compose step — chosen so an
// agent sees uses: on both AI-backed roles and a source other than
// apollo/search. Every adapter named here is a real, implemented v0 adapter,
// because this document's own acceptance criterion is that an agent can use
// it alone to write a pipeline that passes `gtme plan` — an example that
// doesn't itself pass plan would contradict that.
var agentExamples = []agentExample{
	{
		Name:        "minimal-offline",
		Description: "csv/source into a cached enrich step; runs with zero API keys (README quickstart).",
		Yaml: `name: hello-gtme
version: 1
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: score
    use: mock-enrich-py
    cache: 30d
`,
	},
	{
		Name:        "full-funnel-with-uses",
		Description: "source (masked) -> ai/filter (uses:, provides:) -> apollo/enrich (reveal past the filter, ADR-043) -> enrich -> ai/compose (uses:) -> deliver, the shape in SPEC.md §9. provides: (ADR-033) declares the judgment fields the filter stores beside its verdict, namespaced by pipeline (apollo-to-instantly.fit) unless marked canonical: true.",
		Yaml: `name: apollo-to-instantly
version: 1
source:
  use: apollo/search
  with:
    query: "vp marketing, saas, 50-200 employees"
    limit: 500
steps:
  - id: icp-filter
    use: ai/filter
    uses: [first_name, title, company_name]
    provides:
      fit: {enum: [strong, weak]}
      rationale: {}
    with:
      prompt: >
        Keep only contacts likely to own outbound tooling decisions,
        and say how strong the fit is.
      batch_size: 25
  - id: reveal
    use: apollo/enrich
    when: icp-filter.passed
    cache: 30d
  - id: linkedin
    use: harvest/profile
    when: icp-filter.passed
    cache: 30d
  - id: personalize
    use: ai/compose
    uses: [recent_posts, role_history]
    with:
      prompt: >
        Write first_line and ps_line using recent_posts and role_history.
      batch_size: 25
  - id: send
    use: instantly/add-to-campaign
    with:
      campaign: "Q3 VP Marketing"
    variables:
      first_name: first_name
      personalization: first_line
      ps_line: ps_line
    idempotency: email
`,
	},
	{
		Name:        "human-review-then-cron",
		Description: "The routing pattern for a participant step (ADR-049). A pipeline with a human step waits for its person, so keep the waiting out of cron: the reviewing pipeline is one a person runs and it ends in a group:, and the scheduled pipeline sources from that group and never waits. Two files, shown in order.",
		Yaml: `# review.yaml — run by a person; ends in a group, nothing scheduled.
name: review
version: 1
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: draft
    use: ai/compose
    uses: [full_name, title]
    provides: [first_line]
    with:
      prompt: Write one opening line.
  - id: grade
    use: human/review
    of: review.first_line
    provides:
      grade: {enum: [A, B, C, D, F]}
    with:
      render:
        template: "{{full_name}} ({{title}})\n{{review.first_line}}"
  - id: approved
    use: group/deliver
    with:
      group: approved
    idempotency: email
---
# send.yaml — the scheduled half; sources the group, waits for nobody.
name: send
version: 1
source:
  group: approved
steps:
  - id: send
    use: instantly/add-to-campaign
    with:
      campaign: "Q3 reviewed"
    variables:
      first_name: full_name
      personalization: review.first_line
    idempotency: email
`,
	},
	{
		Name:        "csv-sourced-compose-with-uses",
		Description: "csv/source -> cached enrich -> ai/compose (uses: on a compose step, not a filter), no AI filter in the chain.",
		Yaml: `name: csv-personalize
version: 1
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: linkedin
    use: harvest/profile
    cache: 30d
  - id: personalize
    use: ai/compose
    uses: [recent_posts, role_history]
    with:
      prompt: >
        Write first_line and ps_line using recent_posts and role_history.
      batch_size: 25
  - id: send
    use: instantly/add-to-campaign
    with:
      campaign: "CSV import"
    variables:
      first_name: first_name
      personalization: first_line
      ps_line: ps_line
    on_missing: skip
    idempotency: email
`,
	},
}

type agentLedgerDoc struct {
	Note    string             `json:"note"`
	Objects []agentLedgerObj   `json:"objects"`
	Shapes  []agentQueryShape  `json:"query_shapes"`
	Values  []agentConfigValue `json:"config_values"`
}

type agentLedgerObj struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"` // table | view
	Columns []string `json:"columns"`
	Does    string   `json:"does"`
}

type agentQueryShape struct {
	Name string `json:"name"`
	Use  string `json:"use"`
	SQL  string `json:"sql"`
}

type agentConfigValue struct {
	Form string `json:"form"`
	Does string `json:"does"`
}

// ledgerObjectNotes explains each public object in one line; an object the
// migrations create that is not listed here is implementation-only (a
// DECISIONS.md entry records why) and is left out of the doc.
var ledgerObjectNotes = map[string]string{
	"identities":        "one row per person/company; identity_key per SPEC §4",
	"field_values":      "append-only facts: (identity_id, field, value JSON, source, confidence, run_id, created_at)",
	"relations":         "typed edges between identities, e.g. works_at (person → company)",
	"runs":              "one row per gtme run; config_json is the resolved pipeline",
	"run_records":       "a run's membership: state = last completed step id, verdicts = {step_id: pass|fail}",
	"step_events":       "per-record trail: claimed|done|failed|skipped_cache|dry_run|simulated, detail JSON",
	"costs":             "spend per step/record; basis measured|estimated (ADR-046: measured only when read back from vendor-reported cost metadata; a rate multiplied out is estimated)",
	"deliveries":        "sends and handoffs, UNIQUE(target, scope, idempotency) — scope is the resolved idempotency_scope config value (ADR-044, '' = unscoped); status accepted|confirmed|contradicted|sent; variables_hash drives redeliver: on_change (ADR-045)",
	"groups":            "named associations (ADR-021); character derived from events",
	"group_events":      "append-only added|removed|touched events per (group, identity)",
	"payloads":          "raw vendor responses, a purgeable cache tier (ADR-030) — never facts",
	"field_value_ranks": "field_values ranked per (identity, field): confidence DESC, newest first",
	"current_fields":    "rank-1 field_values — the current value per (identity, field), value still JSON-encoded",
	"current_values":    "current_fields with value unwrapped from JSON — the view to query for plain values",
	"group_members":     "current membership as (group_id, identity_id)",
	"group_membership":  "current membership keyed by group_name — the view to query for membership",
}

// agentQueryShapes are the three canonical shapes SPEC §8 asks for: a
// cross-type membership gate, a cross-record aggregate, a config-value query.
var agentQueryShapes = []agentQueryShape{
	{
		Name: "cross-type membership gate (sql/filter)",
		Use:  "keep people whose company is in a group — membership by ledger facts, via relations",
		SQL: `SELECT r.from_id AS identity_id
FROM relations r
JOIN group_membership gm ON gm.identity_id = r.to_id AND gm.group_name = 'target-accounts'
WHERE r.relation = 'works_at'`,
	},
	{
		Name: "cross-record aggregate (sql/transform, company pipeline)",
		Use:  "fan-in: count a company's known people into a field; declare provides: [acct.people_count]",
		SQL: `SELECT r.to_id AS identity_id, count(*) AS "acct.people_count"
FROM relations r
WHERE r.relation = 'works_at'
GROUP BY r.to_id`,
	},
	{
		Name: "per-record derivation (sql/transform)",
		Use:  "derive one field from another; declare provides: [sql.shout]",
		SQL: `SELECT identity_id, upper(value) AS "sql.shout"
FROM current_values WHERE field = 'full_name'`,
	},
	{
		Name: "config value (one column → a list)",
		Use:  "with: {domains: {query: ...}} — the list a source is parameterised by; zero rows fails plan",
		SQL: `SELECT DISTINCT v.value
FROM current_values v
JOIN group_membership gm ON gm.identity_id = v.identity_id AND gm.group_name = 'qualified'
WHERE v.field = 'company_domain'`,
	},
}

var agentConfigValues = []agentConfigValue{
	{"{query: \"SELECT ...\"}", "any value under with: — resolved read-only at plan and at run start; one column → a list, one row and one column → a scalar; zero rows is a plan error; the resolved rows print in the plan and land in runs.config_json (SPEC §7, §9)"},
	{"{segment: NAME}", "the same, reading a segment saved with `gtme query --save NAME \"SQL\"` — a live computed list that may drift between plan and arm; snapshot into a group (`gtme groups add NAME --from-segment SEG`) when the list is a reviewed decision"},
}

// agentLedger renders the public read surface from a freshly migrated
// throwaway ledger, so the columns are what the binary's migrations
// actually build — never hand-maintained.
func agentLedger() agentLedgerDoc {
	doc := agentLedgerDoc{
		Note:   "Read-only SQL (sql/transform, sql/filter, {query:} values, gtme query) is written against these. Prefer current_values and group_membership; the raw tables are for provenance. A sql/* step's result must carry identity_id; :run_id is bound when referenced.",
		Shapes: agentQueryShapes,
		Values: agentConfigValues,
	}
	dir, err := os.MkdirTemp("", "gtme-help-agent-*")
	if err != nil {
		return doc
	}
	defer os.RemoveAll(dir)
	ctx := context.Background()
	l, err := ledger.Open(ctx, filepath.Join(dir, "ledger.db"))
	if err != nil {
		return doc
	}
	defer l.Close()
	// The ledger holds one connection: list the objects first, then read
	// each one's columns — never a nested query while a result set is open.
	rows, err := l.DB().QueryContext(ctx, `SELECT type, name FROM sqlite_master
		WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite_%' ORDER BY type DESC, name`)
	if err != nil {
		return doc
	}
	var objects []agentLedgerObj
	for rows.Next() {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			rows.Close()
			return doc
		}
		if does, public := ledgerObjectNotes[name]; public {
			objects = append(objects, agentLedgerObj{Name: name, Kind: kind, Does: does})
		}
	}
	rows.Close()
	for i := range objects {
		cols, err := l.DB().QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, objects[i].Name)
		if err != nil {
			return doc
		}
		for cols.Next() {
			var c string
			if err := cols.Scan(&c); err == nil {
				objects[i].Columns = append(objects[i].Columns, c)
			}
		}
		cols.Close()
	}
	doc.Objects = objects
	return doc
}
