package instantly

// This file is the entire HTTP surface of the Instantly adapter: endpoints,
// request and response shapes, and the field mapping.
//
// Endpoints: GET  https://api.instantly.ai/api/v2/campaigns   (resolve name → id)
//            POST https://api.instantly.ai/api/v2/leads       (add lead)
// Auth:      Authorization: Bearer $INSTANTLY_API_KEY
// Docs:      https://developer.instantly.ai/api/v2/lead/createlead

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/trevorfox/gtm/internal/httpx"
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

// campaignIDs caches resolved name → id for the life of the process, so one
// `gtm run` resolves a campaign once (SPEC §10.6) even though the worker pool
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
func (a *Adapter) addLead(ctx context.Context, cfg config, apiKey, campaignID string, fields map[string]any) (leadResponse, error) {
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
	return out, err
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
