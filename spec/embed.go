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
