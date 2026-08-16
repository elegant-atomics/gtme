package harvest

// This file is the entire HTTP surface of the HarvestAPI adapter: endpoints,
// response shapes and field mapping. Correcting a changed API means editing only
// this file.
//
// Endpoints: GET https://api.harvestapi.io/linkedin/profile?url=...
//            GET https://api.harvestapi.io/linkedin/profile-posts?profileId=... (or profile=<url>)
// Auth:      X-API-Key header
// Docs:      https://docs.harvestapi.io/linkedin-api-reference/profile/get

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/trevorfox/gtm/internal/httpx"
)

// DefaultBaseURL is HarvestAPI's host.
const DefaultBaseURL = "https://api.harvestapi.io"

const (
	profilePath = "/linkedin/profile"
	postsPath   = "/linkedin/profile-posts"
)

// profileResponse wraps a single profile.
type profileResponse struct {
	Element profile         `json:"element"`
	Status  json.RawMessage `json:"status"` // number on the live API; never consumed
	Error   string          `json:"error"`
}

// profile is the subset of a HarvestAPI profile gtm maps.
type profile struct {
	ID               string `json:"id"`
	PublicIdentifier string `json:"publicIdentifier"`
	LinkedinURL      string `json:"linkedinUrl"`
	FirstName        string `json:"firstName"`
	LastName         string `json:"lastName"`
	Headline         string `json:"headline"`
	About            string `json:"about"`
	FollowerCount    int    `json:"followerCount"`
	OpenToWork       bool   `json:"openToWork"`

	Location struct {
		LinkedinText string `json:"linkedinText"`
		Parsed       struct {
			Text string `json:"text"`
		} `json:"parsed"`
	} `json:"location"`

	CurrentPosition []position `json:"currentPosition"`
	Experience      []position `json:"experience"`
}

// position is one role in the profile's history.
// flexInt tolerates HarvestAPI sending a number either bare or as a quoted
// string — the live API returns startDate.month as a string where the
// documented shape (and our fixtures) had an int. Found by campaign zero's
// harvest increment, 2026-08-15.
type flexInt int

func (f *flexInt) UnmarshalJSON(raw []byte) error {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		// A non-numeric month/year label ("Present") carries no number; the
		// text field is the fallback the mapping already uses.
		*f = 0
		return nil
	}
	*f = flexInt(n)
	return nil
}

type position struct {
	CompanyName string `json:"companyName"`
	Position    string `json:"position"`
	Title       string `json:"title"`
	Description string `json:"description"`
	StartDate   struct {
		Year  flexInt `json:"year"`
		Month flexInt `json:"month"`
		Text  string  `json:"text"`
	} `json:"startDate"`
	EndDate struct {
		Year  flexInt `json:"year"`
		Month flexInt `json:"month"`
		Text  string  `json:"text"`
	} `json:"endDate"`
	Duration string `json:"duration"`
}

// title prefers the explicit position label, falling back to title.
func (p position) role() string {
	if s := strings.TrimSpace(p.Position); s != "" {
		return s
	}
	return strings.TrimSpace(p.Title)
}

// line renders one role as "Title at Company (2021–present)".
func (p position) line() string {
	role := p.role()
	company := strings.TrimSpace(p.CompanyName)
	switch {
	case role == "" && company == "":
		return ""
	case role == "":
		role = "(role not stated)"
	case company == "":
		company = "(company not stated)"
	}
	out := role + " at " + company
	if span := p.span(); span != "" {
		out += " (" + span + ")"
	}
	return out
}

func (p position) span() string {
	start := yearOf(int(p.StartDate.Year), p.StartDate.Text)
	end := yearOf(int(p.EndDate.Year), p.EndDate.Text)
	switch {
	case start == "" && end == "":
		return strings.TrimSpace(p.Duration)
	case end == "":
		return start + "–present"
	case start == "":
		return end
	default:
		return start + "–" + end
	}
}

func yearOf(year int, text string) string {
	if year > 0 {
		return strconv.Itoa(year)
	}
	return strings.TrimSpace(text)
}

// postsResponse wraps a page of posts. HarvestAPI has used both `elements` and
// `element` for list payloads; both are accepted.
type postsResponse struct {
	Elements []post          `json:"elements"`
	Element  []post          `json:"element"`
	Status   json.RawMessage `json:"status"`
	Error    string          `json:"error"`
}

func (r postsResponse) results() []post {
	if len(r.Elements) > 0 {
		return r.Elements
	}
	return r.Element
}

// post is one LinkedIn post.
type post struct {
	Content string `json:"content"`
	Text    string `json:"text"`
	// postedAt is an object on the live API (string in early fixtures) and is
	// never consumed — decoded loosely so shape drift can't fail the batch.
	PostedAt   json.RawMessage `json:"postedAt"`
	PostedDate json.RawMessage `json:"postedDate"`
}

func (p post) body() string {
	if s := strings.TrimSpace(p.Content); s != "" {
		return s
	}
	return strings.TrimSpace(p.Text)
}

// fetchProfile gets one profile by LinkedIn URL.
func (a *Adapter) fetchProfile(ctx context.Context, cfg config, apiKey, linkedinURL string) (profile, error) {
	query := map[string]string{"url": linkedinURL}
	if cfg.MainOnly {
		query["main"] = "true"
	}

	var out profileResponse
	err := httpx.JSON(ctx, a.HTTP, httpx.Request{
		Method:   "GET",
		URL:      strings.TrimRight(cfg.BaseURL, "/") + profilePath,
		Provider: "harvest",
		Headers:  map[string]string{"X-API-Key": apiKey},
		Query:    query,
	}, &out)
	if err != nil {
		return profile{}, err
	}
	if out.Error != "" {
		return profile{}, fmt.Errorf("harvest: %s", out.Error)
	}
	return out.Element, nil
}

// fetchPosts gets recent posts for one profile. The live API selects the
// target with `profileId` (fastest) or `profile` (a URL) — the `profileUrl`
// param our fixtures assumed does not exist ("No valid target provided");
// found by campaign zero's harvest increment, 2026-08-15.
func (a *Adapter) fetchPosts(ctx context.Context, cfg config, apiKey, profileID, linkedinURL string) ([]post, error) {
	query := map[string]string{"page": "1"}
	if strings.TrimSpace(profileID) != "" {
		query["profileId"] = strings.TrimSpace(profileID)
	} else {
		query["profile"] = linkedinURL
	}
	var out postsResponse
	err := httpx.JSON(ctx, a.HTTP, httpx.Request{
		Method:   "GET",
		URL:      strings.TrimRight(cfg.BaseURL, "/") + postsPath,
		Provider: "harvest",
		Headers:  map[string]string{"X-API-Key": apiKey},
		Query:    query,
	}, &out)
	if err != nil {
		return nil, err
	}
	if out.Error != "" {
		return nil, fmt.Errorf("harvest: %s", out.Error)
	}
	return out.results(), nil
}

// fields maps a profile (and its posts) onto gtm field names.
func fields(p profile, posts []post, limit int) map[string]any {
	out := map[string]any{}
	if s := strings.TrimSpace(p.Headline); s != "" {
		out["headline"] = s
	}
	if s := strings.TrimSpace(p.About); s != "" {
		out["about"] = s
	}
	if s := strings.TrimSpace(p.Location.LinkedinText); s != "" {
		out["location"] = s
	} else if s := strings.TrimSpace(p.Location.Parsed.Text); s != "" {
		out["location"] = s
	}
	if p.FollowerCount > 0 {
		out["follower_count"] = p.FollowerCount
	}
	if p.OpenToWork {
		out["open_to_work"] = true
	}

	current := p.CurrentPosition
	if len(current) == 0 && len(p.Experience) > 0 {
		current = p.Experience[:1]
	}
	if len(current) > 0 {
		if role := current[0].role(); role != "" {
			out["current_role"] = role
		}
		if company := strings.TrimSpace(current[0].CompanyName); company != "" {
			out["current_company"] = company
		}
	}

	history := make([]string, 0, len(p.Experience))
	for _, exp := range p.Experience {
		if line := exp.line(); line != "" {
			history = append(history, line)
		}
	}
	if len(history) > 0 {
		out["role_history"] = history
	}

	if limit > 0 && len(posts) > 0 {
		bodies := make([]string, 0, limit)
		for _, post := range posts {
			if body := post.body(); body != "" {
				bodies = append(bodies, body)
			}
			if len(bodies) >= limit {
				break
			}
		}
		if len(bodies) > 0 {
			out["recent_posts"] = bodies
		}
	}
	return out
}
