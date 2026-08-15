package adapters

import "encoding/json"

// SchemaProber is implemented by adapters whose provides schema is only known
// once the config is in hand — csv/source, whose fields are its header row.
// Probing must be free and offline: the planner calls it (SPEC §7 forbids
// network calls and spend at plan time).
type SchemaProber interface {
	ProbeSchema(config map[string]any) (json.RawMessage, error)
}

// EntityTyper is implemented by adapters whose entity type depends on config.
type EntityTyper interface {
	EntityType(config map[string]any) string
}

// ProbeSchema returns the adapter's config-specific provides schema, or nil when
// the adapter has none (in which case the manifest's static schema stands).
func (r *Resolved) ProbeSchema(config map[string]any) (json.RawMessage, error) {
	if r.External || r.newFunc == nil {
		return nil, nil
	}
	prober, ok := r.newFunc().(SchemaProber)
	if !ok {
		return nil, nil
	}
	return prober.ProbeSchema(config)
}

// EntityType returns the config-specific entity type, falling back to the
// manifest's.
func (r *Resolved) EntityType(config map[string]any) string {
	if !r.External && r.newFunc != nil {
		if et, ok := r.newFunc().(EntityTyper); ok {
			if v := et.EntityType(config); v != "" {
				return v
			}
		}
	}
	if v, ok := config["entity_type"].(string); ok && v != "" {
		return v
	}
	return r.Manifest.EntityType
}

// SchemaProperties lists the property names a schema declares, sorted.
func SchemaProperties(raw json.RawMessage) []string { return schemaProperties(raw) }

// Wildcard reports whether a provides schema admits fields it does not name, so
// the planner can tell "provides exactly these" from "provides at least these".
func Wildcard(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var doc struct {
		AdditionalProperties *bool `json:"additionalProperties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false
	}
	return doc.AdditionalProperties != nil && *doc.AdditionalProperties
}
