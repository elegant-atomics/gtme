// Package conformance checks the implementation against the machine-checkable
// artifacts in spec/ — the JSON Schemas, the canonical ledger DDL, the golden
// wire transcripts and the acceptance corpus (ADR-010, SPEC §11).
//
// These tests load spec/ directly. Nothing here re-encodes a spec shape in Go:
// when the spec and the code disagree, the failure names the artifact.
package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// repoRoot locates the checkout the way test/e2e does: from this file's own
// path, so it works regardless of the working directory a test runner picks.
func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("conformance: cannot locate the repo root")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// specPath joins onto the spec/ directory.
func specPath(parts ...string) string {
	return filepath.Join(append([]string{repoRoot(), "spec"}, parts...)...)
}

// compileSchema loads one schema file from spec/schemas/.
func compileSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	path := specPath("schemas", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft7
	if err := c.AddResource(name, strings.NewReader(string(raw))); err != nil {
		t.Fatalf("%s is not a loadable schema: %v", path, err)
	}
	s, err := c.Compile(name)
	if err != nil {
		t.Fatalf("%s does not compile as draft-07: %v", path, err)
	}
	return s
}

// asJSONValue normalizes a Go value into the shape a JSON Schema validator
// expects: plain maps, slices, strings and float64s.
func asJSONValue(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decoding JSON: %v", err)
	}
	return v
}
