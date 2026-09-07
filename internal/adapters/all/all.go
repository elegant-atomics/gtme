// Package all imports every built-in adapter for its registration side effect.
// Anything that resolves adapters by id (the CLI) imports this package; the
// adapter packages themselves stay free of import cycles. It also wires the
// binding tier (SPEC §10a) into the adapter registry: the embedded built-in
// bindings, and the loader that lets external binding.yaml directories resolve
// like any other adapter.
package all

import (
	"github.com/gtme-run/gtme/internal/adapters"
	"github.com/gtme-run/gtme/internal/binding"

	_ "github.com/gtme-run/gtme/internal/adapters/aisteps"
	_ "github.com/gtme-run/gtme/internal/adapters/csvdeliver"
	_ "github.com/gtme-run/gtme/internal/adapters/csvsource"
	_ "github.com/gtme-run/gtme/internal/adapters/demoenrich"
	_ "github.com/gtme-run/gtme/internal/adapters/harvest"
	_ "github.com/gtme-run/gtme/internal/adapters/instantly"
	_ "github.com/gtme-run/gtme/internal/adapters/participants"
)

func init() {
	adapters.BindingLoader = binding.Loader
	binding.RegisterBuiltins()
}
