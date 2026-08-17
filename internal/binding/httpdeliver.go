package binding

// http/deliver (SPEC §10a, ADR-023): the universal Out floor's web half —
// POST the resolved variables per record to any URL. It is literally the
// binding engine's deliver role invoked anonymously: OPEN synthesizes a
// binding from config and every record goes through the same deliverRecord
// path a named deliver binding uses. The step-level idempotency: key is
// REQUIRED (plan-enforced): a generic target cannot infer delivery
// semantics.

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/httpx"
	"github.com/elegant-atomics/gtme/internal/protocol"
)

// HTTPDeliverID is the adapter id.
const HTTPDeliverID = "http/deliver"

var httpDeliverManifest = []byte(`{
  "id": "http/deliver",
  "version": 1,
  "role": "deliver",
  "entity_type": "person",
  "needs": "dynamic",
  "config_schema": {
    "type": "object",
    "required": ["url"],
    "additionalProperties": false,
    "properties": {
      "url": {"type": "string", "minLength": 1, "description": "Target URL; may template {{record.<field>}} / {{config.*}}"},
      "method": {"enum": ["POST", "PUT", "PATCH"], "description": "Default POST"},
      "query": {"type": "object", "additionalProperties": {"type": "string"}},
      "headers": {"type": "object", "additionalProperties": {"type": "string"}},
      "body": {"description": "Body template; default is the resolved variables object"},
      "auth": {
        "type": "object",
        "required": ["type", "env"],
        "additionalProperties": false,
        "properties": {
          "type": {"enum": ["header", "query", "bearer", "basic"]},
          "name": {"type": "string"},
          "env": {"type": "string"},
          "prefix": {"type": "string"}
        }
      },
      "variables": {
        "type": "object",
        "additionalProperties": {"type": "string", "minLength": 1},
        "description": "Egress mapping, injected by the runner from the step-level variables: key (ADR-018)."
      },
      "keep_payloads": {"type": "boolean"},
      "entity_type": {"type": "string"}
    }
  }
}`)

func init() {
	adapters.Register(httpDeliverManifest, func() adapters.Adapter { return &HTTPDeliver{} })
}

// HTTPDeliver implements the adapter. HTTP is the seam tests stub.
type HTTPDeliver struct {
	HTTP httpx.Doer
}

// Run implements adapters.Adapter by delegating to the engine.
func (a *HTTPDeliver) Run(ctx context.Context, p adapters.Ports) error {
	r := protocol.NewReader(p.In)
	w := protocol.NewWriter(p.Out)

	var (
		eng    *Engine
		cfg    map[string]any
		opened bool
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
			b, err := synthDeliverBinding(m.Config)
			if err != nil {
				return err
			}
			eng = &Engine{B: b, HTTP: a.HTTP}
			cfg = eng.configWithDefaults(m.Config)
			opened = true
			if err := w.Write(protocol.Schema(eng.providesSchema())); err != nil {
				return err
			}
		case protocol.TypeRecord:
			if !opened {
				return fmt.Errorf("http/deliver: received a record before OPEN")
			}
			if m.Key == nil {
				return fmt.Errorf("http/deliver: received a record with no key")
			}
			if err := eng.deliverRecord(ctx, w, p, eng.doer(), cfg, "", *m.Key, m.Fields); err != nil {
				return err
			}
		case protocol.TypeEnd:
			// Input complete; keep reading until EOF.
		}
	}
	if !opened {
		return fmt.Errorf("http/deliver: stream ended before OPEN")
	}
	return w.Write(protocol.End())
}

func (e *Engine) doer() httpx.Doer {
	if e.HTTP != nil {
		return e.HTTP
	}
	return httpx.DefaultClient()
}

// synthDeliverBinding turns the step config into the anonymous binding the
// engine interprets (SPEC §10a: inline http/* steps ARE the engine).
func synthDeliverBinding(config map[string]any) (*Binding, error) {
	url, _ := config["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("http/deliver: config.url is required")
	}
	method, _ := config["method"].(string)
	if method == "" {
		method = "POST"
	}
	b := &Binding{
		ID:          HTTPDeliverID,
		Version:     1,
		Role:        adapters.RoleDeliver,
		EntityType:  "person",
		Idempotency: "ledger",
		Request: Request{
			Method:  method,
			URL:     url,
			Query:   strMap(config["query"]),
			Headers: strMap(config["headers"]),
		},
	}
	if body, ok := config["body"]; ok && body != nil {
		b.Request.Body = body
	} else {
		// The default body is the resolved variables object (ADR-023).
		b.Request.Body = map[string]any{"$variables": true}
	}
	if a, ok := config["auth"].(map[string]any); ok {
		auth := &Auth{}
		auth.Type, _ = a["type"].(string)
		auth.Name, _ = a["name"].(string)
		auth.Env, _ = a["env"].(string)
		auth.Prefix, _ = a["prefix"].(string)
		b.Auth = auth
	}
	return b, nil
}
