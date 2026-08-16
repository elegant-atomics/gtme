// Package aisteps holds the two AI adapters: ai/filter, which judges records,
// and ai/compose, which writes fields. Both batch their records into one model
// call, validate what comes back against a strict output schema, and retry once
// with the validation error appended before failing the batch (SPEC §2, §10).
package aisteps

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/trevorfox/gtm/internal/adapters"
	"github.com/trevorfox/gtm/internal/ai"
	"github.com/trevorfox/gtm/internal/protocol"
)

// Adapter ids.
const (
	FilterID  = "ai/filter"
	ComposeID = "ai/compose"
)

// Modes.
const (
	modeFilter  = "filter"
	modeCompose = "compose"
)

//go:embed filter.json
var filterManifest []byte

//go:embed compose.json
var composeManifest []byte

func init() {
	adapters.Register(filterManifest, func() adapters.Adapter { return &Adapter{Mode: modeFilter} })
	adapters.Register(composeManifest, func() adapters.Adapter { return &Adapter{Mode: modeCompose} })
}

// Adapter is one AI step. Mode decides whether it emits verdicts or fields.
type Adapter struct {
	Mode string
	// Engine overrides engine resolution. Tests set it directly; in production it
	// comes from the step's config.
	Engine ai.Engine
}

type config struct {
	Prompt    string
	Engine    string
	Model     string
	MaxTokens int
	Fields    []string
}

func parseConfig(raw map[string]any) (config, error) {
	var c config
	c.Prompt, _ = raw["prompt"].(string)
	if strings.TrimSpace(c.Prompt) == "" {
		return c, fmt.Errorf("config.prompt is required")
	}
	c.Engine, _ = raw["engine"].(string)
	c.Model, _ = raw["model"].(string)
	switch v := raw["max_tokens"].(type) {
	case float64:
		c.MaxTokens = int(v)
	case int:
		c.MaxTokens = v
	}
	if list, ok := raw["fields"].([]any); ok {
		for _, f := range list {
			if s, ok := f.(string); ok {
				c.Fields = append(c.Fields, s)
			}
		}
	}
	return c, nil
}

// record is one batch member.
type record struct {
	key    protocol.Key
	fields map[string]any
}

// Run implements adapters.Adapter: collect the batch, ask the model once (twice
// if the first answer is invalid), then emit.
func (a *Adapter) Run(ctx context.Context, p adapters.Ports) error {
	r := protocol.NewReader(p.In)
	w := protocol.NewWriter(p.Out)

	var (
		cfg      config
		opened   bool
		records  []record
		provides = a.providesSchema()
	)

	for {
		m, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		switch m.Type {
		case protocol.TypeOpen:
			cfg, err = parseConfig(m.Config)
			if err != nil {
				return fmt.Errorf("%s: %w", a.id(), err)
			}
			opened = true
			if err := w.Write(protocol.Schema(provides)); err != nil {
				return err
			}
		case protocol.TypeRecord:
			if m.Key == nil {
				return fmt.Errorf("%s: received a record with no key", a.id())
			}
			records = append(records, record{key: *m.Key, fields: filterFields(m.Fields, cfg.Fields)})
		case protocol.TypeEnd:
			// Input complete; keep reading until EOF.
		}
	}
	if !opened {
		return fmt.Errorf("%s: stream ended before OPEN", a.id())
	}
	if len(records) == 0 {
		return w.Write(protocol.End())
	}

	engine := a.Engine
	model := cfg.Model
	if engine == nil {
		e, resolved, err := ai.Resolve(cfg.Engine, cfg.Model)
		if err != nil {
			return err
		}
		engine, model = e, resolved
	}

	answers, res, err := a.ask(ctx, engine, w, cfg, model, records)
	if err != nil {
		return err
	}

	if res.CostUSD > 0 || res.Priced {
		if err := w.Write(protocol.Cost(nil, "anthropic", res.CostUSD, res.Detail())); err != nil {
			return err
		}
	}
	if err := a.emit(w, records, answers); err != nil {
		return err
	}
	return w.Write(protocol.End())
}

// ask sends the batch, validates the answer, and retries once with the
// validation error appended (SPEC §2). A second failure fails the batch.
func (a *Adapter) ask(ctx context.Context, engine ai.Engine, w *protocol.Writer, cfg config, model string, records []record) (map[string]map[string]any, ai.Response, error) {
	req := ai.Request{
		System:    a.systemPrompt(),
		Prompt:    a.userPrompt(cfg, records, ""),
		Model:     model,
		MaxTokens: cfg.MaxTokens,
		Keys:      keysOf(records),
		Kind:      a.Mode,
	}

	var last ai.Response
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		res, err := engine.Complete(ctx, req)
		last = res
		if err != nil {
			return nil, res, err
		}
		answers, err := a.parse(res.Text, records)
		if err == nil {
			return answers, res, nil
		}
		lastErr = err
		_ = w.Write(protocol.Log("warn", fmt.Sprintf("%s: invalid model output (%v); retrying once", a.id(), err)))
		req.Prompt = a.userPrompt(cfg, records, err.Error())
	}
	return nil, last, fmt.Errorf("%s: model output still invalid after one retry: %w", a.id(), lastErr)
}

func (a *Adapter) id() string {
	if a.Mode == modeCompose {
		return ComposeID
	}
	return FilterID
}

func (a *Adapter) providesSchema() json.RawMessage {
	if a.Mode == modeCompose {
		var doc struct {
			Provides json.RawMessage `json:"provides"`
		}
		if err := json.Unmarshal(composeManifest, &doc); err == nil {
			return doc.Provides
		}
	}
	return json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`)
}

// systemPrompt states the contract. It is deliberately about the format only —
// what to decide or write is the operator's prompt.
func (a *Adapter) systemPrompt() string {
	shape := `{"identity_key": "<copied exactly from the input>", "pass": true or false, "reason": "<one short sentence>"}`
	if a.Mode == modeCompose {
		shape = `{"identity_key": "<copied exactly from the input>", "first_line": "<one sentence>", "ps_line": "<one sentence>"}`
	}
	return "You are one step in an automated data pipeline. You receive a batch of records as JSON and " +
		"return a decision for every record.\n\n" +
		"Respond with a JSON array and nothing else: no prose, no explanation, no markdown fences.\n" +
		"Each element must be an object of exactly this shape:\n" + shape + "\n\n" +
		"Return exactly one element per input record, and copy each identity_key verbatim. " +
		"Never invent, merge, drop, or reorder records."
}

// userPrompt carries the operator's instruction, the batch, and (on a retry) the
// reason the previous answer was rejected.
func (a *Adapter) userPrompt(cfg config, records []record, validationErr string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(cfg.Prompt))
	b.WriteString("\n\nRecords (")
	fmt.Fprintf(&b, "%d", len(records))
	b.WriteString("):\n")

	payload := make([]map[string]any, 0, len(records))
	for _, rec := range records {
		item := map[string]any{"identity_key": rec.key.IdentityKey}
		for k, v := range rec.fields {
			if k == "identity_key" {
				continue
			}
			item[k] = v
		}
		payload = append(payload, item)
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		raw = []byte("[]")
	}
	b.Write(raw)

	if validationErr != "" {
		b.WriteString("\n\nYour previous response was rejected: ")
		b.WriteString(validationErr)
		b.WriteString("\nRespond again with only the JSON array, in the required shape.")
	}
	return b.String()
}

// parse validates the model's answer: JSON array, one element per record, keys
// that match the batch. Returns the answers keyed by identity_key.
func (a *Adapter) parse(text string, records []record) (map[string]map[string]any, error) {
	cleaned := stripFence(text)
	if strings.TrimSpace(cleaned) == "" {
		return nil, fmt.Errorf("response was empty")
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(cleaned), &items); err != nil {
		return nil, fmt.Errorf("response is not a JSON array: %v", err)
	}

	want := map[string]bool{}
	for _, rec := range records {
		want[rec.key.IdentityKey] = true
	}

	out := make(map[string]map[string]any, len(items))
	for i, item := range items {
		key, _ := item["identity_key"].(string)
		if key == "" {
			return nil, fmt.Errorf("element %d has no identity_key", i)
		}
		if !want[key] {
			return nil, fmt.Errorf("element %d has identity_key %q, which was not in the batch", i, key)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("identity_key %q appears more than once", key)
		}
		if err := a.validateItem(item); err != nil {
			return nil, fmt.Errorf("element %d (%s): %v", i, key, err)
		}
		out[key] = item
	}

	if len(out) != len(records) {
		var missing []string
		for _, rec := range records {
			if _, ok := out[rec.key.IdentityKey]; !ok {
				missing = append(missing, rec.key.IdentityKey)
			}
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("missing %d of %d records: %s", len(missing), len(records), strings.Join(missing, ", "))
	}
	return out, nil
}

func (a *Adapter) validateItem(item map[string]any) error {
	if a.Mode == modeCompose {
		for _, field := range []string{"first_line", "ps_line"} {
			v, ok := item[field]
			if !ok {
				return fmt.Errorf("missing %s", field)
			}
			if _, ok := v.(string); !ok {
				return fmt.Errorf("%s must be a string", field)
			}
		}
		return nil
	}
	if _, ok := item["pass"].(bool); !ok {
		return fmt.Errorf("pass must be true or false")
	}
	return nil
}

// emit turns validated answers into protocol messages.
func (a *Adapter) emit(w *protocol.Writer, records []record, answers map[string]map[string]any) error {
	for _, rec := range records {
		item := answers[rec.key.IdentityKey]
		if a.Mode == modeCompose {
			// Trimmed at this adapter's boundary: canonical values must be fixed
			// points of their registry rule (SPEC §4a), and a model legitimately
			// emits stray whitespace.
			trim := func(v any) string {
				s, _ := v.(string)
				return strings.TrimSpace(s)
			}
			fields := map[string]any{
				"first_line": trim(item["first_line"]),
				"ps_line":    trim(item["ps_line"]),
			}
			if err := w.Write(protocol.Record(rec.key, fields, nil)); err != nil {
				return err
			}
			continue
		}
		pass, _ := item["pass"].(bool)
		reason, _ := item["reason"].(string)
		if err := w.Write(protocol.Verdict(rec.key, pass, reason)); err != nil {
			return err
		}
	}
	return nil
}

// filterFields restricts what the model sees, when the step asked for that.
func filterFields(fields map[string]any, only []string) map[string]any {
	if len(only) == 0 {
		return fields
	}
	out := make(map[string]any, len(only))
	for _, f := range only {
		if v, ok := fields[f]; ok {
			out[f] = v
		}
	}
	return out
}

func keysOf(records []record) []string {
	out := make([]string, 0, len(records))
	for _, rec := range records {
		out = append(out, rec.key.IdentityKey)
	}
	return out
}

// stripFence removes a ```json wrapper, which models add now and then despite
// being told not to. Recovering here is cheaper than a retry.
func stripFence(text string) string {
	s := strings.TrimSpace(text)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		// Drop the language tag on the fence line.
		if !strings.Contains(strings.ToLower(s[:i]), "{") && !strings.Contains(s[:i], "[") {
			s = s[i+1:]
		}
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
