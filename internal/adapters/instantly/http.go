package instantly

// This file is the entire HTTP surface of the Instantly adapter: endpoints,
// request and response shapes, and the field mapping.
//
// Endpoints: GET  https://api.instantly.ai/api/v2/campaigns   (resolve name → id)
//            POST https://api.instantly.ai/api/v2/leads       (add lead)
//            GET  https://api.instantly.ai/api/v2/leads/{id}  (re-read, attestation — ADR-036)
//            GET  https://api.instantly.ai/api/v2/campaigns/{id} (sequence + status, preflight — ADR-040)
// Auth:      Authorization: Bearer $INSTANTLY_API_KEY
// Docs:      https://developer.instantly.ai/api/v2/lead/createlead
//            https://developer.instantly.ai/api/v2/lead/getlead

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/elegant-atomics/gtme/internal/httpx"
)

// DefaultBaseURL is Instantly's API host.
const DefaultBaseURL = "https://api.instantly.ai"

const (
	campaignsPath = "/api/v2/campaigns"
	leadsPath     = "/api/v2/leads"
)

// campaignList is a page of campaigns.
type campaignList struct {
	Items []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Status int    `json:"status"`
	} `json:"items"`
	NextStartingAfter string `json:"next_starting_after"`
}

// leadRequest is the body for creating a lead.
type leadRequest struct {
	Campaign         string            `json:"campaign"`
	Email            string            `json:"email"`
	FirstName        string            `json:"first_name,omitempty"`
	LastName         string            `json:"last_name,omitempty"`
	CompanyName      string            `json:"company_name,omitempty"`
	Personalization  string            `json:"personalization,omitempty"`
	CustomVariables  map[string]string `json:"custom_variables,omitempty"`
	SkipIfInCampaign bool              `json:"skip_if_in_campaign,omitempty"`
}

// leadResponse is the created lead.
type leadResponse struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Campaign string `json:"campaign"`
}

// storedLead is what a re-read returns (ADR-036), decoded loosely: the
// first-class fields by name, and custom variables under `payload` (the
// documented shape) or `custom_variables` (the create body's name), whichever
// the API answers with. Anything else is left in Rest so an unrecognised
// shape is detectable rather than silently "confirmed".
type storedLead struct {
	// Fields is the response as decoded: presence is the point — a field
	// the response does not carry at all is unreadable (inconclusive), not
	// empty (contradicted). Custom variables sit under payload or
	// custom_variables.
	Fields map[string]any
}

// getLead re-reads one lead by id, for attestation.
func (a *Adapter) getLead(ctx context.Context, cfg config, apiKey, id string) (storedLead, error) {
	var out map[string]any
	err := httpx.JSON(ctx, a.HTTP, httpx.Request{
		Method:   "GET",
		URL:      strings.TrimRight(cfg.BaseURL, "/") + leadsPath + "/" + id,
		Provider: "instantly",
		Headers:  map[string]string{"Authorization": "Bearer " + apiKey},
		Attempts: 1,
	}, &out)
	return storedLead{Fields: out}, err
}

// field reads a first-class lead field: its string value, and whether the
// response carried it at all.
func (l storedLead) field(name string) (string, bool) {
	v, ok := l.Fields[name]
	if !ok || v == nil {
		return "", false
	}
	return str(v), true
}

// variable reads a custom variable under either name the API uses.
func (l storedLead) variable(name string) (string, bool) {
	for _, bucket := range []string{"payload", "custom_variables"} {
		if m, ok := l.Fields[bucket].(map[string]any); ok {
			if v, ok := m[name]; ok && v != nil {
				return str(v), true
			}
		}
	}
	return "", false
}

// campaignIDs caches resolved name → id for the life of the process, so one
// `gtme run` resolves a campaign once (SPEC §10.6) even though the worker pool
// opens several adapter sessions. Found by campaign zero's first armed run,
// which logged one resolution per session.
var campaignIDs sync.Map

// ResetCampaignCache empties the process-level campaign-id cache. Tests that
// assert on lookup counts call it; nothing else should.
func ResetCampaignCache() { campaignIDs = sync.Map{} }

func campaignCacheKey(cfg config) string {
	return cfg.BaseURL + "\x00" + strings.ToLower(strings.TrimSpace(cfg.Campaign))
}

// resolveCampaign turns a campaign name into its id, once per run (SPEC §10.6).
// A value that already looks like an id is passed through untouched.
func (a *Adapter) resolveCampaign(ctx context.Context, cfg config, apiKey string) (string, error) {
	if looksLikeID(cfg.Campaign) {
		return cfg.Campaign, nil
	}
	if id, ok := campaignIDs.Load(campaignCacheKey(cfg)); ok {
		return id.(string), nil
	}

	after := ""
	for page := 0; page < 20; page++ {
		var list campaignList
		err := httpx.JSON(ctx, a.HTTP, httpx.Request{
			Method:   "GET",
			URL:      strings.TrimRight(cfg.BaseURL, "/") + campaignsPath,
			Provider: "instantly",
			Headers:  map[string]string{"Authorization": "Bearer " + apiKey},
			Query:    map[string]string{"limit": "100", "starting_after": after},
		}, &list)
		if err != nil {
			return "", err
		}
		for _, item := range list.Items {
			if strings.EqualFold(strings.TrimSpace(item.Name), strings.TrimSpace(cfg.Campaign)) {
				campaignIDs.Store(campaignCacheKey(cfg), item.ID)
				return item.ID, nil
			}
		}
		if list.NextStartingAfter == "" || len(list.Items) == 0 {
			break
		}
		after = list.NextStartingAfter
	}
	// Failing loudly matters here: silently creating a campaign, or delivering to
	// the wrong one, is worse than stopping (SPEC §10.6).
	return "", fmt.Errorf("instantly: no campaign named %q — create it first or pass its id", cfg.Campaign)
}

// addLead adds one person to the campaign. Everything beyond the email floor
// derives from the variables: mapping (SPEC §10.6, ADR-018): a target name
// matching a first-class lead field maps into the body, anything else becomes
// a custom variable of that name. Blank values never send — the runner's
// on_missing policy has already skipped or failed such records (SPEC §8), so
// the omission here is defense in depth, not the policy itself.
func (a *Adapter) addLead(ctx context.Context, cfg config, apiKey, campaignID string, fields map[string]any) (leadRequest, leadResponse, error) {
	body := leadRequest{
		Campaign:         campaignID,
		Email:            strings.ToLower(strings.TrimSpace(str(fields["email"]))),
		SkipIfInCampaign: cfg.SkipIfInCampaign,
	}

	vars := map[string]string{}
	for target, field := range cfg.Variables {
		v := str(fields[field])
		if v == "" {
			continue
		}
		switch target {
		case "first_name":
			body.FirstName = v
		case "last_name":
			body.LastName = v
		case "company_name":
			body.CompanyName = v
		case "personalization":
			body.Personalization = v
		default:
			vars[target] = v
		}
	}
	if len(vars) > 0 {
		body.CustomVariables = vars
	}

	var out leadResponse
	err := httpx.JSON(ctx, a.HTTP, httpx.Request{
		Method:   "POST",
		URL:      strings.TrimRight(cfg.BaseURL, "/") + leadsPath,
		Provider: "instantly",
		Headers:  map[string]string{"Authorization": "Bearer " + apiKey},
		Body:     body,
	}, &out)
	return body, out, err
}

// looksLikeID recognizes a UUID, which is what Instantly uses for campaign ids.
func looksLikeID(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// campaignDetail is what a campaign read returns, decoded loosely: the
// status and the sequence steps with their variants' subject and body —
// the parts preflight reads (ADR-040). Anything the response does not
// carry is unreadable, never assumed.
type campaignDetail struct {
	Fields map[string]any
}

// getCampaign reads one campaign by id, for preflight.
func (a *Adapter) getCampaign(ctx context.Context, cfg config, apiKey, id string) (campaignDetail, error) {
	var out map[string]any
	err := httpx.JSON(ctx, a.HTTP, httpx.Request{
		Method:   "GET",
		URL:      strings.TrimRight(cfg.BaseURL, "/") + campaignsPath + "/" + id,
		Provider: "instantly",
		Headers:  map[string]string{"Authorization": "Bearer " + apiKey},
		Attempts: 1,
	}, &out)
	return campaignDetail{Fields: out}, err
}

// campaignStatusActive is Instantly's numeric status for an active campaign.
const campaignStatusActive = 1

// active reports the campaign's status, and whether the response carried
// one at all.
func (c campaignDetail) active() (bool, bool) {
	v, ok := c.Fields["status"]
	if !ok || v == nil {
		return false, false
	}
	switch n := v.(type) {
	case float64:
		return int(n) == campaignStatusActive, true
	case string:
		return strings.EqualFold(n, "active") || n == "1", true
	}
	return false, false
}

// steps flattens the sequence into its email steps, each a list of variant
// bodies (subject + body joined, since a merge field may sit in either).
// ok=false when the response carried no readable sequence.
func (c campaignDetail) steps() ([][]string, bool) {
	sequences, ok := c.Fields["sequences"].([]any)
	if !ok {
		return nil, false
	}
	var out [][]string
	for _, seq := range sequences {
		m, _ := seq.(map[string]any)
		stepList, _ := m["steps"].([]any)
		for _, st := range stepList {
			sm, _ := st.(map[string]any)
			if t, _ := sm["type"].(string); t != "" && t != "email" {
				continue
			}
			var variants []string
			for _, v := range asList(sm["variants"]) {
				vm, _ := v.(map[string]any)
				variants = append(variants, str(vm["subject"])+"\n"+str(vm["body"]))
			}
			if len(variants) == 0 {
				variants = append(variants, str(sm["subject"])+"\n"+str(sm["body"]))
			}
			out = append(out, variants)
		}
	}
	return out, len(out) > 0
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}
