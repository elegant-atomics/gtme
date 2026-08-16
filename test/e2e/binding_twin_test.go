package e2e

// M8's receipt-diff acceptance (SPEC §11): each reference binding, run as an
// external binding adapter against the same fixture-served HTTP as its Go
// twin, produces the same identities, the same field values on the shared
// surface, and the same costs. Deliveries diff dry (SPEC §11: "dry runs where
// delivery is involved" — delivery is the gated edge, so the diff proves the
// contract surface; the conformance kit proves the constructed request).
//
// The twins keep their canonical ids, so the bindings install under shifted
// vendor prefixes (apollox/, harvestx/, instantlyx/); comparisons never key
// on adapter id.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// writeBinding installs an external binding adapter into the harness home,
// rewriting the binding's vendor prefix so it can live beside the Go twin.
func (h *harness) writeBinding(newID string, yamlPath string) {
	h.t.Helper()
	raw, err := os.ReadFile(yamlPath)
	if err != nil {
		h.t.Fatalf("reading %s: %v", yamlPath, err)
	}
	oldID := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "id: ") {
			oldID = strings.TrimSpace(strings.TrimPrefix(line, "id: "))
			break
		}
	}
	if oldID == "" {
		h.t.Fatalf("no id line in %s", yamlPath)
	}
	doc := strings.Replace(string(raw), "id: "+oldID, "id: "+newID, 1)
	dir := filepath.Join(h.home, ".gtm", "adapters", strings.ReplaceAll(newID, "/", "-"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "binding.yaml"), []byte(doc), 0o644); err != nil {
		h.t.Fatalf("write binding: %v", err)
	}
}

// ledgerFields reads every field value in a harness ledger, keyed by identity.
func ledgerFields(h *harness) map[string]map[string]string {
	rows, err := h.open().DB().Query(`
		SELECT i.entity_type || ':' || i.identity_key, f.field, f.value
		FROM field_values f JOIN identities i ON i.id = f.identity_id`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]map[string]string{}
	for rows.Next() {
		var id, field, value string
		if err := rows.Scan(&id, &field, &value); err != nil {
			h.t.Fatal(err)
		}
		if out[id] == nil {
			out[id] = map[string]string{}
		}
		out[id][field] = value
	}
	return out
}

func identityKeys(h *harness) []string {
	keys := h.queryStrings(`SELECT entity_type || ':' || identity_key FROM identities ORDER BY 1`)
	return keys
}

func costTotal(h *harness) float64 {
	rows := h.queryStrings(`SELECT CAST(total(amount_usd) AS TEXT) FROM costs`)
	if len(rows) == 0 {
		return 0
	}
	var f float64
	fmt.Sscanf(rows[0], "%g", &f)
	return f
}

func fixtureServer(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for path, file := range routes {
		body, err := os.ReadFile(filepath.Join(repoRoot(), file))
		if err != nil {
			t.Fatalf("fixture %s: %v", file, err)
		}
		b := body
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestBindingTwinApollo(t *testing.T) {
	srv := fixtureServer(t, map[string]string{
		"/api/v1/mixed_people/search": "internal/adapters/apollo/fixtures/search.json",
	})
	yaml := func(use string) string {
		return fmt.Sprintf(`name: apollo-twin
source:
  use: %s
  with:
    query: vp marketing
    base_url: %q
`, use, srv.URL)
	}

	run := func(use string, install bool) *harness {
		h := newHarness(t)
		if install {
			h.writeBinding(use, filepath.Join(repoRoot(), "spec", "bindings", "apollo-search", "binding.yaml"))
		}
		h.write("p.yaml", yaml(use))
		res := h.runWithEnv([]string{"APOLLO_API_KEY=k"}, "", "run", "p.yaml")
		if res.code != 0 {
			t.Fatalf("%s exit = %d\nstderr:\n%s", use, res.code, res.stderr)
		}
		return h
	}
	goTwin := run("apollo/search", false)
	bindTwin := run("apollox/search", true)

	if g, b := identityKeys(goTwin), identityKeys(bindTwin); !reflect.DeepEqual(g, b) {
		t.Errorf("identities differ:\n  go:      %v\n  binding: %v", g, b)
	}
	// Full field parity — every field, every value, both directions.
	if g, b := ledgerFields(goTwin), ledgerFields(bindTwin); !reflect.DeepEqual(g, b) {
		t.Errorf("fields differ:\n  go:      %v\n  binding: %v", g, b)
	}
	if g, b := costTotal(goTwin), costTotal(bindTwin); g != b {
		t.Errorf("cost totals differ: go %v, binding %v", g, b)
	}
}

func TestBindingTwinHarvest(t *testing.T) {
	srv := fixtureServer(t, map[string]string{
		"/linkedin/profile": "internal/adapters/harvest/fixtures/profile.json",
	})
	// An internal-form-only record: both twins must resolve the public
	// linkedin_url (ADR-020's recovery path) and upgrade the identity.
	csv := "Full Name,Internal\nJane Doe,https://www.linkedin.com/in/ACwAA123\n"
	yaml := func(use, extra string) string {
		return fmt.Sprintf(`name: harvest-twin
source:
  use: csv/source
  with:
    path: contacts.csv
    columns:
      full_name: Full Name
      linkedin_internal_url: Internal
steps:
  - id: enrich
    use: %s
    with:
      base_url: %q
%s`, use, srv.URL, extra)
	}

	goH := newHarness(t)
	goH.write("contacts.csv", csv)
	goH.write("p.yaml", yaml("harvest/profile", "      posts_limit: 0\n"))
	res := goH.runWithEnv([]string{"HARVEST_API_KEY=k"}, "", "run", "p.yaml")
	if res.code != 0 {
		t.Fatalf("go twin exit = %d\nstderr:\n%s", res.code, res.stderr)
	}

	bindH := newHarness(t)
	bindH.writeBinding("harvestx/profile", filepath.Join(repoRoot(), "spec", "bindings", "harvest-profile", "binding.yaml"))
	bindH.write("contacts.csv", csv)
	bindH.write("p.yaml", yaml("harvestx/profile", ""))
	res = bindH.runWithEnv([]string{"HARVEST_API_KEY=k"}, "", "run", "p.yaml")
	if res.code != 0 {
		t.Fatalf("binding twin exit = %d\nstderr:\n%s", res.code, res.stderr)
	}

	// Both twins resolved the same identity (upgraded from name-hash to the
	// public slug tier).
	if g, b := identityKeys(goH), identityKeys(bindH); !reflect.DeepEqual(g, b) {
		t.Fatalf("identities differ:\n  go:      %v\n  binding: %v", g, b)
	}

	// Shared-surface parity: every field the binding wrote, the Go twin wrote
	// with the same value. The Go twin's extras must be exactly the computed
	// tier-2 fields the binding deliberately omits (SPEC §10a graduation rule).
	gf, bf := ledgerFields(goH), ledgerFields(bindH)
	for id, bFields := range bf {
		for field, value := range bFields {
			if gf[id][field] != value {
				t.Errorf("%s %s: binding %q, go %q", id, field, value, gf[id][field])
			}
		}
	}
	extras := map[string]bool{}
	for id, gFields := range gf {
		for field := range gFields {
			if _, ok := bf[id][field]; !ok {
				extras[field] = true
			}
		}
	}
	var extraList []string
	for f := range extras {
		extraList = append(extraList, f)
	}
	sort.Strings(extraList)
	if want := []string{"role_history"}; !reflect.DeepEqual(extraList, want) {
		t.Errorf("go twin extras = %v, want exactly %v (the computed tier-2 surface)", extraList, want)
	}

	if g, b := costTotal(goH), costTotal(bindH); g != b {
		t.Errorf("cost totals differ: go %v, binding %v", g, b)
	}
}

func TestBindingTwinInstantlyDry(t *testing.T) {
	yaml := func(use string) string {
		return fmt.Sprintf(`name: instantly-twin
version: 1

source:
  use: csv/source
  with:
    path: contacts.csv
    columns:
      full_name: Full Name
      email: Email
      company_domain: Company Website

deliver:
  use: %s
  with:
    campaign: 7d467891-4257-4a62-a8b2-08d3837f5714
  variables:
    first_name: full_name
  idempotency: email
`, use)
	}

	run := func(use string, install bool) *harness {
		h := newHarness(t)
		if install {
			h.writeBinding(use, filepath.Join(repoRoot(), "spec", "bindings", "instantly-add-to-campaign", "binding.yaml"))
		}
		h.write("contacts.csv", campaignZeroCSV)
		h.write("p.yaml", yaml(use))
		res := h.runWithEnv([]string{"INSTANTLY_API_KEY=k"}, "", "run", "p.yaml", "--dry-run")
		if res.code != 0 {
			t.Fatalf("%s exit = %d\nstderr:\n%s", use, res.code, res.stderr)
		}
		return h
	}
	goTwin := run("instantly/add-to-campaign", false)
	bindTwin := run("instantlyx/add-to-campaign", true)

	// The dry-run approval artifact is identical: same resolved variables per
	// record, same on_missing holds, zero deliveries either side.
	q := `SELECT detail FROM step_events WHERE event = 'dry_run' ORDER BY detail`
	if g, b := goTwin.queryStrings(q), bindTwin.queryStrings(q); !reflect.DeepEqual(g, b) {
		t.Errorf("dry_run details differ:\n  go:      %v\n  binding: %v", g, b)
	}
	holds := `SELECT count(*) FROM run_records WHERE verdicts != '{}'`
	if g, b := goTwin.queryInt(holds), bindTwin.queryInt(holds); g != b {
		t.Errorf("on_missing holds differ: go %d, binding %d", g, b)
	}
	for name, h := range map[string]*harness{"go": goTwin, "binding": bindTwin} {
		if n := h.queryInt(`SELECT count(*) FROM deliveries`); n != 0 {
			t.Errorf("%s twin wrote %d deliveries on a dry run", name, n)
		}
	}
	if g, b := ledgerFields(goTwin), ledgerFields(bindTwin); !reflect.DeepEqual(g, b) {
		t.Errorf("fields differ:\n  go:      %v\n  binding: %v", g, b)
	}
}
