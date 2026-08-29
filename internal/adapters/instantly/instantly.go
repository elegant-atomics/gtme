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
	"sort"
	"strings"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/httpx"
	"github.com/elegant-atomics/gtme/internal/protocol"
)

// ID is the adapter id.
const ID = "instantly/add-to-campaign"

// firstClassTargets are variables: target names that map into Instantly's
// first-class lead-body fields rather than custom variables (SPEC §10.6).
var firstClassTargets = map[string]bool{
	"first_name":      true,
	"last_name":       true,
	"company_name":    true,
	"personalization": true,
}

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
	// Variables is the egress mapping (ADR-018): target merge-field name →
	// ledger field. Injected by the runner from the step-level variables: key.
	// No merge field is hard-coded (SPEC §10.6).
	Variables map[string]string
	BaseURL   string
}

func parseConfig(raw map[string]any) (config, error) {
	c := config{SkipIfInCampaign: true, BaseURL: DefaultBaseURL}
	c.Campaign, _ = raw["campaign"].(string)
	if strings.TrimSpace(c.Campaign) == "" {
		return c, fmt.Errorf("instantly/add-to-campaign: config.campaign is required")
	}
	if v, ok := raw["skip_if_in_campaign"].(bool); ok {
		c.SkipIfInCampaign = v
	}
	if vars, ok := raw["variables"].(map[string]any); ok {
		c.Variables = map[string]string{}
		for target, field := range vars {
			f, ok := field.(string)
			if !ok || strings.TrimSpace(f) == "" {
				return c, fmt.Errorf("instantly/add-to-campaign: variables: %q must map to a field name", target)
			}
			c.Variables[target] = f
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
			if m.Preflight {
				// A preflight session (SPEC §5, ADR-040): check the live
				// campaign against what this step sends; send nothing.
				status, reason, checks := a.preflight(ctx, cfg, apiKey, campaignID)
				if err := w.Write(protocol.Preflight(status, reason, checks)); err != nil {
					return err
				}
				return w.Write(protocol.End())
			}
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
			sent, created, err := a.addLead(ctx, cfg, apiKey, campaignID, m.Fields)
			if err != nil {
				return err
			}
			delivered++
			// An empty RECORD is the acknowledgement: delivered, nothing learned.
			if err := w.Write(protocol.Record(*m.Key, map[string]any{}, nil)); err != nil {
				return err
			}
			// Attestation (ADR-036): re-read what Instantly stored and report
			// the three-way verdict. A 2xx is never a delivery.
			status, reason := a.attest(ctx, cfg, apiKey, sent, created)
			if err := w.Write(protocol.Attest(*m.Key, status, reason)); err != nil {
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

// attest re-reads the created lead and compares every non-blank field sent
// against what is stored (SPEC §6, ADR-036). confirmed: all present and
// equal. contradicted: a readable value says a field did not persist — the
// hard fail. inconclusive: the re-read failed, the create returned no id, or
// the shape carried no readable value for a sent field — reported ok with a
// warning, because the lead exists and will be mailed regardless, and a
// false "failed" invites a duplicate re-send by hand.
func (a *Adapter) attest(ctx context.Context, cfg config, apiKey string, sent leadRequest, created leadResponse) (string, string) {
	if strings.TrimSpace(created.ID) == "" {
		return protocol.AttestInconclusive, "the create response carried no lead id to re-read"
	}
	stored, err := a.getLead(ctx, cfg, apiKey, created.ID)
	if err != nil {
		return protocol.AttestInconclusive, "re-read failed: " + err.Error()
	}
	return compareLead(sent, stored)
}

// compareLead is the pure half of attest: sent vs stored, field by field.
// A field the response does not carry is unreadable — inconclusive, never
// contradicted by absence; a field it carries with a different value is
// contradicted.
func compareLead(sent leadRequest, stored storedLead) (string, string) {
	type check struct{ name, want string }
	checks := []check{
		{"email", strings.ToLower(sent.Email)},
		{"first_name", sent.FirstName},
		{"last_name", sent.LastName},
		{"company_name", sent.CompanyName},
		{"personalization", sent.Personalization},
	}
	var unreadable []string
	for _, c := range checks {
		if c.want == "" {
			continue
		}
		got, ok := stored.field(c.name)
		if !ok {
			unreadable = append(unreadable, c.name)
			continue
		}
		if c.name == "email" {
			got = strings.ToLower(got)
		}
		if strings.TrimSpace(got) != strings.TrimSpace(c.want) {
			return protocol.AttestContradicted, fmt.Sprintf("%s: sent %q, stored %q", c.name, c.want, got)
		}
	}
	names := make([]string, 0, len(sent.CustomVariables))
	for name := range sent.CustomVariables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		want := sent.CustomVariables[name]
		got, ok := stored.variable(name)
		switch {
		case !ok:
			unreadable = append(unreadable, "custom variable "+name)
		case strings.TrimSpace(got) != strings.TrimSpace(want):
			return protocol.AttestContradicted, fmt.Sprintf("custom variable %s: sent %q, stored %q", name, want, got)
		}
	}
	if len(unreadable) > 0 {
		return protocol.AttestInconclusive, "the re-read carried no readable value for " + strings.Join(unreadable, ", ")
	}
	return protocol.AttestConfirmed, "every field sent is present in the stored lead"
}

// preflight runs the four checks ADR-040 names against the live campaign:
// it exists and is Active; its sequence has at least the step count the
// copy assumes (the highest _step_N suffix among the variables: targets);
// every target appears as {{name}} in some step; no variant lacks one.
// A readable failure is blocked; a target that cannot be read is
// inconclusive — never a block on a guess.
func (a *Adapter) preflight(ctx context.Context, cfg config, apiKey, campaignID string) (string, string, []protocol.Check) {
	detail, err := a.getCampaign(ctx, cfg, apiKey, campaignID)
	if err != nil {
		return protocol.PreflightInconclusive, "campaign could not be read: " + err.Error(), nil
	}
	var checks []protocol.Check
	var blocked []string
	var unreadable []string

	if active, ok := detail.active(); !ok {
		unreadable = append(unreadable, "status")
	} else {
		checks = append(checks, protocol.Check{Name: "campaign active", OK: active, Detail: fmt.Sprintf("status %v", detail.Fields["status"])})
		if !active {
			blocked = append(blocked, "campaign is not Active")
		}
	}

	targets := make([]string, 0, len(cfg.Variables))
	for t := range cfg.Variables {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	assumed := assumedSteps(targets)

	steps, ok := detail.steps()
	if !ok {
		unreadable = append(unreadable, "sequence")
	} else {
		if assumed > 0 {
			okSteps := len(steps) >= assumed
			checks = append(checks, protocol.Check{Name: "sequence step count", OK: okSteps,
				Detail: fmt.Sprintf("copy assumes %d step(s), sequence has %d", assumed, len(steps))})
			if !okSteps {
				blocked = append(blocked, fmt.Sprintf("the copy assumes %d step(s) but the sequence has %d", assumed, len(steps)))
			}
		}
		for _, t := range targets {
			if firstClass(t) {
				continue // lead-body fields, not template merge fields
			}
			placeholder := "{{" + t + "}}"
			referenced := false
			for _, variants := range steps {
				for _, body := range variants {
					if strings.Contains(body, placeholder) {
						referenced = true
					}
				}
			}
			checks = append(checks, protocol.Check{Name: "variable " + t + " referenced", OK: referenced})
			if !referenced {
				blocked = append(blocked, fmt.Sprintf("no sequence step references %s", placeholder))
				continue
			}
			// A step's own copy (<x>_step_N) must be in every variant of
			// step N, or one variant sends with a hole where the email was.
			// Other variables are decoration; a variant may omit them.
			if n := stepIndex(t); n > 0 && n <= len(steps) && len(steps[n-1]) > 1 {
				for _, body := range steps[n-1] {
					if !strings.Contains(body, placeholder) {
						checks = append(checks, protocol.Check{Name: fmt.Sprintf("every variant of step %d carries %s", n, t), OK: false})
						blocked = append(blocked, fmt.Sprintf("step %d has a variant without %s", n, placeholder))
						break
					}
				}
			}
		}
	}

	switch {
	case len(blocked) > 0:
		return protocol.PreflightBlocked, strings.Join(blocked, "; "), checks
	case len(unreadable) > 0:
		return protocol.PreflightInconclusive, "the campaign response carried no readable " + strings.Join(unreadable, ", "), checks
	default:
		return protocol.PreflightOK, "", checks
	}
}

// stepIndex reads the N of a <x>_step_N target, 0 when it has none.
func stepIndex(target string) int {
	i := strings.LastIndex(target, "_step_")
	if i < 0 {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(target[i+len("_step_"):], "%d", &n); err != nil {
		return 0
	}
	return n
}

// assumedSteps is the highest _step_N suffix among the variable targets —
// how many sequence steps the copy was written for.
func assumedSteps(targets []string) int {
	max := 0
	for _, t := range targets {
		if n := stepIndex(t); n > max {
			max = n
		}
	}
	return max
}

// firstClass reports a variables: target Instantly maps into the lead body
// rather than a template merge field (SPEC §10.6).
func firstClass(target string) bool {
	switch target {
	case "first_name", "last_name", "company_name", "personalization":
		return true
	}
	return false
}
