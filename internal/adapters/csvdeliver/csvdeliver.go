// Package csvdeliver is the csv/deliver adapter (SPEC §10a, ADR-023): the
// universal Out floor's file half. It writes delivered records to a CSV —
// universal output to anything with an import button, and the natural
// human-review artifact. Columns are the variables: targets (sorted, stable)
// behind a leading identity_key; the header is written once on file
// creation; rows append across runs, and §8 idempotency is what keeps a
// re-run from appending duplicates.
package csvdeliver

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/protocol"
)

// ID is the adapter id.
const ID = "csv/deliver"

var manifestJSON = []byte(`{
  "id": "csv/deliver",
  "version": 1,
  "role": "deliver",
  "entity_type": "person",
  "needs": "dynamic",
  "config_schema": {
    "type": "object",
    "required": ["path"],
    "additionalProperties": false,
    "properties": {
      "path": {"type": "string", "minLength": 1, "description": "CSV file to write; created with a header, appended across runs"},
      "variables": {
        "type": "object",
        "additionalProperties": {"type": "string", "minLength": 1},
        "description": "Egress mapping, injected by the runner from the step-level variables: key (ADR-018). Its targets are the columns."
      },
      "entity_type": {"type": "string"}
    }
  }
}`)

func init() {
	adapters.Register(manifestJSON, func() adapters.Adapter { return &Adapter{} })
}

// Adapter writes CSV rows.
type Adapter struct{}

type config struct {
	Path    string
	Columns []string // sorted variables: targets
	VarMap  map[string]string
}

func parseConfig(raw map[string]any) (config, error) {
	var c config
	c.Path, _ = raw["path"].(string)
	if strings.TrimSpace(c.Path) == "" {
		return c, fmt.Errorf("csv/deliver: config.path is required")
	}
	c.VarMap = map[string]string{}
	if vars, ok := raw["variables"].(map[string]any); ok {
		for target, field := range vars {
			if f, ok := field.(string); ok {
				c.VarMap[target] = f
				c.Columns = append(c.Columns, target)
			}
		}
	}
	sort.Strings(c.Columns)
	return c, nil
}

// Run implements adapters.Adapter.
func (a *Adapter) Run(ctx context.Context, p adapters.Ports) error {
	r := protocol.NewReader(p.In)
	w := protocol.NewWriter(p.Out)

	var (
		cfg       config
		opened    bool
		delivered int
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
				return err
			}
			if err := ensureHeader(cfg); err != nil {
				return err
			}
			opened = true
			if err := w.Write(protocol.Schema([]byte(`{"type":"object","properties":{}}`))); err != nil {
				return err
			}
		case protocol.TypeRecord:
			if !opened {
				return fmt.Errorf("csv/deliver: received a record before OPEN")
			}
			if m.Key == nil {
				return fmt.Errorf("csv/deliver: received a record with no key")
			}
			if err := appendRow(cfg, *m.Key, m.Fields); err != nil {
				return err
			}
			delivered++
			// An empty RECORD is the acknowledgement: delivered, nothing learned.
			if err := w.Write(protocol.Record(*m.Key, map[string]any{}, nil)); err != nil {
				return err
			}
		case protocol.TypeEnd:
			// Input complete; keep reading until EOF.
		}
	}
	if !opened {
		return fmt.Errorf("csv/deliver: stream ended before OPEN")
	}
	if err := w.Write(protocol.Log("info", fmt.Sprintf("csv/deliver: wrote %d row(s) to %s", delivered, cfg.Path))); err != nil {
		return err
	}
	return w.Write(protocol.End())
}

// ensureHeader creates the file with its header exactly once; O_EXCL makes
// the create atomic, so concurrent sessions cannot double-write it.
func ensureHeader(cfg config) error {
	f, err := os.OpenFile(cfg.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return fmt.Errorf("csv/deliver: %w", err)
	}
	defer f.Close()
	cw := csv.NewWriter(f)
	if err := cw.Write(append([]string{"identity_key"}, cfg.Columns...)); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

// appendRow writes one record as a single O_APPEND write, so concurrent
// sessions interleave whole rows, never partial ones.
func appendRow(cfg config, key protocol.Key, fields map[string]any) error {
	row := make([]string, 0, len(cfg.Columns)+1)
	row = append(row, key.IdentityKey)
	for _, target := range cfg.Columns {
		row = append(row, stringify(fields[cfg.VarMap[target]]))
	}
	var b strings.Builder
	cw := csv.NewWriter(&b)
	if err := cw.Write(row); err != nil {
		return err
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return err
	}
	f, err := os.OpenFile(cfg.Path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("csv/deliver: %w", err)
	}
	defer f.Close()
	_, err = f.WriteString(b.String())
	return err
}

func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}
