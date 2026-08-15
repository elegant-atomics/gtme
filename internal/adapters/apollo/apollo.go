// Package apollo is the apollo/search source adapter: it pages through Apollo's
// people search and emits one record per person, plus the company each person
// works at so the runner can relate them (SPEC §10.2).
package apollo

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
const ID = "apollo/search"

//go:embed manifest.json
var manifestJSON []byte

func init() {
	adapters.Register(manifestJSON, func() adapters.Adapter { return &Adapter{} })
}

// Adapter searches Apollo. HTTP is the seam tests stub.
type Adapter struct {
	HTTP httpx.Doer
}

type config struct {
	Query          string
	Limit          int
	Titles         []string
	Seniorities    []string
	Locations      []string
	EmployeeRanges []string
	Domains        []string
	PerPage        int
	BaseURL        string
}

func parseConfig(raw map[string]any) (config, error) {
	c := config{PerPage: 25, BaseURL: DefaultBaseURL}
	c.Query, _ = raw["query"].(string)
	c.Titles = strList(raw["titles"])
	c.Seniorities = strList(raw["seniorities"])
	c.Locations = strList(raw["locations"])
	c.EmployeeRanges = strList(raw["employee_ranges"])
	c.Domains = strList(raw["domains"])
	if v := intOf(raw["limit"]); v > 0 {
		c.Limit = v
	}
	if v := intOf(raw["per_page"]); v > 0 {
		c.PerPage = v
	}
	if v, ok := raw["base_url"].(string); ok && v != "" {
		c.BaseURL = v
	}
	if c.Query == "" && len(c.Titles) == 0 && len(c.Seniorities) == 0 && len(c.Domains) == 0 && len(c.Locations) == 0 {
		return c, fmt.Errorf("apollo/search: give it something to search for (query, titles, seniorities, locations or domains)")
	}
	if c.Limit > 0 && c.PerPage > c.Limit {
		c.PerPage = c.Limit
	}
	return c, nil
}

// Run implements adapters.Adapter.
func (a *Adapter) Run(ctx context.Context, p adapters.Ports) error {
	r := protocol.NewReader(p.In)
	w := protocol.NewWriter(p.Out)

	open, err := waitForOpen(r)
	if err != nil {
		return err
	}
	cfg, err := parseConfig(open.Config)
	if err != nil {
		return err
	}
	apiKey := p.Getenv("APOLLO_API_KEY")
	if apiKey == "" {
		return &httpx.Error{Kind: httpx.KindAuth, Provider: "apollo", Msg: "APOLLO_API_KEY is not set"}
	}

	if err := w.Write(protocol.Schema(manifestProvides())); err != nil {
		return err
	}

	emitted := 0
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := a.search(ctx, cfg, apiKey, page)
		if err != nil {
			return err
		}
		people := res.results()
		if len(people) == 0 {
			break
		}

		for _, person := range people {
			fields := person.fields()
			if len(fields) == 0 {
				continue
			}
			if err := w.Write(protocol.Message{Type: protocol.TypeRecord, Fields: fields}); err != nil {
				return err
			}
			emitted++
			if cfg.Limit > 0 && emitted >= cfg.Limit {
				break
			}
		}

		if cfg.Limit > 0 && emitted >= cfg.Limit {
			break
		}
		if total := res.totalPages(); total > 0 && page >= total {
			break
		}
		if len(people) < cfg.PerPage {
			break
		}
	}

	// Apollo bills in credits, not dollars, and does not report a price per call;
	// report the call count so a run's receipt still shows what was consumed.
	if err := w.Write(protocol.Cost(nil, "apollo", 0, map[string]any{"people": emitted, "unit": "credits"})); err != nil {
		return err
	}
	if err := w.Write(protocol.Log("info", fmt.Sprintf("apollo/search: %d people", emitted))); err != nil {
		return err
	}
	return w.Write(protocol.End())
}

// manifestProvides pulls the provides schema out of the embedded manifest so the
// SCHEMA message and the contract cannot drift apart.
func manifestProvides() []byte {
	m, err := adapters.ParseManifest(manifestJSON)
	if err != nil {
		return []byte(`{"type":"object","additionalProperties":true}`)
	}
	return m.Provides
}

func waitForOpen(r *protocol.Reader) (protocol.Message, error) {
	for {
		m, err := r.Next()
		if errors.Is(err, io.EOF) {
			return protocol.Message{}, fmt.Errorf("apollo/search: stream ended before OPEN")
		}
		if err != nil {
			return protocol.Message{}, err
		}
		if m.Type == protocol.TypeOpen {
			return m, nil
		}
	}
}

func strList(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range list {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func intOf(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	default:
		return 0
	}
}
