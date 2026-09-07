package registry

// Type discovery (SPEC §4a, ADR-054): types are discovered like adapters,
// from three sources, with nothing copied and no verb — the binary embeds
// person, company and post; an operator may place a file in
// ~/.gtme/types/<name>.json; a binding may ship types/<name>.json beside its
// binding.yaml, read in place under the binding's install directory (and
// pin). Embedded names are reserved: a discovered file under one of them is
// ignored here — `gtme adapters verify` refuses it before it can install.
// Two files of the same name with different content across sources are a
// problem the plan reports naming both paths.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gtme-run/gtme/internal/adapters"
)

// TypesDir is the subdirectory a binding ships its type files in.
const TypesDir = "types"

// HomeTypesDir is the operator's own types directory: $GTME_HOME/types, else
// ~/.gtme/types (the same home rule as adapters, SPEC §6).
func HomeTypesDir() string {
	if home := os.Getenv("GTME_HOME"); home != "" {
		return filepath.Join(home, TypesDir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gtme", TypesDir)
}

// discovered is one candidate type file found on disk.
type discovered struct {
	path   string
	origin string
}

// discover reads every type file on this machine into the registry.
func (r *Registry) discover() {
	var found []discovered
	if dir := HomeTypesDir(); dir != "" {
		for _, p := range typeFiles(dir) {
			found = append(found, discovered{path: p, origin: "home"})
		}
	}
	roots := adapters.SearchPath()
	if adapters.BundleDir != "" {
		roots = append([]string{adapters.BundleDir}, roots...)
	}
	for _, root := range roots {
		for _, dir := range bindingDirs(root) {
			for _, p := range typeFiles(filepath.Join(dir, TypesDir)) {
				found = append(found, discovered{path: p, origin: "binding"})
			}
		}
	}
	for _, d := range found {
		r.add(d)
	}
}

// add folds one discovered file in: same name and same content across sources
// is one type; different content is a problem naming both paths; an
// embedded name is reserved and the file is ignored.
func (r *Registry) add(d discovered) {
	name := strings.TrimSuffix(filepath.Base(d.path), ".json")
	if t, ok := r.byEntity[name]; ok && t.Origin == "embedded" {
		return
	}
	raw, err := os.ReadFile(d.path)
	if err != nil {
		r.problems[name] = fmt.Sprintf("%s: %v", d.path, err)
		return
	}
	t, err := ParseType(raw, d.path, d.origin)
	if err != nil {
		r.problems[name] = fmt.Sprintf("%s: %v", d.path, err)
		delete(r.byEntity, name)
		return
	}
	if t.EntityType != name {
		r.problems[name] = fmt.Sprintf("%s declares entity_type %q; the file is named after the type", d.path, t.EntityType)
		delete(r.byEntity, name)
		return
	}
	if prior, bad := r.problems[name]; bad {
		r.problems[name] = prior + "; also " + d.path
		return
	}
	if existing, ok := r.byEntity[name]; ok {
		if existing.Hash == t.Hash {
			return
		}
		r.problems[name] = fmt.Sprintf("two type files disagree — %s and %s define %q with different content; one binding's type must win, so uninstall one or make them identical (SPEC §4a)",
			existing.Path, t.Path, name)
		delete(r.byEntity, name)
		return
	}
	r.byEntity[name] = t
}

// typeFiles lists the *.json files in a directory, sorted; none if absent.
func typeFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out
}

// bindingDirs lists the binding directories under one search-path root: a
// directory holding binding.yaml, one or two levels down (an id with a slash
// may live nested, harvest/posts, or flattened, harvest-posts — SPEC §6).
func bindingDirs(root string) []string {
	var out []string
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "binding.yaml")); err == nil {
			out = append(out, dir)
			continue
		}
		nested, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, n := range nested {
			if !n.IsDir() {
				continue
			}
			sub := filepath.Join(dir, n.Name())
			if _, err := os.Stat(filepath.Join(sub, "binding.yaml")); err == nil {
				out = append(out, sub)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Reserved reports whether a type name is one the binary embeds (SPEC §4a):
// a binding shipping it is refused by `gtme adapters verify`.
func Reserved(name string) bool {
	r, err := LoadEmbedded()
	if err != nil {
		return false
	}
	return r.Known(name)
}

func contentHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// WithBindingDir returns a copy of the registry with the type files a
// binding directory ships folded in — what `gtme adapters verify` and `add`
// check a fetched binding against before it is installed anywhere the
// search path would discover it. Files are read from <dir>/types.
func (r *Registry) WithBindingDir(dir string) *Registry {
	out := &Registry{byEntity: make(map[string]*Type, len(r.byEntity)), problems: make(map[string]string, len(r.problems))}
	for k, v := range r.byEntity {
		out.byEntity[k] = v
	}
	for k, v := range r.problems {
		out.problems[k] = v
	}
	for _, p := range typeFiles(filepath.Join(dir, TypesDir)) {
		out.add(discovered{path: p, origin: "binding"})
	}
	return out
}

// ShippedTypes lists the type files a binding directory ships: name and
// path, sorted by name. Reserved names are listed too — the caller refuses.
func ShippedTypes(dir string) []string {
	return typeFiles(filepath.Join(dir, TypesDir))
}
