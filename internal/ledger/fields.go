package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/elegant-atomics/gtme/internal/ulid"
)

// FieldWrite is one field learned about an identity.
type FieldWrite struct {
	Field      string
	Value      any     // JSON-encoded on write
	Confidence float64 // 0 means "unspecified" and is stored as 1.0
}

// WriteFields appends field values for one identity. Nothing is ever updated
// or deleted: the ledger is append-only and current values are resolved at read
// time (see Project). Fields with a nil value are skipped — an adapter that
// returned nothing learned nothing.
//
// source is the adapter id (e.g. "harvest/profile@1").
func (l *Ledger) WriteFields(ctx context.Context, identityID, source string, prov Provenance, writes []FieldWrite) (int, error) {
	if len(writes) == 0 {
		return 0, nil
	}
	n := 0
	err := l.tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO field_values (id, identity_id, field, value, source, confidence, run_id, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("ledger: preparing field insert: %w", err)
		}
		defer stmt.Close()

		now := l.stamp(l.now())
		var runArg any
		if prov.RunID != "" {
			runArg = prov.RunID
		}
		for _, w := range writes {
			if w.Value == nil || w.Field == "" {
				continue
			}
			raw, err := json.Marshal(w.Value)
			if err != nil {
				return fmt.Errorf("ledger: encoding field %q: %w", w.Field, err)
			}
			conf := w.Confidence
			if conf == 0 {
				conf = 1.0
			}
			if _, err := stmt.ExecContext(ctx, ulid.New(), identityID, w.Field, string(raw), source, conf, runArg, now); err != nil {
				return fmt.Errorf("ledger: inserting field %q: %w", w.Field, err)
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// WriteFieldMap is WriteFields for a plain field map, with optional per-field
// confidences as sent by adapters (SPEC §5). Fields are written in sorted order
// so the ULIDs of a single batch are deterministic given the map.
func (l *Ledger) WriteFieldMap(ctx context.Context, identityID, source string, prov Provenance, fields map[string]any, confidence map[string]float64) (int, error) {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	writes := make([]FieldWrite, 0, len(keys))
	for _, k := range keys {
		writes = append(writes, FieldWrite{Field: k, Value: fields[k], Confidence: confidence[k]})
	}
	return l.WriteFields(ctx, identityID, source, prov, writes)
}

// LogStepEvent appends a step event (claimed|done|failed|skipped_cache|...).
// identityID may be empty for step-level events; detail may be empty.
func (l *Ledger) LogStepEvent(ctx context.Context, prov Provenance, identityID, event string, detail any) error {
	var encoded string
	if detail != nil {
		raw, err := json.Marshal(detail)
		if err != nil {
			return fmt.Errorf("ledger: encoding event detail: %w", err)
		}
		encoded = string(raw)
	}
	return l.tx(ctx, func(tx *sql.Tx) error {
		return insertStepEvent(ctx, tx, l, prov, identityID, event, encoded)
	})
}
