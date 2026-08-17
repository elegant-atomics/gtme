package e2e

import (
	"encoding/json"
	"testing"
)

const helpAgentPeopleCSV = `email,first_name,full_name,company_domain,linkedin_url,title
jane.doe@acme.com,Jane,Jane Doe,acme.com,https://www.linkedin.com/in/jane-doe/,VP Marketing
`

// TestHelpAgentSurface checks the shape SPEC §8 / ADR-007 require: every verb,
// every installed adapter's manifest, and exactly 3 canonical examples.
func TestHelpAgentSurface(t *testing.T) {
	h := newHarness(t)
	res := h.mustRun("help", "--agent")

	var doc struct {
		Verbs []struct {
			Usage string `json:"usage"`
			Does  string `json:"does"`
		} `json:"verbs"`
		Adapters []struct {
			ID       string          `json:"id"`
			Role     string          `json:"role"`
			Needs    json.RawMessage `json:"needs"`
			Provides json.RawMessage `json:"provides"`
		} `json:"adapters"`
		Examples []struct {
			Name string `json:"name"`
			Yaml string `json:"yaml"`
		} `json:"examples"`
	}
	if err := json.Unmarshal([]byte(res.stdout), &doc); err != nil {
		t.Fatalf("help --agent output must be JSON: %v\n%s", err, res.stdout)
	}

	if len(doc.Examples) != 3 {
		t.Errorf("examples = %d, want exactly 3", len(doc.Examples))
	}

	haveVerb := map[string]bool{}
	for _, v := range doc.Verbs {
		haveVerb[v.Usage] = true
	}
	for _, want := range []string{"gtme init", "gtme plan pipeline.yaml"} {
		found := false
		for u := range haveVerb {
			if u == want {
				found = true
			}
		}
		if !found {
			t.Errorf("verbs missing %q; got %+v", want, doc.Verbs)
		}
	}

	haveAdapter := map[string]string{} // id -> role
	for _, a := range doc.Adapters {
		haveAdapter[a.ID] = a.Role
		// Sources receive no RECORDs (SPEC §5), so they legitimately have no
		// needs schema; every other role does.
		if a.Role != "source" && len(a.Needs) == 0 {
			t.Errorf("adapter %s (role %s) has no needs schema in the doc", a.ID, a.Role)
		}
	}
	for id, role := range map[string]string{
		"csv/source": "source", "ai/filter": "filter", "ai/compose": "compose",
		"harvest/profile": "enrich", "instantly/add-to-campaign": "deliver",
	} {
		if got, ok := haveAdapter[id]; !ok {
			t.Errorf("adapters missing %s", id)
		} else if got != role {
			t.Errorf("%s role = %q, want %q", id, got, role)
		}
	}
}

// TestHelpAgentExamplesPassPlan is the doc's own acceptance criterion (SPEC
// §8): every example it prints must be a pipeline `gtme plan` accepts, given
// only the credentials a real operator would set — nothing about the example
// itself should be broken or aspirational.
func TestHelpAgentExamplesPassPlan(t *testing.T) {
	h := newHarness(t)
	res := h.mustRun("help", "--agent")

	var doc struct {
		Examples []struct {
			Name string `json:"name"`
			Yaml string `json:"yaml"`
		} `json:"examples"`
	}
	if err := json.Unmarshal([]byte(res.stdout), &doc); err != nil {
		t.Fatalf("help --agent output must be JSON: %v", err)
	}
	if len(doc.Examples) == 0 {
		t.Fatal("no examples to check")
	}

	// The CSV-sourced examples' deliver step needs first_name, which the
	// shared peopleCSV fixture (used elsewhere for identity/cache tests) does
	// not carry — csv/source's provides schema is exactly the probed header
	// (a DECISIONS.md rule), so plan-checking these examples for real needs a
	// header that matches what they actually require.
	h.write("people.csv", helpAgentPeopleCSV)
	creds := []string{
		"APOLLO_API_KEY=fixture", "HARVEST_API_KEY=fixture", "INSTANTLY_API_KEY=fixture",
	}
	for _, ex := range doc.Examples {
		path := h.write(ex.Name+".yaml", ex.Yaml)
		plan := h.runWithEnv(creds, "", "plan", path)
		if plan.code != 0 {
			t.Errorf("example %q failed gtme plan (exit %d):\n%s", ex.Name, plan.code, plan.stderr)
		}
	}
}
