package apollo

// This file is the entire HTTP surface of the Apollo adapter: the endpoint, the
// request body, the response shape, and the field mapping. If Apollo changes
// their API, everything that needs correcting is here.
//
// Endpoint: POST https://api.apollo.io/api/v1/mixed_people/search
// Auth:     X-Api-Key header (SPEC §10.2)
// Docs:     https://docs.apollo.io/ (People Search)

import (
	"context"
	"strings"

	"github.com/trevorfox/gtm/internal/httpx"
	"github.com/trevorfox/gtm/internal/identity"
)

// DefaultBaseURL is Apollo's API host.
const DefaultBaseURL = "https://api.apollo.io"

const searchPath = "/api/v1/mixed_people/search"

// searchRequest is the JSON body Apollo's people search takes.
type searchRequest struct {
	Query                      string   `json:"q_keywords,omitempty"`
	PersonTitles               []string `json:"person_titles,omitempty"`
	PersonSeniorities          []string `json:"person_seniorities,omitempty"`
	OrganizationLocations      []string `json:"organization_locations,omitempty"`
	OrganizationEmployeeRanges []string `json:"organization_num_employees_ranges,omitempty"`
	OrganizationDomains        []string `json:"q_organization_domains_list,omitempty"`
	Page                       int      `json:"page"`
	PerPage                    int      `json:"per_page"`
}

// searchResponse is the subset of Apollo's response we read. Apollo has shipped
// pagination both at the top level and nested; both are accepted here.
type searchResponse struct {
	People     []person `json:"people"`
	Contacts   []person `json:"contacts"`
	Page       int      `json:"page"`
	PerPage    int      `json:"per_page"`
	TotalPages int      `json:"total_pages"`
	Pagination struct {
		Page         int `json:"page"`
		PerPage      int `json:"per_page"`
		TotalEntries int `json:"total_entries"`
		TotalPages   int `json:"total_pages"`
	} `json:"pagination"`
}

func (r searchResponse) results() []person {
	if len(r.People) > 0 {
		return r.People
	}
	return r.Contacts
}

func (r searchResponse) totalPages() int {
	if r.Pagination.TotalPages > 0 {
		return r.Pagination.TotalPages
	}
	return r.TotalPages
}

// person is one search result.
type person struct {
	ID          string `json:"id"`
	FirstName   string `json:"first_name"`
	LastName    string `json:"last_name"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	Seniority   string `json:"seniority"`
	Email       string `json:"email"`
	EmailStatus string `json:"email_status"`
	LinkedinURL string `json:"linkedin_url"`
	City        string `json:"city"`
	State       string `json:"state"`
	Country     string `json:"country"`

	Organization struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		WebsiteURL    string `json:"website_url"`
		PrimaryDomain string `json:"primary_domain"`
		LinkedinURL   string `json:"linkedin_url"`
		Industry      string `json:"industry"`
		EmployeeCount int    `json:"estimated_num_employees"`
	} `json:"organization"`
}

// fields maps one Apollo person onto canonical field names, normalized at this
// adapter's own boundary (SPEC §4a: vendor dialect → canonical). Values Apollo
// withholds (notably locked emails on the search endpoint) are omitted rather
// than stored as placeholders.
func (p person) fields() map[string]any {
	out := map[string]any{}
	set := func(k, v string) {
		if s := strings.TrimSpace(v); s != "" {
			out[k] = s
		}
	}

	set("apollo.id", p.ID) // vendor-namespaced: no second provider has this fact
	set("first_name", p.FirstName)
	set("last_name", p.LastName)
	set("full_name", p.Name)
	set("title", p.Title)
	set("seniority", strings.ToLower(p.Seniority))
	set("city", p.City)
	set("state", p.State)
	set("country", p.Country)
	set("email_status", strings.ToLower(p.EmailStatus))

	// A LinkedIn-URL-shaped value is classified and routed to the field for its
	// shape (SPEC §4, ADR-020) — the shapes are distinct vocabulary, never one
	// field holding both.
	switch identity.ClassifyLinkedIn(p.LinkedinURL) {
	case identity.LinkedInPublic:
		out["linkedin_url"] = identity.NormalizeLinkedInURL(p.LinkedinURL)
	case identity.LinkedInInternal:
		set("linkedin_internal_url", p.LinkedinURL)
	case identity.LinkedInSalesNav:
		set("linkedin_sales_nav_url", p.LinkedinURL)
	}

	// Apollo returns a placeholder like "email_not_unlocked@domain.com" for
	// contacts whose email you have not paid to reveal. Storing that would poison
	// the identity key, so it is dropped.
	if email := identity.NormalizeEmail(p.Email); email != "" && !strings.Contains(email, "email_not_unlocked") {
		out["email"] = email
	}

	set("company_name", p.Organization.Name)
	set("company_website", p.Organization.WebsiteURL)
	if u := identity.NormalizeLinkedInURL(p.Organization.LinkedinURL); u != "" {
		out["company_linkedin_url"] = u
	}
	set("company_industry", p.Organization.Industry)
	if domain := identity.NormalizeDomain(p.Organization.PrimaryDomain); domain != "" {
		out["company_domain"] = domain
	} else if domain := identity.NormalizeDomain(p.Organization.WebsiteURL); domain != "" {
		out["company_domain"] = domain
	}
	if p.Organization.EmployeeCount > 0 {
		out["company_employees"] = p.Organization.EmployeeCount
	}
	return out
}

// search fetches one page.
func (a *Adapter) search(ctx context.Context, cfg config, apiKey string, page int) (searchResponse, error) {
	body := searchRequest{
		Query:                      cfg.Query,
		PersonTitles:               cfg.Titles,
		PersonSeniorities:          cfg.Seniorities,
		OrganizationLocations:      cfg.Locations,
		OrganizationEmployeeRanges: cfg.EmployeeRanges,
		OrganizationDomains:        cfg.Domains,
		Page:                       page,
		PerPage:                    cfg.PerPage,
	}

	var out searchResponse
	err := httpx.JSON(ctx, a.HTTP, httpx.Request{
		Method:   "POST",
		URL:      strings.TrimRight(cfg.BaseURL, "/") + searchPath,
		Provider: "apollo",
		Headers: map[string]string{
			"X-Api-Key":     apiKey,
			"Cache-Control": "no-cache",
		},
		Body: body,
	}, &out)
	return out, err
}
