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
			_, err := tc.adapter.parse(tc.text, records)
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
	answers, err := a.parse("```json\n[{\"identity_key\":\"a@x.com\",\"pass\":true}]\n```", records)
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
