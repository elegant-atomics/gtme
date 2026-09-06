package e2e

// M28 acceptance (SPEC §11, ADR-054), fully offline: a type is a file and a
// run is a sequence of typed segments. A person pipeline traverses to posts
// from a fixture adapter, writes authored_by per post, coalesces the post two
// parents share, counts parents in/empty and children traversed/coalesced,
// and ends in a group typed post; a second traverse back to people reaches a
// person already in the run and coalesces; sql/traverse follows works_at to
// the companies of the run's people; the plan shows every type change; a
// cross-segment when: is refused naming the traverse; once: treats a parent
// that reached a traverse as finished; a dry run executes the crossing.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const postsYAML = `name: people-to-posts
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: posts
    use: mock/posts
group: posts-found
`

func TestTraverseOpensATypedSegment(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("posts.yaml", postsYAML)

	plan := h.mustRun("plan", "posts.yaml")
	contains(t, plan.stderr, "traverse:  person → post via mock/posts (authored_by)", "plan prints the type change and its relation")
	contains(t, plan.stderr, "writes:    works_at → company (from company_domain)", "plan prints the relation a reference field writes")
	contains(t, plan.stderr, `terminus: records completing the run are added to group "posts-found" as post`, "the terminus takes the last segment's type")

	res := h.mustRun("run", "posts.yaml")
	// Jane yields two posts, Bob one Jane also authored, Carol none: three
	// parents in, two with children, one empty; two posts minted, one
	// coalesced (SPEC §8).
	contains(t, res.stderr, "posts: 3 in, 2 out, 1 empty, 0 cached, 0 filtered, 0 failed — 2 traversed (post), 1 coalesced", "traverse step line")
	contains(t, res.stderr, "posts: 3 parent(s) in, 2 out, 1 empty — 2 traversed (post), 1 coalesced", "traverse receipt line")
	contains(t, res.stderr, `group "posts-found": 2 record(s) added`, "the terminus adds the posts")

	if n := h.queryInt(`SELECT count(*) FROM identities WHERE entity_type = 'post'`); n != 2 {
		t.Errorf("post identities = %d, want 2", n)
	}
	keys := h.queryStrings(`SELECT identity_key FROM identities WHERE entity_type = 'post' ORDER BY identity_key`)
	if got := strings.Join(keys, ","); got != "https://www.linkedin.com/posts/jane-1,https://www.linkedin.com/posts/shared-1" {
		t.Errorf("post keys = %q", got)
	}
	// authored_by runs from the post to its author (relation.from: record):
	// jane-1 → jane, shared-1 → jane, shared-1 → bob.
	if n := h.queryInt(`SELECT count(*) FROM relations r JOIN identities p ON p.id = r.from_id JOIN identities a ON a.id = r.to_id
		WHERE r.relation = 'authored_by' AND p.entity_type = 'post' AND a.entity_type = 'person'`); n != 3 {
		t.Errorf("authored_by post→person edges = %d, want 3", n)
	}
	// The coalesce is a ledger fact on the identity that absorbed the row.
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE step_id = 'posts' AND event = 'coalesced'`); n != 1 {
		t.Errorf("coalesced events = %d, want 1", n)
	}
	// Every parent is finished at the traverse with its child count.
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE step_id = 'posts' AND event = 'done' AND json_extract(detail, '$.children') = 0`); n != 1 {
		t.Errorf("parents with zero children = %d, want 1 (carol)", n)
	}
	// The group is typed post, and only posts are in it.
	if got := h.queryStrings(`SELECT COALESCE(entity_type, '') FROM groups WHERE name = 'posts-found'`); len(got) != 1 || got[0] != "post" {
		t.Errorf("posts-found entity_type = %v, want post", got)
	}
	if n := h.queryInt(`SELECT count(*) FROM group_members m JOIN identities i ON i.id = m.identity_id JOIN groups g ON g.id = m.group_id
		WHERE g.name = 'posts-found' AND i.entity_type != 'post'`); n != 0 {
		t.Errorf("non-post members in posts-found = %d", n)
	}
	list := h.mustRun("groups")
	contains(t, list.stderr, "posts-found", "groups list")
	contains(t, list.stderr, "post", "groups list shows the type")
	show := h.mustRun("groups", "show", "posts-found")
	contains(t, show.stderr, "group posts-found (post)", "groups show names the type")
	contains(t, show.stderr, "written by:  people-to-posts", "groups show derives the producer from group_events.run_id")

	// A person cannot join a group of posts.
	add := h.run("groups", "add", "posts-found", "jane.doe@acme.com")
	if add.code == 0 {
		t.Errorf("adding a person to a post group should fail")
	}
	contains(t, add.stderr, `group "posts-found" holds post records, not person`, "refusal names both types")
}

func TestTraverseBackCoalescesAPersonAlreadyInTheRun(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("engage.yaml", `name: engagers
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: posts
    use: mock/posts
  - id: engagers
    use: mock/engagers
group: engaged
`)
	plan := h.mustRun("plan", "engage.yaml")
	contains(t, plan.stderr, "traverse:  post → person via mock/engagers (engaged_with)", "the second crossing")
	contains(t, plan.stderr, `added to group "engaged" as person`, "the terminus takes the last segment's type")

	res := h.mustRun("run", "engage.yaml")
	// Two posts in; the shared post's engagers are Bob (a parent of the first
	// traverse — reached again, coalescing) and Dave (new); jane-1's engager
	// is Dave again (coalescing). One person minted, two coalesced.
	contains(t, res.stderr, "engagers: 2 in, 2 out, 0 cached, 0 filtered, 0 failed — 1 traversed (person), 2 coalesced", "second traverse line")
	contains(t, res.stderr, `group "engaged": 2 record(s) added`, "the terminus adds the last segment's completers only")
	members := h.queryStrings(`SELECT i.identity_key FROM group_members m JOIN identities i ON i.id = m.identity_id JOIN groups g ON g.id = m.group_id
		WHERE g.name = 'engaged' ORDER BY i.identity_key`)
	if got := strings.Join(members, ","); got != "bob@globex.io,dave@newco.example" {
		t.Errorf("engaged = %q, want bob (coalesced, state advanced) and dave", got)
	}
	// Bob is one row in the run — a parent that continued as a child.
	if n := h.queryInt(`SELECT count(*) FROM run_records rr JOIN identities i ON i.id = rr.identity_id WHERE i.identity_key = 'bob@globex.io'`); n != 1 {
		t.Errorf("bob's run_records rows = %d, want 1", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM relations WHERE relation = 'engaged_with'`); n != 3 {
		t.Errorf("engaged_with edges = %d, want 3", n)
	}
	// The graph the run built, from the vocabulary views.
	if n := h.queryInt(`SELECT count(*) FROM identities WHERE entity_type = 'person'`); n != 4 {
		t.Errorf("people = %d, want 4 (three sourced, dave traversed)", n)
	}
}

func TestSQLTraverseFollowsWorksAt(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("companies.yaml", `name: to-companies
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: to-company
    use: sql/traverse
    with:
      entity_type: company
      query: >
        SELECT r.to_id AS identity_id, r.from_id AS parent_id
        FROM relations r WHERE r.relation = 'works_at'
group: accounts
`)
	plan := h.mustRun("plan", "companies.yaml")
	contains(t, plan.stderr, "traverse:  person → company via sql/traverse (follows an existing relation; mints nothing)", "plan line")
	contains(t, plan.stderr, "cross-record: this query reads relations", "annotated cross-record like every sql/* step")

	res := h.mustRun("run", "companies.yaml")
	contains(t, res.stderr, "to-company: 3 in, 3 out, 0 cached, 0 filtered, 0 failed — 3 traversed (company), 0 coalesced", "sql/traverse line")
	contains(t, res.stderr, `group "accounts": 3 record(s) added`, "the companies join the terminus")
	if got := h.queryStrings(`SELECT COALESCE(entity_type,'') FROM groups WHERE name = 'accounts'`); len(got) != 1 || got[0] != "company" {
		t.Errorf("accounts type = %v", got)
	}
	// It mints nothing and writes no relation: the three companies and the
	// three works_at edges the source's reference wrote are all there is.
	if n := h.queryInt(`SELECT count(*) FROM identities WHERE entity_type = 'company'`); n != 3 {
		t.Errorf("companies = %d, want 3", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM relations`); n != 3 {
		t.Errorf("relations = %d, want 3", n)
	}
	// A company run ending in a person group fails plan naming both.
	h.mustRun("groups", "add", "people", "jane.doe@acme.com")
	h.write("wrong.yaml", strings.Replace(strings.Replace(h.readFile("companies.yaml"), "name: to-companies", "name: to-companies-wrong", 1), "group: accounts", "group: people", 1))
	bad := h.run("plan", "wrong.yaml")
	if bad.code != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", bad.code, bad.stderr)
	}
	contains(t, bad.stderr, `group "people" holds person records, but this pipeline would add company records`, "the mismatch names the group, its type, and the pipeline's type")
}

// readFile reads a workspace file back.
func (h *harness) readFile(name string) string {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.work, name))
	if err != nil {
		h.t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

func TestTraversePlanRules(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)

	// when: may name a step in the current segment only.
	h.write("cross.yaml", `name: cross-when
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: gate
    use: sql/filter
    with:
      query: SELECT id AS identity_id FROM identities WHERE identity_key != 'carol@initech.dev'
  - id: posts
    use: mock/posts
  - id: park
    use: group/deliver
    when: gate.passed
    with:
      group: parked
`)
	res := h.run("plan", "cross.yaml")
	if res.code != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `when: gate.passed names a step before the traverse "posts"`, "cross-segment when: is refused")
	contains(t, res.stderr, `gate at the traverse instead`, "the error names the fix")

	// Gating at the traverse is the form the error names: only children of
	// passing parents are ever minted.
	h.write("gated.yaml", strings.Replace(strings.Replace(h.readFile("cross.yaml"), "    when: gate.passed\n", "", 1),
		"    use: mock/posts\n", "    use: mock/posts\n    when: gate.passed\n", 1))
	plan := h.mustRun("plan", "gated.yaml")
	contains(t, plan.stderr, "when:      gate.passed", "when: on the traverse plans")
	run := h.mustRun("run", "gated.yaml")
	contains(t, run.stderr, "posts: 2 in, 2 out, 0 cached, 0 filtered, 0 failed — 2 traversed (post), 1 coalesced", "only passing parents reach the traverse")

	// from must equal the segment's type: a traverse from post cannot follow
	// a person source.
	h.write("wrongfrom.yaml", `name: wrong-from
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: engagers
    use: mock/engagers
`)
	res = h.run("plan", "wrongfrom.yaml")
	if res.code != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "mock/engagers traverses from post, but the records here are person", "from mismatch")

	// A step of another type after a traverse is refused: a person deliver
	// in the post segment.
	h.write("wrongseg.yaml", `name: wrong-segment
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: posts
    use: mock/posts
  - id: send
    use: mock/deliver
    with:
      campaign: q3
`)
	res = h.run("plan", "wrongseg.yaml")
	if res.code != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "mock/deliver is a person adapter, but the records here are post", "segment type mismatch")

	// limit: on a traverse caps children per parent, engine-owned.
	h.write("limited.yaml", strings.Replace(postsYAML, "    use: mock/posts\n", "    use: mock/posts\n    limit: 1\n", 1))
	plan = h.mustRun("plan", "limited.yaml")
	contains(t, plan.stderr, "limit:     1 child(ren) per parent (engine-owned)", "limit line")
	run = h.mustRun("run", "limited.yaml")
	contains(t, run.stderr, "— 2 traversed (post), 0 coalesced", "jane yields her first post only; bob his only one")
}

func TestOnceFinishesParentsAtATraverseAndDryRunExecutesIt(t *testing.T) {
	h := onceWorld(t)
	h.write("drain.yaml", `name: drain-posts
source:
  group: todo
  once: true
steps:
  - id: posts
    use: mock/posts
group: posts-found
`)
	plan := h.mustRun("plan", "drain.yaml")
	contains(t, plan.stderr, `entity:    person (group "todo")`, "the group source takes the group's type")

	// A dry run executes the crossing (spend at a traverse is spend as at a
	// source) but finishes nothing for once:.
	dry := h.mustRun("run", "--dry-run", "drain.yaml")
	contains(t, dry.stderr, "— 2 traversed (post), 1 coalesced", "the dry run traverses")
	contains(t, dry.stderr, `group "posts-found": 2 record(s) would be added (held back — dry run)`, "the dry terminus holds")
	if n := h.queryInt(`SELECT count(*) FROM identities WHERE entity_type = 'post'`); n != 2 {
		t.Errorf("posts after a dry run = %d, want 2 (minted, as a source mints)", n)
	}
	again := h.mustRun("plan", "drain.yaml")
	contains(t, again.stderr, "once:      3 member(s), 3 not yet worked, sourcing 3", "a rehearsal finishes nothing")

	armed := h.mustRun("run", "drain.yaml")
	contains(t, armed.stderr, `source: sourced 3 members of group "todo" (3 of 3 not yet worked, oldest first)`, "armed run 1")
	// A child known from the dry run is a new run record, not a coalesce
	// (ADR-053 (3)): the counts read as on the rehearsal, and nothing is
	// minted twice.
	contains(t, armed.stderr, "— 2 traversed (post), 1 coalesced", "the armed run's counts match the rehearsal")
	if n := h.queryInt(`SELECT count(*) FROM identities WHERE entity_type = 'post'`); n != 2 {
		t.Errorf("posts after the armed run = %d, want 2 (resolved, not re-minted)", n)
	}
	// Every parent — with children or none — is finished at the traverse.
	after := h.mustRun("plan", "drain.yaml")
	contains(t, after.stderr, "once:      3 member(s), 0 not yet worked, sourcing 0", "parents finished at the traverse")
	second := h.mustRun("run", "drain.yaml")
	contains(t, second.stderr, `source: sourced 0 members of group "todo" (0 of 3 not yet worked, oldest first)`, "nothing is re-offered")
}

func TestGroupSourceOverPostsValidatesAgainstPost(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("posts.yaml", postsYAML)
	h.mustRun("run", "posts.yaml")

	h.write("consume.yaml", `name: consume-posts
source:
  group: posts-found
steps:
  - id: derive
    use: sql/transform
    with:
      provides: [title]
      query: SELECT identity_id, 'x' AS title FROM current_values
`)
	res := h.run("plan", "consume.yaml")
	if res.code != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `"title" is not a canonical post field`, "field names validate against the group's type")

	// A signal is never delivered to: a person deliver adapter in a post
	// pipeline is a type mismatch, and a group/deliver handoff is fine.
	h.write("hand.yaml", `name: hand-posts
source:
  group: posts-found
steps:
  - id: park
    use: group/deliver
    with:
      group: posts-parked
`)
	run := h.mustRun("run", "hand.yaml")
	contains(t, run.stderr, `park: 2 record(s) handed off to group "posts-parked"`, "posts hand off to a group")
	if got := h.queryStrings(`SELECT COALESCE(entity_type,'') FROM groups WHERE name = 'posts-parked'`); len(got) != 1 || got[0] != "post" {
		t.Errorf("posts-parked type = %v", got)
	}
	show := h.mustRun("groups", "show", "posts-found")
	contains(t, show.stderr, "sourced by:  hand-posts", "groups show derives consumers from the run snapshots")
}

func TestLegacyUntypedGroupIsEntityBlindUntilTyped(t *testing.T) {
	h := onceWorld(t)
	// A group created before ADR-054 has no type: simulate one by clearing it.
	l := h.open()
	if _, err := l.DB().Exec(`UPDATE groups SET entity_type = NULL WHERE name = 'todo'`); err != nil {
		t.Fatal(err)
	}
	l.Close()
	h.write("blind.yaml", `name: blind
source:
  group: todo
steps:
  - id: posts
    use: mock/posts
`)
	res := h.run("plan", "blind.yaml")
	if res.code != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "a traverse needs a typed segment", "an entity-blind pipeline cannot traverse")
	contains(t, res.stderr, "gtme groups add todo --type person", "the error names the fix")
	list := h.mustRun("groups")
	contains(t, list.stderr, "(untyped)", "an untyped group is listed as such")

	typed := h.mustRun("groups", "add", "todo", "--type", "person")
	contains(t, typed.stderr, "group todo: type set to person", "--type sets it once")
	plan := h.mustRun("plan", "blind.yaml")
	contains(t, plan.stderr, `entity:    person (group "todo")`, "typed now")
	// The type is set once: a second --type of another type is refused.
	bad := h.run("groups", "add", "todo", "--type", "company")
	if bad.code == 0 {
		t.Errorf("retyping a group should fail")
	}
	contains(t, bad.stderr, "holds person records, not company", "refusal names both")
}
