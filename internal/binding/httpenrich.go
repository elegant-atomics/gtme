package binding

// http/enrich (SPEC §10a, ADR-024): the generic fetch enricher. Two modes —
// markdown: true fetches a page, converts it, and stores it under the
// declared content field; extract: applies the engine's inline JSON
// extraction. Deterministic acquisition only: ai/* steps judge what this
// step stored, via uses:. No-JS fetching, mandatory freshness, capped size,
// payloads attached for ADR-030 retention.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/httpx"
	"github.com/elegant-atomics/gtme/internal/protocol"
)

// HTTPEnrichID is the adapter id.
const HTTPEnrichID = "http/enrich"

var httpEnrichManifest = []byte(`{
  "id": "http/enrich",
  "version": 1,
  "role": "enrich",
  "entity_type": "person",
  "needs": "dynamic",
  "provides": {"type": "object", "additionalProperties": true},
  "config_schema": {
    "type": "object",
    "required": ["url", "freshness_days"],
    "additionalProperties": false,
    "properties": {
      "url": {"type": "string", "minLength": 1, "description": "Request URL, templated from {{record.<field>}} placeholders — which are also the step's derived needs (SPEC §10a)"},
      "method": {"enum": ["GET", "POST"], "description": "Default GET"},
      "query": {"type": "object", "additionalProperties": {"type": "string"}},
      "headers": {"type": "object", "additionalProperties": {"type": "string"}},
      "markdown": {"type": "boolean", "description": "Fetch a page and store it as markdown under field:"},
      "field": {"type": "string", "description": "The declared content field (markdown mode) — canonical or namespaced (§4a)"},
      "extract": {"type": "object", "additionalProperties": {"type": "string"}, "description": "Inline JSON mode: field → dotted response path"},
      "freshness_days": {"type": "integer", "minimum": 1, "description": "REQUIRED, no default: web content rots. Doubles as the step's cache window."},
      "max_bytes": {"type": "integer", "minimum": 1, "description": "Response size cap (default 262144). Oversized responses fail the record, never truncate silently."},
      "auth": {
        "type": "object",
        "required": ["type", "env"],
        "additionalProperties": false,
        "properties": {
          "type": {"enum": ["header", "query", "bearer"]},
          "name": {"type": "string"},
          "env": {"type": "string"},
          "prefix": {"type": "string"}
        }
      },
      "keep_payloads": {"type": "boolean", "description": "ADR-030 per-step override"},
      "entity_type": {"type": "string"}
    }
  }
}`)

func init() {
	adapters.Register(httpEnrichManifest, func() adapters.Adapter { return &HTTPEnrich{} })
}

// HTTPEnrich implements the adapter. HTTP is the seam tests stub.
type HTTPEnrich struct {
	HTTP httpx.Doer
}

type httpEnrichConfig struct {
	raw      map[string]any
	URL      string
	Method   string
	Query    map[string]string
	Headers  map[string]string
	Markdown bool
	Field    string
	Extract  map[string]string
	MaxBytes int
	Auth     *Auth
}

func parseHTTPEnrichConfig(raw map[string]any) (httpEnrichConfig, error) {
	c := httpEnrichConfig{raw: raw, Method: "GET", MaxBytes: MaxPayloadBytes,
		Query: map[string]string{}, Headers: map[string]string{}}
	c.URL, _ = raw["url"].(string)
	if strings.TrimSpace(c.URL) == "" {
		return c, fmt.Errorf("http/enrich: config.url is required")
	}
	if v, ok := raw["method"].(string); ok && v != "" {
		c.Method = v
	}
	for k, v := range strMap(raw["query"]) {
		c.Query[k] = v
	}
	for k, v := range strMap(raw["headers"]) {
		c.Headers[k] = v
	}
	c.Markdown, _ = raw["markdown"].(bool)
	c.Field, _ = raw["field"].(string)
	c.Extract = strMap(raw["extract"])
	if v, ok := raw["max_bytes"].(float64); ok && v > 0 {
		c.MaxBytes = int(v)
	}
	if a, ok := raw["auth"].(map[string]any); ok {
		auth := &Auth{}
		auth.Type, _ = a["type"].(string)
		auth.Name, _ = a["name"].(string)
		auth.Env, _ = a["env"].(string)
		auth.Prefix, _ = a["prefix"].(string)
		c.Auth = auth
	}
	switch {
	case c.Markdown && strings.TrimSpace(c.Field) == "":
		return c, fmt.Errorf("http/enrich: markdown mode needs field: — the declared content field")
	case c.Markdown && len(c.Extract) > 0:
		return c, fmt.Errorf("http/enrich: markdown: and extract: are mutually exclusive")
	case !c.Markdown && len(c.Extract) == 0:
		return c, fmt.Errorf("http/enrich: declare markdown: true + field:, or extract:")
	}
	return c, nil
}

func strMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]string{}
	for k, item := range m {
		if s, ok := item.(string); ok {
			out[k] = s
		}
	}
	return out
}

// ProbeSchema is the §7 dynamic-provides mechanism: the step's provides are
// exactly what its config declares.
func (a *HTTPEnrich) ProbeSchema(config map[string]any) (json.RawMessage, error) {
	props := map[string]any{}
	if f, _ := config["field"].(string); strings.TrimSpace(f) != "" {
		props[strings.TrimSpace(f)] = map[string]any{"type": "string"}
	}
	for f := range strMap(config["extract"]) {
		props[f] = map[string]any{}
	}
	if len(props) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(map[string]any{"type": "object", "additionalProperties": false, "properties": props})
	return raw, err
}

// Run implements adapters.Adapter: one fetch per record.
func (a *HTTPEnrich) Run(ctx context.Context, p adapters.Ports) error {
	r := protocol.NewReader(p.In)
	w := protocol.NewWriter(p.Out)

	var (
		cfg    httpEnrichConfig
		opened bool
	)
	simulate := p.Getenv(SimulateEnv) != ""

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
			cfg, err = parseHTTPEnrichConfig(m.Config)
			if err != nil {
				return err
			}
			opened = true
			schema, _ := a.ProbeSchema(m.Config)
			if err := w.Write(protocol.Schema(schema)); err != nil {
				return err
			}
			if simulate {
				// Replaying retained payloads is the ROADMAP simulate-replay verb;
				// this build surfaces the gap (the runner also counts it).
				if err := w.Write(protocol.Log("warn", "http/enrich: simulation gap — live fetching only")); err != nil {
					return err
				}
			}
		case protocol.TypeRecord:
			if !opened {
				return fmt.Errorf("http/enrich: received a record before OPEN")
			}
			if m.Key == nil {
				return fmt.Errorf("http/enrich: received a record with no key")
			}
			if simulate {
				continue
			}
			if err := a.fetch(ctx, w, p, cfg, *m.Key, m.Fields); err != nil {
				return err
			}
		case protocol.TypeEnd:
			// Input complete; keep reading until EOF.
		}
	}
	if !opened {
		return fmt.Errorf("http/enrich: stream ended before OPEN")
	}
	return w.Write(protocol.End())
}

// fetch performs one templated request and emits what was acquired.
func (a *HTTPEnrich) fetch(ctx context.Context, w *protocol.Writer, p adapters.Ports, cfg httpEnrichConfig, key protocol.Key, in map[string]any) error {
	tctx := tmplContext{Config: cfg.raw, Record: in}
	url := tctx.renderString(cfg.URL)
	if url == "" {
		return w.Write(protocol.Log("warn",
			"http/enrich: skipping "+key.IdentityKey+": url template resolved empty (missing fields)"))
	}

	req, err := http.NewRequestWithContext(ctx, cfg.Method, url, nil)
	if err != nil {
		return w.Write(protocol.Log("warn", "http/enrich: "+key.IdentityKey+": "+err.Error()))
	}
	q := req.URL.Query()
	for k, v := range cfg.Query {
		if s := tctx.renderString(v); s != "" {
			q.Set(k, s)
		}
	}
	req.URL.RawQuery = q.Encode()
	for k, v := range cfg.Headers {
		if s := tctx.renderString(v); s != "" {
			req.Header.Set(k, s)
		}
	}
	if auth := cfg.Auth; auth != nil {
		cred := p.Getenv(auth.Env)
		switch auth.Type {
		case "header":
			req.Header.Set(auth.Name, auth.Prefix+cred)
		case "query":
			q := req.URL.Query()
			q.Set(auth.Name, cred)
			req.URL.RawQuery = q.Encode()
		case "bearer":
			req.Header.Set("Authorization", "Bearer "+cred)
		}
	}

	client := a.HTTP
	if client == nil {
		client = httpx.DefaultClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		return &httpx.Error{Kind: httpx.KindNetwork, Provider: "http", Msg: err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(cfg.MaxBytes)+1))
	if err != nil {
		return &httpx.Error{Kind: httpx.KindNetwork, Provider: "http", Msg: "reading response: " + err.Error()}
	}
	if resp.StatusCode >= 400 {
		return w.Write(protocol.Log("warn", fmt.Sprintf(
			"http/enrich: %s: HTTP %d from %s — nothing stored", key.IdentityKey, resp.StatusCode, url)))
	}
	if len(body) > cfg.MaxBytes {
		// Never truncate silently (SPEC §10a): an oversized page is not stored.
		return w.Write(protocol.Log("warn", fmt.Sprintf(
			"http/enrich: %s: response exceeds the %d-byte cap — nothing stored", key.IdentityKey, cfg.MaxBytes)))
	}

	learned := map[string]any{}
	if cfg.Markdown {
		if md := HTMLToMarkdown(string(body)); md != "" {
			learned[strings.TrimSpace(cfg.Field)] = md
		}
	} else {
		var doc any
		if err := json.Unmarshal(body, &doc); err != nil {
			return w.Write(protocol.Log("warn",
				"http/enrich: "+key.IdentityKey+": response was not JSON — nothing stored"))
		}
		for field, path := range cfg.Extract {
			v := atPaths(doc, []string{path})
			if s, ok := v.(string); ok {
				v = strings.TrimSpace(s)
			}
			if !isEmpty(v) {
				learned[field] = v
			}
		}
	}
	if len(learned) == 0 {
		return w.Write(protocol.Log("warn", "http/enrich: nothing acquired for "+key.IdentityKey))
	}

	msg := protocol.Record(key, learned, nil)
	keep := true
	if v, ok := cfg.raw["keep_payloads"].(bool); ok {
		keep = v
	}
	if keep {
		ct := resp.Header.Get("Content-Type")
		msg.Payload = &protocol.Payload{ContentType: ct, Body: string(body)}
	}
	return w.Write(msg)
}
