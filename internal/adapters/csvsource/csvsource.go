// Package csvsource is the csv/source adapter: it reads a CSV file and emits one
// record per row, with the header row naming the fields (SPEC §10.1). It exists
// so every part of gtme is testable with zero API keys.
//
// It is also the ingress edge of ADR-018: config `columns:` maps canonical
// field names → CSV headers as written; headers already matching canonical
// names auto-map; everything else is namespaced csv.<normalized_header>.
// Canonical values are normalized per the registry (SPEC §4a) here, at this
// adapter's own boundary — an invalid value is dropped from its record with a
// logged reason, never a crash.
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
	"strconv"
	"strings"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/protocol"
	"github.com/elegant-atomics/gtme/internal/registry"
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
	Columns    map[string]string // canonical name → header as written (ADR-018)
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
	if cols, ok := raw["columns"].(map[string]any); ok {
		c.Columns = map[string]string{}
		for name, header := range cols {
			h, ok := header.(string)
			if !ok || strings.TrimSpace(h) == "" {
				return c, fmt.Errorf("csv/source: columns: %q must map to a header name", name)
			}
			c.Columns[name] = h
		}
	}
	return c, nil
}

func (c config) entityType() string {
	if c.EntityType != "" {
		return c.EntityType
	}
	return "person" // the manifest's entity_type
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
	reg, err := registry.Load()
	if err != nil {
		return fmt.Errorf("csv/source: %w", err)
	}
	fields, err := mapHeader(reg, cfg, header)
	if err != nil {
		return err
	}

	provides, err := schemaFor(reg, cfg.entityType(), fields)
	if err != nil {
		return err
	}
	if err := w.Write(protocol.Schema(provides)); err != nil {
		return err
	}

	emitted, row := 0, 1
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		record, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("csv/source: reading %s: %w", cfg.Path, err)
		}
		row++
		values, dropped := rowFields(reg, cfg.entityType(), fields, record)
		for _, d := range dropped {
			if err := w.Write(protocol.Log("warn", fmt.Sprintf("row %d: dropped %s", row, d))); err != nil {
				return err
			}
		}
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

// ProbeSchema reports the fields this CSV will provide by reading its header
// and applying the columns: mapping. It touches only the local filesystem, so
// the planner can call it without spending anything (SPEC §7); a columns:
// entry naming a header the file does not have fails here, at plan time.
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
	reg, err := registry.Load()
	if err != nil {
		return nil, fmt.Errorf("csv/source: %w", err)
	}
	fields, err := mapHeader(reg, cfg, header)
	if err != nil {
		return nil, err
	}
	return schemaFor(reg, cfg.entityType(), fields)
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

// mapHeader decides each column's output field name (SPEC §10.1, ADR-018), in
// precedence order: an explicit columns: mapping; a header already matching a
// canonical name (auto-map, zero config); csv.<normalized_header> for the
// rest. Near-misses are the planner's to SUGGEST from the probed schema —
// never silently guessed here.
func mapHeader(reg *registry.Registry, cfg config, header []string) ([]string, error) {
	norm := normalizeHeader(header)
	out := make([]string, len(header))
	entity := cfg.entityType()

	claimedName := map[string]bool{} // canonical names taken by columns:
	claimedCol := map[int]bool{}     // column indexes taken by columns:
	names := make([]string, 0, len(cfg.Columns))
	for name := range cfg.Columns {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := reg.ValidateName(entity, name); err != nil {
			return nil, fmt.Errorf("csv/source: columns: %w", err)
		}
		idx := findColumn(header, norm, cfg.Columns[name])
		if idx < 0 {
			return nil, fmt.Errorf("csv/source: columns: no CSV column named %q for %s (headers: %s)",
				cfg.Columns[name], name, strings.Join(header, ", "))
		}
		out[idx] = name
		claimedName[name] = true
		claimedCol[idx] = true
	}

	for i, n := range norm {
		if claimedCol[i] {
			continue
		}
		if _, ok := reg.Lookup(entity, n); ok && !claimedName[n] {
			out[i] = n // auto-map: the header already speaks canonical
			continue
		}
		out[i] = "csv." + n // kept, queryable, visibly non-canonical
	}
	return out, nil
}

// findColumn matches a columns: header reference against the file's headers:
// exact first, then case-insensitive trimmed, then normalized form.
func findColumn(header, norm []string, want string) int {
	for i, h := range header {
		if h == want {
			return i
		}
	}
	for i, h := range header {
		if strings.EqualFold(strings.TrimSpace(h), strings.TrimSpace(want)) {
			return i
		}
	}
	wantNorm := normalizeHeader([]string{want})[0]
	for i, n := range norm {
		if n == wantNorm {
			return i
		}
	}
	return -1
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

// rowFields builds one record's fields, normalizing canonical values per the
// registry at ingress (SPEC §10.1). An invalid value never crashes the run and
// never reaches the ledger: the field is dropped with a reason.
func rowFields(reg *registry.Registry, entity string, fields, row []string) (map[string]any, []string) {
	out := map[string]any{}
	var dropped []string
	for i, name := range fields {
		if i >= len(row) || name == "" {
			continue
		}
		raw := strings.TrimSpace(row[i])
		if raw == "" {
			continue // an empty cell is not a value
		}
		v, err := ingestValue(reg, entity, name, raw)
		if err != nil {
			dropped = append(dropped, err.Error())
			continue
		}
		out[name] = v
	}
	return out, dropped
}

// ingestValue coerces a CSV cell (always a string) toward the field's declared
// registry type, then applies the field's normalization rule.
func ingestValue(reg *registry.Registry, entity, name, raw string) (any, error) {
	f, canonical := reg.Lookup(entity, name)
	if !canonical {
		return raw, nil // csv.* leftovers keep the trimmed cell as-is
	}
	var v any = raw
	switch f.Type {
	case "integer", "number":
		n, err := strconv.ParseFloat(strings.ReplaceAll(raw, ",", ""), 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a %s", name, raw, f.Type)
		}
		v = n
	case "boolean":
		b, err := strconv.ParseBool(strings.ToLower(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a boolean", name, raw)
		}
		v = b
	}
	out, err := reg.NormalizeValue(entity, name, v)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func schemaFor(reg *registry.Registry, entity string, fields []string) (json.RawMessage, error) {
	props := map[string]any{}
	for _, f := range fields {
		if f == "" {
			continue
		}
		typ := "string"
		if rf, ok := reg.Lookup(entity, f); ok && rf.Type != "array" {
			typ = rf.Type
		}
		props[f] = map[string]any{"type": typ}
	}

	// A probed header is exact: these are the columns, and no others. Closing the
	// schema is what lets `gtme plan` catch a pipeline that needs a field the CSV
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
