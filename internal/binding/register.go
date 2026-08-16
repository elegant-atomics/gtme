package binding

import (
	"fmt"
	"io/fs"
	"os"

	"github.com/trevorfox/gtm/internal/adapters"
	"github.com/trevorfox/gtm/spec"
)

// builtinBindings are the embedded bindings registered as built-in adapters.
// apollo-search joined attio-assert when its Go twin was retired (the M8
// receipt diff proved parity; the scaffolding had done its job). The
// harvest-profile and instantly-add-to-campaign reference ports stay
// unregistered — their ids belong to the Go adapters that keep tier-2
// capabilities (posts/role_history; campaign-name resolution); they load as
// external binding adapters or through the conformance kit.
var builtinBindings = []string{"apollo-search", "attio-assert"}

// Loader is the adapters.BindingLoader implementation: binding.yaml (plus the
// fixtures beside it) → manifest + engine factory. Wired up by
// internal/adapters/all.
func Loader(dir string, raw []byte) (*adapters.Manifest, func() adapters.Adapter, bool, error) {
	b, err := Parse(raw)
	if err != nil {
		return nil, nil, false, err
	}
	m, err := b.Manifest()
	if err != nil {
		return nil, nil, false, err
	}
	fixtures, err := LoadFixtures(os.DirFS(dir))
	if err != nil {
		return nil, nil, false, err
	}
	newFunc := func() adapters.Adapter { return &Engine{B: b, Fixtures: fixtures} }
	return m, newFunc, fixtures != nil, nil
}

// RegisterBuiltins registers the embedded built-in bindings. Called once from
// internal/adapters/all; panics on a bad embedded document because that is a
// programming error, the same stance adapters.Register takes.
func RegisterBuiltins() {
	for _, name := range builtinBindings {
		b, fixtures, err := LoadFS(mustSub(name))
		if err != nil {
			panic(fmt.Sprintf("binding: embedded %s: %v", name, err))
		}
		m, err := b.Manifest()
		if err != nil {
			panic(fmt.Sprintf("binding: embedded %s: %v", name, err))
		}
		bb := b
		fx := fixtures
		adapters.RegisterBinding(m, fixtures != nil, func() adapters.Adapter {
			return &Engine{B: bb, Fixtures: fx}
		})
	}
}

// LoadFS loads a binding and its fixtures from an fs.FS rooted at the
// binding's directory (embedded or on disk).
func LoadFS(dir fs.FS) (*Binding, *FixtureSet, error) {
	raw, err := fs.ReadFile(dir, "binding.yaml")
	if err != nil {
		return nil, nil, err
	}
	b, err := Parse(raw)
	if err != nil {
		return nil, nil, err
	}
	fixtures, err := LoadFixtures(dir)
	if err != nil {
		return nil, nil, err
	}
	return b, fixtures, nil
}

// Shipped lists the names of every binding embedded under spec/bindings/.
func Shipped() []string {
	entries, err := fs.ReadDir(spec.Bindings, "bindings")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// ShippedFS returns the fs.FS rooted at one shipped binding's directory.
func ShippedFS(name string) (fs.FS, error) {
	return fs.Sub(spec.Bindings, "bindings/"+name)
}

func mustSub(name string) fs.FS {
	sub, err := ShippedFS(name)
	if err != nil {
		panic(err)
	}
	return sub
}
