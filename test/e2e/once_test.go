package e2e

// `once:` on a group source (SPEC §8/§9, ADR-052, M26): a bounded consumer
// advances past what it finished instead of replaying its first batch;
// finished means completed the final step or stopped by a filter verdict;
// failed records are re-offered; the group is never mutated; a dry run
// finishes nothing.

import (
	"strings"
	"testing"
)

// onceWorld mints three people and adds them to `todo` one at a time, so
// insertion order is carol, jane, bob.
func onceWorld(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("mint.yaml", "name: mint\nsource:\n  use: csv/source\n  with:\n    path: people.csv\n")
	h.mustRun("run", "mint.yaml")
	for _, key := range []string{"carol@initech.dev", "jane.doe@acme.com", "bob@globex.io"} {
		h.mustRun("groups", "add", "todo", key)
	}
	return h
}

func groupKeys(h *harness, group string) string {
	return strings.Join(h.queryStrings(`SELECT i.identity_key FROM group_members m
		JOIN identities i ON i.id = m.identity_id JOIN groups g ON g.id = m.group_id
		WHERE g.name = ? ORDER BY i.identity_key`, group), ",")
}

func groupEventCount(h *harness, group string) int {
	return h.queryInt(`SELECT count(*) FROM group_events e JOIN groups g ON g.id = e.group_id WHERE g.name = ?`, group)
}

// TestOnceAdvancesABoundedConsumer: limit: 2 with once: true sources
// members 1–2, then 3, then nothing; the plan names the eligible count; the
// source group's events are byte-identical throughout; and the same
// pipeline without once: replays 1–2 forever (the reported #43 behavior,
// unchanged when the key is absent).
func TestOnceAdvancesABoundedConsumer(t *testing.T) {
	h := onceWorld(t)
	h.write("work.yaml", `name: work
source:
  group: todo
  limit: 2
  once: true
steps:
  - id: park
    use: group/deliver
    with:
      group: worked
`)
	before := groupEventCount(h, "todo")

	plan := h.mustRun("plan", "work.yaml")
	contains(t, plan.stderr, "limit:     2 member(s), oldest-added first", "plan: limit line")
	contains(t, plan.stderr, "once:      3 member(s), 3 not yet worked, sourcing 2 (oldest first)", "plan: eligible count")

	res := h.mustRun("run", "work.yaml")
	contains(t, res.stderr, `source: sourced 2 members of group "todo" (3 of 3 not yet worked; limit 2, oldest first)`, "run 1 source line")
	if got := groupKeys(h, "worked"); got != "carol@initech.dev,jane.doe@acme.com" {
		t.Errorf("after run 1 worked = %q, want the two oldest", got)
	}

	plan = h.mustRun("plan", "work.yaml")
	contains(t, plan.stderr, "once:      3 member(s), 1 not yet worked, sourcing 1 (oldest first)", "plan after run 1")

	res = h.mustRun("run", "work.yaml")
	contains(t, res.stderr, `source: sourced 1 members of group "todo" (1 of 3 not yet worked; limit 2, oldest first)`, "run 2 source line")
	if got := groupKeys(h, "worked"); got != "bob@globex.io,carol@initech.dev,jane.doe@acme.com" {
		t.Errorf("after run 2 worked = %q, want all three", got)
	}

	res = h.mustRun("run", "work.yaml")
	contains(t, res.stderr, `source: sourced 0 members of group "todo" (0 of 3 not yet worked; limit 2, oldest first)`, "run 3 source line")
	contains(t, res.stderr, "park: 0 in, 0 out", "run 3: no step did work")
	if res.code != 0 {
		t.Errorf("a fully drained queue is not an error, got exit %d", res.code)
	}

	if after := groupEventCount(h, "todo"); after != before {
		t.Errorf("todo's group_events went %d → %d; once: must never touch the group", before, after)
	}

	// Without once: the reported behavior stands — the oldest two, every run.
	h.write("replay.yaml", `name: replay
source:
  group: todo
  limit: 2
steps:
  - id: park
    use: group/deliver
    with:
      group: replayed
`)
	for i := 0; i < 2; i++ {
		res = h.mustRun("run", "replay.yaml")
		contains(t, res.stderr, `source: sourced 2 members of group "todo" (limit 2, oldest first)`, "replay source line")
	}
	if got := groupKeys(h, "replayed"); got != "carol@initech.dev,jane.doe@acme.com" {
		t.Errorf("replay reached %q; without once: it must stay on the oldest two", got)
	}
}

// failBobManifest / failBobScript: an enrich fixture whose output for bob is
// invalid against provides (an undeclared field), so his record fails the
// step (SPEC §5) while everyone else's advances.
const failBobManifest = `{
  "id": "fail-bob",
  "version": 1,
  "role": "enrich",
  "entity_type": "person",
  "needs": {"type":"object","required":["email"],"properties":{"email":{"type":"string"}}},
  "provides": {"type":"object","additionalProperties":false,"properties":{"headline":{"type":"string"}}}
}`

const failBobScript = `#!/usr/bin/env python3
import json, sys
PROVIDES = {"type":"object","additionalProperties":False,"properties":{"headline":{"type":"string"}}}
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    if msg.get("type") == "OPEN":
        print(json.dumps({"type":"SCHEMA","provides":PROVIDES}), flush=True)
    elif msg.get("type") == "RECORD":
        fields = {"headline": "fixture"}
        if "bob@" in (msg.get("fields") or {}).get("email", ""):
            fields["bogus"] = "not in provides"
        print(json.dumps({"type":"RECORD","key":msg["key"],"fields":fields}), flush=True)
    elif msg.get("type") == "END":
        break
print(json.dumps({"type":"END"}), flush=True)
`

// TestOnceFinishedMeansCompletedOrFiltered: a record a filter froze counts as
// finished and is not re-offered; a record whose step failed is re-offered
// on the next run.
func TestOnceFinishedMeansCompletedOrFiltered(t *testing.T) {
	h := onceWorld(t)

	h.write("gated.yaml", `name: gated
source:
  group: todo
  once: true
steps:
  - id: gate
    use: sql/filter
    with:
      query: SELECT id AS identity_id FROM identities WHERE identity_key != 'bob@globex.io'
  - id: park
    use: group/deliver
    with:
      group: gated-worked
`)
	res := h.mustRun("run", "gated.yaml")
	contains(t, res.stderr, `source: sourced 3 members of group "todo" (3 of 3 not yet worked, oldest first)`, "gated run 1 source")
	contains(t, res.stderr, "gate: 3 in, 2 out, 0 cached, 1 filtered", "gated run 1 filter line")
	if got := groupKeys(h, "gated-worked"); got != "carol@initech.dev,jane.doe@acme.com" {
		t.Errorf("gated-worked = %q, want carol and jane", got)
	}
	res = h.mustRun("run", "gated.yaml")
	contains(t, res.stderr, `source: sourced 0 members of group "todo" (0 of 3 not yet worked, oldest first)`,
		"gated run 2: the filtered record is finished, not re-offered")

	h.writeAdapter("fail-bob", failBobManifest, failBobScript)
	h.write("flaky.yaml", `name: flaky
source:
  group: todo
  once: true
steps:
  - id: enrich
    use: fail-bob
  - id: park
    use: group/deliver
    with:
      group: flaky-worked
`)
	res = h.mustRun("run", "flaky.yaml")
	contains(t, res.stderr, `source: sourced 3 members of group "todo" (3 of 3 not yet worked, oldest first)`, "flaky run 1 source")
	contains(t, res.stderr, "enrich: 3 in, 2 out, 0 cached, 0 filtered, 1 failed", "flaky run 1 enrich line")
	if got := groupKeys(h, "flaky-worked"); got != "carol@initech.dev,jane.doe@acme.com" {
		t.Errorf("flaky-worked = %q, want carol and jane", got)
	}
	res = h.mustRun("run", "flaky.yaml")
	contains(t, res.stderr, `source: sourced 1 members of group "todo" (1 of 3 not yet worked, oldest first)`,
		"flaky run 2: the failed record is re-offered")
	contains(t, res.stderr, "enrich: 1 in, 0 out, 0 cached, 0 filtered, 1 failed", "flaky run 2: bob fails again, still visible")
}

// TestOnceDryRunFinishesNothing: a --dry-run followed by an armed run
// sources the same members — a rehearsal repeats rather than advancing the
// queue (SPEC §8, ADR-052 (7)).
func TestOnceDryRunFinishesNothing(t *testing.T) {
	h := onceWorld(t)
	h.write("work.yaml", `name: work
source:
  group: todo
  limit: 2
  once: true
steps:
  - id: park
    use: group/deliver
    with:
      group: worked
`)
	dry := h.mustRun("run", "--dry-run", "work.yaml")
	contains(t, dry.stderr, `source: sourced 2 members of group "todo" (3 of 3 not yet worked; limit 2, oldest first)`, "dry source line")
	if got := groupKeys(h, "worked"); got != "" {
		t.Fatalf("a dry run handed off %q; it must hand off nothing", got)
	}

	plan := h.mustRun("plan", "work.yaml")
	contains(t, plan.stderr, "once:      3 member(s), 3 not yet worked, sourcing 2 (oldest first)", "plan after a dry run still sees 3 eligible")

	armed := h.mustRun("run", "work.yaml")
	contains(t, armed.stderr, `source: sourced 2 members of group "todo" (3 of 3 not yet worked; limit 2, oldest first)`, "armed source line matches the dry one")
	if got := groupKeys(h, "worked"); got != "carol@initech.dev,jane.doe@acme.com" {
		t.Errorf("armed run worked = %q, want the same two the dry run rehearsed", got)
	}
}
