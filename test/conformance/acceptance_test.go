package conformance

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// acceptanceStory is one file under spec/acceptance/ — the structured form of
// one of SPEC.md's operator stories (ADR-012).
type acceptanceStory struct {
	Story     string   `yaml:"story"`
	Invariant string   `yaml:"invariant"`
	Given     string   `yaml:"given"`
	When      string   `yaml:"when"`
	Then      string   `yaml:"then"`
	SpecRefs  []string `yaml:"spec_refs"`
}

// expectedStories is the full set from SPEC.md's "Operator stories — acceptance
// criteria" section. All eight are normative, so the corpus is complete or it is
// not usable.
var expectedStories = []string{
	"launch", "top-up", "interrogate", "iterate",
	"segment", "guard", "recover", "report",
}

// TestAcceptanceCorpusIsCompleteAndWellFormed is a structural check only: it
// says the acceptance criteria are loadable, complete, and non-empty. It does
// not assert that any e2e test implements a story.
func TestAcceptanceCorpusIsCompleteAndWellFormed(t *testing.T) {
	dir := specPath("acceptance")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading spec/acceptance: %v", err)
	}

	found := map[string][]string{} // story name → files declaring it
	for _, e := range entries {
		if e.IsDir() || !(strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml")) {
			continue
		}
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("reading %s: %v", name, err)
			}
			dec := yaml.NewDecoder(strings.NewReader(string(raw)))
			dec.KnownFields(true)
			var story acceptanceStory
			if err := dec.Decode(&story); err != nil {
				t.Fatalf("%s is not a well-formed acceptance story: %v", name, err)
			}
			for _, field := range []struct {
				key, value string
			}{
				{"story", story.Story},
				{"invariant", story.Invariant},
				{"given", story.Given},
				{"when", story.When},
				{"then", story.Then},
			} {
				if strings.TrimSpace(field.value) == "" {
					t.Errorf("%s: `%s:` is empty; every story states all four of given/when/then plus its invariant", name, field.key)
				}
			}
			if story.Story != "" {
				found[story.Story] = append(found[story.Story], name)
			}
			// The filename should name the story, so a reader can find one without grepping.
			if base := strings.TrimSuffix(strings.TrimSuffix(name, ".yaml"), ".yml"); base != story.Story {
				t.Errorf("%s declares story %q; name the file after the story", name, story.Story)
			}
		})
	}

	t.Run("all eight operator stories are present exactly once", func(t *testing.T) {
		var missing, duplicated, unexpected []string
		want := map[string]bool{}
		for _, s := range expectedStories {
			want[s] = true
			switch n := len(found[s]); {
			case n == 0:
				missing = append(missing, s)
			case n > 1:
				duplicated = append(duplicated, s+" ("+strings.Join(found[s], ", ")+")")
			}
		}
		for s := range found {
			if !want[s] {
				unexpected = append(unexpected, s)
			}
		}
		sort.Strings(missing)
		sort.Strings(duplicated)
		sort.Strings(unexpected)
		if len(missing) > 0 {
			t.Errorf("spec/acceptance is missing operator stories from SPEC.md: %s", strings.Join(missing, ", "))
		}
		if len(duplicated) > 0 {
			t.Errorf("spec/acceptance declares these stories more than once: %s", strings.Join(duplicated, "; "))
		}
		if len(unexpected) > 0 {
			t.Errorf("spec/acceptance declares stories SPEC.md does not: %s\n"+
				"  The eight in SPEC.md are normative (ADR-012); add one there first.",
				strings.Join(unexpected, ", "))
		}
	})
}
