package planner

import (
	"strconv"
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

// isBoxRow reports a box's body row — opening and closing on a rail, vertical
// or diagonal. The spine connectors also carry `│`, but they end in text.
func isBoxRow(line string) bool {
	r := []rune(strings.TrimSpace(line))
	if len(r) < 2 {
		return false
	}
	const rails = "│┃║╲╱┊"
	return strings.ContainsRune(rails, r[0]) && strings.ContainsRune(rails, r[len(r)-1])
}

// frame drops the fixed index gutter, so geometry is measured on the box.
func frame(line string) string {
	r := []rune(line)
	if len(r) < vizGutter {
		return ""
	}
	return string(r[vizGutter:])
}

func indentOf(line string) int {
	f := frame(line)
	return len(f) - len(strings.TrimLeft(f, " "))
}

// The index is a reference handle, like a line number: it sits in one column
// whatever the frame beside it is doing, including a tapered one.
func TestVizIndexSitsInAFixedGutter(t *testing.T) {
	seen := 0
	for _, l := range strings.Split(viz(t, vizPlan()), "\n") {
		g := []rune(l)
		if len(g) < vizGutter {
			continue
		}
		if !isBoxRow(frame(l)) {
			continue
		}
		gut := strings.TrimSpace(string(g[:vizGutter]))
		if gut == "" {
			continue
		}
		seen++
		if _, err := strconv.Atoi(gut); err != nil {
			t.Errorf("gutter holds %q, want a step index", gut)
		}
	}
	if seen != 8 {
		t.Errorf("indexed rows = %d, want one per step (8)", seen)
	}
}

// The index left the frame, so the head row opens on the glyph pair.
func TestVizHeadRowOpensOnTheGlyphPair(t *testing.T) {
	for _, l := range strings.Split(viz(t, vizPlan()), "\n") {
		if !strings.Contains(l, "apollo/search") {
			continue
		}
		body := strings.TrimLeft(frame(l), " ")
		if !strings.HasPrefix(body, "\u2502 \U0001F310\U0001F4E5 SOURCE") {
			t.Errorf("head row should open on the glyph pair, got %q", body)
		}
		return
	}
	t.Fatal("no source row rendered")
}

// An unknown price keeps the column's shape: every other entry is $N/rec.
func TestVizUnknownCostKeepsTheColumnShape(t *testing.T) {
	out := viz(t, vizPlan())
	if strings.Contains(out, "$ ?") {
		t.Errorf("unknown cost renders as a broken format string:\n%s", out)
	}
	if !strings.Contains(out, "$?/rec") {
		t.Errorf("unknown cost should read $?/rec:\n%s", out)
	}
}

// A tapered box's rows are deliberately not all the same width — that is what
// draws the funnel. What must hold is that every row stays centred on the same
// axis, so the two sides taper in step and neither rail wanders.
func TestVizBoxRowsShareOneAxis(t *testing.T) {
	// Indent is measured past the gutter, so the axis is too.
	const axis = 2*(vizIndent-vizGutter) + vizWidth
	rows := 0
	for _, line := range strings.Split(viz(t, vizPlan()), "\n") {
		if !isBoxRow(line) {
			continue
		}
		rows++
		if got := 2*indentOf(line) + displayWidth(strings.TrimLeft(line, " ")); got != axis {
			t.Errorf("row is off the axis (%d, want %d):\n%s", got, axis, line)
		}
	}
	if rows == 0 {
		t.Fatal("no box rows rendered")
	}
}

// A review is strictly 1:1 — it writes fields, and `when: <review>.passed` is
// refused, so it cannot even reject. In a diagram where the funnel's taper
// means "fewer records out", a tapered review would claim a cardinality it
// does not have; broken rails say what is true, that the run pauses there.
func TestVizReviewDoesNotTaper(t *testing.T) {
	lines := strings.Split(viz(t, vizPlan()), "\n")
	for i, l := range lines {
		if !strings.Contains(l, "human/review") {
			continue
		}
		if !strings.Contains(l, "┊") {
			t.Errorf("review should carry the paused rail:\n%s", l)
		}
		if indentOf(l) != vizIndent-vizGutter || indentOf(lines[i-1]) != vizIndent-vizGutter {
			t.Errorf("review tapers; it is 1:1 and must not:\n%s\n%s", lines[i-1], l)
		}
		return
	}
	t.Fatal("no review step rendered")
}

// A funnel's sides descend one column per row, so the diagonals stack into a
// continuous line rather than kinking at a single step.
func TestVizFunnelTapersOneColumnPerRow(t *testing.T) {
	lines := strings.Split(viz(t, vizPlan()), "\n")
	var run []int
	for _, l := range lines {
		trimmed := strings.TrimLeft(frame(l), " ")
		if trimmed == "" {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "┌"), strings.HasPrefix(trimmed, "╲"):
			run = append(run, indentOf(l))
		case len(run) > 0:
			for i := 1; i < len(run); i++ {
				if run[i] != run[i-1]+1 {
					t.Errorf("funnel side steps %d columns between rows, want 1", run[i]-run[i-1])
				}
			}
			if len(run) < 3 {
				t.Errorf("funnel drew %d diagonal rows, want the full taper", len(run))
			}
			return
		}
	}
	t.Fatal("no funnel rendered")
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
		if isBoxRow(frame(line)) && 2*indentOf(line)+displayWidth(strings.TrimLeft(frame(line), " ")) != 2*(vizIndent-vizGutter)+vizWidth {
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

// The fork is two lines: the failing branch, then the arrow that carries the
// passing one. A third line spent on a bare rail is noise.
func TestVizForkIsTwoLines(t *testing.T) {
	out := viz(t, vizPlan())
	if !strings.Contains(out, "▼ pass") {
		t.Errorf("the passing branch should label the arrow:\n%s", out)
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == "│ pass" {
			t.Errorf("fork still spends a line on the passing branch alone")
		}
	}
}

// Every rule between two steps carries the spine, so the boxes read as one
// run rather than a stack of unconnected frames.
func TestVizRulesJoinTheSpine(t *testing.T) {
	lines := strings.Split(viz(t, vizPlan()), "\n")
	var down, up int
	for _, l := range lines {
		for _, j := range []string{"┬", "┰"} {
			if strings.Contains(l, j) {
				down++
			}
		}
		for _, j := range []string{"┴", "┸"} {
			if strings.Contains(l, j) {
				up++
			}
		}
	}
	// Eight steps: seven outgoing joints and seven incoming ones.
	if down != 7 {
		t.Errorf("outgoing spine joints = %d, want 7", down)
	}
	if up != 7 {
		t.Errorf("incoming spine joints = %d, want 7", up)
	}
}

// vizMaxLine is the widest line the diagram may produce: the frame plus the
// connector labels beside it, inside a conventional 80-column terminal.
func TestVizNoLineExceedsTheTerminalWidth(t *testing.T) {
	p := vizPlan()
	// A step providing many long field names is the case that overflows.
	p.Steps[2].Provides = []string{
		"company_industry", "company_employees", "company_linkedin_url",
		"company_website", "email_status", "seniority", "linkedin_url", "city",
	}
	for _, l := range strings.Split(viz(t, p), "\n") {
		if got := displayWidth(l); got > vizMaxLine {
			t.Errorf("line is %d columns, want <= %d:\n%s", got, vizMaxLine, l)
		}
	}
}

// The wave is the compose step's floor; its corners stay rounded with it.
func TestVizComposeFloorKeepsRoundedCorners(t *testing.T) {
	out := viz(t, vizPlan())
	for _, l := range strings.Split(out, "\n") {
		if !strings.Contains(l, "~") {
			continue
		}
		trimmed := strings.TrimSpace(l)
		if !strings.HasPrefix(trimmed, "╰") || !strings.HasSuffix(trimmed, "╯") {
			t.Errorf("wavy floor should keep rounded corners, got:\n%s", l)
		}
		return
	}
	t.Fatal("no wavy floor rendered")
}

// Canonical fields carry more meaning than vendor-namespaced ones (§4a), so
// when the label truncates it is the vendor noise that goes.
func TestVizDeltaPrefersCanonicalFields(t *testing.T) {
	p := vizPlan()
	p.Steps[0].Provides = []string{"apollo.has_email", "apollo.id", "first_name", "title"}
	out := viz(t, p)
	for _, l := range strings.Split(out, "\n") {
		if !strings.Contains(l, "+ ") {
			continue
		}
		if !strings.Contains(l, "+ first_name, title") {
			t.Errorf("canonical fields should lead the label, got:\n%s", l)
		}
		return
	}
	t.Fatal("no delta label rendered")
}

// A two-digit step index must not shift the columns beside it.
func TestVizIndexPaddingHoldsTheColumns(t *testing.T) {
	p := vizPlan()
	for i := 0; i < 4; i++ { // grow past nine steps
		p.Steps = append(p.Steps, p.Steps[2])
	}
	out := viz(t, p)
	var col int
	for _, l := range strings.Split(out, "\n") {
		i := strings.Index(l, " source ")
		if i < 0 {
			i = strings.Index(l, " enrich ")
		}
		if i < 0 {
			continue
		}
		if col == 0 {
			col = i
		} else if i != col {
			t.Errorf("role column moved from %d to %d:\n%s", col, i, l)
		}
	}
}

// ADR-049: an agent/* step never prompts, so it does not wait for a person.
func TestVizAgentDoesNotWaitForAPerson(t *testing.T) {
	p := vizPlan()
	p.Steps[6].Use = "agent/review"
	p.Steps[6].Participant = adapters.KindAgent
	out := viz(t, p)
	if strings.Contains(out, "waits for a person") {
		t.Errorf("agent step claims to wait for a person:\n%s", out)
	}
	if !strings.Contains(out, "waits for an agent") {
		t.Errorf("agent step does not say what it waits for:\n%s", out)
	}
}

// A group handoff crosses no network and spends nothing, and its target is
// the point of it (ADR-032).
func TestVizGroupDeliverShowsTargetAndCostsNothing(t *testing.T) {
	p := vizPlan()
	p.Steps[7] = Step{ID: "handoff", Use: "group/deliver", Role: adapters.RoleDeliver,
		IsDeliver: true, IsGroupDeliver: true, TargetGroup: "approved",
		EntityType: "person", RecordGroup: "every-role", Idempotency: "email"}
	out := viz(t, p)
	var row string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "group/deliver") {
			row = l
		}
	}
	if row == "" {
		t.Fatal("handoff not rendered")
	}
	if strings.Contains(row, "$ ?") {
		t.Errorf("handoff priced as unknown; it spends nothing:\n%s", row)
	}
	if !strings.Contains(out, "approved") {
		t.Errorf("handoff hides its target group:\n%s", out)
	}
}

// A group source draws from the ledger: which group, and how many.
func TestVizGroupSourceShowsGroupAndLimit(t *testing.T) {
	p := vizPlan()
	p.Steps[0] = Step{ID: "members", Use: "group/source", Role: adapters.RoleSource,
		IsSource: true, IsGroupSource: true, SourceGroup: "warm-leads", Limit: 200,
		EntityType: "person"}
	out := viz(t, p)
	for _, want := range []string{"warm-leads", "200"} {
		if !strings.Contains(out, want) {
			t.Errorf("group source omits %q:\n%s", want, out)
		}
	}
}

// A group is one mechanism — the ledger's membership tables, runner-owned,
// no network — so it carries one executor glyph in both directions. The role
// slot already says which way the records are moving.
func TestVizGroupUsesOneGlyphBothDirections(t *testing.T) {
	p := vizPlan()
	p.Steps[0] = Step{ID: "members", Use: "group/source", Role: adapters.RoleSource,
		IsSource: true, IsGroupSource: true, SourceGroup: "warm", EntityType: "person"}
	p.Steps[7] = Step{ID: "handoff", Use: "group/deliver", Role: adapters.RoleDeliver,
		IsDeliver: true, IsGroupDeliver: true, TargetGroup: "approved",
		EntityType: "person", RecordGroup: "every-role"}
	out := viz(t, p)
	for _, want := range []string{"👥📥", "👥🚀"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "📂") {
		t.Errorf("group deliver still carries a second glyph:\n%s", out)
	}
}

// The role word is a column you scan vertically, so it is uniform: all caps,
// every role, not just the one that shouts.
func TestVizRoleWordsAreAllCaps(t *testing.T) {
	out := viz(t, vizPlan())
	for _, want := range []string{"SOURCE", "FILTER", "ENRICH", "VERIFY", "COMPOSE", "REVIEW", "DELIVER"} {
		if !strings.Contains(out, want) {
			t.Errorf("role word %q not rendered in caps:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{" source ", " filter ", " enrich ", " compose "} {
		if strings.Contains(out, unwanted) {
			t.Errorf("lowercase role word %q survives", unwanted)
		}
	}
}

// One column, one meaning: the right column is the price on every row. A
// source has a price too, and hiding it behind the entity type made the
// column mean two different things.
func TestVizRightColumnIsAlwaysCost(t *testing.T) {
	p := vizPlan()
	zero := 0.0
	p.Steps[0].CostEstimate = &zero
	out := viz(t, p)
	for _, l := range strings.Split(out, "\n") {
		if !strings.Contains(l, "apollo/search") {
			continue
		}
		if !strings.Contains(l, "$0.0000/rec") {
			t.Errorf("source row does not carry its cost:\n%s", l)
		}
		if strings.Contains(l, "person") {
			t.Errorf("entity type still occupies the cost column:\n%s", l)
		}
		return
	}
	t.Fatal("no source row rendered")
}

func TestVizHeaderCarriesTheEntityType(t *testing.T) {
	out := viz(t, vizPlan())
	if !strings.Contains(strings.SplitN(out, "\n", 3)[1], "person") {
		t.Errorf("header does not name the entity type:\n%s", out)
	}
}

// A gate decides whether a record reaches a paid step. Showing one and
// silently dropping the rest is the diagram asserting something untrue.
func TestVizShowsEveryGateNotJustTheFirst(t *testing.T) {
	p := vizPlan()
	p.Steps[2].When = "icp.passed"
	p.Steps[2].Require = []string{"customers"}
	p.Steps[2].Exclude = []string{"suppressed"}
	out := viz(t, p)
	for _, want := range []string{"icp.passed", "customers", "suppressed"} {
		if !strings.Contains(out, want) {
			t.Errorf("gate %q silently dropped:\n%s", want, out)
		}
	}
}

// A default that lies is worse than one that admits ignorance: an
// unrecognised role must not render as enrich.
func TestVizUnknownRoleIsNotDisguisedAsEnrich(t *testing.T) {
	p := vizPlan()
	p.Steps[2].Role = "teleport"
	out := viz(t, p)
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "apollo/enrich") && strings.Contains(l, "💎") {
			t.Errorf("unknown role rendered as enrich:\n%s", l)
		}
	}
	if !strings.Contains(out, "❔") {
		t.Errorf("unknown role not marked as unknown:\n%s", out)
	}
}
