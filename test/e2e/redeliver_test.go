package e2e

// The M22 acceptance (SPEC §11, ADR-045): a natively idempotent target
// re-delivers when resolved values change and skips (reason `unchanged`)
// when they do not; `redeliver: always` repeats regardless; a target
// without native idempotency refuses `redeliver:` at plan time.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

const redeliverCSV = "Full Name,Email\n%s,jane.doe@acme.com\n"

func redeliverPipeline(srvURL, extra string) string {
	return `name: attio-sync
source:
  use: csv/source
  with:
    path: contacts.csv
    columns:
      full_name: Full Name
      email: Email
steps:
  - id: sync
    use: attio/assert
    with:
      base_url: ` + jsonString(srvURL) + `
` + extra + `    variables:
      name: full_name
    idempotency: email
`
}

func TestRedeliverOnChange(t *testing.T) {
	asserts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asserts++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"id":{"workspace_id":"ws","object_id":"obj","record_id":"rec_1"}}}`)
	}))
	defer srv.Close()

	h := newHarness(t)
	env := []string{"ATTIO_API_KEY=k"}
	h.write("contacts.csv", fmt.Sprintf(redeliverCSV, "Jane Doe"))
	p := h.write("p.yaml", redeliverPipeline(srv.URL, ""))

	// attio/assert declares idempotency: native, so the default is on_change
	// and the plan says so.
	plan := h.runWithEnv(env, "", "plan", p)
	if plan.code != 0 {
		t.Fatalf("plan exit = %d\n%s", plan.code, plan.stderr)
	}
	contains(t, plan.stderr, "redeliver: on_change", "plan output")

	run := func() result {
		res := h.runWithEnv(env, "", "run", p)
		if res.code != 0 {
			t.Fatalf("run exit = %d\n%s", res.code, res.stderr)
		}
		return res
	}

	run()
	if asserts != 1 {
		t.Fatalf("asserts after first run = %d, want 1", asserts)
	}
	if n := h.queryInt(`SELECT count(*) FROM deliveries WHERE target = 'attio/assert'`); n != 1 {
		t.Fatalf("deliveries = %d, want 1", n)
	}
	hash1 := h.queryStrings(`SELECT variables_hash FROM deliveries WHERE target = 'attio/assert'`)[0]
	if hash1 == "" {
		t.Fatal("delivery carries no variables hash")
	}

	// Unchanged re-run: no second assert, skip reason `unchanged`.
	res := run()
	if asserts != 1 {
		t.Errorf("asserts after unchanged re-run = %d, want 1", asserts)
	}
	contains(t, res.stderr, "1 cached", "unchanged re-run receipt")
	if n := h.queryInt(`SELECT count(*) FROM step_events WHERE step_id='sync' AND json_extract(detail,'$.reason')='unchanged'`); n != 1 {
		t.Errorf("unchanged skip events = %d, want 1", n)
	}

	// The value moves: the target is re-asserted, the row (not a new one)
	// takes the new hash, and status returns to accepted.
	h.write("contacts.csv", fmt.Sprintf(redeliverCSV, "Jane Q. Doe"))
	run()
	if asserts != 2 {
		t.Errorf("asserts after changed re-run = %d, want 2", asserts)
	}
	if n := h.queryInt(`SELECT count(*) FROM deliveries WHERE target = 'attio/assert'`); n != 1 {
		t.Errorf("deliveries after re-delivery = %d, want 1 (upsert, not append)", n)
	}
	hash2 := h.queryStrings(`SELECT variables_hash FROM deliveries WHERE target = 'attio/assert'`)[0]
	if hash2 == hash1 {
		t.Error("variables hash did not move on re-delivery")
	}

	// redeliver: always repeats without a change.
	p = h.write("p.yaml", redeliverPipeline(srv.URL, "    redeliver: always\n"))
	run()
	if asserts != 3 {
		t.Errorf("asserts under redeliver: always = %d, want 3", asserts)
	}

	// redeliver: never restores the hard floor even for a native target.
	h.write("contacts.csv", fmt.Sprintf(redeliverCSV, "Jane R. Doe"))
	p = h.write("p.yaml", redeliverPipeline(srv.URL, "    redeliver: never\n"))
	res = run()
	if asserts != 3 {
		t.Errorf("asserts under redeliver: never = %d, want 3", asserts)
	}
	contains(t, res.stderr, "1 cached", "never re-run receipt")
}

// TestRedeliverNeedsNativeIdempotency: intent cannot opt an unsafe target
// into repeats — csv/deliver appends, so repeating duplicates the row.
func TestRedeliverNeedsNativeIdempotency(t *testing.T) {
	h := newHarness(t)
	h.write("contacts.csv", fmt.Sprintf(redeliverCSV, "Jane Doe"))
	p := h.write("p.yaml", `name: nope
source:
  use: csv/source
  with:
    path: contacts.csv
    columns:
      full_name: Full Name
      email: Email
steps:
  - id: out
    use: csv/deliver
    with:
      path: out.csv
    redeliver: on_change
    variables:
      contact_email: email
    idempotency: email
`)
	res := h.run("plan", p)
	if res.code != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "idempotency: native", "plan error")
}
