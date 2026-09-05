package e2e

// M14 step 5 acceptance (SPEC §11, ADR-036): a delivery lands `accepted`, and
// a fixture adapter declaring `attests` yields confirmed, contradicted, and
// inconclusive in turn with the documented receipt behaviour; gtme show
// distinguishes them.

import (
	"encoding/json"
	"strings"
	"testing"
)

const attestYAML = `name: attest
source:
  use: csv/source
  with:
    path: people.csv
steps:
  - id: send
    use: %s
    with:
      campaign: q3
    variables:
      name: full_name
    idempotency: email
`

func TestDeliveriesAreAcceptedUntilAttested(t *testing.T) {
	type outcome struct {
		mode     string
		exit     int
		status   string // deliveries.status
		advanced int    // run_records past the step
		failed   int
		receipt  []string
	}
	cases := []outcome{
		{"confirmed", 0, "confirmed", 3, 0, []string{"send: attested 3 confirmed, 0 contradicted, 0 inconclusive (deliveries are accepted, never sent, until a provider attests)"}},
		{"contradicted", 0, "contradicted", 0, 3, []string{"send: attested 0 confirmed, 3 contradicted, 0 inconclusive", "send: 3 in, 0 out, 0 cached, 0 filtered, 3 failed"}},
		{"inconclusive", 0, "accepted", 3, 0, []string{"send: attested 0 confirmed, 0 contradicted, 3 inconclusive", "jane.doe@acme.com: accepted, not confirmed — fixture re-read: inconclusive", "send [warn]: jane.doe@acme.com delivered (accepted) but not confirmed"}},
		{"silent", 0, "accepted", 3, 0, []string{"send: attested 0 confirmed, 0 contradicted, 3 inconclusive", "accepted, not confirmed — the adapter reported no attestation for this record"}},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			h := newHarness(t)
			h.write("people.csv", peopleCSV)
			h.write("p.yaml", strings.Replace(attestYAML, "%s", "mock/attest", 1))
			res := h.runWithEnv([]string{"MOCK_ATTEST=" + tc.mode, "GTME_CONCURRENCY=1"}, "", "run", "p.yaml")
			if res.code != tc.exit {
				t.Fatalf("exit = %d, want %d\nstderr:\n%s", res.code, tc.exit, res.stderr)
			}
			for _, want := range tc.receipt {
				contains(t, res.stderr, want, "receipt")
			}
			statuses := h.queryStrings(`SELECT DISTINCT status FROM deliveries WHERE target = 'mock/attest'`)
			if strings.Join(statuses, ",") != tc.status {
				t.Errorf("deliveries.status = %v, want %s", statuses, tc.status)
			}
			if n := h.queryInt(`SELECT count(*) FROM deliveries`); n != 3 {
				t.Errorf("deliveries = %d, want 3 — the row exists whatever the attestation says (the record is at the target)", n)
			}
			if n := h.queryInt(`SELECT count(*) FROM run_records WHERE state = 'send'`); n != tc.advanced {
				t.Errorf("advanced = %d, want %d", n, tc.advanced)
			}
			if n := h.queryInt(`SELECT count(*) FROM step_events WHERE step_id = 'send' AND event = 'failed'`); n != tc.failed {
				t.Errorf("failed events = %d, want %d", n, tc.failed)
			}
			if n := h.queryInt(`SELECT count(*) FROM deliveries WHERE sent_at IS NOT NULL`); n != 0 {
				t.Errorf("sent_at set on %d rows; only a provider attesting execution may set it", n)
			}
			// gtme show carries the delivery with its status, never `sent`.
			show := h.mustRun("show", "jane.doe@acme.com")
			var doc struct {
				Deliveries []map[string]any `json:"deliveries"`
			}
			if err := json.Unmarshal([]byte(show.stdout), &doc); err != nil {
				t.Fatalf("show output: %v", err)
			}
			if len(doc.Deliveries) != 1 || doc.Deliveries[0]["target"] != "mock/attest" || doc.Deliveries[0]["status"] != tc.status {
				t.Errorf("show deliveries = %v", doc.Deliveries)
			}
			if _, has := doc.Deliveries[0]["sent_at"]; has {
				t.Errorf("show must not present a sent_at that was never attested")
			}

			// A re-run never re-sends: the row holds, contradicted or not.
			again := h.runWithEnv([]string{"MOCK_ATTEST=" + tc.mode}, "", "run", "p.yaml")
			contains(t, again.stderr, "send: 3 in, 0 out, 3 cached", "already delivered")
		})
	}

	// An adapter that does not declare attests is accepted, full stop — no
	// attestation lines, no warnings.
	t.Run("non-attesting", func(t *testing.T) {
		h := newHarness(t)
		h.write("people.csv", peopleCSV)
		h.write("p.yaml", strings.Replace(attestYAML, "%s", "mock/deliver", 1))
		res := h.mustRun("run", "p.yaml")
		if strings.Contains(res.stderr, "attested") || strings.Contains(res.stderr, "not confirmed") {
			t.Errorf("a non-attesting adapter must not produce attestation lines:\n%s", res.stderr)
		}
		statuses := h.queryStrings(`SELECT DISTINCT status FROM deliveries`)
		if strings.Join(statuses, ",") != "accepted" {
			t.Errorf("status = %v, want accepted", statuses)
		}
	})
}
