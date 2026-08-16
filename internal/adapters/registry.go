package adapters

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// builtinsMu guards the registry, which adapter packages populate from init().
var (
	builtinsMu sync.RWMutex
	builtins   = map[string]*builtin{}
)

type builtin struct {
	manifest *Manifest
	newFunc  func() Adapter
	// binding marks a built-in registered from a binding document (SPEC §10a);
	// hasFixtures says whether it ships conformance fixtures, which is what
	// --simulate serves (SPEC §8).
	binding     bool
	hasFixtures bool
}

// BindingLoader turns a binding.yaml document (and its directory, where
// conformance fixtures live) into a manifest and an engine factory. Set by
// internal/adapters/all (the binding package cannot be imported from here
// without a cycle); nil means binding discovery is off.
var BindingLoader func(dir string, raw []byte) (*Manifest, func() Adapter, bool, error)

// Register adds a built-in adapter. rawManifest is the adapter's embedded
// manifest.json. Called from adapter package init(); panics on a bad manifest
// because that is a programming error, not a runtime condition.
func Register(rawManifest []byte, newFunc func() Adapter) {
	m, err := ParseManifest(rawManifest)
	if err != nil {
		panic(err)
	}
	builtinsMu.Lock()
	defer builtinsMu.Unlock()
	if _, dup := builtins[m.ID]; dup {
		panic(fmt.Sprintf("adapters: %s registered twice", m.ID))
	}
	builtins[m.ID] = &builtin{manifest: m, newFunc: newFunc}
}

// RegisterBinding adds a built-in adapter whose implementation is a binding
// interpreted by the generic engine (SPEC §10a). The manifest is the binding's
// §6 bridge; newFunc returns the engine loaded with the binding document.
func RegisterBinding(m *Manifest, hasFixtures bool, newFunc func() Adapter) {
	builtinsMu.Lock()
	defer builtinsMu.Unlock()
	if _, dup := builtins[m.ID]; dup {
		panic(fmt.Sprintf("adapters: %s registered twice", m.ID))
	}
	builtins[m.ID] = &builtin{manifest: m, newFunc: newFunc, binding: true, hasFixtures: hasFixtures}
}

// Builtins lists the ids of compiled-in adapters, sorted.
func Builtins() []string {
	builtinsMu.RLock()
	defer builtinsMu.RUnlock()
	out := make([]string, 0, len(builtins))
	for id := range builtins {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Resolved is an adapter ready to be invoked.
type Resolved struct {
	Manifest *Manifest
	// External reports whether this adapter is an executable on disk.
	External bool
	// Dir is the adapter directory for external adapters.
	Dir string
	// Binding reports whether this adapter is a binding interpreted by the
	// generic engine (SPEC §10a); HasFixtures whether it ships conformance
	// fixtures (what --simulate serves, SPEC §8).
	Binding     bool
	HasFixtures bool

	newFunc    func() Adapter
	executable string
}

// Open launches one session. env carries the credentials the manifest declared.
func (r *Resolved) Open(ctx context.Context, p Ports) (*Session, error) {
	if r.External {
		return launchExec(ctx, r.Dir, r.executable, p)
	}
	return launchBuiltin(ctx, r.newFunc(), p), nil
}

// SearchPath lists the directories external adapters are discovered in:
// GTM_ADAPTER_PATH entries first, then ~/.gtm/adapters (SPEC §6).
func SearchPath() []string {
	var dirs []string
	if p := os.Getenv("GTM_ADAPTER_PATH"); p != "" {
		for _, d := range filepath.SplitList(p) {
			if d != "" {
				dirs = append(dirs, d)
			}
		}
	}
	if home := os.Getenv("GTM_HOME"); home != "" {
		dirs = append(dirs, filepath.Join(home, "adapters"))
	} else if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".gtm", "adapters"))
	}
	return dirs
}

// Resolve finds an adapter by id: built-ins first, then each search path
// directory. An id with slashes may live in nested directories
// (harvest/profile) or in one flattened directory (harvest-profile).
func Resolve(id string) (*Resolved, error) {
	builtinsMu.RLock()
	b, ok := builtins[id]
	builtinsMu.RUnlock()
	if ok {
		return &Resolved{Manifest: b.manifest, newFunc: b.newFunc,
			Binding: b.binding, HasFixtures: b.hasFixtures}, nil
	}

	var tried []string
	for _, root := range SearchPath() {
		for _, name := range []string{filepath.FromSlash(id), strings.ReplaceAll(id, "/", "-")} {
			dir := filepath.Join(root, name)
			manifestPath := filepath.Join(dir, "manifest.json")
			raw, err := os.ReadFile(manifestPath)
			if err != nil {
				// A directory holding a binding.yaml instead of an executable is a
				// binding adapter (SPEC §10a discovery, mirror of §6).
				if BindingLoader != nil {
					if rawB, berr := os.ReadFile(filepath.Join(dir, "binding.yaml")); berr == nil {
						m, newFunc, hasFixtures, lerr := BindingLoader(dir, rawB)
						if lerr != nil {
							return nil, fmt.Errorf("adapters: %s: %w", filepath.Join(dir, "binding.yaml"), lerr)
						}
						if m.ID != id {
							return nil, fmt.Errorf("adapters: %s declares id %q, expected %q",
								filepath.Join(dir, "binding.yaml"), m.ID, id)
						}
						return &Resolved{Manifest: m, Dir: dir, newFunc: newFunc,
							Binding: true, HasFixtures: hasFixtures}, nil
					}
				}
				tried = append(tried, manifestPath)
				continue
			}
			m, err := ParseManifest(raw)
			if err != nil {
				return nil, fmt.Errorf("adapters: %s: %w", manifestPath, err)
			}
			if m.ID != id {
				return nil, fmt.Errorf("adapters: %s declares id %q, expected %q", manifestPath, m.ID, id)
			}
			exe := filepath.Join(dir, "run")
			info, err := os.Stat(exe)
			if err != nil {
				return nil, fmt.Errorf("adapters: %s: missing executable %s", id, exe)
			}
			if info.Mode()&0o111 == 0 {
				return nil, fmt.Errorf("adapters: %s: %s is not executable (chmod +x)", id, exe)
			}
			return &Resolved{Manifest: m, External: true, Dir: dir, executable: exe}, nil
		}
	}

	msg := &strings.Builder{}
	fmt.Fprintf(msg, "adapters: unknown adapter %q", id)
	if names := Builtins(); len(names) > 0 {
		fmt.Fprintf(msg, "\n  built-in: %s", strings.Join(names, ", "))
	}
	for _, t := range tried {
		fmt.Fprintf(msg, "\n  looked for: %s", t)
	}
	return nil, errNotFound{msg.String()}
}

// Installed lists every adapter manifest gtm can currently resolve: every
// built-in, plus every external adapter found on SearchPath. Used by
// `gtm help --agent` (SPEC §8, ADR-007) to regenerate the surface doc from the
// live registry rather than a hand-maintained list. Best-effort: an
// unreadable or invalid external manifest is skipped rather than failing the
// whole listing, since a broken third-party adapter directory should not
// break introspection of the adapters that do work.
func Installed() []*Manifest {
	builtinsMu.RLock()
	out := make([]*Manifest, 0, len(builtins))
	for _, b := range builtins {
		out = append(out, b.manifest)
	}
	builtinsMu.RUnlock()

	seen := make(map[string]bool, len(out))
	for _, m := range out {
		seen[m.ID] = true
	}
	for _, root := range SearchPath() {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(root, e.Name(), "manifest.json"))
			if err != nil {
				continue
			}
			m, err := ParseManifest(raw)
			if err != nil || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

type errNotFound struct{ msg string }

func (e errNotFound) Error() string { return e.msg }

// IsNotFound reports whether err is an unknown-adapter error.
func IsNotFound(err error) bool {
	_, ok := err.(errNotFound)
	return ok
}
