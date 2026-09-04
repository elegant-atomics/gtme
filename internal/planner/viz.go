package planner

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/pipeline"
)

// Plan visualization (SPEC §7, ADR-051). Viz renders the resolved plan as a
// diagram: one silhouette per role, an executor-and-role glyph pair, and edges
// labelled with the fields each step adds to the available set — §7 step 2's
// walk made visible. It is a second view of what Print already writes, never
// the only home of a §7 fact, and it neither calls the network nor spends.
//
// The frame is fixed: no colour, no TTY detection, no terminal-width
// autodetection, so the bytes are deterministic and golden-testable.
const (
	vizWidth   = 64 // box width, borders included
	vizIndent  = 2
	vizSpine   = vizIndent + vizWidth/2 // the connector column
	vizMaxLine = 78                     // widest line, inside an 80-column terminal
)

// Viz writes the diagram for p.
func Viz(w io.Writer, p *Plan) {
	fmt.Fprintf(w, "pipeline %s (version %d)\n", p.Pipeline.Name, p.Pipeline.Version)
	fmt.Fprintln(w, vizHeadline(p))
	fmt.Fprintln(w)

	// The available set as the walk sees it, so each edge can name what its
	// step contributed rather than repeating the whole set.
	seen := map[string]bool{}
	for i := range p.Steps {
		s := &p.Steps[i]
		for _, line := range vizBox(s, i+1, i > 0, i < len(p.Steps)-1) {
			fmt.Fprintln(w, line)
		}
		if i == len(p.Steps)-1 {
			break
		}
		// The arrow leaving a step says what is now available — which is what
		// that step just contributed, not what the next one will.
		forked := s.Role == adapters.RoleFilter
		if forked {
			vizFork(w)
		}
		vizEdge(w, delta(s, seen), forked)
	}

	fmt.Fprintln(w)
	for _, note := range p.Notes {
		fmt.Fprintf(w, "note     %s\n", note)
	}
	for _, warning := range p.Warnings {
		fmt.Fprintf(w, "warning  %s\n", warning)
	}
}

// vizHeadline is the one-line summary: size, send count, and the per-record
// estimate with its gap visible (ADR-046 — unpriced steps are counted, never
// silently treated as zero).
func vizHeadline(p *Plan) string {
	var sends, unpriced int
	var est float64
	for i := range p.Steps {
		s := &p.Steps[i]
		if s.IsDeliver {
			sends++
		}
		switch {
		case s.CostEstimate != nil:
			est += *s.CostEstimate
		case !s.IsSource && !s.IsSQL:
			unpriced++
		}
	}
	head := fmt.Sprintf("%s · %s · est $%.4f/record",
		plural(len(p.Steps), "step"), plural(sends, "send"), est)
	if unpriced > 0 {
		head += fmt.Sprintf(" (%s unpriced)", plural(unpriced, "step"))
	}
	return head
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// delta is what this step adds to the available set, and advances it.
// Canonical fields lead: a vendor-namespaced name (§4a) says less about what
// the record now holds, so it is the first thing a truncated label drops.
func delta(s *Step, seen map[string]bool) []string {
	var canonical, vendor []string
	for _, f := range s.Provides {
		if seen[f] {
			continue
		}
		seen[f] = true
		if strings.Contains(f, ".") {
			vendor = append(vendor, f)
		} else {
			canonical = append(canonical, f)
		}
	}
	return append(canonical, vendor...)
}

// vizEdge draws the connector, labelled with the step's contribution to the
// available set. After a fork the `│ pass` line is already the connector, so an
// unlabelled edge adds nothing and is skipped.
func vizEdge(w io.Writer, added []string, forked bool) {
	pad := strings.Repeat(" ", vizSpine)
	if len(added) > 0 {
		fmt.Fprintf(w, "%s│  + %s\n", pad, fitFields(added, vizMaxLine-vizSpine-5))
	}
	arrow := pad + "▼"
	if forked {
		arrow += " pass"
	}
	fmt.Fprintln(w, arrow)
}

// vizFork is the filter's failing branch: SPEC §7 freezes such a record at the
// step rather than dropping it. The passing branch labels the arrow itself
// (see vizEdge), so the fork costs one line, not three.
func vizFork(w io.Writer) {
	fmt.Fprintln(w, strings.Repeat(" ", vizSpine)+"├──╴ fail — record freezes here")
}

// joinCapped lists at most n fields, summarising the rest as "+N".
func joinCapped(v []string, n int) string {
	if len(v) <= n {
		return strings.Join(v, ", ")
	}
	return fmt.Sprintf("%s, +%d", strings.Join(v[:n], ", "), len(v)-n)
}

// fitFields lists as many fields as avail columns hold, summarising the rest
// as "+N" — so a step providing twenty fields still names some of them rather
// than running off the edge or collapsing to a bare count.
func fitFields(v []string, avail int) string {
	for n := len(v); n > 1; n-- {
		if displayWidth(joinCapped(v, n)) <= avail {
			return joinCapped(v, n)
		}
	}
	return joinCapped(v, 1)
}

// vizBox renders one step. Shape carries role; the rows carry the facts.
// in/out say whether the spine enters from above and leaves below, so the
// frame can be joined to it rather than floating over it.
func vizBox(s *Step, n int, in, out bool) []string {
	head := fmt.Sprintf("%s%s %-3d %-10s %s", executorGlyph(s), roleGlyph(s), n, vizRole(s), s.Use)
	if s.Manifest != nil {
		head += fmt.Sprintf("@%d", s.Manifest.Version)
	}
	rows := [][2]string{{head, vizHeadRight(s)}}
	if l, r := vizGate(s); l != "" || r != "" {
		rows = append(rows, [2]string{"      " + l, r})
	}
	return vizFrame(s, rows, in, out)
}

// vizHeadRight is the top row's right column: the entity a source mints, and
// the price of everything downstream of it.
func vizHeadRight(s *Step) string {
	if s.IsSource {
		return s.EntityType
	}
	switch {
	case s.CostUnset:
		return "unset"
	case s.CostEstimate != nil:
		return fmt.Sprintf("$%.4f/rec", *s.CostEstimate)
	case s.IsSQL, s.IsGroupSource, s.IsGroupDeliver:
		// Runner-owned and ledger-local: no vendor to bill it.
		return "$0.0000"
	}
	return "$ ?"
}

// vizGate is the second row: what admits a record to this step, and what
// bounds the step's work.
func vizGate(s *Step) (left, right string) {
	switch {
	case s.IsGroupSource:
		left = "members of " + s.SourceGroup
	case s.IsGroupDeliver:
		left = "→ group " + strconv.Quote(s.TargetGroup)
	case s.When != "":
		left = "when " + s.When
	case s.IsDeliver && s.Idempotency != "":
		left = "keyed on " + s.Idempotency
	case len(s.Require) > 0:
		left = "members of " + strings.Join(s.Require, ", ")
	case s.Of != "":
		left = "of " + s.Of
	case len(s.Required) > 0:
		left = "requires " + joinCapped(s.Required, 3)
	case s.IsSQL:
		left = "offline · reads the ledger"
	}
	switch {
	case s.IsGroupSource && s.Limit > 0:
		right = fmt.Sprintf("limit %d, oldest first", s.Limit)
	case s.IsGroupDeliver:
		right = "handoff · no network"
	case s.IsDeliver && s.RecordGroup != "":
		right = "touch → " + s.RecordGroup
	case s.Cache > 0:
		right = "cache " + pipeline.FormatCache(s.Cache)
	case s.Deferred:
		right = "⏳ ends in flight"
	case s.Batch:
		right = fmt.Sprintf("batch %d", s.BatchSize)
	case s.Participant == adapters.KindHuman:
		right = "waits for a person"
	case s.Participant == adapters.KindAgent:
		// ADR-049: an agent/* step never prompts — it waits in the ledger.
		right = "waits for an agent"
	}
	return left, right
}

func vizRole(s *Step) string {
	switch {
	case s.IsSource:
		return "source"
	case s.IsDeliver:
		return "DELIVER"
	}
	return s.Role
}

// executorGlyph names who runs the step — the dimension the role cannot say.
// A sql/transform and an apollo/enrich are both role enrich, but one is free
// and offline and the other spends per record.
func executorGlyph(s *Step) string {
	switch {
	case s.IsGroupSource:
		return "👥"
	case s.IsGroupDeliver:
		return "📂"
	case s.IsSQL:
		return "💾"
	}
	switch s.Participant {
	case adapters.KindHuman:
		return "🧑"
	case adapters.KindAgent:
		return "🤖"
	case "ai":
		return "💻"
	}
	return "🌐"
}

// roleGlyph names what the step does to the record. ✍️ carries an explicit
// U+FE0F: bare U+270D defaults to text presentation and renders one column
// wide, which is the raggedness the fixed frame exists to prevent.
func roleGlyph(s *Step) string {
	switch {
	case s.IsSource:
		return "📥"
	case s.IsDeliver:
		return "🚀"
	}
	switch s.Role {
	case adapters.RoleFilter:
		return "🤏"
	case adapters.RoleVerify:
		return "👌"
	case adapters.RoleCompose:
		return "✍️"
	case adapters.RoleReview:
		return "👀"
	}
	return "💎"
}

// vizFrame draws rows inside the silhouette for s. The tapered shapes — the
// filter's funnel and the review step's trapezoid — step one column per row on
// each side, so their diagonals stack into a continuous line; a single-step
// diagonal kinks against the rule it meets. Every row stays centred on the
// same axis, so the two sides taper in step.
func vizFrame(s *Step, rows [][2]string, in, out bool) []string {
	rail, fill := "│", "─"
	tl, tr, bl, br := "┌", "┐", "└", "┘"
	up, down := "┴", "┬"
	botFill := ""
	// taper is the column each successive row moves inward (funnel) or
	// outward (trapezoid); 0 leaves the sides vertical.
	taper := 0

	switch {
	case s.IsSource:
		tl, tr, bl, br = "╭", "╮", "╰", "╯"
	case s.IsDeliver:
		rail, fill = "┃", "━"
		tl, tr, bl, br = "┏", "┓", "┗", "┛"
		// A light spine meeting a heavy rule: the joint keeps both weights.
		up, down = "┸", "┰"
	case s.Role == adapters.RoleFilter:
		rail, bl, br = "╲", "╲", "╱" // funnel: narrows toward the outlet
		taper = 1
	case s.Role == adapters.RoleReview:
		// Broken rails: the run pauses here. Not a taper — a review writes
		// fields and cannot reject (`when: <review>.passed` is refused), so it
		// is strictly 1:1, and in a diagram where the funnel's taper means
		// "fewer records out" a tapered review would claim what is not true.
		rail = "┊"
	case s.Role == adapters.RoleVerify:
		rail = "║" // adds no fields
		tl, tr, bl, br = "╓", "╖", "╙", "╜"
	case s.Role == adapters.RoleCompose:
		botFill = "~" // document
		bl, br = "╰", "╯"
	}
	if botFill == "" {
		botFill = fill
	}

	// A trapezoid starts at its narrow lid and widens; a funnel starts wide.
	steps := len(rows) + 1
	indent := vizIndent
	if taper < 0 {
		indent = vizIndent + steps
	}
	width := vizWidth - 2*(indent-vizIndent)

	line := func(left, right, f, joint string, joined bool) string {
		return strings.Repeat(" ", indent) + left + spineRule(f, joint, joined, indent, width) + right
	}
	advance := func() {
		indent += taper
		width -= 2 * taper
	}

	out2 := []string{line(tl, tr, fill, up, in)}
	for _, r := range rows {
		advance()
		lRail, rRail := rail, rail
		if taper > 0 {
			rRail = "╱"
		} else if taper < 0 {
			lRail, rRail = "╱", "╲"
		}
		out2 = append(out2, strings.Repeat(" ", indent)+lRail+vizRow(r[0], r[1], width-2)+rRail)
	}
	advance()
	return append(out2, line(bl, br, botFill, down, out))
}

// spineRule is a box's horizontal rule with the spine's joint set into it at
// the connector column, or plain fill when no edge meets it there.
func spineRule(fill, joint string, joined bool, indent, width int) string {
	if !joined {
		return strings.Repeat(fill, width-2)
	}
	at := vizSpine - indent - 1 // the joint's index within the fill
	return strings.Repeat(fill, at) + joint + strings.Repeat(fill, width-3-at)
}

// vizRow lays one row's left and right text into the frame's inner width,
// truncating the left rather than letting it overflow.
func vizRow(left, right string, inner int) string {
	left = clipWidth(left, inner-3-displayWidth(right))
	gap := inner - 2 - displayWidth(left) - displayWidth(right)
	if gap < 1 {
		gap = 1
	}
	return " " + left + strings.Repeat(" ", gap) + right + " "
}

func clipWidth(s string, max int) string {
	if displayWidth(s) <= max {
		return s
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := runeWidth(r)
		if w+rw > max-1 {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + "…"
}

// displayWidth is the terminal column count of s. Emoji occupy two columns
// while len() and the rune count both say one, so padding must measure rather
// than count — otherwise every box edge on an emoji row shifts.
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

func runeWidth(r rune) int {
	switch {
	case r == 0xFE0F:
		// Variation selector-16 promotes the preceding rune to emoji
		// presentation: it was counted narrow, and emoji presentation is wide.
		return 1
	case r == 0xFE0E, r == 0x200D, r >= 0x200B && r <= 0x200F:
		// Text selector, zero-width joiner, and the bidi marks take no columns.
		return 0
	case isWide(r):
		return 2
	}
	return 1
}

// isWide reports the East Asian Wide and Fullwidth ranges, which is where
// every emoji this vocabulary uses lives.
func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x231A && r <= 0x231B, // watch, hourglass
		r >= 0x23E9 && r <= 0x23F3, // media controls, ⏳
		r >= 0x25FD && r <= 0x25FE,
		r >= 0x2614 && r <= 0x2615,
		r >= 0x2648 && r <= 0x2653,
		r == 0x267F, r == 0x2693, r == 0x26A1,
		r >= 0x26AA && r <= 0x26AB,
		r >= 0x26BD && r <= 0x26BE,
		r >= 0x26C4 && r <= 0x26C5,
		r == 0x26CE, r == 0x26D4, r == 0x26EA,
		r >= 0x26F2 && r <= 0x26F3,
		r == 0x26F5, r == 0x26FA, r == 0x26FD,
		r == 0x2705,
		r >= 0x270A && r <= 0x270B, // raised fist/hand — wide without a selector
		r == 0x2728, r == 0x274C, r == 0x274E,
		r >= 0x2753 && r <= 0x2755,
		r == 0x2757,
		r >= 0x2795 && r <= 0x2797,
		r == 0x27B0, r == 0x27BF,
		r >= 0x2B1B && r <= 0x2B1C,
		r == 0x2B50, r == 0x2B55,
		r >= 0x2E80 && r <= 0x303E, // CJK radicals through symbols
		r >= 0x3041 && r <= 0x33FF,
		r >= 0x3400 && r <= 0x4DBF,
		r >= 0x4E00 && r <= 0x9FFF,
		r >= 0xA000 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFE10 && r <= 0xFE19,
		r >= 0xFE30 && r <= 0xFE6F,
		r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F300 && r <= 0x1F64F, // symbols, pictographs, emoticons
		r >= 0x1F680 && r <= 0x1F6FF, // transport
		r >= 0x1F7E0 && r <= 0x1F7EB,
		r >= 0x1F900 && r <= 0x1F9FF, // supplemental pictographs
		r >= 0x1FA70 && r <= 0x1FAFF,
		r >= 0x20000 && r <= 0x3FFFD:
		return true
	}
	return false
}
