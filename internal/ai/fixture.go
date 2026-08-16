package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// fixtureEngine replays scripted responses from a file named by GTM_AI_FIXTURE.
// It is how the AI steps are tested offline (SPEC §11 M5: "tests use a fake
// engine"), including the malformed-output-then-retry path.
//
// The file is a JSON array of responses. Each entry is either a literal string
// to return, or the sentinel "$auto", which synthesizes a schema-valid answer
// for whatever batch is in flight — so a test can exercise batching without
// hard-coding identity keys.
type fixtureEngine struct {
	mu        sync.Mutex
	responses []string
	next      int
}

// FixtureAuto is the sentinel that makes the fixture engine answer correctly.
const FixtureAuto = "$auto"

// fixtures caches one engine per script path, so every AI step in a process
// draws from the same script in order — a run is one script, not one per step.
var (
	fixturesMu sync.Mutex
	fixtures   = map[string]*fixtureEngine{}
)

func newFixtureEngine(getenv func(string) string) (Engine, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	path := envOverride(getenv, "GTM_AI_FIXTURE")
	if path == "" {
		return nil, fmt.Errorf("ai: engine fixture needs GTM_AI_FIXTURE to point at a responses file")
	}

	fixturesMu.Lock()
	defer fixturesMu.Unlock()
	if e, ok := fixtures[path]; ok {
		return e, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ai: reading fixture %s: %w", path, err)
	}
	var responses []string
	if err := json.Unmarshal(raw, &responses); err != nil {
		return nil, fmt.Errorf("ai: fixture %s must be a JSON array of strings: %w", path, err)
	}
	e := &fixtureEngine{responses: responses}
	fixtures[path] = e
	return e, nil
}

func (e *fixtureEngine) Name() string { return EngineFixture }

// Complete returns the next scripted response, repeating the last one once the
// script runs out.
func (e *fixtureEngine) Complete(ctx context.Context, req Request) (Response, error) {
	e.mu.Lock()
	if len(e.responses) == 0 {
		e.mu.Unlock()
		return Response{}, fmt.Errorf("ai: fixture script is empty")
	}
	i := e.next
	if i >= len(e.responses) {
		i = len(e.responses) - 1
	}
	e.next++
	text := e.responses[i]
	e.mu.Unlock()

	if strings.TrimSpace(text) == FixtureAuto {
		text = autoAnswer(req)
	}
	return Response{
		Text:         text,
		Model:        "fixture",
		Engine:       EngineFixture,
		InputTokens:  len(req.Prompt) / 4,
		OutputTokens: len(text) / 4,
		CostUSD:      0,
		Priced:       true,
	}, nil
}

// autoAnswer builds a valid response for the batch: pass everything for a
// filter, and formulaic lines for a compose.
func autoAnswer(req Request) string {
	type filterItem struct {
		IdentityKey string `json:"identity_key"`
		Pass        bool   `json:"pass"`
		Reason      string `json:"reason"`
	}
	type composeItem struct {
		IdentityKey string `json:"identity_key"`
		FirstLine   string `json:"first_line"`
		PSLine      string `json:"ps_line"`
	}

	var out any
	switch req.Kind {
	case "compose":
		items := make([]composeItem, 0, len(req.Keys))
		for _, k := range req.Keys {
			items = append(items, composeItem{
				IdentityKey: k,
				FirstLine:   "Fixture first line for " + k,
				PSLine:      "Fixture ps line for " + k,
			})
		}
		out = items
	default:
		items := make([]filterItem, 0, len(req.Keys))
		for _, k := range req.Keys {
			items = append(items, filterItem{IdentityKey: k, Pass: true, Reason: "fixture pass"})
		}
		out = items
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "[]"
	}
	return string(raw)
}
