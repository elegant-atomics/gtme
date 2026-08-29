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
		Verbs:    agentVerbs,
		Adapters: agentAdapters(),
		Examples: agentExamples,
		Ledger:   agentLedger(),
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
	Examples []agentExample `json:"examples"`
	// Ledger is the public read surface (SPEC §3) and the canonical query
	// shapes (ADR-037), so an agent can write a sql/* step or a {query:}
	// value without reading spec/ledger.sql.
	Ledger agentLedgerDoc `json:"ledger"`
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
	{"gtme runs [RUN_ID|last]", "list runs, or print one run's receipt (records/cost per step)"},
	{"gtme freeze [RUN_ID|last] [--bundle DIR]", "print the pipeline.yaml that produced a run, reconstructed from its stored config; --bundle assembles a portable campaign bundle instead (pipeline + referenced bindings with fixtures + registry slice + hash manifest), which `gtme run` accepts wherever it accepts a pipeline path (ADR-029)"},
	{"gtme groups [show NAME | add NAME KEY...|--from-segment NAME|--query SQL | remove NAME KEY... [--note TEXT]]", "list groups with derived character (members, added/removed/touched tallies), inspect one, or hand-edit membership; snapshots evaluate a segment or SQL into extensional membership with provenance (ADR-021); --note records a removal's reason (ADR-032)"},
	{"gtme vacuum", "evict expired payloads from the ADR-030 cache tier — and nothing else; facts are append-only forever (SPEC §8)"},
	{"gtme help --agent", "print this document"},
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
		Description: "source -> ai/filter (uses:) -> enrich -> ai/compose (uses:) -> deliver, the shape in SPEC.md §9.",
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
    uses: [full_name, title, company_domain]
    with:
      prompt: >
        Keep only contacts likely to own outbound tooling decisions.
      batch_size: 25
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
	"costs":             "spend per step/record",
	"deliveries":        "sends and handoffs, UNIQUE(target, idempotency); status accepted|confirmed|contradicted|sent",
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
