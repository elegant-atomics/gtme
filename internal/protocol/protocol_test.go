package protocol

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	key := Key{EntityType: "person", IdentityKey: "jane@acme.com"}
	msgs := []Message{
		{Type: TypeOpen, StepID: "enrich", RunID: "run1", Config: map[string]any{"note": "hi"}},
		Record(key, map[string]any{"headline": "VP Marketing"}, map[string]float64{"headline": 0.9}),
		Verdict(key, true, "in ICP"),
		Cost(&key, "harvest", 0.012, map[string]any{"credits": 1}),
		Cost(nil, "openai", 0.5, nil),
		Log("warn", "rate limited"),
		End(),
	}
	for _, m := range msgs {
		if err := w.Write(m); err != nil {
			t.Fatalf("Write(%s): %v", m.Type, err)
		}
	}
	if lines := strings.Count(buf.String(), "\n"); lines != len(msgs) {
		t.Errorf("wrote %d lines, want %d (one per message)", lines, len(msgs))
	}

	r := NewReader(bytes.NewReader(buf.Bytes()))
	for i, want := range msgs {
		got, err := r.Next()
		if err != nil {
			t.Fatalf("Next %d: %v", i, err)
		}
		if got.Type != want.Type {
			t.Fatalf("message %d type = %q, want %q", i, got.Type, want.Type)
		}
		switch got.Type {
		case TypeOpen:
			if got.RunID != "run1" || got.StepID != "enrich" || got.Config["note"] != "hi" {
				t.Errorf("OPEN = %+v", got)
			}
		case TypeRecord:
			if got.Key == nil || *got.Key != key {
				t.Errorf("RECORD key = %+v", got.Key)
			}
			if got.Fields["headline"] != "VP Marketing" || got.Confidence["headline"] != 0.9 {
				t.Errorf("RECORD payload = %+v / %+v", got.Fields, got.Confidence)
			}
		case TypeVerdict:
			if !got.Passed() || got.Reason != "in ICP" {
				t.Errorf("VERDICT = %+v", got)
			}
		case TypeCost:
			if got.Provider == "harvest" && got.Amount() != 0.012 {
				t.Errorf("COST amount = %v", got.Amount())
			}
			if got.Provider == "openai" && got.Key != nil {
				t.Errorf("step-level COST should have no key, got %+v", got.Key)
			}
		case TypeLog:
			if got.Level != "warn" || got.Msg != "rate limited" {
				t.Errorf("LOG = %+v", got)
			}
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Errorf("error after last message = %v, want io.EOF", err)
	}
}

func TestVerdictWithoutPassIsFail(t *testing.T) {
	r := NewReader(strings.NewReader(`{"type":"VERDICT","key":{"entity_type":"person","identity_key":"a@b.c"}}` + "\n"))
	m, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if m.Passed() {
		t.Error("a verdict that does not say pass must not pass")
	}
}

func TestReaderSkipsBlankLinesAndKeepsUnknownTypes(t *testing.T) {
	in := "\n" + `{"type":"FUTURE","whatever":1}` + "\n\n" + `{"type":"END"}` + "\n"
	r := NewReader(strings.NewReader(in))

	first, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if first.Type != "FUTURE" {
		t.Errorf("type = %q, want FUTURE (unknown types reach the caller, which ignores them)", first.Type)
	}
	second, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if second.Type != TypeEnd {
		t.Errorf("type = %q, want END", second.Type)
	}
}

func TestReaderRejectsMalformedAndTypelessLines(t *testing.T) {
	if _, err := NewReader(strings.NewReader("{not json}\n")).Next(); err == nil {
		t.Error("want an error for a malformed line; dropping it would lose a record")
	}
	if _, err := NewReader(strings.NewReader(`{"key":{}}` + "\n")).Next(); err == nil {
		t.Error("want an error for a line with no type")
	}
}

func TestReaderHandlesLongLines(t *testing.T) {
	long := strings.Repeat("x", 200*1024)
	var buf bytes.Buffer
	if err := NewWriter(&buf).Write(Record(Key{"person", "a@b.c"}, map[string]any{"bio": long}, nil)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	m, err := NewReader(bytes.NewReader(buf.Bytes())).Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got := m.Fields["bio"].(string); len(got) != len(long) {
		t.Errorf("bio length = %d, want %d", len(got), len(long))
	}
}

func TestKeyString(t *testing.T) {
	if got := (Key{"person", "jane@acme.com"}).String(); got != "person:jane@acme.com" {
		t.Errorf("String() = %q", got)
	}
	if !(Key{}).Zero() {
		t.Error("empty key should be Zero")
	}
}

// ADR-046: a COST carries its basis on the wire, and an unlabeled amount is
// estimated — a dollar figure is a guess until proven otherwise.
func TestCostBasis(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	measured := Cost(nil, "claude-code", 0.02, nil)
	measured.Basis = BasisMeasured
	if err := w.Write(measured); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Cost(nil, "harvest", 0.012, nil)); err != nil {
		t.Fatal(err)
	}
	// An adapter that predates ADR-046 (or a foreign one) sends no basis.
	if err := w.Write(Message{Type: TypeCost, Provider: "mock"}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if !strings.Contains(lines[0], `"basis":"measured"`) {
		t.Errorf("measured COST on the wire = %s, want a basis member", lines[0])
	}
	// Built-ins label every emission: Cost() says estimated out loud.
	if !strings.Contains(lines[1], `"basis":"estimated"`) {
		t.Errorf("Cost() on the wire = %s, want basis estimated", lines[1])
	}

	r := NewReader(bytes.NewReader(buf.Bytes()))
	got, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if got.CostBasis() != BasisMeasured {
		t.Errorf("CostBasis() = %q, want measured", got.CostBasis())
	}
	got, err = r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if got.CostBasis() != BasisEstimated {
		t.Errorf("CostBasis() of Cost() = %q, want estimated", got.CostBasis())
	}
	got, err = r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if got.Basis != "" || got.CostBasis() != BasisEstimated {
		t.Errorf("CostBasis() of an unlabeled COST = %q (wire %q), want estimated", got.CostBasis(), got.Basis)
	}
}
