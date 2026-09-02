// Package participants registers the runner-owned participant adapters
// (SPEC §10.3b, ADR-049): human/filter, human/compose, human/review and
// their agent/* aliases. They exist as manifests — so `gtme plan` resolves
// them, `gtme help --agent` lists them, and the planner applies the
// participant-role grammar (uses:, provides:, of:) exactly as it does for
// ai/* — but no protocol session is ever opened: the runner asks at a
// terminal, or the records wait in the ledger for `gtme answer`. The one
// implementation lives in internal/participant and the runner.
package participants

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/elegant-atomics/gtme/internal/adapters"
)

//go:embed human-filter.json
var humanFilter []byte

//go:embed human-compose.json
var humanCompose []byte

//go:embed human-review.json
var humanReview []byte

//go:embed agent-filter.json
var agentFilter []byte

//go:embed agent-compose.json
var agentCompose []byte

//go:embed agent-review.json
var agentReview []byte

func init() {
	for _, raw := range [][]byte{humanFilter, humanCompose, humanReview, agentFilter, agentCompose, agentReview} {
		adapters.Register(raw, func() adapters.Adapter { return neverOpened{} })
	}
}

// neverOpened is the adapter behind a runner-owned manifest: opening a
// session is a programming error, not a runtime condition, so it fails
// loudly rather than pretending to speak the protocol.
type neverOpened struct{}

func (neverOpened) Run(ctx context.Context, p adapters.Ports) error {
	return fmt.Errorf("participants: human/* and agent/* steps are runner-owned and never open a session (ADR-049)")
}
