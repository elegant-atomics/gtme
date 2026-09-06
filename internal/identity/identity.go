// Package identity holds the normalization rules identity keys and canonical
// values share (SPEC §4, §4a): each rule exists exactly once, here. Which
// rules a type keys on, and in what order, is the type file's identity list,
// read by internal/registry — adapters never compute keys.
package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// Person and Company name the two embedded subject types (SPEC §4a,
// ADR-054). Since ADR-054 the set of types is a set of files, and no code
// here switches on these names — they exist for the callers that mean one
// specific type (the reference minting in the runner, tests).
const (
	Person  = "person"
	Company = "company"
)

// Strength ranks how durable a key is: a tier's position in its type file's
// identity list, first (strongest) highest (SPEC §4, ADR-054). A record that
// arrives with a stronger key than the identity it matches upgrades that
// identity in place. Values are compared relatively, within one type, and
// never persisted.
type Strength int

// Key is a canonical identity key plus how strong it is.
type Key struct {
	EntityType string
	Value      string
	Strength   Strength
}

// KeyRules are the normalization rules a field tier may name (SPEC §4a,
// ADR-054): each is a public-identifier rule, so a key derived from it
// cannot fork when a second vendor arrives.
var KeyRules = map[string]bool{"email": true, "domain": true, "linkedin_url": true, "handle": true, "url": true}

// KeyForm derives the identity-key value a field tier contributes from a
// raw value under one of the KeyRules: the rule's normalized value, except
// that linkedin_url keys on the public slug ("in/jane-doe", SPEC §4) rather
// than the stored canonical URL. Empty means the value is not a key.
func KeyForm(rule, value string) string {
	switch rule {
	case "email":
		return NormalizeEmail(value)
	case "domain":
		return NormalizeDomain(value)
	case "linkedin_url":
		return NormalizeLinkedIn(value)
	case "handle":
		return NormalizeHandle(value)
	case "url":
		return NormalizeURL(value)
	}
	return ""
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

// NormalizeName is the hash-tier value rule (SPEC §4a, ADR-054): lowercase,
// trimmed, internal whitespace collapsed.
func NormalizeName(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// NameHash is the nh: fallback tier's key: sha256 over the joined
// components, hex, under the tier's prefix (SPEC §4).
func NameHash(prefix, s string) string {
	sum := sha256.Sum256([]byte(s))
	return prefix + hex.EncodeToString(sum[:])
}

// NormalizeURL is the registry's url rule (SPEC §4, ADR-054): a
// platform-public URL as a key. Trim; lowercase the scheme and host; drop
// the fragment and any trailing slash; keep the path and query as written.
// Anything without an http(s) scheme and a host is not a URL under this
// rule and returns "".
func NormalizeURL(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = s[:i]
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	u.Scheme = scheme
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	u.RawFragment = ""
	out := u.String()
	for strings.HasSuffix(out, "/") && !strings.HasSuffix(out, "://") {
		out = strings.TrimSuffix(out, "/")
	}
	return out
}

// Str reads a field as a string. Numbers and other scalars that arrive from
// CSV or JSON are formatted rather than dropped.
func Str(fields map[string]any, key string) string {
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
