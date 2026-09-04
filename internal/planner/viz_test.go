package planner

import (
	"strings"
	"testing"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/pipeline"
)

// vizPlan is a plan exercising every role the vocabulary names (ADR-051).
func vizPlan() *Plan {
	return &Plan{
		Pipeline: &pipeline.Pipeline{Name: "every-role", Version: 1},
		Steps: []Step{
			{ID: "source", Use: "apollo/search", Role: adapters.RoleSource, IsSource: true,
				EntityType: "person", Provides: []string{"first_name", "title"}},
			{ID: "icp", Use: "ai/filter", Role: adapters.RoleFilter, Participant: "ai",
				EntityType: "person", Needs: []string{"title"}, Batch: true, BatchSize: 25},
			{ID: "reveal", Use: "apollo/enrich", Role: adapters.RoleEnrich,
				EntityType: "person", Provides: []string{"email"}},
			{ID: "score", Use: "sql/transform", Role: adapters.RoleEnrich, IsSQL: true,
				EntityType: "person", Provides: []string{"fit_score"}},
			{ID: "check", Use: "neverbounce/email", Role: adapters.RoleVerify,
				EntityType: "person", Provides: []string{"email_status"}},
			{ID: "write", Use: "ai/compose", Role: adapters.RoleCompose, Participant: "ai",
				EntityType: "person", Provides: []string{"first_line"}},
			{ID: "approve", Use: "human/review", Role: adapters.RoleReview,
				Participant: adapters.KindHuman, EntityType: "person"},
			{ID: "send", Use: "instantly/add-to-campaign", Role: adapters.RoleDeliver,
				IsDeliver: true, EntityType: "person", RecordGroup: "every-role",
				Idempotency: "email"},
		},
		Available: []string{"email", "first_line", "first_name", "title"},
	}
}

func viz(t *testing.T, p *Plan) string {
	t.Helper()
	var b strings.Builder
	Viz(&b, p)
	return b.String()
}

func TestDisplayWidthCountsEmojiAsTwoColumns(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"abc", 3},
		{"", 0},
		{"🌐", 2},            // executor glyph
		{"✍️", 2},           // U+270D U+FE0F — emoji presentation, two columns
		{"🌐📥", 4},           // the two-slot column
		{"─", 1},            // box drawing stays one column
		{"…", 1},            // the truncation marker
		{"🌐 1  source", 12}, // emoji plus ASCII
	} {
		if got := displayWidth(tc.in); got != tc.want {
			t.Errorf("displayWidth(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// isBoxRow reports a box's body row — opening and closing on a vertical rail.
// The spine connectors also carry `│`, but they end in text, not a rail.
func isBoxRow(line string) bool {
	r := []rune(strings.TrimSpace(line))
	if len(r) < 2 {
		return false
	}
	const rails = "│┃║"
	return strings.ContainsRune(rails, r[0]) && strings.ContainsRune(rails, r[len(r)-1])
}

// The frame must not move. Every body row a box contributes is the same number
// of display columns, or an emoji row ragged the right rail.
func TestVizBoxRowsAreUniformWidth(t *testing.T) {
	rows := 0
	for _, line := range strings.Split(viz(t, vizPlan()), "\n") {
		if !isBoxRow(line) {
			continue
		}
		rows++
		if got := displayWidth(line); got != vizWidth+vizIndent {
			t.Errorf("row is %d columns, want %d:\n%s", got, vizWidth+vizIndent, line)
		}
	}
	if rows == 0 {
		t.Fatal("no box rows rendered")
	}
}

func TestVizShapeCarriesRole(t *testing.T) {
	out := viz(t, vizPlan())
	for _, tc := range []struct{ role, glyph string }{
		{"source (rounded)", "╭"},
		{"enrich (light rect)", "┌"},
		{"verify (doubled rails)", "║"},
		{"compose (wavy floor)", "~"},
		{"deliver (heavy)", "┏"},
		{"filter (funnel outlet)", "╲"},
		{"review (trapezoid lid)", "╱"},
	} {
		if !strings.Contains(out, tc.glyph) {
			t.Errorf("%s: no %q in output:\n%s", tc.role, tc.glyph, out)
		}
	}
}

func TestVizGlyphPairIsExecutorThenRole(t *testing.T) {
	out := viz(t, vizPlan())
	for _, want := range []string{
		"🌐📥",  // vendor source
		"💻🤏",  // ai filter
		"🌐💎",  // vendor enrich
		"💾💎",  // sql enrich — same role, different executor
		"🌐👌",  // vendor verify
		"💻✍️", // ai compose
		"🧑👀",  // human review
		"🌐🚀",  // vendor deliver
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no glyph pair %q in output:\n%s", want, out)
		}
	}
}

func TestVizEdgeCarriesAvailableSetDelta(t *testing.T) {
	out := viz(t, vizPlan())
	// The enrich step's arrow names what that step added, not the whole set.
	if !strings.Contains(out, "+ email") {
		t.Errorf("edge does not carry the enrich step's delta:\n%s", out)
	}
	if !strings.Contains(out, "+ fit_score") {
		t.Errorf("edge does not carry the sql step's delta:\n%s", out)
	}
	// A step that provides nothing gets no delta label.
	if strings.Contains(out, "+ \n") {
		t.Errorf("empty delta label rendered:\n%s", out)
	}
}

func TestVizFilterForkNamesBothOutcomes(t *testing.T) {
	out := viz(t, vizPlan())
	if !strings.Contains(out, "pass") || !strings.Contains(out, "fail") {
		t.Errorf("filter fork does not name both outcomes:\n%s", out)
	}
}

func TestVizAgentExecutorDiffersFromHuman(t *testing.T) {
	p := vizPlan()
	p.Steps[6].Use = "agent/review"
	p.Steps[6].Participant = adapters.KindAgent
	out := viz(t, p)
	if !strings.Contains(out, "🤖👀") {
		t.Errorf("agent review not marked 🤖:\n%s", out)
	}
	if strings.Contains(out, "🧑👀") {
		t.Errorf("agent review still marked as human:\n%s", out)
	}
}

func TestVizTruncatesRatherThanOverflowing(t *testing.T) {
	p := vizPlan()
	p.Steps[2].Use = "a-very-long-vendor-namespace/an-extremely-long-adapter-name-here"
	out := viz(t, p)
	if !strings.Contains(out, "…") {
		t.Errorf("long adapter name not truncated:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if isBoxRow(line) && displayWidth(line) != vizWidth+vizIndent {
			t.Errorf("truncation broke the frame:\n%s", line)
		}
	}
}

func TestVizHeaderCountsSendsAndUnpricedSteps(t *testing.T) {
	out := viz(t, vizPlan())
	head := strings.SplitN(out, "\n", 3)[1]
	for _, want := range []string{"8 steps", "1 send"} {
		if !strings.Contains(head, want) {
			t.Errorf("header %q missing %q", head, want)
		}
	}
}

// The delta belongs to the edge leaving a step, not the edge entering the next
// one: the arrow says what is now available, which is what the step just drawn
// contributed. The source's fields must appear on the first arrow.
func TestVizDeltaBelongsToTheStepAbove(t *testing.T) {
	out := viz(t, vizPlan())
	lines := strings.Split(out, "\n")

	var firstArrow int
	for i, l := range lines {
		if strings.Contains(l, "▼") {
			firstArrow = i
			break
		}
	}
	if firstArrow == 0 {
		t.Fatal("no connector rendered")
	}
	label := lines[firstArrow-1]
	if !strings.Contains(label, "first_name") || !strings.Contains(label, "title") {
		t.Errorf("first edge should carry the source's fields, got %q", label)
	}
	if strings.Contains(label, "email") {
		t.Errorf("first edge carries a later step's fields: %q", label)
	}
}

// The fork's `│ pass` is itself the connector; a bare rail under it is noise.
func TestVizForkDoesNotDoubleTheConnector(t *testing.T) {
	lines := strings.Split(viz(t, vizPlan()), "\n")
	for i, l := range lines {
		if !strings.HasSuffix(l, "│ pass") || i+1 >= len(lines) {
			continue
		}
		if strings.TrimSpace(lines[i+1]) == "│" {
			t.Errorf("bare rail duplicates the fork's connector at line %d", i+1)
		}
	}
}
