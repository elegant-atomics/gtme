// Package all imports every built-in adapter for its registration side effect.
// Anything that resolves adapters by id (the CLI) imports this package; the
// adapter packages themselves stay free of import cycles.
package all

import (
	_ "github.com/trevorfox/gtm/internal/adapters/aisteps"
	_ "github.com/trevorfox/gtm/internal/adapters/apollo"
	_ "github.com/trevorfox/gtm/internal/adapters/csvsource"
	_ "github.com/trevorfox/gtm/internal/adapters/harvest"
	_ "github.com/trevorfox/gtm/internal/adapters/instantly"
)
