// Package harvest is the harvest/profile enrich adapter: it looks up a person's
// LinkedIn profile by URL and returns their headline, role history and recent
// posts (SPEC §10.4).
package harvest

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/httpx"
	"github.com/elegant-atomics/gtme/internal/identity"
	"github.com/elegant-atomics/gtme/internal/protocol"
)

// ID is the adapter id.
const ID = "harvest/profile"

// DefaultPostsLimit is how many recent posts are fetched unless configured.
// Posts cost a second call, so this stays small.
const DefaultPostsLimit = 3

//go:embed manifest.json
var manifestJSON []byte

func init() {
	adapters.Register(manifestJSON, func() adapters.Adapter { return &Adapter{} })
}

// Adapter enriches people from LinkedIn. HTTP is the seam tests stub.
type Adapter struct {
	HTTP httpx.Doer
}

type config struct {
	PostsLimit  int
	MainOnly    bool
	CostPer     float64
	BaseURL     string
	postsSetter bool
}

func parseConfig(raw map[string]any) config {
	c := config{PostsLimit: DefaultPostsLimit, BaseURL: DefaultBaseURL, CostPer: 0.012}
	switch v := raw["posts_limit"].(type) {
	case float64:
		c.PostsLimit, c.postsSetter = int(v), true
	case int:
		c.PostsLimit, c.postsSetter = v, true
	}
	if v, ok := raw["main_only"].(bool); ok {
		c.MainOnly = v
	}
	switch v := raw["cost_per_profile_usd"].(type) {
	case float64:
		c.CostPer = v
	case int:
		c.CostPer = float64(v)
	}
	if v, ok := raw["base_url"].(string); ok && v != "" {
		c.BaseURL = v
	}
	return c
}

// Run implements adapters.Adapter: one profile lookup per record.
func (a *Adapter) Run(ctx context.Context, p adapters.Ports) error {
	r := protocol.NewReader(p.In)
	w := protocol.NewWriter(p.Out)

	var (
		cfg      config
		opened   bool
		apiKey   = p.Getenv("HARVEST_API_KEY")
		provides = manifestProvides()
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
			cfg = parseConfig(m.Config)
			opened = true
			if apiKey == "" {
				return &httpx.Error{Kind: httpx.KindAuth, Provider: "harvest", Msg: "HARVEST_API_KEY is not set"}
			}
			if err := w.Write(protocol.Schema(provides)); err != nil {
				return err
			}

		case protocol.TypeRecord:
			if !opened {
				return fmt.Errorf("harvest/profile: received a record before OPEN")
			}
			if m.Key == nil {
				return fmt.Errorf("harvest/profile: received a record with no key")
			}
			if err := a.enrich(ctx, w, cfg, apiKey, *m.Key, m.Fields); err != nil {
				return err
			}

		case protocol.TypeEnd:
			// Input complete; keep reading until EOF.
		}
	}

	if !opened {
		return fmt.Errorf("harvest/profile: stream ended before OPEN")
	}
	return w.Write(protocol.End())
}

// enrich looks up one person and emits what was learned. Any one LinkedIn URL
// shape will do (SPEC §10.4, one-of needs), preferring the public form; a
// lookup that started from a non-public shape additionally provides the
// resolved public linkedin_url — ADR-020's recovery path, from which the key
// upgrade to the slug tier follows automatically.
func (a *Adapter) enrich(ctx context.Context, w *protocol.Writer, cfg config, apiKey string, key protocol.Key, in map[string]any) error {
	lookup, hadPublic := lookupURL(in)
	if lookup == "" {
		return w.Write(protocol.Log("warn", "harvest/profile: skipping "+key.IdentityKey+": no LinkedIn URL of any shape"))
	}

	prof, err := a.fetchProfile(ctx, cfg, apiKey, lookup)
	if err != nil {
		return err
	}

	var posts []post
	if cfg.PostsLimit > 0 {
		posts, err = a.fetchPosts(ctx, cfg, apiKey, prof.ID, lookup)
		if err != nil {
			// Posts are a bonus; a profile without them is still worth keeping.
			_ = w.Write(protocol.Log("warn", fmt.Sprintf("harvest/profile: posts for %s: %v", key.IdentityKey, err)))
			posts = nil
		}
	}

	learned := fields(prof, posts, cfg.PostsLimit)
	if !hadPublic {
		if resolved := identity.NormalizeLinkedInURL(prof.LinkedinURL); resolved != "" {
			learned["linkedin_url"] = resolved
		} else if resolved := identity.NormalizeLinkedInURL("https://www.linkedin.com/in/" + strings.TrimSpace(prof.PublicIdentifier)); resolved != "" {
			learned["linkedin_url"] = resolved
		}
	}
	calls := 1
	if cfg.PostsLimit > 0 {
		calls = 2
	}
	if err := w.Write(protocol.Cost(&key, "harvest", cfg.CostPer, map[string]any{
		"calls": calls, "posts": len(posts),
	})); err != nil {
		return err
	}

	if len(learned) == 0 {
		return w.Write(protocol.Log("warn", "harvest/profile: nothing returned for "+key.IdentityKey))
	}
	return w.Write(protocol.Record(key, learned, nil))
}

// lookupURL picks the URL to look a record up by — public form first, then
// internal, then Sales Navigator (SPEC §10.4) — and reports whether the public
// form was already known.
func lookupURL(in map[string]any) (url string, hadPublic bool) {
	get := func(name string) string {
		s, _ := in[name].(string)
		return strings.TrimSpace(s)
	}
	if u := get("linkedin_url"); u != "" {
		return urlify(u), true
	}
	if u := get("linkedin_internal_url"); u != "" {
		return urlify(u), false
	}
	if u := get("linkedin_sales_nav_url"); u != "" {
		return urlify(u), false
	}
	return "", false
}

// urlify accepts a bare slug from older ledgers ("in/jane-doe") as well as a
// full URL; HarvestAPI wants a URL.
func urlify(s string) string {
	if strings.Contains(s, "://") {
		return s
	}
	return "https://www.linkedin.com/" + strings.TrimPrefix(s, "/")
}

// manifestProvides reads the provides schema from the embedded manifest so the
// SCHEMA message cannot drift from the contract.
func manifestProvides() []byte {
	m, err := adapters.ParseManifest(manifestJSON)
	if err != nil {
		return []byte(`{"type":"object","additionalProperties":true}`)
	}
	return m.Provides
}
