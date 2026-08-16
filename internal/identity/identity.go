// Package identity canonicalizes incoming records into identity keys.
// Canonicalization lives here and nowhere else — adapters never compute keys
// (SPEC §4).
package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// Entity types recognized in v0. The set is extensible; the ledger stores
// whatever it is handed.
const (
	Person  = "person"
	Company = "company"
)

// Strength ranks how durable a key is. A record that arrives with a stronger
// key than the identity it matches upgrades that identity in place (SPEC §4).
// Values are compared relatively and never persisted.
type Strength int

const (
	StrengthNameHash Strength = 1
	StrengthTwitter  Strength = 2 // reserved handle tier, key prefix "tw:" (ADR-020)
	StrengthGitHub   Strength = 3 // reserved handle tier, key prefix "gh:" (ADR-020)
	StrengthSlug     Strength = 4 // normalized public LinkedIn slug
	StrengthDomain   Strength = 4 // registrable domain, for companies
	StrengthEmail    Strength = 5
)

// Key is a canonical identity key plus how strong it is.
type Key struct {
	EntityType string
	Value      string
	Strength   Strength
}

// Candidates returns every key that can be derived from fields for the given
// entity type, strongest first. The first element is the key a new identity
// should be created with; all of them are worth looking up, because the record
// may already exist under a weaker key.
//
// An empty slice (with a nil error) means the record carries nothing
// identifying; callers should treat that as a failed record.
func Candidates(entityType string, fields map[string]any) ([]Key, error) {
	switch entityType {
	case Person:
		return personCandidates(fields), nil
	case Company:
		return companyCandidates(fields), nil
	default:
		return nil, fmt.Errorf("identity: unknown entity_type %q", entityType)
	}
}

// KeyFor returns the strongest key derivable from fields.
func KeyFor(entityType string, fields map[string]any) (Key, error) {
	cands, err := Candidates(entityType, fields)
	if err != nil {
		return Key{}, err
	}
	if len(cands) == 0 {
		return Key{}, fmt.Errorf("identity: no identity key derivable for %s record", entityType)
	}
	return cands[0], nil
}

func personCandidates(fields map[string]any) []Key {
	var out []Key
	if email := NormalizeEmail(str(fields, "email")); email != "" {
		out = append(out, Key{Person, email, StrengthEmail})
	}
	// Only the public vanity form keys (SPEC §4, ADR-020): internal and
	// Sales-Navigator URLs are separate fields and never key material.
	if slug := NormalizeLinkedIn(str(fields, "linkedin_url")); slug != "" {
		out = append(out, Key{Person, slug, StrengthSlug})
	}
	if h := NormalizeHandle(str(fields, "github_username")); h != "" {
		out = append(out, Key{Person, "gh:" + h, StrengthGitHub})
	}
	if h := NormalizeHandle(str(fields, "twitter_handle")); h != "" {
		out = append(out, Key{Person, "tw:" + h, StrengthTwitter})
	}
	if name := normalizeName(personName(fields)); name != "" {
		domain := NormalizeDomain(str(fields, "company_domain"))
		out = append(out, Key{Person, nameHash(name + "|" + domain), StrengthNameHash})
	}
	return out
}

func companyCandidates(fields map[string]any) []Key {
	var out []Key
	if d := NormalizeDomain(firstNonEmpty(str(fields, "domain"), str(fields, "company_domain"), str(fields, "website"))); d != "" {
		out = append(out, Key{Company, d, StrengthDomain})
	}
	if name := normalizeName(firstNonEmpty(str(fields, "name"), str(fields, "company_name"))); name != "" {
		out = append(out, Key{Company, nameHash(name), StrengthNameHash})
	}
	return out
}

// NormalizeEmail lowercases and trims an email address. Anything without an
// "@" between two non-empty halves is not an email.
func NormalizeEmail(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	at := strings.LastIndex(s, "@")
	if at <= 0 || at == len(s)-1 || strings.ContainsAny(s, " \t") {
		return ""
	}
	return s
}

// LinkedInShape classifies the observable URL shapes SPEC §4 (ADR-020) keeps
// as explicitly distinct fields, so they can never collide under one name.
type LinkedInShape int

const (
	LinkedInNone     LinkedInShape = iota // not a usable LinkedIn URL
	LinkedInPublic                        // public vanity URL → linkedin_url
	LinkedInInternal                      // opaque member token / profile paths → linkedin_internal_url
	LinkedInSalesNav                      // sales/… paths → linkedin_sales_nav_url
)

// ClassifyLinkedIn reports which shape a LinkedIn-URL-ish value is. Adapters
// use it to emit the matching canonical field at their own boundary (SPEC §4).
func ClassifyLinkedIn(s string) LinkedInShape {
	path := linkedinPath(s)
	if path == "" {
		return LinkedInNone
	}
	segs := strings.Split(path, "/")
	switch strings.ToLower(segs[0]) {
	case "sales":
		return LinkedInSalesNav
	case "profile", "talent":
		return LinkedInInternal
	case "in", "pub":
		if len(segs) < 2 || segs[1] == "" {
			return LinkedInNone
		}
		if isMemberToken(segs[1]) {
			return LinkedInInternal
		}
		return LinkedInPublic
	case "company", "school", "showcase":
		// Public organization pages; keyable for companies.
		if len(segs) < 2 || segs[1] == "" {
			return LinkedInNone
		}
		return LinkedInPublic
	default:
		return LinkedInNone
	}
}

// NormalizeLinkedIn reduces a PUBLIC LinkedIn URL to its path slug: protocol,
// host, query, fragment and trailing slash stripped, lowercased — e.g.
// "https://www.linkedin.com/in/Jane-Doe/?trk=x" becomes "in/jane-doe". Any
// non-public shape returns "" — internal and Sales-Navigator forms are never
// key material (SPEC §4, ADR-020).
func NormalizeLinkedIn(s string) string {
	if ClassifyLinkedIn(s) != LinkedInPublic {
		return ""
	}
	return strings.ToLower(linkedinPath(s))
}

// NormalizeLinkedInURL is the registry's linkedin_url rule (SPEC §4a): the
// canonical stored form of a public LinkedIn URL. Any other shape is an
// invalid value for the field and returns "".
func NormalizeLinkedInURL(s string) string {
	slug := NormalizeLinkedIn(s)
	if slug == "" {
		return ""
	}
	return "https://www.linkedin.com/" + slug
}

// linkedinPath extracts a LinkedIn URL's path — query/fragment stripped, host
// dropped, trailing slash trimmed, percent-escapes resolved — with case
// preserved (internal member tokens are case-sensitive).
func linkedinPath(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = stripPrefixFold(s, "https://")
	s = stripPrefixFold(s, "http://")
	// Drop a leading host (anything up to the first slash that looks like a host).
	if i := strings.Index(s, "/"); i > 0 && strings.Contains(s[:i], ".") {
		s = s[i+1:]
	}
	s = strings.Trim(s, "/")
	if s == "" {
		return ""
	}
	// Percent-escapes are common in scraped URLs; unescape so equivalent URLs
	// collapse to one value.
	if un, err := url.PathUnescape(s); err == nil {
		s = un
	}
	return s
}

// isMemberToken reports whether a path slug is a LinkedIn opaque member token
// (SPEC §4): a case-insensitive acwaa/acoaa prefix followed by a base64-like
// tail. Errs toward false — a false positive would demote a real vanity slug,
// a false negative keys on an opaque token; the prefix check plus length makes
// either vanishingly rare.
func isMemberToken(slug string) bool {
	l := strings.ToLower(slug)
	if !strings.HasPrefix(l, "acwaa") && !strings.HasPrefix(l, "acoaa") {
		return false
	}
	if len(slug) < 12 {
		return false
	}
	for _, r := range slug {
		ok := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '='
		if !ok {
			return false
		}
	}
	return true
}

// NormalizeHandle is the registry's handle rule (SPEC §4a, the reserved
// github_username/twitter_handle tiers): trim, strip a leading @, strip a
// github.com / twitter.com / x.com URL prefix, lowercase.
func NormalizeHandle(s string) string {
	s = strings.TrimSpace(s)
	s = stripPrefixFold(s, "https://")
	s = stripPrefixFold(s, "http://")
	s = stripPrefixFold(s, "www.")
	for _, host := range []string{"github.com/", "twitter.com/", "x.com/"} {
		s = stripPrefixFold(s, host)
	}
	s = strings.TrimPrefix(s, "@")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || strings.ContainsAny(s, " \t") {
		return ""
	}
	return s
}

// stripPrefixFold removes a case-insensitive prefix.
func stripPrefixFold(s, prefix string) string {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):]
	}
	return s
}

// NormalizeDomain reduces a domain, host or URL to its registrable domain
// (eTLD+1), lowercased — e.g. "https://www.Acme.co.uk/about" becomes
// "acme.co.uk". Returns "" if no registrable domain can be found.
func NormalizeDomain(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 { // an email in a domain column
		s = s[i+1:]
	}
	if i := strings.Index(s, ":"); i >= 0 { // port
		s = s[:i]
	}
	s = strings.Trim(s, ".")
	if s == "" || !strings.Contains(s, ".") {
		return ""
	}
	etld1, err := publicsuffix.EffectiveTLDPlusOne(s)
	if err != nil {
		return s
	}
	return etld1
}

func personName(fields map[string]any) string {
	if s := firstNonEmpty(str(fields, "full_name"), str(fields, "name")); s != "" {
		return s
	}
	first, last := strings.TrimSpace(str(fields, "first_name")), strings.TrimSpace(str(fields, "last_name"))
	if first != "" && last != "" {
		return first + " " + last
	}
	return ""
}

// normalizeName lowercases, trims, and collapses internal whitespace.
func normalizeName(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func nameHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "nh:" + hex.EncodeToString(sum[:])
}

// str reads a field as a string. Numbers and other scalars that arrive from
// CSV or JSON are formatted rather than dropped.
func str(fields map[string]any, key string) string {
	v, ok := fields[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	default:
		return fmt.Sprint(t)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
