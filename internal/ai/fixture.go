package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// fixtureEngine replays scripted responses from a file named by GTME_AI_FIXTURE.
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
	// logPath, when set (GTME_AI_FIXTURE_LOG), receives one JSON line per
	// request — system, shared, payload, prompt — so a test can assert what
	// the engine was actually shown (SPEC §11 M14: "the AI fixture engine
	// receives compact, fenced records").
	logPath string
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
	path := envOverride(getenv, "GTME_AI_FIXTURE")
	if path == "" {
		return nil, fmt.Errorf("ai: engine fixture needs GTME_AI_FIXTURE to point at a responses file")
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
	e := &fixtureEngine{responses: responses, logPath: envOverride(getenv, "GTME_AI_FIXTURE_LOG")}
	fixtures[path] = e
	return e, nil
}

// logRequest appends the request to the fixture log, best-effort.
func (e *fixtureEngine) logRequest(req Request) {
	if e.logPath == "" {
		return
	}
	line, err := json.Marshal(map[string]string{
		"system": req.System, "shared": req.Shared, "payload": req.Payload, "prompt": req.Prompt,
	})
	if err != nil {
		return
	}
	f, err := os.OpenFile(e.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(line, '\n'))
}

func (e *fixtureEngine) Name() string { return EngineFixture }

// Complete returns the next scripted response, repeating the last one once the
// script runs out.
func (e *fixtureEngine) Complete(ctx context.Context, req Request) (Response, error) {
	e.mu.Lock()
	e.logRequest(req)
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
// filter, and a formulaic value per declared field (ADR-033) — a compose with
// nothing declared gets its default first_line/ps_line from the adapter's
// Fields, so the synthesized answer is schema-valid for any shape.
func autoAnswer(req Request) string {
	items := make([]map[string]any, 0, len(req.Keys))
	for _, k := range req.Keys {
		item := map[string]any{"identity_key": k}
		if req.Kind == "filter" {
			item["pass"] = true
			item["reason"] = "fixture pass"
		}
		for _, f := range req.Fields {
			item[f.Name] = fixtureValue(f, k)
		}
		items = append(items, item)
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

// fixtureValue is one synthesized, visibly synthetic value: the first enum
// member when a domain is declared, else a typed zero-ish sample, else a
// labelled string ("Fixture first line for <key>").
func fixtureValue(f FieldShape, key string) any {
	if len(f.Enum) > 0 {
		return f.Enum[0]
	}
	switch f.Type {
	case "integer", "number":
		return 0
	case "boolean":
		return true
	case "array":
		return []any{}
	default:
		// The bare name, humanized: "qualify.first_line" → "first line".
		bare := f.Name[strings.LastIndex(f.Name, ".")+1:]
		return "Fixture " + strings.ReplaceAll(bare, "_", " ") + " for " + key
	}
}
