package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elegant-atomics/gtme/internal/binding"
)

// bindingsDoc is the shape `gtme help --bindings` prints (SPEC §8, ADR-041):
// the second agent surface, for an agent that needs an adapter gtme does not
// ship. Only the fields the acceptance criterion reads are decoded here.
type bindingsDoc struct {
	Note      string `json:"note"`
	Discovery struct {
		Path       string   `json:"path"`
		SearchPath []string `json:"search_path"`
	} `json:"discovery"`
	Fixtures struct {
		File string `json:"file"`
		Does string `json:"does"`
	} `json:"fixtures"`
	Reference struct {
		ID          string   `json:"id"`
		Role        string   `json:"role"`
		Directory   string   `json:"directory"`
		Credentials []string `json:"credentials"`
		BindingYAML string   `json:"binding_yaml"`
		Conformance string   `json:"conformance_json"`
	} `json:"reference"`
	Verbs []struct {
		Usage string `json:"usage"`
		Does  string `json:"does"`
	} `json:"verbs"`
	Schema json.RawMessage `json:"schema"`
}

func helpBindings(t *testing.T, h *harness) (bindingsDoc, string) {
	t.Helper()
	res := h.mustRun("help", "--bindings")
	if res.stderr != "" {
		t.Errorf("help --bindings wrote to stderr (stdout is the document):\n%s", res.stderr)
	}
	var doc bindingsDoc
	if err := json.Unmarshal([]byte(res.stdout), &doc); err != nil {
		t.Fatalf("help --bindings output must be JSON: %v\n%s", err, res.stdout)
	}
	return doc, res.stdout
}

// TestHelpBindingsSchemaIsTheArtifact: the printed schema equals
// spec/binding-schema.json byte for byte (SPEC §11 M18) — the document is
// regenerated from the embedded artifact, never a hand-maintained copy. The
// file's trailing newline is the one byte a JSON value cannot carry.
func TestHelpBindingsSchemaIsTheArtifact(t *testing.T) {
	h := newHarness(t)
	doc, _ := helpBindings(t, h)

	want, err := os.ReadFile(filepath.Join(repoRoot(), "spec", "binding-schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(doc.Schema), bytes.TrimSpace(want)) {
		t.Errorf("schema in help --bindings differs from spec/binding-schema.json\n--- printed (%d bytes)\n%.300s\n--- file (%d bytes)\n%.300s",
			len(doc.Schema), doc.Schema, len(want), want)
	}
}

// TestHelpBindingsNamesThePathAndTheFixtures: the text names the discovery
// path, the environment that alters it, the fixtures file and what serves it
// (SPEC §11 M18) — an agent reading only this document must learn where a
// binding goes and what must sit beside it.
func TestHelpBindingsNamesThePathAndTheFixtures(t *testing.T) {
	h := newHarness(t)
	doc, raw := helpBindings(t, h)

	contains(t, doc.Discovery.Path, "~/.gtme/adapters/", "discovery.path")
	contains(t, doc.Discovery.Path, "binding.yaml", "discovery.path")
	contains(t, raw, "GTME_ADAPTER_PATH", "help --bindings")
	if len(doc.Discovery.SearchPath) == 0 {
		t.Error("discovery.search_path should list the live directories gtme will search")
	}
	if doc.Fixtures.File != "fixtures/conformance.json" {
		t.Errorf("fixtures.file = %q, want fixtures/conformance.json", doc.Fixtures.File)
	}
	contains(t, doc.Fixtures.Does, "--simulate", "fixtures.does")
	if doc.Reference.Directory != strings.ReplaceAll(doc.Reference.ID, "/", "-") {
		t.Errorf("reference.directory = %q, want the id %q with slashes → dashes", doc.Reference.Directory, doc.Reference.ID)
	}
	if !strings.Contains(doc.Reference.BindingYAML, "id: "+doc.Reference.ID) {
		t.Errorf("reference.binding_yaml should be the %s document verbatim", doc.Reference.ID)
	}
	haveVerb := map[string]bool{}
	for _, v := range doc.Verbs {
		haveVerb[v.Usage] = true
	}
	for _, want := range []string{"gtme plan pipeline.yaml", "gtme run pipeline.yaml --simulate", "gtme help --bindings"} {
		if !haveVerb[want] {
			t.Errorf("verbs missing %q; got %+v", want, doc.Verbs)
		}
	}
}

// TestHelpBindingsReferenceRoundTrips is the document's own acceptance
// criterion (SPEC §8, ADR-041): the reference binding it prints validates
// against the schema it prints, and — installed on the discovery path it
// names, under the directory it names — resolves through `gtme plan`. The
// reference is a shipped built-in, so it is installed under a shifted vendor
// prefix (as binding_twin_test does) to prove the path resolved it, not the
// binary.
func TestHelpBindingsReferenceRoundTrips(t *testing.T) {
	h := newHarness(t)
	doc, _ := helpBindings(t, h)

	if _, err := binding.Parse([]byte(doc.Reference.BindingYAML)); err != nil {
		t.Fatalf("the printed reference binding does not validate: %v", err)
	}

	vendor, op, ok := strings.Cut(doc.Reference.ID, "/")
	if !ok {
		t.Fatalf("reference id %q is not vendor/op", doc.Reference.ID)
	}
	newID := vendor + "x/" + op
	dir := filepath.Join(h.home, ".gtme", "adapters", strings.ReplaceAll(newID, "/", "-"))
	if err := os.MkdirAll(filepath.Join(dir, "fixtures"), 0o755); err != nil {
		t.Fatal(err)
	}
	yamlDoc := strings.Replace(doc.Reference.BindingYAML, "id: "+doc.Reference.ID, "id: "+newID, 1)
	if err := os.WriteFile(filepath.Join(dir, "binding.yaml"), []byte(yamlDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "conformance.json"), []byte(doc.Reference.Conformance), 0o644); err != nil {
		t.Fatal(err)
	}

	var pipeline string
	switch doc.Reference.Role {
	case "source":
		// A source's required config is its own (the reference's asks for a
		// search); plan validates it, so supply the minimum.
		pipeline = "name: ref\nsource:\n  use: " + newID + "\n  with:\n    query: vp marketing\nsteps: []\n"
	case "deliver":
		pipeline = "name: ref\nsource:\n  use: csv/source\n  with:\n    path: people.csv\nsteps:\n  - id: deliver\n    use: " + newID + "\n    variables:\n      name: full_name\n    idempotency: email\n"
	default:
		pipeline = "name: ref\nsource:\n  use: csv/source\n  with:\n    path: people.csv\nsteps:\n  - id: step\n    use: " + newID + "\n"
	}
	h.write("people.csv", helpAgentPeopleCSV)
	path := h.write("ref.yaml", pipeline)
	var creds []string
	for _, c := range doc.Reference.Credentials {
		creds = append(creds, c+"=fixture")
	}
	res := h.runWithEnv(creds, "", "plan", path)
	if res.code != 0 {
		t.Fatalf("gtme plan did not resolve the reference binding installed at %s (exit %d):\n%s", dir, res.code, res.stderr)
	}
	contains(t, res.stderr, newID, "plan output")
}

// TestHelpAgentPointsAtBindings: `help --agent` carries the pointer (SPEC §8):
// a `bindings` field and the verb, nothing more — the pipeline document does
// not carry the contract.
func TestHelpAgentPointsAtBindings(t *testing.T) {
	h := newHarness(t)
	res := h.mustRun("help", "--agent")

	var doc struct {
		Verbs []struct {
			Usage string `json:"usage"`
		} `json:"verbs"`
		Bindings struct {
			See  string `json:"see"`
			Does string `json:"does"`
		} `json:"bindings"`
	}
	if err := json.Unmarshal([]byte(res.stdout), &doc); err != nil {
		t.Fatalf("help --agent output must be JSON: %v", err)
	}
	if doc.Bindings.See != "gtme help --bindings" {
		t.Errorf("bindings.see = %q, want the verb", doc.Bindings.See)
	}
	contains(t, doc.Bindings.Does, "binding", "bindings.does")
	found := false
	for _, v := range doc.Verbs {
		if v.Usage == "gtme help --bindings" {
			found = true
		}
	}
	if !found {
		t.Error("help --agent verbs should list `gtme help --bindings`")
	}
	if strings.Contains(res.stdout, "\"$schema\"") {
		t.Error("help --agent must not carry the binding schema (SPEC §8)")
	}
}

// TestUnknownAdapterNamesTheBindingSurface: the unknown-adapter error names
// binding.yaml and the verb (SPEC §11 M18) — the round-trip agent read the
// old message as "manifest.json only".
func TestUnknownAdapterNamesTheBindingSurface(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", helpAgentPeopleCSV)
	path := h.write("p.yaml", "name: nope\nsource:\n  use: csv/source\n  with:\n    path: people.csv\nsteps:\n  - id: x\n    use: nobody/nothing\n")
	res := h.run("plan", path)
	if res.code != 2 {
		t.Errorf("exit = %d, want 2\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "binding.yaml", "stderr")
	contains(t, res.stderr, "gtme help --bindings", "stderr")
}
