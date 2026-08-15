// Package csvsource is the csv/source adapter: it reads a CSV file and emits one
// record per row, with the header row naming the fields (SPEC §10.1). It exists
// so every part of gtm is testable with zero API keys.
package csvsource

import (
	"context"
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/trevorfox/gtm/internal/adapters"
	"github.com/trevorfox/gtm/internal/protocol"
)

// ID is the adapter id.
const ID = "csv/source"

//go:embed manifest.json
var manifestJSON []byte

func init() {
	adapters.Register(manifestJSON, func() adapters.Adapter { return &Adapter{} })
}

// Adapter reads rows from a CSV file.
type Adapter struct{}

type config struct {
	Path       string
	EntityType string
	Limit      int
}

func parseConfig(raw map[string]any) (config, error) {
	var c config
	if raw == nil {
		return c, fmt.Errorf("csv/source: config.path is required")
	}
	c.Path, _ = raw["path"].(string)
	if c.Path == "" {
		return c, fmt.Errorf("csv/source: config.path is required")
	}
	c.EntityType, _ = raw["entity_type"].(string)
	switch v := raw["limit"].(type) {
	case float64:
		c.Limit = int(v)
	case int:
		c.Limit = v
	}
	return c, nil
}

// Run implements adapters.Adapter.
func (a *Adapter) Run(ctx context.Context, p adapters.Ports) error {
	r := protocol.NewReader(p.In)
	w := protocol.NewWriter(p.Out)

	open, err := waitForOpen(r)
	if err != nil {
		return err
	}
	cfg, err := parseConfig(open.Config)
	if err != nil {
		return err
	}

	f, err := os.Open(cfg.Path)
	if err != nil {
		return fmt.Errorf("csv/source: opening %s: %w", cfg.Path, err)
	}
	defer f.Close()

	cr := csv.NewReader(f)
	cr.FieldsPerRecord = -1 // ragged rows are common in exports; pad instead of failing
	cr.TrimLeadingSpace = true

	header, err := cr.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("csv/source: %s is empty", cfg.Path)
		}
		return fmt.Errorf("csv/source: reading %s: %w", cfg.Path, err)
	}
	fields := normalizeHeader(header)

	provides, err := schemaFor(fields)
	if err != nil {
		return err
	}
	if err := w.Write(protocol.Schema(provides)); err != nil {
		return err
	}

	emitted := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		row, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("csv/source: reading %s: %w", cfg.Path, err)
		}
		values := rowFields(fields, row)
		if len(values) == 0 {
			continue // a blank line carries nothing
		}
		if err := w.Write(protocol.Message{Type: protocol.TypeRecord, Fields: values}); err != nil {
			return err
		}
		emitted++
		if cfg.Limit > 0 && emitted >= cfg.Limit {
			break
		}
	}

	if err := w.Write(protocol.Log("info", fmt.Sprintf("read %d rows from %s", emitted, cfg.Path))); err != nil {
		return err
	}
	return w.Write(protocol.End())
}

// ProbeSchema reports the fields this CSV will provide by reading its header.
// It touches only the local filesystem, so the planner can call it without
// spending anything (SPEC §7).
func (a *Adapter) ProbeSchema(raw map[string]any) (json.RawMessage, error) {
	cfg, err := parseConfig(raw)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("csv/source: opening %s: %w", cfg.Path, err)
	}
	defer f.Close()

	cr := csv.NewReader(f)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("csv/source: reading header of %s: %w", cfg.Path, err)
	}
	return schemaFor(normalizeHeader(header))
}

// EntityType reports the entity type this source will emit, honouring the config
// override.
func (a *Adapter) EntityType(raw map[string]any) string {
	if cfg, err := parseConfig(raw); err == nil && cfg.EntityType != "" {
		return cfg.EntityType
	}
	return ""
}

func waitForOpen(r *protocol.Reader) (protocol.Message, error) {
	for {
		m, err := r.Next()
		if errors.Is(err, io.EOF) {
			return protocol.Message{}, fmt.Errorf("csv/source: stream ended before OPEN")
		}
		if err != nil {
			return protocol.Message{}, err
		}
		if m.Type == protocol.TypeOpen {
			return m, nil
		}
	}
}

// normalizeHeader lowercases header cells and turns separators into underscores,
// so "First Name" and "first-name" both become first_name.
func normalizeHeader(header []string) []string {
	out := make([]string, len(header))
	used := map[string]int{}
	for i, h := range header {
		name := strings.ToLower(strings.TrimSpace(h))
		name = strings.NewReplacer(" ", "_", "-", "_", ".", "_", "/", "_").Replace(name)
		name = strings.Trim(strings.Join(strings.FieldsFunc(name, func(r rune) bool { return r == '_' }), "_"), "_")
		if name == "" {
			name = fmt.Sprintf("column_%d", i+1)
		}
		if n, dup := used[name]; dup {
			used[name] = n + 1
			name = fmt.Sprintf("%s_%d", name, n+1)
		} else {
			used[name] = 1
		}
		out[i] = name
	}
	return out
}

func rowFields(fields, row []string) map[string]any {
	out := map[string]any{}
	for i, name := range fields {
		if i >= len(row) {
			break
		}
		v := strings.TrimSpace(row[i])
		if v == "" {
			continue // an empty cell is not a value
		}
		out[name] = v
	}
	return out
}

func schemaFor(fields []string) (json.RawMessage, error) {
	props := map[string]any{}
	for _, f := range fields {
		props[f] = map[string]any{"type": "string"}
	}
	names := make([]string, 0, len(props))
	for f := range props {
		names = append(names, f)
	}
	sort.Strings(names)

	// A probed header is exact: these are the columns, and no others. Closing the
	// schema is what lets `gtm plan` catch a pipeline that needs a field the CSV
	// does not have, instead of discovering it per record at run time.
	raw, err := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	})
	if err != nil {
		return nil, fmt.Errorf("csv/source: building schema: %w", err)
	}
	return raw, nil
}
