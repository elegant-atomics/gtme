// Package bundle implements campaign bundles (SPEC §8, ADR-029): `gtme freeze
// --bundle` snapshots a run into a self-contained, diffable, portable folder —
// the pipeline YAML, every referenced binding at its exact version (with its
// conformance fixtures, so `--simulate` works offline on the bundle), the
// registry slice, and a manifest of content hashes. Contracts travel;
// membership, cache, and credentials stay with the ledger and the operator.
package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/binding"
	"github.com/elegant-atomics/gtme/internal/pipeline"
	"github.com/elegant-atomics/gtme/spec"
)

// FormatVersion is the bundle layout version (spec/bundle-manifest.json).
const FormatVersion = 1

// ManifestFile is the manifest's name at the bundle root.
const ManifestFile = "manifest.json"

// Manifest mirrors spec/bundle-manifest.json.
type Manifest struct {
	BundleFormatVersion int               `json:"bundle_format_version"`
	Name                string            `json:"name"`
	SourceRunID         string            `json:"source_run_id"`
	CreatedAt           string            `json:"created_at"`
	GTMVersion          string            `json:"gtm_version,omitempty"`
	Contents            map[string]string `json:"contents"`
}

// IsBundle reports whether a path is a bundle directory: the shape `gtme run`
// sniffs to accept a bundle path wherever it accepts a pipeline path (SPEC §8).
func IsBundle(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	_, err = os.Stat(filepath.Join(path, ManifestFile))
	return err == nil
}

// Write assembles a bundle for a frozen pipeline. Returned warnings name what
// could NOT travel (external process adapters are executables, not data) —
// surfaced, never silent.
func Write(dir string, p *pipeline.Pipeline, sourceRunID, gtmVersion, createdAt string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if entries, err := os.ReadDir(dir); err != nil {
		return nil, err
	} else if len(entries) > 0 {
		return nil, fmt.Errorf("bundle: %s is not empty — a bundle wants a fresh directory", dir)
	}

	files := map[string][]byte{}

	raw, err := pipeline.Marshal(p)
	if err != nil {
		return nil, err
	}
	files["pipeline.yaml"] = raw

	// Every referenced binding travels at its exact version, fixtures included
	// (SPEC §8: simulation must work on a bundle from what is inside it).
	var warnings []string
	for _, s := range p.AllSteps() {
		if strings.TrimSpace(s.Use) == "" {
			continue // a group source references ledger state, not an adapter
		}
		res, err := adapters.Resolve(s.Use)
		if err != nil {
			return nil, fmt.Errorf("bundle: step %s: %w", s.ID, err)
		}
		switch {
		case res.Binding:
			sub := flatten(s.Use)
			docs, err := bindingFiles(res)
			if err != nil {
				return nil, fmt.Errorf("bundle: step %s: %w", s.ID, err)
			}
			for name, body := range docs {
				files[path("adapters", sub, name)] = body
			}
		case res.External:
			warnings = append(warnings,
				fmt.Sprintf("step %s (%s) is an external process adapter — executables are not data and do not travel; install it on the target machine", s.ID, s.Use))
		default:
			// A built-in process adapter ships inside the gtme binary itself;
			// the bundle needs the same binary either way.
		}
	}

	// The registry slice: what the canonical names in this bundle mean —
	// included for review and diffing (the binary enforces its own embedded
	// copy at run time, per §4a's one-artifact rule).
	regFiles, err := fs.ReadDir(spec.Fields, "fields")
	if err != nil {
		return nil, err
	}
	for _, e := range regFiles {
		body, err := fs.ReadFile(spec.Fields, "fields/"+e.Name())
		if err != nil {
			return nil, err
		}
		files[path("registry", e.Name())] = body
	}

	m := Manifest{
		BundleFormatVersion: FormatVersion,
		Name:                p.Name,
		SourceRunID:         sourceRunID,
		CreatedAt:           createdAt,
		GTMVersion:          gtmVersion,
		Contents:            map[string]string{},
	}
	for name, body := range files {
		m.Contents[name] = hash(body)
	}
	manifestJSON, err := json.MarshalIndent(m, "", "  ") // map keys marshal sorted: stable ordering
	if err != nil {
		return nil, err
	}
	files[ManifestFile] = append(manifestJSON, '\n')

	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(full, body, 0o644); err != nil {
			return nil, err
		}
	}
	sort.Strings(warnings)
	return warnings, nil
}

// Load verifies a bundle's content hashes and returns its manifest and
// pipeline. A tampered or truncated bundle fails loudly — diffable means the
// manifest is the truth about what the folder should hold.
func Load(dir string) (*Manifest, *pipeline.Pipeline, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return nil, nil, fmt.Errorf("bundle: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, fmt.Errorf("bundle: parsing %s: %w", ManifestFile, err)
	}
	if m.BundleFormatVersion != FormatVersion {
		return nil, nil, fmt.Errorf("bundle: format version %d, this gtme understands %d", m.BundleFormatVersion, FormatVersion)
	}
	for name, want := range m.Contents {
		body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return nil, nil, fmt.Errorf("bundle: %s is listed in the manifest but unreadable: %w", name, err)
		}
		if got := hash(body); got != want {
			return nil, nil, fmt.Errorf("bundle: %s does not match its manifest hash — the bundle has been modified since it was frozen", name)
		}
	}
	p, err := pipeline.Load(filepath.Join(dir, "pipeline.yaml"))
	if err != nil {
		return nil, nil, err
	}
	return &m, p, nil
}

// AdaptersDir is where a bundle's bindings live; `gtme run` points adapter
// resolution here first, so the bundle resolves nothing outside itself
// except credentials (SPEC §8).
func AdaptersDir(dir string) string { return filepath.Join(dir, "adapters") }

// bindingFiles collects a resolved binding's document and fixtures, whether it
// lives on disk (external) or embedded (a shipped built-in).
func bindingFiles(res *adapters.Resolved) (map[string][]byte, error) {
	var dir fs.FS
	if res.External || res.Dir != "" {
		dir = os.DirFS(res.Dir)
	} else {
		sub, err := binding.ShippedFS(flatten(res.Manifest.ID))
		if err != nil {
			return nil, err
		}
		dir = sub
	}
	out := map[string][]byte{}
	doc, err := fs.ReadFile(dir, "binding.yaml")
	if err != nil {
		return nil, fmt.Errorf("reading binding.yaml: %w", err)
	}
	out["binding.yaml"] = doc
	if fixtures, err := fs.ReadFile(dir, binding.FixtureFile); err == nil {
		out[binding.FixtureFile] = fixtures
	}
	// A registry-installed binding carries its pin (SPEC §8 `.source.json`,
	// ADR-042); the bundle records it so a frozen campaign names the exact
	// commit its adapter came from.
	if src, err := fs.ReadFile(dir, ".source.json"); err == nil {
		out[".source.json"] = src
	}
	return out, nil
}

func flatten(id string) string { return strings.ReplaceAll(id, "/", "-") }

func path(parts ...string) string { return strings.Join(parts, "/") }

func hash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
