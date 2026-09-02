package participant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

const gradeSchema = `{"type":"object","additionalProperties":false,"properties":{"review.grade":{"type":"string","enum":["A","B","C"]},"review.note":{}},"required":["review.grade","review.note"]}`

func TestParseRefusesOutsideTheContract(t *testing.T) {
	c, err := ContractFor("review", json.RawMessage(gradeSchema))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Parse(map[string]string{"review.grade": "Z", "review.note": "x"}); err == nil || !strings.Contains(err.Error(), "must be one of A, B, C") {
		t.Errorf("enum refusal = %v", err)
	}
	if _, err := c.Parse(map[string]string{"review.grade": "B"}); err == nil || !strings.Contains(err.Error(), "missing review.note") {
		t.Errorf("missing refusal = %v", err)
	}
	if _, err := c.Parse(map[string]string{"review.grade": "B", "review.note": "ok", "pass": "true"}); err == nil || !strings.Contains(err.Error(), "not an output of this step") {
		t.Errorf("unknown refusal = %v", err)
	}
	a, err := c.Parse(map[string]string{"review.grade": "B", "review.note": "fine"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Fields["review.grade"] != "B" || a.Fields["review.note"] != "fine" {
		t.Errorf("parsed = %+v", a.Fields)
	}

	f := Contract{Role: "filter"}
	if _, err := f.Parse(map[string]string{"reason": "no"}); err == nil {
		t.Error("a filter needs pass")
	}
	v, err := f.Parse(map[string]string{"pass": "n", "reason": "not a fit"})
	if err != nil || v.Pass || v.Reason != "not a fit" || len(v.Fields) != 0 {
		t.Errorf("filter parse = %+v, %v", v, err)
	}
	if _, err := f.Parse(map[string]string{"pass": "maybe"}); err == nil {
		t.Error("pass=maybe must be refused")
	}
	typed := Contract{Role: "compose", Fields: []Field{{Name: "n", Type: "integer"}}}
	if _, err := typed.Parse(map[string]string{"n": "3.5"}); err == nil {
		t.Error("a non-integer must be refused")
	}
	if got, err := typed.Parse(map[string]string{"n": "3"}); err != nil || got.Fields["n"] != float64(3) {
		t.Errorf("integer parse = %v, %v", got.Fields, err)
	}
}

func TestRenderSurfaces(t *testing.T) {
	fields := map[string]any{"first_line": "Hi Jane", "title": "VP", "email": "j@x.com"}
	s := Surface{Of: "first_line", Uses: []string{"title"}}
	got := s.Render(fields)
	if !strings.HasPrefix(got, `first_line (the value under review): "Hi Jane"`) || !strings.Contains(got, `title: "VP"`) || strings.Contains(got, "email") {
		t.Errorf("render = %q", got)
	}
	tpl := Surface{Template: "{{title}} — {{first_line}} ({{missing}})"}
	if got := tpl.Render(fields); got != "VP — Hi Jane ((no missing))" {
		t.Errorf("template = %q", got)
	}
}

func TestWalkAsksValidatesAndStopsOnEOF(t *testing.T) {
	c, _ := ContractFor("review", json.RawMessage(gradeSchema))
	var out bytes.Buffer
	// Record one: a bad grade, re-asked, then B and a note. Record two: EOF
	// after the grade — the rest stays pending.
	in := strings.NewReader("Z\nB\nlooks fine\nA\n")
	w := &Walker{In: in, Out: &out, Contract: c, Surface: Surface{Of: "first_line"}, StepID: "grade", Adapter: "human/review"}
	var got []Answer
	n, err := w.Walk(context.Background(), []Pending{
		{IdentityKey: "jane", Fields: map[string]any{"first_line": "Hi"}},
		{IdentityKey: "bob", Fields: map[string]any{"first_line": "Yo"}},
	}, func(p Pending, a Answer) error { got = append(got, a); return nil })
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v, want ErrInterrupted at EOF", err)
	}
	if n != 1 || len(got) != 1 || got[0].Fields["review.grade"] != "B" || got[0].Fields["review.note"] != "looks fine" {
		t.Errorf("answered = %d, got %+v", n, got)
	}
	if !strings.Contains(out.String(), "must be one of A, B, C") {
		t.Errorf("the bad grade should be refused on the spot:\n%s", out.String())
	}

	// Cancellation mid-walk is the Ctrl-C case.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pr, _ := io.Pipe()
	w = &Walker{In: pr, Out: &out, Contract: c}
	if _, err := w.Walk(ctx, []Pending{{IdentityKey: "jane"}}, func(Pending, Answer) error { return nil }); !errors.Is(err, ErrInterrupted) {
		t.Errorf("cancelled walk err = %v", err)
	}
}
