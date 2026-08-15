package conformance

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/trevorfox/gtm/internal/pipeline"
)

// yamlToJSON re-encodes a YAML document as JSON so a JSON Schema validator can
// see it, which is how pipeline.yaml is checked against spec/schemas/.
func yamlToJSON(t *testing.T, raw []byte, what string) any {
	t.Helper()
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s is not valid YAML: %v", what, err)
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("%s does not convert to JSON: %v", what, err)
	}
	return asJSONValue(t, encoded)
}

// specSectionYAML pulls the first fenced ```yaml block out of a SPEC.md section,
// so the example the spec shows an operator is the one that gets validated.
var specSectionYAML = regexp.MustCompile("(?s)\n## 9\\. pipeline\\.yaml.*?\n```yaml\n(.*?)\n```")

// TestSpecExamplePipelineValidates checks SPEC §9's own example pipeline against
// spec/schemas/pipeline.schema.json. The example includes `uses:` (ADR-004).
func TestSpecExamplePipelineValidates(t *testing.T) {
	specPath := filepath.Join(repoRoot(), "SPEC.md")
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("reading %s: %v", specPath, err)
	}
	m := specSectionYAML.FindSubmatch(raw)
	if m == nil {
		t.Fatal("SPEC.md §9 no longer contains a fenced ```yaml example; this test cannot find the canonical pipeline")
	}
	schema := compileSchema(t, "pipeline.schema.json")
	if err := schema.Validate(yamlToJSON(t, m[1], "the SPEC.md §9 example pipeline")); err != nil {
		t.Errorf("the SPEC.md §9 example pipeline does not satisfy spec/schemas/pipeline.schema.json:\n%v\n\n%s",
			err, m[1])
	}
}

// TestSpecExamplePipelineParses is the other half of the §9 contract: the
// schema saying a document is well-formed is worth nothing if the parser the
// operator actually hits rejects it. SPEC §9's example carries `uses:`
// (ADR-004), so this fails until internal/pipeline knows that key.
func TestSpecExamplePipelineParses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(), "SPEC.md"))
	if err != nil {
		t.Fatalf("reading SPEC.md: %v", err)
	}
	m := specSectionYAML.FindSubmatch(raw)
	if m == nil {
		t.Fatal("SPEC.md §9 no longer contains a fenced ```yaml example")
	}
	if _, err := pipeline.Parse(m[1]); err != nil {
		t.Errorf("internal/pipeline rejects the SPEC.md §9 example pipeline: %v\n"+
			"  SPEC.md §9 is DECIDED; the example there must be a pipeline gtm can run.\n\n%s", err, m[1])
	}
}

// TestExamplePipelinesValidate checks every pipeline shipped under examples/.
func TestExamplePipelinesValidate(t *testing.T) {
	dir := filepath.Join(repoRoot(), "examples")
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking examples/: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("examples/ holds no pipeline YAML")
	}

	schema := compileSchema(t, "pipeline.schema.json")
	for _, path := range files {
		rel, _ := filepath.Rel(repoRoot(), path)
		t.Run(rel, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			if err := schema.Validate(yamlToJSON(t, raw, rel)); err != nil {
				t.Errorf("%s does not satisfy spec/schemas/pipeline.schema.json:\n%v", rel, err)
			}
		})
	}
}

// TestAdapterManifestsValidate checks every manifest.json in the tree against
// spec/schemas/manifest.schema.json — built-ins, the external Python adapter,
// and the test fixture adapters alike, since SPEC §6 makes no distinction.
func TestAdapterManifestsValidate(t *testing.T) {
	root := repoRoot()
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "bin") {
			return fs.SkipDir
		}
		if !d.IsDir() && d.Name() == "manifest.json" {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repo: %v", err)
	}
	// The AI adapters embed their manifests under different names.
	for _, extra := range []string{
		filepath.Join(root, "internal", "adapters", "aisteps", "filter.json"),
		filepath.Join(root, "internal", "adapters", "aisteps", "compose.json"),
	} {
		if _, err := os.Stat(extra); err == nil {
			files = append(files, extra)
		}
	}
	if len(files) == 0 {
		t.Fatal("found no adapter manifests to check")
	}

	schema := compileSchema(t, "manifest.schema.json")
	for _, path := range files {
		rel, _ := filepath.Rel(root, path)
		t.Run(rel, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			if err := schema.Validate(asJSONValue(t, raw)); err != nil {
				t.Errorf("%s does not satisfy spec/schemas/manifest.schema.json:\n%v", rel, err)
			}
		})
	}
}
