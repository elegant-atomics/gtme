package aisteps

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/ai"
	"github.com/elegant-atomics/gtme/internal/protocol"
)

// scriptEngine returns canned answers and records the prompts it was asked.
type scriptEngine struct {
	mu        sync.Mutex
	answers   []string
	prompts   []string
	err       error
	callCount int
}

func (e *scriptEngine) Name() string { return "script" }

func (e *scriptEngine) Complete(ctx context.Context, req ai.Request) (ai.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.prompts = append(e.prompts, req.Prompt)
	i := e.callCount
	e.callCount++
	if e.err != nil {
		return ai.Response{}, e.err
	}
	if i >= len(e.answers) {
		i = len(e.answers) - 1
	}
	return ai.Response{
		Text:         e.answers[i],
		Model:        "script",
		Engine:       "script",
		InputTokens:  10,
		OutputTokens: 20,
		Priced:       true,
	}, nil
}

// drive runs an adapter over pipes with a batch of records.
func drive(t *testing.T, a *Adapter, config map[string]any, keys ...string) ([]protocol.Message, error) {
	t.Helper()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	go func() {
		w := protocol.NewWriter(inW)
		w.Write(protocol.Message{Type: protocol.TypeOpen, StepID: "step", RunID: "run1", Config: config})
		for _, k := range keys {
			w.Write(protocol.Record(protocol.Key{EntityType: "person", IdentityKey: k},
				map[string]any{"email": k, "title": "VP Marketing"}, nil))
		}
		w.Write(protocol.End())
		inW.Close()
	}()

	runErr := make(chan error, 1)
	go func() {
		err := a.Run(context.Background(), adapters.Ports{In: inR, Out: outW, Log: io.Discard})
		outW.CloseWithError(err)
		runErr <- err
	}()

	var msgs []protocol.Message
	r := protocol.NewReader(outR)
	for {
		m, err := r.Next()
		if err != nil {
			break
		}
		msgs = append(msgs, m)
	}
	return msgs, <-runErr
}

func TestFilterEmitsVerdicts(t *testing.T) {
	engine := &scriptEngine{answers: []string{
		`[{"identity_key":"a@x.com","pass":true,"reason":"in icp"},
		  {"identity_key":"b@x.com","pass":false,"reason":"wrong role"}]`,
	}}
	a := &Adapter{Mode: modeFilter, Engine: engine}

	msgs, err := drive(t, a, map[string]any{"prompt": "Keep decision makers."}, "a@x.com", "b@x.com")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var verdicts, costs int
	for _, m := range msgs {
		switch m.Type {
		case protocol.TypeVerdict:
			verdicts++
			switch m.Key.IdentityKey {
			case "a@x.com":
				if !m.Passed() || m.Reason != "in icp" {
					t.Errorf("a@x.com verdict = %+v", m)
				}
			case "b@x.com":
				if m.Passed() {
					t.Error("b@x.com should not pass")
				}
			}
		case protocol.TypeCost:
			costs++
			if m.Detail["input_tokens"] != float64(10) {
				t.Errorf("cost detail = %v", m.Detail)
			}
		}
	}
	if verdicts != 2 {
		t.Errorf("verdicts = %d, want 2", verdicts)
	}
	if costs != 1 {
		t.Errorf("cost messages = %d, want 1 per batch", costs)
	}

	// The operator's prompt and the batch both reach the model.
	if !strings.Contains(engine.prompts[0], "Keep decision makers.") ||
		!strings.Contains(engine.prompts[0], "a@x.com") {
		t.Errorf("prompt = %q", engine.prompts[0])
	}
}

func TestComposeEmitsFields(t *testing.T) {
	engine := &scriptEngine{answers: []string{
		`[{"identity_key":"a@x.com","first_line":"Saw your post","ps_line":"PS: nice logo"}]`,
	}}
	a := &Adapter{Mode: modeCompose, Engine: engine}

	msgs, err := drive(t, a, map[string]any{"prompt": "Write two lines."}, "a@x.com")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var records int
	for _, m := range msgs {
		if m.Type == protocol.TypeRecord {
			records++
			if m.Fields["first_line"] != "Saw your post" || m.Fields["ps_line"] != "PS: nice logo" {
				t.Errorf("fields = %v", m.Fields)
			}
		}
	}
	if records != 1 {
		t.Errorf("records = %d, want 1", records)
	}
}

func TestRetriesOnceThenSucceeds(t *testing.T) {
	engine := &scriptEngine{answers: []string{
		"Sure! Here you go: not actually json",
		`[{"identity_key":"a@x.com","pass":true,"reason":"ok"}]`,
	}}
	a := &Adapter{Mode: modeFilter, Engine: engine}

	msgs, err := drive(t, a, map[string]any{"prompt": "Keep them."}, "a@x.com")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if engine.callCount != 2 {
		t.Errorf("engine calls = %d, want 2 (one retry)", engine.callCount)
	}
	// The retry tells the model exactly what was wrong.
	if !strings.Contains(engine.prompts[1], "previous response was rejected") {
		t.Errorf("retry prompt = %q", engine.prompts[1])
	}
	found := false
	for _, m := range msgs {
		if m.Type == protocol.TypeVerdict && m.Passed() {
			found = true
		}
	}
	if !found {
		t.Error("expected a passing verdict after the retry")
	}
}

func TestFailsAfterOneRetry(t *testing.T) {
	engine := &scriptEngine{answers: []string{"nope", "still nope"}}
	a := &Adapter{Mode: modeFilter, Engine: engine}

	_, err := drive(t, a, map[string]any{"prompt": "Keep them."}, "a@x.com")
	if err == nil {
		t.Fatal("want an error when the model cannot produce valid output twice")
	}
	if engine.callCount != 2 {
		t.Errorf("engine calls = %d, want exactly 2", engine.callCount)
	}
	if !strings.Contains(err.Error(), "still invalid after one retry") {
		t.Errorf("error = %v", err)
	}
}

func TestValidationRejectsBadAnswers(t *testing.T) {
	records := []record{
		{key: protocol.Key{EntityType: "person", IdentityKey: "a@x.com"}},
		{key: protocol.Key{EntityType: "person", IdentityKey: "b@x.com"}},
	}
	filter := &Adapter{Mode: modeFilter}
	compose := &Adapter{Mode: modeCompose}

	cases := []struct {
		name    string
		adapter *Adapter
		text    string
		want    string
	}{
		{"not an array", filter, `{"identity_key":"a@x.com","pass":true}`, "not a JSON array"},
		{"invented key", filter,
			`[{"identity_key":"a@x.com","pass":true},{"identity_key":"nobody@x.com","pass":true}]`,
			"not in the batch"},
		{"missing a record", filter, `[{"identity_key":"a@x.com","pass":true}]`, "missing 1 of 2"},
		{"duplicate key", filter,
			`[{"identity_key":"a@x.com","pass":true},{"identity_key":"a@x.com","pass":false}]`,
			"more than once"},
		{"pass not boolean", filter,
			`[{"identity_key":"a@x.com","pass":"yes"},{"identity_key":"b@x.com","pass":true}]`,
			"pass must be true or false"},
		{"no identity key", filter, `[{"pass":true}]`, "no identity_key"},
		{"compose missing field", compose,
			`[{"identity_key":"a@x.com","first_line":"hi"},{"identity_key":"b@x.com","first_line":"hi","ps_line":"x"}]`,
			"missing ps_line"},
		{"compose wrong type", compose,
			`[{"identity_key":"a@x.com","first_line":42,"ps_line":"x"},{"identity_key":"b@x.com","first_line":"a","ps_line":"b"}]`,
			"first_line must be a string"},
		{"empty", filter, "   ", "response was empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sh, err := tc.adapter.shapeFor(config{})
			if err != nil {
				t.Fatalf("shapeFor: %v", err)
			}
			_, err = tc.adapter.parse(tc.text, sh, records)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestParseAcceptsFencedJSON(t *testing.T) {
	records := []record{{key: protocol.Key{EntityType: "person", IdentityKey: "a@x.com"}}}
	a := &Adapter{Mode: modeFilter}
	answers, err := a.parse("```json\n[{\"identity_key\":\"a@x.com\",\"pass\":true}]\n```", shape{}, records)
	if err != nil {
		t.Fatalf("a fenced answer should be recovered, not retried: %v", err)
	}
	if len(answers) != 1 {
		t.Errorf("answers = %v", answers)
	}
}

func TestEngineErrorsPropagate(t *testing.T) {
	engine := &scriptEngine{answers: []string{"unused"}, err: errors.New("rate limited")}
	a := &Adapter{Mode: modeFilter, Engine: engine}
	if _, err := drive(t, a, map[string]any{"prompt": "x"}, "a@x.com"); err == nil {
		t.Fatal("want the engine error to fail the batch")
	}
}

func TestPromptRequired(t *testing.T) {
	a := &Adapter{Mode: modeFilter, Engine: &scriptEngine{answers: []string{"[]"}}}
	if _, err := drive(t, a, map[string]any{}, "a@x.com"); err == nil {
		t.Fatal("want an error when prompt is missing")
	}
}

func TestFieldsRestriction(t *testing.T) {
	engine := &scriptEngine{answers: []string{`[{"identity_key":"a@x.com","pass":true}]`}}
	a := &Adapter{Mode: modeFilter, Engine: engine}
	if _, err := drive(t, a, map[string]any{"prompt": "x", "fields": []any{"title"}}, "a@x.com"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(engine.prompts[0], `"email"`) {
		t.Errorf("fields restriction should hide email:\n%s", engine.prompts[0])
	}
	if !strings.Contains(engine.prompts[0], "VP Marketing") {
		t.Errorf("title should still be shown:\n%s", engine.prompts[0])
	}
}

func TestManifestsAreRegistered(t *testing.T) {
	for _, id := range []string{FilterID, ComposeID} {
		resolved, err := adapters.Resolve(id)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", id, err)
		}
		if !resolved.Manifest.Batch {
			t.Errorf("%s must be a batch adapter", id)
		}
		// ADR-019: AI steps declare dynamic needs — uses: narrows them, and
		// without uses: they fall back to every field the ledger knows.
		if !resolved.Manifest.NeedsDynamic() || len(resolved.Manifest.NeedsFields()) != 0 {
			t.Errorf("%s should declare dynamic needs with no static fields", id)
		}
	}
	compose, err := adapters.Resolve(ComposeID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := compose.Manifest.ProvidesFields(); strings.Join(got, ",") != "first_line,ps_line" {
		t.Errorf("ai/compose provides = %v", got)
	}
}

// declared is a derived provides schema as the runner injects it (ADR-033):
// names already namespaced by the planner, declaration order under required.
var declared = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"properties": map[string]any{
		"qualify.state":     map[string]any{"type": "string", "enum": []any{"now", "later"}},
		"qualify.rationale": map[string]any{},
		"qualify.score":     map[string]any{"type": "integer"},
	},
	"required": []any{"qualify.state", "qualify.rationale", "qualify.score"},
}

// TestFilterWithDeclaredProvidesEmitsRecordAndVerdict is ADR-033 on the wire:
// the prompt's required shape is generated from the schema, the answer is
// validated against it (an enum violation is rejected and retried), and the
// filter emits a RECORD carrying the declared fields before its VERDICT —
// for the failing record too, so the reasoning is queryable either way.
func TestFilterWithDeclaredProvidesEmitsRecordAndVerdict(t *testing.T) {
	engine := &scriptEngine{answers: []string{
		`[{"identity_key":"a@x.com","pass":true,"reason":"fits","qualify.state":"never","qualify.rationale":"x","qualify.score":1},
		  {"identity_key":"b@x.com","pass":false,"reason":"no","qualify.state":"later","qualify.rationale":"y","qualify.score":2}]`,
		`[{"identity_key":"a@x.com","pass":true,"reason":"fits","qualify.state":" now ","qualify.rationale":"x","qualify.score":1},
		  {"identity_key":"b@x.com","pass":false,"reason":"no","qualify.state":"later","qualify.rationale":"y","qualify.score":2}]`,
	}}
	a := &Adapter{Mode: modeFilter, Engine: engine}
	msgs, err := drive(t, a, map[string]any{"prompt": "Judge.", "provides": declared}, "a@x.com", "b@x.com")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if engine.callCount != 2 {
		t.Errorf("engine calls = %d, want 2 (the enum violation is retried)", engine.callCount)
	}
	if !strings.Contains(engine.prompts[1], `qualify.state must be one of now, later (got "never")`) {
		t.Errorf("retry prompt should name the enum violation:\n%s", engine.prompts[1])
	}

	// SCHEMA announces the derived shape; RECORD precedes VERDICT per key.
	var order []string
	records := map[string]map[string]any{}
	for _, m := range msgs {
		switch m.Type {
		case protocol.TypeSchema:
			if !strings.Contains(string(m.Provides), `"qualify.state"`) {
				t.Errorf("SCHEMA = %s", m.Provides)
			}
		case protocol.TypeRecord:
			order = append(order, "record:"+m.Key.IdentityKey)
			records[m.Key.IdentityKey] = m.Fields
		case protocol.TypeVerdict:
			order = append(order, "verdict:"+m.Key.IdentityKey)
		}
	}
	if got := strings.Join(order, " "); got != "record:a@x.com verdict:a@x.com record:b@x.com verdict:b@x.com" {
		t.Errorf("message order = %s", got)
	}
	if records["a@x.com"]["qualify.state"] != "now" || records["a@x.com"]["qualify.score"] != float64(1) {
		t.Errorf("a@x.com fields = %v (strings are trimmed at the boundary)", records["a@x.com"])
	}
	if records["b@x.com"]["qualify.state"] != "later" {
		t.Errorf("the failing record's fields must still be emitted: %v", records["b@x.com"])
	}
	for _, f := range records["a@x.com"] {
		if f == nil {
			t.Errorf("a declared field arrived null: %v", records["a@x.com"])
		}
	}
	if _, has := records["a@x.com"]["pass"]; has {
		t.Errorf("pass belongs to the VERDICT, not the RECORD: %v", records["a@x.com"])
	}
}

// TestSystemPromptShapeIsGeneratedFromSchema: no literal shape string survives
// — the element shape names every declared field, with enum alternatives.
func TestSystemPromptShapeIsGeneratedFromSchema(t *testing.T) {
	a := &Adapter{Mode: modeFilter}
	cfg, err := parseConfig(map[string]any{"prompt": "x", "provides": declared})
	if err != nil {
		t.Fatal(err)
	}
	sh, err := a.shapeFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sys := a.systemPrompt(sh, cfg)
	for _, want := range []string{
		`"identity_key": "<copied exactly from the input>"`,
		`"pass": true or false`,
		`"qualify.state": "now" | "later"`,
		`"qualify.rationale": "<text>"`,
		`"qualify.score": <integer>`,
		"exactly one of them, verbatim",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt lacks %q:\n%s", want, sys)
		}
	}
	// The declaration order is the prompt order.
	if strings.Index(sys, "qualify.state") > strings.Index(sys, "qualify.rationale") ||
		strings.Index(sys, "qualify.rationale") > strings.Index(sys, "qualify.score") {
		t.Errorf("fields should follow declaration order:\n%s", sys)
	}

	// A compose declaring nothing keeps its manifest shape (first_line, ps_line).
	c := &Adapter{Mode: modeCompose}
	sh, err = c.shapeFor(config{})
	if err != nil {
		t.Fatal(err)
	}
	sys = c.systemPrompt(sh, config{})
	if !strings.Contains(sys, `"first_line": "<string>", "ps_line": "<string>"`) || strings.Contains(sys, `"pass"`) {
		t.Errorf("default compose shape:\n%s", sys)
	}
}

// TestComposeWithDeclaredProvidesReplacesTheDefault: a compose declaring
// its own fields emits exactly those — first_line/ps_line are gone.
func TestComposeWithDeclaredProvidesReplacesTheDefault(t *testing.T) {
	provides := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"outreach.subject": map[string]any{"type": "string"},
			"outreach.body":    map[string]any{"type": "string"},
		},
		"required": []any{"outreach.subject", "outreach.body"},
	}
	engine := &scriptEngine{answers: []string{
		`[{"identity_key":"a@x.com","outreach.subject":"Hi","outreach.body":"Long text","first_line":"ignored"}]`,
	}}
	a := &Adapter{Mode: modeCompose, Engine: engine}
	msgs, err := drive(t, a, map[string]any{"prompt": "Write.", "provides": provides}, "a@x.com")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range msgs {
		if m.Type != protocol.TypeRecord {
			continue
		}
		if m.Fields["outreach.subject"] != "Hi" || m.Fields["outreach.body"] != "Long text" {
			t.Errorf("fields = %v", m.Fields)
		}
		if _, has := m.Fields["first_line"]; has {
			t.Errorf("undeclared fields must not be emitted: %v", m.Fields)
		}
	}
}

// TestDeclaredValidationMessages covers what the retry tells the model.
func TestDeclaredValidationMessages(t *testing.T) {
	a := &Adapter{Mode: modeFilter}
	cfg, _ := parseConfig(map[string]any{"prompt": "x", "provides": declared})
	sh, err := a.shapeFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	records := []record{{key: protocol.Key{EntityType: "company", IdentityKey: "acme.com"}}}
	ok := `"qualify.rationale":"r","qualify.score":3`
	cases := []struct{ name, text, want string }{
		{"missing", `[{"identity_key":"acme.com","pass":true,"qualify.state":"now","qualify.score":3}]`, "missing qualify.rationale"},
		{"null", `[{"identity_key":"acme.com","pass":true,"qualify.state":"now","qualify.rationale":null,"qualify.score":3}]`, "qualify.rationale must not be null"},
		{"enum", `[{"identity_key":"acme.com","pass":true,"qualify.state":"soon",` + ok + `}]`, `qualify.state must be one of now, later (got "soon")`},
		{"enum type", `[{"identity_key":"acme.com","pass":true,"qualify.state":3,` + ok + `}]`, "qualify.state must be a string"},
		{"integer", `[{"identity_key":"acme.com","pass":true,"qualify.state":"now","qualify.rationale":"r","qualify.score":3.5}]`, "qualify.score must be an integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.parse(tc.text, sh, records)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// splitRecorder keeps every ai.Request an engine was handed.
type splitRecorder struct {
	inner ai.Engine
	reqs  []ai.Request
}

func (s *splitRecorder) Name() string { return s.inner.Name() }
func (s *splitRecorder) Complete(ctx context.Context, req ai.Request) (ai.Response, error) {
	s.reqs = append(s.reqs, req)
	return s.inner.Complete(ctx, req)
}
