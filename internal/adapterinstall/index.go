package adapterinstall

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/elegant-atomics/gtme/spec"
)

// DefaultRegistry is the index URL baked into the binary (ADR-042);
// GTME_REGISTRY overrides it.
const DefaultRegistry = "https://raw.githubusercontent.com/elegant-atomics/gtme-bindings/main/index.json"

// RegistryURL is the index the verbs read.
func RegistryURL() string {
	if v := os.Getenv("GTME_REGISTRY"); v != "" {
		return v
	}
	return DefaultRegistry
}

// Entry is one index row (spec/schemas/registry-index.schema.json).
type Entry struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Vendor      string   `json:"vendor,omitempty"`
	Role        string   `json:"role"`
	EntityType  string   `json:"entity_type"`
	Needs       []string `json:"needs,omitempty"`
	Provides    []string `json:"provides,omitempty"`
	Credentials []string `json:"credentials,omitempty"`
	Source      struct {
		URL  string `json:"url"`
		Path string `json:"path"`
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
	} `json:"source"`
	SHA256 string `json:"sha256"`
	Tier   string `json:"tier"`
	Since  string `json:"since,omitempty"`
}

// Index is the registry's published document.
type Index struct {
	Version     int     `json:"version"`
	GeneratedAt string  `json:"generated_at,omitempty"`
	Bindings    []Entry `json:"bindings"`
}

var indexSchema = func() *jsonschema.Schema {
	c := jsonschema.NewCompiler()
	if err := c.AddResource("registry-index.schema.json", strings.NewReader(string(spec.RegistryIndexSchema))); err != nil {
		panic(fmt.Sprintf("adapters: index schema resource: %v", err))
	}
	s, err := c.Compile("registry-index.schema.json")
	if err != nil {
		panic(fmt.Sprintf("adapters: compiling registry-index.schema.json: %v", err))
	}
	return s
}()

// LoadIndex fetches and schema-validates the registry index.
func LoadIndex() (*Index, error) {
	url := RegistryURL()
	resp, err := get(url, "application/json")
	if err != nil {
		return nil, fmt.Errorf("adapters: fetching registry index %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("adapters: registry index %s: %s", url, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("adapters: reading registry index: %w", err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("adapters: registry index %s is not JSON: %w", url, err)
	}
	if err := indexSchema.Validate(doc); err != nil {
		return nil, fmt.Errorf("adapters: registry index %s does not conform to registry-index.schema.json: %w", url, err)
	}
	var ix Index
	if err := json.Unmarshal(raw, &ix); err != nil {
		return nil, err
	}
	return &ix, nil
}

// Search matches id, vendor, description and role, case-insensitively
// (SPEC §8).
func (ix *Index) Search(q string) []Entry {
	q = strings.ToLower(q)
	var out []Entry
	for _, e := range ix.Bindings {
		hay := strings.ToLower(e.ID + " " + e.Vendor + " " + e.Description + " " + e.Role)
		if strings.Contains(hay, q) {
			out = append(out, e)
		}
	}
	return out
}

// FindSource returns the entry publishing the given repository path, if the
// index carries one — the hook for the content-hash refusal (SPEC §11 M19).
func (ix *Index) FindSource(url, path string) *Entry {
	for i := range ix.Bindings {
		if ix.Bindings[i].Source.URL == url && ix.Bindings[i].Source.Path == path {
			return &ix.Bindings[i]
		}
	}
	return nil
}
