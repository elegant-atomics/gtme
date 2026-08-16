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
	System    string
	Prompt    string
	Model     string
	MaxTokens int
	// Keys are the identity keys of the records in this batch. Engines use them
	// for logging; the fixture engine uses them to synthesize valid answers.
	Keys []string
	// Kind is "filter" or "compose", for the same reason.
	Kind string
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
// spec defines; GTM_AI_ENGINE overrides it so tests (and an operator debugging a
// pipeline) can swap in the fixture engine without editing the pipeline. getenv
// is the caller's env view — an adapter passes its Ports.Getenv so credentials
// injected by the runner (including ~/.gtm/secrets) are seen; nil falls back to
// the process env.
func Resolve(engine, model string, getenv func(string) string) (Engine, string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if v := os.Getenv("GTM_AI_ENGINE"); v != "" {
		engine = v
	}
	if model == "" {
		model = os.Getenv("GTM_AI_MODEL")
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
		e, err := newFixtureEngine()
		return e, model, err
	default:
		return nil, model, fmt.Errorf("ai: unknown engine %q (want %q or %q)", engine, EngineAPI, EngineClaudeCode)
	}
}
