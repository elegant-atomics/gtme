// Package instantly is the instantly/add-to-campaign deliver adapter: it adds a
// person to an Instantly campaign, with the composed lines as custom variables
// (SPEC §10.6).
//
// This adapter puts people into a live sending sequence. Everything about it is
// therefore conservative: the campaign must already exist, the campaign name is
// resolved once per run, and delivery is gated by the runner's idempotency table
// so a re-run cannot add the same person twice.
package instantly

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/trevorfox/gtm/internal/adapters"
	"github.com/trevorfox/gtm/internal/httpx"
	"github.com/trevorfox/gtm/internal/protocol"
)

// ID is the adapter id.
const ID = "instantly/add-to-campaign"

// DefaultVariables are the fields passed to Instantly as custom variables.
var DefaultVariables = []string{"first_line", "ps_line", "title"}

//go:embed manifest.json
var manifestJSON []byte

func init() {
	adapters.Register(manifestJSON, func() adapters.Adapter { return &Adapter{} })
}

// Adapter delivers leads to Instantly. HTTP is the seam tests stub.
type Adapter struct {
	HTTP httpx.Doer
}

type config struct {
	Campaign         string
	SkipIfInCampaign bool
	Variables        []string
	BaseURL          string
}

func parseConfig(raw map[string]any) (config, error) {
	c := config{SkipIfInCampaign: true, Variables: DefaultVariables, BaseURL: DefaultBaseURL}
	c.Campaign, _ = raw["campaign"].(string)
	if strings.TrimSpace(c.Campaign) == "" {
		return c, fmt.Errorf("instantly/add-to-campaign: config.campaign is required")
	}
	if v, ok := raw["skip_if_in_campaign"].(bool); ok {
		c.SkipIfInCampaign = v
	}
	if list, ok := raw["variables"].([]any); ok {
		var vars []string
		for _, item := range list {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				vars = append(vars, s)
			}
		}
		if len(vars) > 0 {
			c.Variables = vars
		}
	}
	if v, ok := raw["base_url"].(string); ok && v != "" {
		c.BaseURL = v
	}
	return c, nil
}

// Run implements adapters.Adapter.
func (a *Adapter) Run(ctx context.Context, p adapters.Ports) error {
	r := protocol.NewReader(p.In)
	w := protocol.NewWriter(p.Out)

	var (
		cfg        config
		opened     bool
		campaignID string
		apiKey     = p.Getenv("INSTANTLY_API_KEY")
		delivered  int
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
			cfg, err = parseConfig(m.Config)
			if err != nil {
				return err
			}
			if apiKey == "" {
				return &httpx.Error{Kind: httpx.KindAuth, Provider: "instantly", Msg: "INSTANTLY_API_KEY is not set"}
			}
			// Resolve the campaign once per invocation, before any lead is sent.
			campaignID, err = a.resolveCampaign(ctx, cfg, apiKey)
			if err != nil {
				return err
			}
			opened = true
			if err := w.Write(protocol.Schema([]byte(`{"type":"object","properties":{}}`))); err != nil {
				return err
			}
			if err := w.Write(protocol.Log("info", "instantly: campaign "+campaignID)); err != nil {
				return err
			}

		case protocol.TypeRecord:
			if !opened {
				return fmt.Errorf("instantly/add-to-campaign: received a record before OPEN")
			}
			if m.Key == nil {
				return fmt.Errorf("instantly/add-to-campaign: received a record with no key")
			}
			if _, err := a.addLead(ctx, cfg, apiKey, campaignID, m.Fields); err != nil {
				return err
			}
			delivered++
			// An empty RECORD is the acknowledgement: delivered, nothing learned.
			if err := w.Write(protocol.Record(*m.Key, map[string]any{}, nil)); err != nil {
				return err
			}
			if err := w.Write(protocol.Cost(m.Key, "instantly", 0, map[string]any{"leads": 1})); err != nil {
				return err
			}

		case protocol.TypeEnd:
			// Input complete; keep reading until EOF.
		}
	}

	if !opened {
		return fmt.Errorf("instantly/add-to-campaign: stream ended before OPEN")
	}
	if err := w.Write(protocol.Log("info", fmt.Sprintf("instantly: added %d leads", delivered))); err != nil {
		return err
	}
	return w.Write(protocol.End())
}
