package e2e

// M11 acceptance (SPEC §10a/§11): the transform floor plus payload
// retention, offline — http/enrich fetches a local page to markdown into a
// declared field with the raw response retained as a payload and the second
// run cache-skipping the fetch; sql/enrich derives a field with query-hash
// provenance; sql/filter gates by predicate in both styles; gtme vacuum
// reports; --simulate counts http/enrich as a gap while SQL runs.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const homepageHTML = `<html><head><title>x</title><style>p{}</style></head>
<body><h1>Acme Inc</h1><p>We make <strong>anvils</strong> for coyotes.</p>
<ul><li>Fast shipping</li></ul><a href="/pricing">Pricing</a></body></html>`

func TestHTTPEnrichMarkdownPayloadAndCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(homepageHTML))
	}))
	defer srv.Close()

	h := newHarness(t)
	h.write("contacts.csv", campaignZeroCSV)
	h.write("p.yaml", `name: homepage-read
source:
  use: csv/source
  with:
    path: contacts.csv
    columns:
      full_name: Full Name
      email: Email
      company_domain: Company Website
steps:
  - id: fetch
    use: http/enrich
    with:
      url: "`+srv.URL+`/site?d={{record.company_domain}}"
      markdown: true
      field: web.homepage
      freshness_days: 7
`)
	res := h.run("run", "p.yaml")
	if res.code != 0 {
		t.Fatalf("run exit = %d\nstderr:\n%s", res.code, res.stderr)
	}

	// The declared content field holds markdown, provenance http/enrich@1.
	values := h.queryStrings(`SELECT value FROM field_values WHERE field = 'web.homepage' AND source = 'http/enrich@1'`)
	if len(values) != 3 {
		t.Fatalf("web.homepage rows = %d, want 3", len(values))
	}
	if !strings.Contains(values[0], `# Acme Inc`) || !strings.Contains(values[0], `**anvils**`) ||
		!strings.Contains(values[0], `- Fast shipping`) {
		t.Errorf("markdown conversion missing structure:\n%s", values[0])
	}
	// The raw response is retained in the payload cache tier (ADR-030).
	if n := h.queryInt(`SELECT count(*) FROM payloads WHERE adapter = 'http/enrich' AND content_type LIKE 'text/html%'`); n != 3 {
		t.Errorf("payloads = %d, want 3", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM payloads WHERE expires_at IS NOT NULL`); n != 3 {
		t.Errorf("payloads without expiry = default TTL missing")
	}

	// Second run inside the freshness window: fetch-once economics — the
	// content field is fresh, so the step cache-skips every record.
	res = h.run("run", "p.yaml")
	if res.code != 0 {
		t.Fatalf("re-run exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "fetch: 0 in, 0 out, 3 cached", "cache-skip step line")

	// gtme vacuum: nothing has expired; facts and payloads survive.
	res = h.mustRun("vacuum")
	contains(t, res.stderr, "evicted 0 expired payload(s)", "vacuum output")
	if n := h.queryInt(`SELECT count(*) FROM payloads`); n != 3 {
		t.Errorf("vacuum touched unexpired payloads: %d left", n)
	}

	// Under --simulate, http/enrich is a counted gap (live fetching only).
	res = h.run("run", "p.yaml", "--simulate")
	if res.code != 0 {
		t.Fatalf("simulate exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "simulation gap: fetch (http/enrich)", "simulate receipt")
}

func TestSQLEnrichAndFilter(t *testing.T) {
	h := newHarness(t)
	h.write("contacts.csv", campaignZeroCSV)
	h.write("p.yaml", `name: transform-floor
source:
  use: csv/source
  with:
    path: contacts.csv
    columns:
      full_name: Full Name
      email: Email
      company_domain: Company Website
steps:
  - id: shout
    use: sql/enrich
    with:
      uses: [full_name]
      provides: [sql.shout]
      query: >
        SELECT identity_id, upper(json_extract(value, '$')) AS "sql.shout"
        FROM current_fields WHERE field = 'full_name'
  - id: colorful
    use: sql/filter
    with:
      query: >
        SELECT identity_id FROM current_fields WHERE field = 'csv.favorite_color'
`)
	res := h.run("run", "p.yaml")
	if res.code != 0 {
		t.Fatalf("run exit = %d\nstderr:\n%s", res.code, res.stderr)
	}

	// Derived values append with query-hash provenance (SPEC §10a).
	shouts := h.queryStrings(`SELECT value FROM field_values WHERE field = 'sql.shout' ORDER BY value`)
	if len(shouts) != 2 || !strings.Contains(shouts[0]+shouts[1], "JANE DOE") {
		t.Errorf("sql.shout = %v, want JANE DOE + BOB STONE (carol has no name)", shouts)
	}
	sources := h.queryStrings(`SELECT DISTINCT source FROM field_values WHERE field = 'sql.shout'`)
	if len(sources) != 1 || !strings.HasPrefix(sources[0], "sql/enrich @ ") {
		t.Errorf("provenance = %v, want sql/enrich @ <hash>", sources)
	}

	// Membership-style filter: jane (teal) and carol (mauve) pass, bob has no
	// favorite color and is filtered with the predicate named.
	contains(t, res.stderr, "colorful: 3 in, 2 out", "filter step line")
	if n := h.queryInt(`SELECT count(*) FROM run_records WHERE verdicts LIKE '%"colorful":"fail"%'`); n != 1 {
		t.Errorf("filtered records = %d, want 1", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE event = 'done' AND detail LIKE '%not selected by predicate%'`); n != 1 {
		t.Errorf("membership-style fail reason missing")
	}

	// Explicit-pass style: a pass column with a reason judges directly.
	h.write("p2.yaml", `name: transform-floor-2
source:
  use: csv/source
  with:
    path: contacts.csv
    columns:
      full_name: Full Name
      email: Email
steps:
  - id: named
    use: sql/filter
    with:
      query: >
        SELECT i.id AS identity_id,
               CASE WHEN f.value IS NULL THEN 0 ELSE 1 END AS pass,
               'has a name' AS reason
        FROM identities i
        LEFT JOIN current_fields f ON f.identity_id = i.id AND f.field = 'full_name'
        WHERE i.entity_type = 'person'
`)
	res = h.run("run", "p2.yaml")
	if res.code != 0 {
		t.Fatalf("p2 exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "named: 3 in, 2 out", "explicit-pass step line")

	// SQL steps run under --simulate (offline by construction).
	res = h.run("run", "p2.yaml", "--simulate")
	if res.code != 0 {
		t.Fatalf("simulate exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "named: 3 in, 2 out", "simulated sql step line")
}

// TestTransformPlanGates: the declared contracts fail loudly at plan time.
func TestTransformPlanGates(t *testing.T) {
	h := newHarness(t)
	h.write("contacts.csv", campaignZeroCSV)
	base := `name: gates
source:
  use: csv/source
  with:
    path: contacts.csv
    columns:
      full_name: Full Name
      email: Email
steps:
%s`

	// http/enrich without freshness_days: web content rots — no default.
	h.write("p.yaml", strings.Replace(base, "%s", `  - id: fetch
    use: http/enrich
    with:
      url: "https://example.com/{{record.company_domain}}"
      markdown: true
      field: web.homepage
`, 1))
	res := h.run("plan", "p.yaml")
	if res.code != 2 {
		t.Fatalf("missing freshness_days: plan exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "freshness_days", "plan error")

	// The derived needs gate: the URL references a field nothing provides.
	h.write("p.yaml", strings.Replace(base, "%s", `  - id: fetch
    use: http/enrich
    with:
      url: "https://example.com/{{record.company_domain}}"
      markdown: true
      field: web.homepage
      freshness_days: 7
`, 1))
	res = h.run("plan", "p.yaml")
	if res.code != 2 {
		t.Fatalf("missing derived need: plan exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "company_domain", "plan error names the derived need")

	// sql/enrich without declared provides.
	h.write("p.yaml", strings.Replace(base, "%s", `  - id: derive
    use: sql/enrich
    with:
      query: SELECT identity_id FROM identities
`, 1))
	res = h.run("plan", "p.yaml")
	if res.code != 2 {
		t.Fatalf("missing provides: plan exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "config.provides", "plan error")

	// A mutating statement is rejected before anything runs.
	h.write("p.yaml", strings.Replace(base, "%s", `  - id: derive
    use: sql/enrich
    with:
      provides: [sql.x]
      query: DELETE FROM identities
`, 1))
	res = h.run("plan", "p.yaml")
	if res.code != 2 {
		t.Fatalf("mutating SQL: plan exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
}
