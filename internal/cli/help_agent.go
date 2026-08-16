package cli

import (
	"encoding/json"

	"github.com/trevorfox/gtm/internal/adapters"
)

// cmdHelpAgent emits the full CLI + adapter surface as one compact,
// machine-readable document (SPEC §8, DECISIONS.md ADR-007), regenerated from
// the live verb table below and the installed adapter registry — never
// hand-maintained by a human. Acceptance criterion (SPEC §8): this document
// alone must be enough for an agent to author a pipeline.yaml that passes
// `gtm plan`, so every field an agent would need to make that decision
// (needs/provides/config_schema/credentials) is here, not just names.
func cmdHelpAgent(env Env) error {
	doc := agentDoc{
		Verbs:    agentVerbs,
		Adapters: agentAdapters(),
		Examples: agentExamples,
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
}

type agentVerb struct {
	Usage string `json:"usage"`
	Does  string `json:"does"`
}

// agentVerbs is the entire v0 verb set (SPEC §8, ADR-005) — kept next to
// usage() deliberately, so a change to one is a visible diff away from the
// other, even though nothing enforces they stay in sync mechanically.
var agentVerbs = []agentVerb{
	{"gtm init", "create ~/.gtm and the ledger"},
	{"gtm secret set KEY [VALUE]", "store a credential in ~/.gtm/secrets (VALUE omitted = prompt, no echo)"},
	{"gtm plan pipeline.yaml", "resolve adapters, validate every step's needs/uses and credentials, print the plan — no network, no spend"},
	{"gtm run pipeline.yaml [--resume RUN_ID] [--dry-run] [--simulate]", "execute a pipeline; --resume continues a run that stopped partway; --dry-run holds deliver steps back and receipts their resolved variables instead of sending; --simulate executes everything offline from fixtures (no network, no spend, nothing persists)"},
	{"gtm query \"SQL\" [--save NAME] [--name NAME] [--list] [--format ndjson|table|csv] [--limit N]", "read-only SQL against the ledger; --save stores it as a named segment"},
	{"gtm show <identity-key> [--fields a,b] [--provenance]", "print the current-value projection for one identity"},
	{"gtm show --run RUN_ID|last [--fields a,b] [--provenance] [--limit N]", "list the records a run touched"},
	{"gtm runs [RUN_ID|last]", "list runs, or print one run's receipt (records/cost per step)"},
	{"gtm freeze [RUN_ID|last] [--bundle DIR]", "print the pipeline.yaml that produced a run, reconstructed from its stored config; --bundle assembles a portable campaign bundle instead (pipeline + referenced bindings with fixtures + registry slice + hash manifest), which `gtm run` accepts wherever it accepts a pipeline path (ADR-029)"},
	{"gtm groups [show NAME | add NAME KEY...|--from-segment NAME|--query SQL | remove NAME KEY...]", "list groups with derived character (members, added/removed/touched tallies), inspect one, or hand-edit membership; snapshots evaluate a segment or SQL into extensional membership with provenance (ADR-021)"},
	{"gtm help --agent", "print this document"},
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
// GTM_ADAPTER_PATH / ~/.gtm/adapters), so this document always matches what
// `gtm plan` will actually resolve.
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
// it alone to write a pipeline that passes `gtm plan` — an example that
// doesn't itself pass plan would contradict that.
var agentExamples = []agentExample{
	{
		Name:        "minimal-offline",
		Description: "csv/source into a cached enrich step; runs with zero API keys (README quickstart).",
		Yaml: `name: hello-gtm
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
deliver:
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
deliver:
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
