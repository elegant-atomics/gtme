package e2e

// M28 acceptance (SPEC §11, ADR-054), the type side: a traverse binding
// emitting posts with no key fails plan and verify naming the missing tier;
// a binding shipping types/post.json is refused by verify as a reserved
// name; two installed bindings shipping types/job_posting.json with
// different content fail plan naming both paths; a binding that ships the
// type it emits plans, verifies (printing the type), and simulates from its
// fixtures.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixturePostsBinding = `id: fixture/posts
version: 1
role: traverse
from: person
entity_type: post
relation: {name: authored_by, from: record}
needs:
  type: object
  required: [email]
  properties:
    email: {type: string}
provides:
  type: object
  additionalProperties: false
  properties:
    url: {type: string}
    text: {type: string}
config_schema:
  type: object
  additionalProperties: false
  properties:
    base_url: {type: string, default: "http://fixture.test"}
cost_estimate_usd: 0
request:
  method: GET
  url: "{{config.base_url}}/posts"
  query:
    author: "{{record.email}}"
extract:
  records: posts
  fields:
    url: {path: url, transform: url}
    text: text
`

const fixturePostsFixtures = `{
  "input": {"email": "jane.doe@acme.com"},
  "responses": [
    {"match": "GET /posts", "body": {"posts": [
      {"url": "https://www.linkedin.com/posts/fixture-1/", "text": "one"},
      {"url": "https://www.linkedin.com/posts/fixture-2", "text": "two"}
    ]}}
  ]
}`

const jobPostingType = `{
  "entity_type": "job_posting",
  "version": 1,
  "kind": "signal",
  "description": "A job posting: a signal keyed on its public URL.",
  "identity": [{"field": "url"}],
  "fields": [
    {"name": "url", "tier": "identity", "type": "string", "format": "uri", "normalization": "url", "description": "The posting's public URL.", "example": "https://jobs.example/1"},
    {"name": "title", "tier": "core", "type": "string", "normalization": "trim", "description": "The role's title.", "example": "Head of Growth"}
  ]
}`

const jobsBinding = `id: jobs/%s
version: 1
role: traverse
from: company
entity_type: job_posting
relation: {name: posted_by, from: record}
needs:
  type: object
  required: [company_domain]
  properties:
    company_domain: {type: string}
provides:
  type: object
  additionalProperties: false
  properties:
    url: {type: string}
    title: {type: string}
cost_estimate_usd: 0
request:
  method: GET
  url: "http://fixture.test/jobs"
  query:
    domain: "{{record.company_domain}}"
extract:
  records: jobs
  fields:
    url: {path: url, transform: url}
    title: title
`

const jobsFixtures = `{
  "input": {"company_domain": "acme.com"},
  "responses": [{"match": "GET /jobs", "body": {"jobs": [{"url": "https://jobs.example/1", "title": "Head of Growth"}]}}]
}`

// writeBindingDir installs a binding directory into the harness home with
// its fixtures and, optionally, the type files it ships.
func (h *harness) writeBindingDir(dirName, doc, fixtures string, types map[string]string) string {
	h.t.Helper()
	dir := filepath.Join(h.home, ".gtme", "adapters", dirName)
	for _, sub := range []string{"fixtures", "types"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			h.t.Fatalf("mkdir: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "binding.yaml"), []byte(doc), 0o644); err != nil {
		h.t.Fatalf("write binding: %v", err)
	}
	if fixtures != "" {
		if err := os.WriteFile(filepath.Join(dir, "fixtures", "conformance.json"), []byte(fixtures), 0o644); err != nil {
			h.t.Fatalf("write fixtures: %v", err)
		}
	}
	for name, body := range types {
		if err := os.WriteFile(filepath.Join(dir, "types", name+".json"), []byte(body), 0o644); err != nil {
			h.t.Fatalf("write type: %v", err)
		}
	}
	return dir
}

func TestTraverseBindingVerifiesPlansAndSimulates(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.writeBindingDir("fixture-posts", fixturePostsBinding, fixturePostsFixtures, nil)

	verify := h.mustRun("adapters", "verify", "fixture/posts")
	contains(t, verify.stderr, "fixture/posts v1 — traverse (person → post, authored_by)", "verify prints the crossing")
	contains(t, verify.stderr, "2 record(s) extracted", "the fixtures drive the crossing from a sample parent")

	h.write("posts.yaml", `name: fixture-posts
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: posts
    use: fixture/posts
group: posts-found
`)
	plan := h.mustRun("plan", "posts.yaml")
	contains(t, plan.stderr, "traverse:  person → post via fixture/posts (authored_by)", "plan line")

	sim := h.mustRun("run", "posts.yaml", "--simulate")
	// Every parent gets the same two fixture posts: the first parent mints
	// them, the rest coalesce (SPEC §8) — the crossing rehearsed from
	// fixtures with no network.
	contains(t, sim.stderr, "posts: 3 in, 3 out, 0 cached, 0 filtered, 0 failed — 2 traversed (post), 4 coalesced", "simulated traverse line")
	contains(t, sim.stderr, `group "posts-found": 2 record(s) would be added (held back — simulated run)`, "simulated terminus")
	if n := h.queryInt(`SELECT count(*) FROM identities WHERE entity_type = 'post'`); n != 0 {
		t.Errorf("a simulated run persisted %d posts", n)
	}
}

func TestTraverseBindingWithoutAKeyFailsPlanAndVerify(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	unkeyed := strings.Replace(strings.Replace(fixturePostsBinding, "id: fixture/posts", "id: fixture/unkeyed", 1),
		"    url: {type: string}\n    text: {type: string}", "    text: {type: string}", 1)
	unkeyed = strings.Replace(unkeyed, "    url: {path: url, transform: url}\n", "", 1)
	h.writeBindingDir("fixture-unkeyed", unkeyed, fixturePostsFixtures, nil)

	verify := h.run("adapters", "verify", "fixture/unkeyed")
	if verify.code != 2 {
		t.Fatalf("verify exit = %d, want 2\n%s", verify.code, verify.stderr)
	}
	contains(t, verify.stderr, "no identity-key path: none of the fields it provides (text) can key a post (SPEC §4 tiers: url)", "verify names the missing tier")

	h.write("unkeyed.yaml", `name: unkeyed
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: posts
    use: fixture/unkeyed
`)
	plan := h.run("plan", "unkeyed.yaml")
	if plan.code != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", plan.code, plan.stderr)
	}
	contains(t, plan.stderr, "no identity-key path: none of the fields it provides (text) can key a post", "plan names the missing tier, before anything is billed")
}

func TestBindingShippingAnEmbeddedTypeIsRefused(t *testing.T) {
	h := newHarness(t)
	h.writeBindingDir("fixture-posts", fixturePostsBinding, fixturePostsFixtures, map[string]string{
		"post": strings.Replace(jobPostingType, "job_posting", "post", 1),
	})
	verify := h.run("adapters", "verify", "fixture/posts")
	if verify.code != 2 {
		t.Fatalf("verify exit = %d, want 2\n%s", verify.code, verify.stderr)
	}
	contains(t, verify.stderr, `ships types/post.json, but "post" is a type this build embeds`, "reserved name")
	contains(t, verify.stderr, "refusing to install", "nothing installs")
}

func TestBindingBringsTheTypeItEmits(t *testing.T) {
	h := newHarness(t)
	h.write("companies.csv", companiesCSV)
	dirA := h.writeBindingDir("jobs-a", strings.Replace(jobsBinding, "%s", "a", 1), jobsFixtures, map[string]string{"job_posting": jobPostingType})

	verify := h.mustRun("adapters", "verify", "jobs/a")
	contains(t, verify.stderr, "jobs/a v1 — traverse (company → job_posting, posted_by)", "verify prints the crossing")
	contains(t, verify.stderr, "ships type:  job_posting (types/job_posting.json) — read in place", "verify prints the shipped type")

	h.write("jobs.yaml", `name: company-jobs
source:
  use: csv/source
  with:
    path: companies.csv
    entity_type: company
steps:
  - id: jobs
    use: jobs/a
group: openings
`)
	plan := h.mustRun("plan", "jobs.yaml")
	contains(t, plan.stderr, "traverse:  company → job_posting via jobs/a (posted_by)", "the type resolves in place, under the binding")
	contains(t, plan.stderr, `added to group "openings" as job_posting`, "the terminus takes the binding's type")

	// A second binding shipping a different job_posting.json: the name no
	// longer resolves to exactly one file, and the plan names both paths.
	dirB := h.writeBindingDir("jobs-b", strings.Replace(jobsBinding, "%s", "b", 1), jobsFixtures, map[string]string{
		"job_posting": strings.Replace(jobPostingType, "A job posting", "A job posting, differently", 1),
	})
	res := h.run("plan", "jobs.yaml")
	if res.code != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "two type files disagree", "conflict")
	contains(t, res.stderr, filepath.Join(dirA, "types", "job_posting.json"), "names the first path")
	contains(t, res.stderr, filepath.Join(dirB, "types", "job_posting.json"), "names the second path")

	// Identical content is one type, not a conflict.
	if err := os.WriteFile(filepath.Join(dirB, "types", "job_posting.json"), []byte(jobPostingType), 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustRun("plan", "jobs.yaml")
}

func TestOperatorTypeFileInHome(t *testing.T) {
	h := newHarness(t)
	h.write("companies.csv", companiesCSV)
	// A hand-placed type under ~/.gtme/types is the third discovery source.
	dir := filepath.Join(h.home, ".gtme", "types")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "job_posting.json"), []byte(jobPostingType), 0o644); err != nil {
		t.Fatal(err)
	}
	h.writeBindingDir("jobs-a", strings.Replace(jobsBinding, "%s", "a", 1), jobsFixtures, nil)
	h.write("jobs.yaml", `name: company-jobs
source:
  use: csv/source
  with:
    path: companies.csv
    entity_type: company
steps:
  - id: jobs
    use: jobs/a
`)
	h.mustRun("plan", "jobs.yaml")
	// A type nothing defines fails plan naming the sources.
	if err := os.Remove(filepath.Join(dir, "job_posting.json")); err != nil {
		t.Fatal(err)
	}
	res := h.run("plan", "jobs.yaml")
	if res.code != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `type "job_posting" has no type file`, "an unknown type is a plan error")
}
