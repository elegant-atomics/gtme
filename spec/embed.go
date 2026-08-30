// Package spec embeds the machine-checkable spec artifacts the binary needs at
// runtime. The field registry (SPEC §4a) is loaded by internal/registry; the
// test suite reads the same files from disk so the embedded and on-disk views
// can never drift (they are the same files).
package spec

import "embed"

// Fields holds the canonical field registry, one JSON file per entity type
// (SPEC §4a, ADR-017).
//
//go:embed fields/*.json
var Fields embed.FS

// BindingSchema is the schema every declarative binding validates against
// (SPEC §10a, ADR-022).
//
//go:embed binding-schema.json
var BindingSchema []byte

// RegistryIndexSchema validates a bindings registry index (SPEC §8,
// ADR-042): `gtme adapters search` refuses an index that does not conform.
//
//go:embed schemas/registry-index.schema.json
var RegistryIndexSchema []byte

// Bindings holds the shipped binding documents and their conformance fixtures
// (SPEC §10a): the reference ports of the Go vendor adapters, and the
// built-in attio/assert binding.
//
//go:embed bindings
var Bindings embed.FS
