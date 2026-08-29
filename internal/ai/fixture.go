package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	// deferrable says whether Submit/Collect are on (FixtureDeferEnv). A
	// real provider holds a batch across processes and so must this one:
	// submitted requests live in <script>.batches/<token>.json and the
	// script cursor in <script>.cursor, both only in deferred mode.
	deferrable bool
	path       string
}

// FixtureAuto is the sentinel that makes the fixture engine answer correctly.
const FixtureAuto = "$auto"

// FixturePending is the sentinel a scripted Collect consumes to say "still
// processing" (ADR-038 tests): the run stays pending until the next entry.
const FixturePending = "$pending"

// FixtureDeferEnv enables the fixture engine's batch surface (tests only).
// Without it a deferred step answers synchronously — what --simulate wants.
const FixtureDeferEnv = "GTME_AI_FIXTURE_DEFER"

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
	e := &fixtureEngine{responses: responses, logPath: envOverride(getenv, "GTME_AI_FIXTURE_LOG"),
		deferrable: envOverride(getenv, FixtureDeferEnv) != "", path: path}
	if e.deferrable {
		if raw, err := os.ReadFile(path + ".cursor"); err == nil {
			fmt.Sscanf(string(raw), "%d", &e.next)
		}
	}
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
	e.saveCursor()
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

// Deferrable reports whether the batch surface is switched on.
func (e *fixtureEngine) Deferrable() bool { return e.deferrable }

// saveCursor persists the script position in deferred mode (caller holds
// the lock), so the next process continues the script where this one left
// it — a "$pending" is consumed once, not once per process.
func (e *fixtureEngine) saveCursor() {
	if !e.deferrable {
		return
	}
	_ = os.WriteFile(e.path+".cursor", []byte(fmt.Sprint(e.next)), 0o644)
}

func (e *fixtureEngine) batchFile(token string) string {
	return filepath.Join(e.path+".batches", token+".json")
}

// Submit stores the batch on disk under a synthetic token; it consumes no
// script entries — the answers are consumed at Collect, like a real provider.
func (e *fixtureEngine) Submit(ctx context.Context, reqs []BatchRequest) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	dir := e.path + ".batches"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	entries, _ := os.ReadDir(dir)
	token := fmt.Sprintf("fixture-batch-%d", len(entries)+1)
	raw, err := json.Marshal(reqs)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(e.batchFile(token), raw, 0o644); err != nil {
		return "", err
	}
	for _, r := range reqs {
		e.logRequest(r.Request)
	}
	return token, nil
}

// Collect answers a stored batch from the script: a "$pending" entry means
// still processing (consumed, so the next Collect proceeds); otherwise one
// entry per request, "$auto" synthesizing per record. An answered batch is
// removed, as a collected batch would be.
func (e *fixtureEngine) Collect(ctx context.Context, token string) (map[string]BatchResult, bool, error) {
	raw, err := os.ReadFile(e.batchFile(token))
	if err != nil {
		return nil, false, fmt.Errorf("ai: fixture knows no batch %q", token)
	}
	var reqs []BatchRequest
	if err := json.Unmarshal(raw, &reqs); err != nil {
		return nil, false, fmt.Errorf("ai: fixture batch %q: %w", token, err)
	}
	e.mu.Lock()
	if e.next < len(e.responses) && strings.TrimSpace(e.responses[e.next]) == FixturePending {
		e.next++
		e.saveCursor()
		e.mu.Unlock()
		return nil, false, nil
	}
	e.mu.Unlock()
	out := make(map[string]BatchResult, len(reqs))
	for _, r := range reqs {
		res, err := e.Complete(ctx, r.Request)
		out[r.CustomID] = BatchResult{Response: res, Err: err}
	}
	_ = os.Remove(e.batchFile(token))
	return out, true, nil
}
