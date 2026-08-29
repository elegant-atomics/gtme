// Package ai is the engine layer behind AI steps. Engines are interchangeable
// (SPEC §2): the Anthropic Messages API by default, the claude CLI when the
// operator prefers it, and a scripted fixture engine so tests never touch the
// network.
package ai

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// Engine names (SPEC §2). "fixture" is test-only and selected by environment,
// never by pipeline config.
const (
	EngineAPI        = "api"
	EngineClaudeCode = "claude-code"
	EngineFixture    = "fixture"
)

// DefaultModel is the model AI steps use unless a step overrides it (SPEC §2).
const DefaultModel = "claude-sonnet-4-6"

// DefaultMaxTokens bounds one batch's response.
const DefaultMaxTokens = 8192

// Request is one completion.
type Request struct {
	System string
	// Prompt is the whole user turn — Shared then Payload in the stated
	// order (SPEC §10.3, ADR-035) — for engines that take one string.
	Prompt string
	// Shared and Payload are the two halves of Prompt, exposed so an engine
	// can place a cache breakpoint between them and so the order is A/B-able
	// without touching assembly: Shared is what every batch of a step sends
	// alike (the operator's prompt), Payload is this batch's records. Either
	// may be empty, in which case Prompt is authoritative.
	Shared    string
	Payload   string
	Model     string
	MaxTokens int
	// Keys are the identity keys of the records in this batch. Engines use them
	// for logging; the fixture engine uses them to synthesize valid answers.
	Keys []string
	// Kind is "filter" or "compose", for the same reason.
	Kind string
	// Fields is the step's output shape beyond identity_key (and, for a
	// filter, pass/reason): the declared or default provides (ADR-033), in
	// order. The fixture engine synthesizes a value per field from it.
	Fields []FieldShape
}

// FieldShape is one output field an AI step expects back from the model.
type FieldShape struct {
	Name string
	// Type is a JSON-Schema primitive type, or "" when the field is untyped.
	Type string
	// Enum is the declared value domain, if any.
	Enum []string
}

// Response is what an engine returns, including what it cost.
type Response struct {
	Text   string
	Model  string
	Engine string

	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int

	// CostUSD is 0 when the engine cannot price the call; Priced says which.
	CostUSD float64
	Priced  bool
}

// Detail is the COST message detail an adapter reports (SPEC §5).
func (r Response) Detail() map[string]any {
	return map[string]any{
		"model":         r.Model,
		"engine":        r.Engine,
		"input_tokens":  r.InputTokens,
		"output_tokens": r.OutputTokens,
		"priced":        r.Priced,
	}
}

// Engine turns a prompt into text.
type Engine interface {
	Name() string
	Complete(ctx context.Context, req Request) (Response, error)
}

// Resolve picks an engine. The step's config chooses between the engines the
// spec defines; GTME_AI_ENGINE overrides it so tests (and an operator debugging a
// pipeline) can swap in the fixture engine without editing the pipeline. getenv
// is the caller's env view — an adapter passes its Ports.Getenv so credentials
// injected by the runner (including ~/.gtme/secrets) are seen; nil falls back to
// the process env.
func Resolve(engine, model string, getenv func(string) string) (Engine, string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	// The Ports view is consulted first so the runner can put a step onto the
	// fixture engine under --simulate (SPEC §8) without touching process env;
	// process env still works for tests and operators.
	if v := envOverride(getenv, "GTME_AI_ENGINE"); v != "" {
		engine = v
	}
	if model == "" {
		model = envOverride(getenv, "GTME_AI_MODEL")
	}
	if model == "" {
		model = DefaultModel
	}

	switch strings.TrimSpace(engine) {
	case "", EngineAPI:
		e, err := newAPIEngine(getenv)
		return e, model, err
	case EngineClaudeCode:
		e, err := newClaudeCodeEngine()
		return e, model, err
	case EngineFixture:
		e, err := newFixtureEngine(getenv)
		return e, model, err
	default:
		return nil, model, fmt.Errorf("ai: unknown engine %q (want %q or %q)", engine, EngineAPI, EngineClaudeCode)
	}
}

func envOverride(getenv func(string) string, key string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return os.Getenv(key)
}

// ProvenanceModel is the model identifier ai/* provenance records (SPEC §10a,
// ADR-026: `ai/compose @ <model-id>`). It mirrors Resolve's engine/model
// resolution without constructing an engine, so the runner — which writes
// provenance — computes the same answer the adapter will. The fixture engine
// reports "fixture", which is what marks simulated judgments as synthetic.
func ProvenanceModel(engine, model string, getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	if v := envOverride(getenv, "GTME_AI_ENGINE"); v != "" {
		engine = v
	}
	if strings.TrimSpace(engine) == EngineFixture {
		return "fixture"
	}
	if model == "" {
		model = envOverride(getenv, "GTME_AI_MODEL")
	}
	if model == "" {
		model = DefaultModel
	}
	return model
}

// BatchEngine is an engine that can take a batch of requests now and answer
// later under a token (SPEC §5/§8, ADR-038): the Message Batches API for the
// api engine, a scripted stand-in for the fixture engine. Engines without a
// batch surface (claude-code) do not implement it, and a deferred step on
// them answers synchronously.
type BatchEngine interface {
	Engine
	// Submit dispatches every request, keyed by its CustomID, and returns the
	// provider's handle.
	Submit(ctx context.Context, reqs []BatchRequest) (token string, err error)
	// Collect fetches results for a token. ready=false means the provider is
	// still processing; results is keyed by CustomID, and a request the
	// provider answered with an error carries that error.
	Collect(ctx context.Context, token string) (results map[string]BatchResult, ready bool, err error)
}

// BatchRequest is one request inside a batch.
type BatchRequest struct {
	CustomID string
	Request  Request
}

// BatchResult is one request's outcome.
type BatchResult struct {
	Response Response
	Err      error
}

// Deferrable reports whether an engine can defer a batch right now.
func Deferrable(e Engine) bool {
	be, ok := e.(BatchEngine)
	if !ok {
		return false
	}
	if d, ok := be.(interface{ Deferrable() bool }); ok {
		return d.Deferrable()
	}
	return true
}
