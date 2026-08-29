package runner

import (
	"encoding/json"
	"testing"

	"github.com/elegant-atomics/gtme/internal/planner"
)

// TestInputHashExcludesTheStepsOwnOutputs: a needs-all step must not see
// its own last answer, or any of this pipeline's judgment fields, as a
// changed input (ADR-039); uses: narrows the hash to the declared fields.
func TestInputHashExcludesTheStepsOwnOutputs(t *testing.T) {
	needsAll := &planner.Step{NeedsAll: true, Provides: []string{"qualify.state", "qualify.rationale"}}
	before := map[string]any{"title": "VP", "email": "a@x.com"}
	after := map[string]any{"title": "VP", "email": "a@x.com",
		"qualify.state": "now", "qualify.rationale": "fits", "qualify.other": "x"}
	if inputHash(needsAll, "qualify", before) != inputHash(needsAll, "qualify", after) {
		t.Error("the step's own outputs and the pipeline's namespaced fields must not change the hash")
	}
	changed := map[string]any{"title": "CFO", "email": "a@x.com"}
	if inputHash(needsAll, "qualify", before) == inputHash(needsAll, "qualify", changed) {
		t.Error("a changed fact must change the hash")
	}
	other := map[string]any{"title": "VP", "email": "a@x.com", "other.field": "new"}
	if inputHash(needsAll, "qualify", before) == inputHash(needsAll, "qualify", other) {
		t.Error("a new fact from elsewhere is a changed input for a needs-all step")
	}

	uses := &planner.Step{Needs: []string{"title"}}
	if inputHash(uses, "qualify", before) != inputHash(uses, "qualify", other) {
		t.Error("with uses: only the declared fields count")
	}
	if inputHash(uses, "qualify", before) == inputHash(uses, "qualify", changed) {
		t.Error("a changed uses: field must change the hash")
	}
	// Canonical: key order never matters.
	a := digest(map[string]any{"b": 1, "a": 2})
	b := digest(map[string]any{"a": 2, "b": 1})
	if a != b || len(a) != signatureLen {
		t.Errorf("digest = %q vs %q", a, b)
	}
	if _, err := json.Marshal(json.RawMessage(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
}
