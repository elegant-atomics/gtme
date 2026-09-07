// Package demoenrich is demo/enrich (SPEC §10 item 9, ADR-056): the priced,
// keyless enrichment. It exists so the zero-key onboarding path can print
// the top-up receipt — cached, avoided — on a persisting ledger, which
// --simulate cannot (ADR-028). It calls no vendor, needs no credential and
// retains no payload; its output is deterministic from the identity key and
// is labelled synthetic in the value itself, and its cost is a stated
// pretend price emitted as an estimated COST under provider "demo", so the
// receipt's arithmetic is real and the adapter id says where the dollars
// came from wherever they appear.
package demoenrich

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"io"

	"github.com/gtme-run/gtme/internal/adapters"
	"github.com/gtme-run/gtme/internal/protocol"
)

const ID = "demo/enrich"

// DefaultCostPerRecord is the pretend price, in USD, of one enrichment.
const DefaultCostPerRecord = 0.01

// Note is the fixed demo.note value: the record itself says it is synthetic.
const Note = "synthetic — demo/enrich called no vendor"

//go:embed manifest.json
var manifestJSON []byte

func init() {
	adapters.Register(manifestJSON, func() adapters.Adapter { return &Adapter{} })
	// The plan's estimate follows the operator's figure (ADR-046), as a
	// binding's templated cost would.
	adapters.SetCostRate(ID, func(config map[string]any) (float64, bool) {
		return costPerRecord(config), true
	})
}

// Adapter is demo/enrich.
type Adapter struct{}

func costPerRecord(config map[string]any) float64 {
	switch v := config["cost_per_record_usd"].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return DefaultCostPerRecord
}

// Score derives demo.score (0–100) from an identity key: the first two
// bytes of its SHA-256, modulo 101. Deterministic, so a re-run within the
// freshness window is a cache hit and a run past it reproduces the value.
func Score(identityKey string) int {
	sum := sha256.Sum256([]byte(identityKey))
	return int(uint16(sum[0])<<8|uint16(sum[1])) % 101
}

func (a *Adapter) Run(ctx context.Context, p adapters.Ports) error {
	r := protocol.NewReader(p.In)
	w := protocol.NewWriter(p.Out)

	var (
		opened  bool
		costPer = DefaultCostPerRecord
	)
	for {
		m, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		switch m.Type {
		case protocol.TypeOpen:
			opened = true
			costPer = costPerRecord(m.Config)
			if err := w.Write(protocol.Schema(provides)); err != nil {
				return err
			}
		case protocol.TypeRecord:
			if !opened {
				return fmt.Errorf("%s: received a record before OPEN", ID)
			}
			if m.Key == nil {
				return fmt.Errorf("%s: received a record with no key", ID)
			}
			key := *m.Key
			if err := w.Write(protocol.Cost(&key, "demo", costPer, map[string]any{"pretend": true})); err != nil {
				return err
			}
			fields := map[string]any{
				"demo.score": Score(key.IdentityKey),
				"demo.note":  Note,
			}
			if err := w.Write(protocol.Record(key, fields, nil)); err != nil {
				return err
			}
		case protocol.TypeEnd:
			// Input complete; keep reading until EOF.
		}
	}
	if !opened {
		return fmt.Errorf("%s: stream ended before OPEN", ID)
	}
	return w.Write(protocol.End())
}

var provides = []byte(`{"type":"object","additionalProperties":false,"properties":{"demo.score":{"type":"integer","minimum":0,"maximum":100},"demo.note":{"type":"string"}}}`)
