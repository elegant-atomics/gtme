package e2e

// Record accounting (SPEC §7/§8, ADR-053, M27): a receipt may not assert more
// than the run can substantiate. `out` means the step contributed something
// and `empty` names the rest; a declared uses: field absent at run time is
// visible and on_missing: governs it; the source line reconciles rows read
// against records sourced with coalescing recorded per record; a paid run
// that produced nothing says so; every per-step line reconciles.

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// stepLine parses "id: N in, N out[, N empty], N cached, N filtered, N failed
// [, N gated][, N skipped][, N simulated][, N held (dry run)][, N in flight |
// N awaiting X]" and checks the ADR-053 identity: in = out + empty + cached +
// filtered + failed + gated + skipped + simulated + held + in flight.
var stepLine = regexp.MustCompile(`^(\S+): (\d+) in, (\d+) out(?:, (\d+) empty)?, (\d+) cached, (\d+) filtered, (\d+) failed(?:, (\d+) gated)?(?:, (\d+) skipped)?(?:, (\d+) simulated)?(?:, (\d+) held \(dry run\))?(?:, (\d+) (?:in flight|awaiting \S+))?`)

func reconcile(t *testing.T, stderr string) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(stderr, "\n") {
		m := stepLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		n++
		num := func(i int) int {
			if m[i] == "" {
				return 0
			}
			v, _ := strconv.Atoi(m[i])
			return v
		}
		in := num(2)
		sum := 0
		for i := 3; i <= 12; i++ {
			sum += num(i)
		}
		if in != sum {
			t.Errorf("step line does not reconcile (in=%d, classified=%d): %s", in, sum, line)
		}
	}
	return n
}

// TestEmptyCountsRecordsAStepDidNotWrite: a sql/transform whose query matches
// nothing reports empty, not out, and the records still advance; an
// http/enrich over its byte cap does the same, keeps the oversized response
// as a payload, and the downstream compose declaring the field in uses: runs
// by default with the receipt naming the gap.
func TestEmptyCountsRecordsAStepDidNotWrite(t *testing.T) {
	h := newHarness(t)
	h.write("people.csv", peopleCSV)
	h.write("nothing.yaml", `name: derive-nothing
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: shout
    use: sql/transform
    with:
      uses: [full_name]
      provides: [sql.shout]
      query: SELECT id AS identity_id, 'x' AS "sql.shout" FROM identities WHERE 0
  - id: park
    use: group/deliver
    with:
      group: parked
`)
	res := h.mustRun("run", "nothing.yaml")
	contains(t, res.stderr, "shout: 3 in, 0 out, 3 empty, 0 cached, 0 filtered, 0 failed", "transform line")
	contains(t, res.stderr, "park: 3 in, 3 out, 0 cached, 0 filtered, 0 failed", "the empty records still advanced")
	contains(t, res.stderr, "empty", "receipt table carries the column")
	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field = 'sql.shout'`); n != 0 {
		t.Errorf("sql.shout rows = %d, want 0", n)
	}
	if reconcile(t, res.stderr) < 2 {
		t.Error("expected at least two step lines to reconcile")
	}

	// A page four times the cap: nothing stored, the record advances empty,
	// the response is retained anyway (SPEC §10, ADR-053 (4)).
	big := "<html><body>" + strings.Repeat("<p>anvils</p>", 200) + "</body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(big))
	}))
	defer srv.Close()
	h.write("oversized.yaml", `name: oversized-enrich
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: fetch
    use: http/enrich
    with:
      url: "`+srv.URL+`/site?d={{record.company_domain}}"
      markdown: true
      field: web.homepage
      freshness_days: 7
      max_bytes: 500
  - id: write
    use: ai/compose
    uses: [web.homepage]
    with:
      prompt: Write from the homepage.
`)
	env := h.fixtureScript("ai.json", "$auto")
	res = h.runWithEnv(env, "", "run", "oversized.yaml")
	if res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "exceeds the 500-byte cap — nothing stored", "the warning still prints")
	contains(t, res.stderr, "fetch: 3 in, 0 out, 3 empty, 0 cached, 0 filtered, 0 failed", "oversized responses are empty, not out")
	contains(t, res.stderr, "write: 3 in, 3 out, 0 cached, 0 filtered, 0 failed (3 missing web.homepage)", "compose ran by default and the receipt names the gap")
	contains(t, res.stderr, "write: 3 missing web.homepage — dispatched anyway (on_missing: run)", "receipt block")
	if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field = 'web.homepage'`); n != 0 {
		t.Errorf("web.homepage rows = %d, want 0 (nothing stored)", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM payloads WHERE adapter = 'http/enrich' AND length(body) > 500`); n != 3 {
		t.Errorf("retained oversized payloads = %d, want 3", n)
	}
	reconcile(t, res.stderr)
}

// TestOnMissingGovernsADeclaredFieldAbsentAtRunTime: run (default) dispatches
// and the receipt counts; skip advances the record untouched; fail fails it
// naming the field; run is refused on a deliver step; the key is refused on
// an enrich step.
func TestOnMissingGovernsADeclaredFieldAbsentAtRunTime(t *testing.T) {
	pipeline := func(policy string) string {
		return `name: outreach
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: write
    use: ai/compose
    uses: [title, linkedin_url]
` + policy + `    with:
      prompt: Write.
`
	}
	// carol has no linkedin_url in peopleCSV.
	t.Run("run is the default and the receipt counts", func(t *testing.T) {
		h := newHarness(t)
		h.write("people.csv", peopleCSV)
		h.write("p.yaml", pipeline(""))
		res := h.runWithEnv(h.fixtureScript("ai.json", "$auto"), "", "run", "p.yaml")
		if res.code != 0 {
			t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
		}
		contains(t, res.stderr, "write: 3 in, 3 out, 0 cached, 0 filtered, 0 failed (1 missing linkedin_url)", "step line")
		contains(t, res.stderr, "write: 1 missing linkedin_url — dispatched anyway (on_missing: run); set on_missing: skip or fail to hold them", "receipt")
		if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field = 'first_line'`); n != 3 {
			t.Errorf("first_line rows = %d, want 3 (everyone was dispatched)", n)
		}
		reconcile(t, res.stderr)
	})
	t.Run("skip advances the record untouched", func(t *testing.T) {
		h := newHarness(t)
		h.write("people.csv", peopleCSV)
		h.write("p.yaml", pipeline("    on_missing: skip\n"))
		plan := h.mustRun("plan", "p.yaml")
		contains(t, plan.stderr, "on_missing: skip (a declared uses: field absent at run time — ADR-053)", "plan output")
		res := h.runWithEnv(h.fixtureScript("ai.json", "$auto"), "", "run", "p.yaml")
		if res.code != 0 {
			t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
		}
		contains(t, res.stderr, "write: 3 in, 2 out, 0 cached, 0 filtered, 0 failed, 1 skipped", "step line")
		contains(t, res.stderr, "write: 1 record(s) held back by on_missing:", "receipt block")
		contains(t, res.stderr, "carol@initech.dev: missing linkedin_url", "receipt names the record and the field")
		states := h.queryStrings(`SELECT rr.state FROM run_records rr JOIN identities i ON i.id = rr.identity_id WHERE i.identity_key = 'carol@initech.dev'`)
		if len(states) != 1 || states[0] != "write" {
			t.Errorf("carol's state = %v, want write (advanced untouched)", states)
		}
		if n := h.queryInt(`SELECT count(*) FROM field_values WHERE field = 'first_line'`); n != 2 {
			t.Errorf("first_line rows = %d, want 2 (carol was not dispatched)", n)
		}
		reconcile(t, res.stderr)
	})
	t.Run("fail fails the record naming the field", func(t *testing.T) {
		h := newHarness(t)
		h.write("people.csv", peopleCSV)
		h.write("p.yaml", pipeline("    on_missing: fail\n"))
		res := h.runWithEnv(h.fixtureScript("ai.json", "$auto"), "", "run", "p.yaml")
		if res.code != 0 {
			t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
		}
		contains(t, res.stderr, "write: 3 in, 2 out, 0 cached, 0 filtered, 1 failed", "step line")
		details := h.queryStrings(`SELECT detail FROM step_events WHERE step_id = 'write' AND event = 'failed'`)
		if len(details) != 1 || !strings.Contains(details[0], "linkedin_url") {
			t.Errorf("failed event = %v, want one naming linkedin_url", details)
		}
		reconcile(t, res.stderr)
	})
	t.Run("run is refused on a deliver step, the key on an enrich step", func(t *testing.T) {
		h := newHarness(t)
		h.write("people.csv", peopleCSV)
		h.write("deliver.yaml", `name: bad-deliver
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: park
    use: group/deliver
    with:
      group: parked
    on_missing: run
`)
		res := h.run("plan", "deliver.yaml")
		if res.code != 2 {
			t.Fatalf("exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
		}
		contains(t, res.stderr, "on_missing: run is not valid on a deliver step", "deliver refusal")
		h.write("enrich.yaml", `name: bad-enrich
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: score
    use: mock-enrich-py
    on_missing: skip
`)
		res = h.run("plan", "enrich.yaml")
		if res.code != 2 {
			t.Fatalf("exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
		}
		contains(t, res.stderr, "on_missing: is only valid on deliver steps and participant steps", "enrich refusal")
	})
}

// TestSourceReconcilesCoalescedRows: two rows that resolve to one identity
// source one record, the line says so, and a coalesced step event answers
// which row merged into which identity.
func TestSourceReconcilesCoalescedRows(t *testing.T) {
	h := newHarness(t)
	h.write("contacts.csv", "full_name,email,title\nJane Doe,jane@example.com,VP Sales\nJane D.,JANE@example.com,Head of Sales\nBob Stone,bob@example.com,CTO\n")
	h.write("p.yaml", `name: import
source:
  use: csv/source
  with:
    path: contacts.csv
steps:
  - id: park
    use: group/deliver
    with:
      group: imported
`)
	// A rehearsal's deliver line reconciles: held, not out — and the source
	// line reconciles there too.
	dry := h.mustRun("run", "--dry-run", "p.yaml")
	contains(t, dry.stderr, "source: sourced 2 records (1 coalesced into known identities)", "dry-run source line")
	contains(t, dry.stderr, "park: 2 in, 0 out, 0 cached, 0 filtered, 0 failed, 2 held (dry run)", "dry-run deliver line")
	reconcile(t, dry.stderr)

	res := h.mustRun("run", "p.yaml")
	contains(t, res.stderr, "read 3 rows from contacts.csv", "the adapter's own read line")
	contains(t, res.stderr, "source: sourced 2 records (1 coalesced into known identities)", "the reconciling source line")
	contains(t, res.stderr, "park: 2 in, 2 out", "the next step saw two")
	if n := h.queryInt(`SELECT count(*) FROM run_records rr JOIN runs r ON r.id = rr.run_id WHERE r.dry = 0`); n != 2 {
		t.Errorf("armed run's run_records = %d, want 2", n)
	}
	details := h.queryStrings(`SELECT DISTINCT detail FROM step_events WHERE step_id = 'source' AND event = 'coalesced'`)
	if len(details) != 1 || !strings.Contains(details[0], `"into":"jane@example.com"`) || !strings.Contains(details[0], `"keys":["jane@example.com"`) {
		t.Errorf("coalesced event = %v, want one naming the row's key and the identity it merged into", details)
	}
	// Which identity absorbed the row is answerable by SQL, not inferred.
	who := h.queryStrings(`SELECT DISTINCT i.identity_key FROM step_events e JOIN identities i ON i.id = e.identity_id WHERE e.event = 'coalesced'`)
	if len(who) != 1 || who[0] != "jane@example.com" {
		t.Errorf("coalesced into = %v, want jane@example.com", who)
	}
	reconcile(t, res.stderr)
}

const paidSearchManifest = `{
  "id": "paid-search",
  "version": 1,
  "role": "source",
  "entity_type": "person",
  "provides": {"type":"object","additionalProperties":false,"properties":{"email":{"type":"string"}}}
}`

// paidSearchScript charges for a query that finds nobody.
const paidSearchScript = `#!/usr/bin/env python3
import json, sys
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    if msg.get("type") == "OPEN":
        print(json.dumps({"type":"SCHEMA","provides":{"type":"object","additionalProperties":False,"properties":{"email":{"type":"string"}}}}), flush=True)
    elif msg.get("type") == "END":
        print(json.dumps({"type":"COST","provider":"vendor","amount_usd":4.10}), flush=True)
        break
print(json.dumps({"type":"END"}), flush=True)
`

// TestPaidRunThatSourcedNothingIsMarked: a run that spent money and produced
// no records says so on the receipt and in gtme runs; the exit code is
// unchanged.
func TestPaidRunThatSourcedNothingIsMarked(t *testing.T) {
	h := newHarness(t)
	h.writeAdapter("paid-search", paidSearchManifest, paidSearchScript)
	h.write("p.yaml", `name: search
source:
  use: paid-search
steps:
  - id: park
    use: group/deliver
    with:
      group: found
`)
	res := h.run("run", "p.yaml")
	if res.code != 0 {
		t.Fatalf("exit = %d, want 0 (information, not a new outcome)\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "— done — 0 records, $4.1000 spent (estimated)", "receipt title")
	contains(t, res.stderr, "source: sourced 0 records", "source line")
	list := h.mustRun("runs")
	contains(t, list.stderr, "done — 0 records, $4.1000 spent (estimated)", "gtme runs list")
	last := h.mustRun("runs", "last")
	contains(t, last.stderr, "status:   done — 0 records, $4.1000 spent (estimated)", "gtme runs last")
	reconcile(t, res.stderr)
}
