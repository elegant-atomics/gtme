package e2e

// M10 acceptance (SPEC §8/§11, ADR-029), offline: freeze a run into a
// campaign bundle, move the bundle to a clean ledger, and simulate + dry-run
// it successfully — the bundle resolving its own bindings (an external one
// and a built-in one, at their frozen versions), simulation served entirely
// from fixtures inside the bundle, and tampering caught by the manifest.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBundleFreezeMoveSimulateDry(t *testing.T) {
	srv := bindingFixtureServer(t, "apollo-search")
	keys := []string{"APOLLO_API_KEY=k", "ATTIO_API_KEY=k"}

	// Harness A: an external binding source feeding the built-in attio/assert
	// binding, run dry to mint the run the freeze snapshots.
	a := newHarness(t)
	a.writeBinding("apollox/search", filepath.Join(repoRoot(), "spec", "bindings", "apollo-search", "binding.yaml"))
	a.write("p.yaml", `name: bundle-proof
source:
  use: apollox/search
  with:
    query: vp marketing
    base_url: `+jsonString(srv.URL)+`

deliver:
  use: attio/assert
  variables:
    name: full_name
  idempotency: email
`)
	res := a.runWithEnv(keys, "", "run", "p.yaml", "--dry-run")
	if res.code != 0 {
		t.Fatalf("seed run exit = %d\nstderr:\n%s", res.code, res.stderr)
	}

	// Freeze straight into a clean harness's workspace — the "move to a clean
	// ledger" of the acceptance criterion.
	b := newHarness(t)
	bundleDir := filepath.Join(b.work, "bundle")
	res = a.mustRun("freeze", "last", "--bundle", bundleDir)
	contains(t, res.stderr, "self-contained except credentials", "freeze output")

	for _, name := range []string{
		"manifest.json", "pipeline.yaml",
		"adapters/apollox-search/binding.yaml",
		"adapters/apollox-search/fixtures/conformance.json",
		"adapters/attio-assert/binding.yaml",
		"adapters/attio-assert/fixtures/conformance.json",
		"registry/person.json", "registry/company.json",
	} {
		if _, err := os.Stat(filepath.Join(bundleDir, name)); err != nil {
			t.Errorf("bundle is missing %s", name)
		}
	}
	var manifest struct {
		BundleFormatVersion int               `json:"bundle_format_version"`
		Name                string            `json:"name"`
		SourceRunID         string            `json:"source_run_id"`
		Contents            map[string]string `json:"contents"`
	}
	raw := readFile(t, filepath.Join(bundleDir, "manifest.json"))
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if manifest.BundleFormatVersion != 1 || manifest.Name != "bundle-proof" || manifest.SourceRunID == "" {
		t.Errorf("manifest = %+v", manifest)
	}

	// Simulate on the clean ledger: zero keys, zero network — the source
	// binding serves the conformance fixtures packed inside the bundle.
	res = b.run("run", "bundle", "--simulate")
	if res.code != 0 {
		t.Fatalf("bundle simulate exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "hashes verified", "bundle banner")
	contains(t, res.stderr, "SIMULATED", "simulate receipt")
	contains(t, res.stderr, "sourced 2 records", "fixture-served source")
	if n := b.queryInt(`SELECT count(*) FROM runs`); n != 0 {
		t.Errorf("simulate persisted %d runs in the clean ledger, want 0", n)
	}

	// Dry-run on the clean ledger: the source runs live (against the local
	// fixture server), delivery stays held, resolved variables receipt.
	res = b.runWithEnv(keys, "", "run", "bundle", "--dry-run")
	if res.code != 0 {
		t.Fatalf("bundle dry-run exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "resolved variables", "dry receipt")
	contains(t, res.stderr, `name: "Jane Doe"`, "dry resolved variables")
	if n := b.queryInt(`SELECT count(*) FROM deliveries`); n != 0 {
		t.Errorf("dry-run wrote %d deliveries, want 0", n)
	}

	// Diffable means the manifest is the truth: a tampered bundle fails loudly.
	pipelinePath := filepath.Join(bundleDir, "pipeline.yaml")
	tampered := strings.Replace(readFile(t, pipelinePath), "vp marketing", "everyone", 1)
	if err := os.WriteFile(pipelinePath, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	res = b.run("run", "bundle", "--simulate")
	if res.code != 2 {
		t.Fatalf("tampered bundle exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "does not match its manifest hash", "tamper detection")
}
