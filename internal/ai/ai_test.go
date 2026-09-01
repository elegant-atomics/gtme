package ai

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPriceKnownAndUnknownModels(t *testing.T) {
	// 1M input + 1M output on Sonnet-tier list pricing.
	cost, ok := Price("claude-sonnet-4-6", 1_000_000, 1_000_000, 0, 0)
	if !ok {
		t.Fatal("claude-sonnet-4-6 should be priceable")
	}
	if math.Abs(cost-18.0) > 1e-9 {
		t.Errorf("cost = %v, want 18 ($3 in + $15 out)", cost)
	}

	// Cache reads are a tenth of input; writes are 1.25x.
	cost, _ = Price("claude-sonnet-4-6", 0, 0, 1_000_000, 1_000_000)
	if math.Abs(cost-(0.3+3.75)) > 1e-9 {
		t.Errorf("cache cost = %v, want 4.05", cost)
	}

	if _, ok := Price("some-other-vendor-model", 100, 100, 0, 0); ok {
		t.Error("an unknown model must report unpriced rather than guessing")
	}
	// A dated snapshot prices like its family.
	if _, ok := Price("claude-haiku-4-5-20251001", 10, 10, 0, 0); !ok {
		t.Error("dated snapshots should match their family")
	}
}

func TestResolveRejectsUnknownEngine(t *testing.T) {
	t.Setenv("GTME_AI_ENGINE", "")
	if _, _, err := Resolve("telepathy", "", nil); err == nil {
		t.Fatal("want an error for an unknown engine")
	}
}

func TestResolveDefaultsModelAndHonoursEnv(t *testing.T) {
	fixture := writeFixture(t, `["[]"]`)
	t.Setenv("GTME_AI_ENGINE", EngineFixture)
	t.Setenv("GTME_AI_FIXTURE", fixture)

	engine, model, err := Resolve("api", "", nil) // env overrides the configured engine
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if engine.Name() != EngineFixture {
		t.Errorf("engine = %q, want fixture", engine.Name())
	}
	if model != DefaultModel {
		t.Errorf("model = %q, want %q", model, DefaultModel)
	}

	t.Setenv("GTME_AI_MODEL", "claude-haiku-4-5")
	if _, model, err = Resolve("", "", nil); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if model != "claude-haiku-4-5" {
		t.Errorf("model = %q, want the env override", model)
	}
	if _, model, err = Resolve("", "claude-opus-5", nil); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if model != "claude-opus-5" {
		t.Errorf("model = %q, want the step's model to win over the env", model)
	}
}

func TestAPIEngineNeedsAKey(t *testing.T) {
	t.Setenv("GTME_AI_ENGINE", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	_, _, err := Resolve(EngineAPI, "", nil)
	if err == nil {
		t.Fatal("want an error when ANTHROPIC_API_KEY is unset")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("error = %v", err)
	}
}

func TestFixtureEngineReplaysScriptThenRepeatsLast(t *testing.T) {
	fixture := writeFixture(t, `["first","second"]`)
	t.Setenv("GTME_AI_ENGINE", EngineFixture)
	t.Setenv("GTME_AI_FIXTURE", fixture)

	engine, _, err := Resolve("", "", nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"first", "second", "second"}
	for i, w := range want {
		res, err := engine.Complete(context.Background(), Request{Prompt: "p"})
		if err != nil {
			t.Fatalf("Complete %d: %v", i, err)
		}
		if res.Text != w {
			t.Errorf("response %d = %q, want %q", i, res.Text, w)
		}
	}
}

func TestFixtureEngineAutoAnswers(t *testing.T) {
	fixture := writeFixture(t, `["$auto","$auto"]`)
	t.Setenv("GTME_AI_ENGINE", EngineFixture)
	t.Setenv("GTME_AI_FIXTURE", fixture)

	engine, _, err := Resolve("", "", nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	res, err := engine.Complete(context.Background(), Request{Kind: "filter", Keys: []string{"a@x.com", "b@x.com"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var filters []struct {
		IdentityKey string `json:"identity_key"`
		Pass        bool   `json:"pass"`
	}
	if err := json.Unmarshal([]byte(res.Text), &filters); err != nil {
		t.Fatalf("auto filter answer is not valid JSON: %v (%s)", err, res.Text)
	}
	if len(filters) != 2 || !filters[0].Pass || filters[0].IdentityKey != "a@x.com" {
		t.Errorf("auto filter answer = %+v", filters)
	}

	// The adapter hands the engine its output shape (ADR-033) — here the
	// compose default — and the answer is synthesized from it.
	res, err = engine.Complete(context.Background(), Request{Kind: "compose", Keys: []string{"a@x.com"},
		Fields: []FieldShape{{Name: "first_line", Type: "string"}, {Name: "ps_line", Type: "string"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var composed []struct {
		IdentityKey string `json:"identity_key"`
		FirstLine   string `json:"first_line"`
		PSLine      string `json:"ps_line"`
	}
	if err := json.Unmarshal([]byte(res.Text), &composed); err != nil {
		t.Fatalf("auto compose answer is not valid JSON: %v (%s)", err, res.Text)
	}
	if len(composed) != 1 || composed[0].FirstLine != "Fixture first line for a@x.com" || composed[0].PSLine == "" {
		t.Errorf("auto compose answer = %+v", composed)
	}

	// A declared shape (ADR-033): enum fields take their first member, typed
	// fields a typed sample, so the synthesized answer is always schema-valid.
	res, err = engine.Complete(context.Background(), Request{Kind: "filter", Keys: []string{"a@x.com"},
		Fields: []FieldShape{
			{Name: "q.state", Enum: []string{"now", "later"}},
			{Name: "q.score", Type: "integer"},
			{Name: "q.hot", Type: "boolean"},
			{Name: "q.rationale"},
		}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var judged []map[string]any
	if err := json.Unmarshal([]byte(res.Text), &judged); err != nil {
		t.Fatalf("auto declared answer is not valid JSON: %v (%s)", err, res.Text)
	}
	got := judged[0]
	if got["pass"] != true || got["q.state"] != "now" || got["q.score"] != float64(0) ||
		got["q.hot"] != true || got["q.rationale"] != "Fixture rationale for a@x.com" {
		t.Errorf("auto declared answer = %v", got)
	}
}

func TestFixtureEngineNeedsAScript(t *testing.T) {
	t.Setenv("GTME_AI_ENGINE", EngineFixture)
	t.Setenv("GTME_AI_FIXTURE", "")
	if _, _, err := Resolve("", "", nil); err == nil {
		t.Fatal("want an error without GTME_AI_FIXTURE")
	}
	t.Setenv("GTME_AI_FIXTURE", writeFixture(t, `{"not":"an array"}`))
	if _, _, err := Resolve("", "", nil); err == nil {
		t.Fatal("want an error for a malformed script")
	}
}

func TestResponseDetail(t *testing.T) {
	res := Response{Model: "m", Engine: "e", InputTokens: 3, OutputTokens: 4, Priced: true}
	d := res.Detail()
	if d["model"] != "m" || d["input_tokens"] != 3 || d["priced"] != true {
		t.Errorf("detail = %v", d)
	}
}

// writeFixture writes a script file and returns its path. Each call uses a unique
// name so the per-path engine cache does not leak between tests.
func writeFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// TestResolveUsesCallerEnvForAPIKey pins the credentials path a built-in
// adapter actually has (SPEC §6): the runner injects ~/.gtme/secrets into the
// session env, never the process env. Found by the first live compose run,
// which failed with a key stored via `gtme secret set`.
func TestResolveUsesCallerEnvForAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("GTME_AI_ENGINE", "")
	getenv := func(k string) string {
		if k == "ANTHROPIC_API_KEY" {
			return "session-injected"
		}
		return ""
	}
	if _, _, err := Resolve(EngineAPI, "", getenv); err != nil {
		t.Fatalf("Resolve must see the session-injected key: %v", err)
	}
	if _, _, err := Resolve(EngineAPI, "", nil); err == nil {
		t.Fatal("nil getenv with an empty process env should fail")
	}
}

// ADR-046: the claude CLI's total_cost_usd is vendor-reported cost metadata,
// so a response carrying it is measured; one without it is priced from our
// own table, which is an estimate however exact the token counts are.
func TestClaudeCodeCostBasis(t *testing.T) {
	measured, err := parseClaudeCodeOutput([]byte(`{"result":"ok","model":"claude-sonnet-5","total_cost_usd":0.0123,"usage":{"input_tokens":10,"output_tokens":5}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if !measured.Measured || measured.CostUSD != 0.0123 || !measured.Priced {
		t.Errorf("reported cost: measured=%v cost=%v priced=%v, want measured 0.0123", measured.Measured, measured.CostUSD, measured.Priced)
	}
	estimated, err := parseClaudeCodeOutput([]byte(`{"result":"ok","model":"claude-sonnet-5","usage":{"input_tokens":1000,"output_tokens":500}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if estimated.Measured {
		t.Error("a table-priced response must not claim measured")
	}
	if !estimated.Priced || estimated.CostUSD == 0 {
		t.Errorf("table pricing: cost=%v priced=%v, want a priced estimate", estimated.CostUSD, estimated.Priced)
	}
	if d := measured.Detail(); d["basis"] != "measured" {
		t.Errorf("detail basis = %v, want measured", d["basis"])
	}
}
