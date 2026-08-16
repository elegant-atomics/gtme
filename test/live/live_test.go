//go:build live

// Package live holds the manual smoke tests for the paid providers. They are
// behind the `live` build tag and are never part of `make check`.
//
//	make live            # read-only calls against Apollo, Harvest and Anthropic
//	make live-deliver    # additionally adds ONE lead to a real Instantly campaign
//
// SPEC §12 makes live runs a human gate: nothing here spends money or touches a
// real campaign unless the corresponding environment variable is set explicitly.
// Every test skips itself when its credential is absent.
package live

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/trevorfox/gtm/internal/adapters"
	"github.com/trevorfox/gtm/internal/adapters/harvest"
	"github.com/trevorfox/gtm/internal/adapters/instantly"
	"github.com/trevorfox/gtm/internal/ai"
	"github.com/trevorfox/gtm/internal/binding"
	"github.com/trevorfox/gtm/internal/protocol"
)

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is not set; skipping the live check", name)
	}
	return v
}

// drive runs one adapter with real credentials.
func drive(t *testing.T, a adapters.Adapter, config map[string]any, env map[string]string, records ...protocol.Message) []protocol.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	go func() {
		w := protocol.NewWriter(inW)
		w.Write(protocol.Message{Type: protocol.TypeOpen, StepID: "live", RunID: "live", Config: config})
		for _, rec := range records {
			w.Write(rec)
		}
		w.Write(protocol.End())
		inW.Close()
	}()

	errCh := make(chan error, 1)
	go func() {
		err := a.Run(ctx, adapters.Ports{In: inR, Out: outW, Log: os.Stderr, Env: env})
		outW.CloseWithError(err)
		errCh <- err
	}()

	var msgs []protocol.Message
	r := protocol.NewReader(outR)
	for {
		m, err := r.Next()
		if err != nil {
			break
		}
		msgs = append(msgs, m)
		t.Logf("← %s", m.Type)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("adapter failed: %v", err)
	}
	return msgs
}

// TestApolloSearchLive spends Apollo credits: one small search.
func TestApolloSearchLive(t *testing.T) {
	key := requireEnv(t, "APOLLO_API_KEY")
	apolloFS, err := binding.ShippedFS("apollo-search")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := binding.LoadFS(apolloFS)
	if err != nil {
		t.Fatal(err)
	}
	msgs := drive(t, &binding.Engine{B: b},
		map[string]any{"titles": []any{"vp marketing"}, "limit": float64(2), "per_page": float64(2)},
		map[string]string{"APOLLO_API_KEY": key})

	people := 0
	for _, m := range msgs {
		if m.Type == protocol.TypeRecord {
			people++
			t.Logf("person: %v", m.Fields)
		}
	}
	if people == 0 {
		t.Error("expected at least one person; check the extraction in spec/bindings/apollo-search/binding.yaml")
	}
}

// TestHarvestProfileLive spends one HarvestAPI lookup. Point it at a profile you
// are happy to fetch with GTM_LIVE_LINKEDIN_URL.
func TestHarvestProfileLive(t *testing.T) {
	key := requireEnv(t, "HARVEST_API_KEY")
	url := requireEnv(t, "GTM_LIVE_LINKEDIN_URL")

	msgs := drive(t, &harvest.Adapter{},
		map[string]any{"posts_limit": float64(2)},
		map[string]string{"HARVEST_API_KEY": key},
		protocol.Record(protocol.Key{EntityType: "person", IdentityKey: "live-check"},
			map[string]any{"linkedin_url": url}, nil))

	for _, m := range msgs {
		if m.Type == protocol.TypeRecord {
			t.Logf("profile: %v", m.Fields)
			if m.Fields["headline"] == nil {
				t.Error("no headline; check the field mapping in harvest/http.go")
			}
			return
		}
	}
	t.Error("no profile returned")
}

// TestInstantlyCampaignsLive is read-only: it resolves a campaign name to an id
// without adding anyone to it.
func TestInstantlyCampaignsLive(t *testing.T) {
	key := requireEnv(t, "INSTANTLY_API_KEY")
	campaign := requireEnv(t, "GTM_LIVE_CAMPAIGN")

	// A record is never sent: the adapter resolves the campaign at OPEN, and with
	// no records there is nothing to deliver.
	drive(t, &instantly.Adapter{},
		map[string]any{"campaign": campaign},
		map[string]string{"INSTANTLY_API_KEY": key})
	t.Logf("campaign %q resolved", campaign)
}

// TestInstantlyDeliverLive actually adds one lead to a real campaign. It requires
// a second, explicit opt-in because it puts a person into a sending sequence.
func TestInstantlyDeliverLive(t *testing.T) {
	key := requireEnv(t, "INSTANTLY_API_KEY")
	campaign := requireEnv(t, "GTM_LIVE_CAMPAIGN")
	email := requireEnv(t, "GTM_LIVE_DELIVER_EMAIL")
	if os.Getenv("GTM_LIVE_DELIVER") != "yes" {
		t.Skip("set GTM_LIVE_DELIVER=yes to add a real lead to a real campaign")
	}
	if !strings.Contains(email, "@") {
		t.Fatalf("GTM_LIVE_DELIVER_EMAIL = %q", email)
	}

	msgs := drive(t, &instantly.Adapter{},
		map[string]any{"campaign": campaign},
		map[string]string{"INSTANTLY_API_KEY": key},
		protocol.Record(protocol.Key{EntityType: "person", IdentityKey: email},
			map[string]any{
				"email":      email,
				"first_name": "Live",
				"first_line": "This is a gtm live smoke test.",
			}, nil))

	for _, m := range msgs {
		if m.Type == protocol.TypeRecord {
			t.Logf("delivered %s to %s", email, campaign)
			return
		}
	}
	t.Error("no delivery acknowledgement")
}

// TestAnthropicEngineLive spends a few cents of tokens.
func TestAnthropicEngineLive(t *testing.T) {
	requireEnv(t, "ANTHROPIC_API_KEY")
	t.Setenv("GTM_AI_ENGINE", ai.EngineAPI)

	engine, model, err := ai.Resolve(ai.EngineAPI, os.Getenv("GTM_AI_MODEL"), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	res, err := engine.Complete(context.Background(), ai.Request{
		System:    "Reply with a JSON array and nothing else.",
		Prompt:    `Return [{"identity_key":"a@x.com","pass":true,"reason":"live check"}] exactly.`,
		Model:     model,
		MaxTokens: 256,
		Kind:      "filter",
		Keys:      []string{"a@x.com"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	t.Logf("model=%s in=%d out=%d cost=$%.6f priced=%v", res.Model, res.InputTokens, res.OutputTokens, res.CostUSD, res.Priced)
	if !strings.Contains(res.Text, "a@x.com") {
		t.Errorf("unexpected answer: %s", res.Text)
	}
	if !res.Priced {
		t.Errorf("model %q is missing from the pricing table in internal/ai/pricing.go", res.Model)
	}
}
