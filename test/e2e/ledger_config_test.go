package e2e

// M14 step 4 acceptance (SPEC §11, ADR-037): a source with: carrying
// {query: …} plans with the rows shown and fails on zero; a sql/transform
// joining relations is annotated cross-record; a query naming an unknown
// column fails plan; the ledger read surface is in help --agent; the two
// vocabulary views read as vocabulary.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfigValuesFromTheLedger(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("mint.yaml", `name: mint
source:
  use: csv/source
  with:
    path: people.csv
`)
	h.mustRun("run", "mint.yaml")
	h.mustRun("groups", "add", "vip", "jane.doe@acme.com", "bob@globex.io")

	// A segment: the vocabulary views make it read as vocabulary — no
	// json_extract, no groups↔group_members join.
	h.mustRun("query", "--save", "vip-titles",
		`SELECT DISTINCT v.value FROM current_values v
		 JOIN group_membership gm ON gm.identity_id = v.identity_id AND gm.group_name = 'vip'
		 WHERE v.field = 'title' ORDER BY v.value`)

	// {segment:} feeds an AI step's config list; {query:} a scalar — both
	// resolved at plan, rows shown, validated against config_schema after.
	h.write("judge.yaml", `name: judge
source:
  use: csv/source
  with:
    path: {query: "SELECT 'people.csv'"}
steps:
  - id: judge
    use: ai/filter
    with:
      prompt: Keep the vip titles.
      fields: {segment: vip-titles}
`)
	plan := h.mustRun("plan", "judge.yaml")
	contains(t, plan.stderr, `with.path ← {query: SELECT 'people.csv'} → 1 row (scalar): people.csv`, "plan shows the scalar")
	contains(t, plan.stderr, `with.fields ← {segment: vip-titles} → 2 rows (list): Head of Growth, VP Marketing`, "plan shows the list")

	env := h.fixtureScript("ai.json", "$auto")
	res := h.runWithEnv(env, "", "run", "judge.yaml")
	if res.code != 0 {
		t.Fatalf("run exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "judge: 3 in, 3 out", "the resolved config ran")
	// runs.config_json records what actually ran — the resolved values.
	cfg := h.queryStrings(`SELECT config_json FROM runs WHERE pipeline = 'judge'`)
	if len(cfg) != 1 {
		t.Fatalf("runs = %d", len(cfg))
	}
	var recorded struct {
		Source struct {
			With map[string]any `json:"with"`
		} `json:"source"`
		Steps []struct {
			With map[string]any `json:"with"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(cfg[0]), &recorded); err != nil {
		t.Fatalf("config_json: %v", err)
	}
	if recorded.Source.With["path"] != "people.csv" {
		t.Errorf("recorded source path = %v, want the resolved scalar", recorded.Source.With["path"])
	}
	if fields, _ := recorded.Steps[0].With["fields"].([]any); len(fields) != 2 || fields[0] != "Head of Growth" {
		t.Errorf("recorded fields = %v, want the resolved list", recorded.Steps[0].With["fields"])
	}
	// gtme freeze reproduces the run against the resolved values.
	frozen := h.mustRun("freeze", "last")
	contains(t, frozen.stdout, "- Head of Growth", "frozen pipeline carries the resolved list")
	if strings.Contains(frozen.stdout, "segment:") {
		t.Errorf("frozen pipeline must carry values, not the segment reference:\n%s", frozen.stdout)
	}

	// Zero rows is a plan error — an empty list searches everything.
	h.write("empty.yaml", `name: empty
source:
  use: csv/source
  with:
    path: {query: "SELECT identity_key FROM identities WHERE entity_type = 'unicorn'"}
`)
	res = h.run("plan", "empty.yaml")
	if res.code != 2 {
		t.Fatalf("zero rows: exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "with.path: the query yielded zero rows", "stderr")

	// A missing segment names the fix; two columns is a shape error.
	h.write("nosegment.yaml", `name: nosegment
source:
  use: csv/source
  with:
    path: {segment: nope}
`)
	res = h.run("plan", "nosegment.yaml")
	contains(t, res.stderr, `no saved segment named "nope" — save one with `+"`gtme query --save nope", "stderr")
	h.write("twocols.yaml", `name: twocols
source:
  use: csv/source
  with:
    path: {query: "SELECT id, identity_key FROM identities"}
`)
	res = h.run("plan", "twocols.yaml")
	contains(t, res.stderr, "must yield exactly one column (got id, identity_key)", "stderr")
}

func TestSQLAtPlanExplainsAndAnnotates(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	base := `name: sqlplan
source:
  use: csv/source
  with:
    path: people.csv
steps:
%s`
	// An unknown column fails the plan, not the run.
	h.write("bad.yaml", strings.Replace(base, "%s", `  - id: derive
    use: sql/transform
    with:
      provides: [sql.x]
      query: SELECT identity_id, nonexistent AS "sql.x" FROM current_values
`, 1))
	res := h.run("plan", "bad.yaml")
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `step "derive": sql/transform: the query does not plan against the ledger`, "stderr")
	contains(t, res.stderr, "no such column: nonexistent", "stderr")
	if n := h.queryInt(`SELECT count(*) FROM runs`); n != 0 {
		t.Errorf("runs = %d, want 0 — plan does not execute", n)
	}

	// A join over relations is annotated cross-record; a per-record
	// transform over current_values is not.
	h.write("fanin.yaml", strings.Replace(base, "%s", `  - id: shout
    use: sql/transform
    with:
      provides: [sql.shout]
      query: SELECT identity_id, upper(value) AS "sql.shout" FROM current_values WHERE field = 'full_name'
  - id: colleagues
    use: sql/transform
    with:
      provides: [sql.colleagues]
      query: >
        SELECT r.from_id AS identity_id, count(*) AS "sql.colleagues"
        FROM relations r JOIN relations o ON o.to_id = r.to_id AND o.relation = 'works_at'
        WHERE r.relation = 'works_at' GROUP BY r.from_id
`, 1))
	plan := h.mustRun("plan", "fanin.yaml")
	contains(t, plan.stderr, "note:      cross-record: this query reads relations — it may read any identity in the ledger", "annotation")
	if strings.Count(plan.stderr, "cross-record:") != 1 {
		t.Errorf("only the relations join is cross-record:\n%s", plan.stderr)
	}
	res = h.mustRun("run", "fanin.yaml")
	contains(t, res.stderr, "colleagues: 3 in, 3 out", "the cross-record transform ran")
	// Every person is their company's only known person here.
	if n := h.queryInt(`SELECT count(*) FROM current_values WHERE field = 'sql.colleagues' AND value = 1`); n != 3 {
		t.Errorf("sql.colleagues = 1 rows: %d, want 3 (current_values unwraps the JSON)", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM current_values WHERE field = 'sql.shout' AND value = 'JANE DOE'`); n != 1 {
		t.Errorf("current_values must present plain values")
	}
}

func TestHelpAgentCarriesTheLedgerReadSurface(t *testing.T) {
	h := newHarness(t)
	res := h.mustRun("help", "--agent")
	var doc struct {
		Ledger struct {
			Note    string `json:"note"`
			Objects []struct {
				Name    string   `json:"name"`
				Kind    string   `json:"kind"`
				Columns []string `json:"columns"`
			} `json:"objects"`
			Shapes []struct {
				Name string `json:"name"`
				SQL  string `json:"sql"`
			} `json:"query_shapes"`
			Values []struct {
				Form string `json:"form"`
			} `json:"config_values"`
		} `json:"ledger"`
	}
	if err := json.Unmarshal([]byte(res.stdout), &doc); err != nil {
		t.Fatalf("help --agent output must be JSON: %v", err)
	}
	have := map[string][]string{}
	kinds := map[string]string{}
	for _, o := range doc.Ledger.Objects {
		have[o.Name] = o.Columns
		kinds[o.Name] = o.Kind
	}
	for name, wantCols := range map[string]string{
		"current_values":   "identity_id,field,value,source,confidence,run_id,created_at",
		"group_membership": "group_name,group_id,identity_id",
		"deliveries":       "id,identity_id,target,idempotency,run_id,created_at,status,sent_at",
		"relations":        "from_id,relation,to_id,created_at",
	} {
		if got := strings.Join(have[name], ","); got != wantCols {
			t.Errorf("%s columns = %q, want %q", name, got, wantCols)
		}
	}
	if kinds["current_values"] != "view" || kinds["deliveries"] != "table" {
		t.Errorf("kinds = %v", kinds)
	}
	for _, private := range []string{"schema_migrations", "identity_aliases", "saved_queries"} {
		if _, ok := have[private]; ok {
			t.Errorf("%s is implementation-only and must not be in the read surface", private)
		}
	}
	if len(doc.Ledger.Shapes) < 3 || len(doc.Ledger.Values) != 2 {
		t.Errorf("shapes = %d, values = %d", len(doc.Ledger.Shapes), len(doc.Ledger.Values))
	}
	// Every shape must at least plan against the ledger.
	h.write("people.csv", peopleCSV)
	for _, shape := range doc.Ledger.Shapes {
		q := h.run("query", shape.SQL)
		if q.code != 0 {
			t.Errorf("query shape %q does not run: %s", shape.Name, q.stderr)
		}
	}
}
