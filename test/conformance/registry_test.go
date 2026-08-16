package conformance

// The canonical field registry (SPEC §4a, ADR-017): the spec/fields/ artifacts
// validate against their own schema, the binary's embedded copy is the same
// bytes, and every manifest this repo ships — built-in, external, fixture —
// passes registry validation (enforcement layer 1).

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trevorfox/gtm/internal/adapters"
	_ "github.com/trevorfox/gtm/internal/adapters/all"
	"github.com/trevorfox/gtm/internal/registry"
	"github.com/trevorfox/gtm/spec"
)

func TestRegistryFilesValidateAgainstSchema(t *testing.T) {
	schema := compileSchema(t, "field-registry.schema.json")
	entries, err := os.ReadDir(specPath("fields"))
	if err != nil {
		t.Fatalf("reading spec/fields: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("spec/fields is empty; the registry is mandatory (SPEC §4a)")
	}
	for _, e := range entries {
		raw, err := os.ReadFile(specPath("fields", e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		if err := schema.Validate(asJSONValue(t, raw)); err != nil {
			t.Errorf("spec/fields/%s does not validate against field-registry.schema.json:\n%v", e.Name(), err)
		}
	}
}

// The embedded registry is the same files — the binary can never disagree with
// the spec/ directory it was built from.
func TestEmbeddedRegistryMatchesSpecDir(t *testing.T) {
	entries, err := os.ReadDir(specPath("fields"))
	if err != nil {
		t.Fatalf("reading spec/fields: %v", err)
	}
	for _, e := range entries {
		disk, err := os.ReadFile(specPath("fields", e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		embedded, err := spec.Fields.ReadFile("fields/" + e.Name())
		if err != nil {
			t.Fatalf("embedded fields/%s: %v", e.Name(), err)
		}
		if string(disk) != string(embedded) {
			t.Errorf("embedded fields/%s differs from spec/fields/%s — rebuild", e.Name(), e.Name())
		}
	}
}

// Enforcement layer 1 over everything this repo ships: every property named in
// a manifest's static needs/provides schemas is canonical for its entity type
// or vendor-namespaced (SPEC §4a).
func TestShippedManifestsPassRegistryValidation(t *testing.T) {
	reg, err := registry.Load()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	check := func(name string, m *adapters.Manifest) {
		for _, f := range m.NeedsFields() {
			if err := reg.ValidateName(m.EntityType, f); err != nil {
				t.Errorf("%s needs: %v", name, err)
			}
		}
		for _, f := range m.ProvidesFields() {
			if err := reg.ValidateName(m.EntityType, f); err != nil {
				t.Errorf("%s provides: %v", name, err)
			}
		}
	}

	for _, id := range adapters.Builtins() {
		resolved, err := adapters.Resolve(id)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", id, err)
		}
		check("built-in "+id, resolved.Manifest)
	}
	for _, dir := range []string{
		filepath.Join(repoRoot(), "adapters", "mock-enrich-py"),
		filepath.Join(repoRoot(), "test", "fixtures", "adapters", "mock-deliver"),
	} {
		raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		m, err := adapters.ParseManifest(raw)
		if err != nil {
			t.Fatalf("parsing %s: %v", dir, err)
		}
		check(filepath.Base(dir), m)
	}
}
