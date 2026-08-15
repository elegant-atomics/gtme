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
type Strength int

const (
	StrengthNameHash Strength = 1
	StrengthSlug     Strength = 2 // normalized LinkedIn slug
	StrengthDomain   Strength = 2 // registrable domain, for companies
	StrengthEmail    Strength = 3
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
	if slug := NormalizeLinkedIn(str(fields, "linkedin_url")); slug != "" {
		out = append(out, Key{Person, slug, StrengthSlug})
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

// NormalizeLinkedIn reduces a LinkedIn URL to its path slug: protocol, host,
// query, fragment and trailing slash stripped, lowercased — e.g.
// "https://www.linkedin.com/in/Jane-Doe/?trk=x" becomes "in/jane-doe".
func NormalizeLinkedIn(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.ToLower(s)
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	// Drop a leading host (anything up to the first slash that looks like a host).
	if i := strings.Index(s, "/"); i > 0 && strings.Contains(s[:i], ".") {
		s = s[i+1:]
	}
	s = strings.Trim(s, "/")
	if s == "" {
		return ""
	}
	// Percent-escapes are common in scraped URLs; unescape so equivalent URLs
	// collapse to one key.
	if un, err := url.PathUnescape(s); err == nil {
		s = un
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
