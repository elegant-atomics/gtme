// Package all imports every built-in adapter for its registration side effect.
// Anything that resolves adapters by id (the CLI) imports this package; the
// adapter packages themselves stay free of import cycles. It also wires the
// binding tier (SPEC §10a) into the adapter registry: the embedded built-in
// bindings, and the loader that lets external binding.yaml directories resolve
// like any other adapter.
package all

import (
	"github.com/trevorfox/gtm/internal/adapters"
	"github.com/trevorfox/gtm/internal/binding"

	_ "github.com/trevorfox/gtm/internal/adapters/aisteps"
	_ "github.com/trevorfox/gtm/internal/adapters/apollo"
	_ "github.com/trevorfox/gtm/internal/adapters/csvdeliver"
	_ "github.com/trevorfox/gtm/internal/adapters/csvsource"
	_ "github.com/trevorfox/gtm/internal/adapters/harvest"
	_ "github.com/trevorfox/gtm/internal/adapters/instantly"
)

func init() {
	adapters.BindingLoader = binding.Loader
	binding.RegisterBuiltins()
}
