package e2e

// M12 acceptance (SPEC §10a/§11, ADR-023): the universal Out floor, offline —
// http/deliver dry-runs to a receipt, delivers mapped variables to a local
// URL when armed with idempotency holding on re-run (and refuses to plan
// without being told its key); csv/deliver writes a reviewable file once.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const outFloorYAML = `name: out-floor
source:
  use: csv/source
  with:
    path: contacts.csv
    columns:
      full_name: Full Name
      email: Email

deliver:
  use: %s
  with:
%s
  variables:
    first_name: full_name
    contact_email: email
  idempotency: email
`

func TestHTTPDeliver(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer hook-secret" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	h := newHarness(t)
	h.write("contacts.csv", campaignZeroCSV)
	yaml := strings.Replace(outFloorYAML, "%s\n", "http/deliver\n", 1)
	yaml = strings.Replace(yaml, "%s", `    url: "`+srv.URL+`/hook"
    auth: { type: bearer, env: HOOK_TOKEN }`, 1)
	h.write("p.yaml", yaml)
	env := []string{"HOOK_TOKEN=hook-secret"}

	// Dry: resolved variables receipt, zero calls.
	res := h.runWithEnv(env, "", "run", "p.yaml", "--dry-run")
	if res.code != 0 {
		t.Fatalf("dry exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, `first_name: "Jane Doe"`, "dry receipt")
	if len(bodies) != 0 {
		t.Fatalf("dry run called the target %d time(s)", len(bodies))
	}

	// Armed: the resolved variables land as the body (the $variables default).
	res = h.runWithEnv(env, "", "run", "p.yaml")
	if res.code != 0 {
		t.Fatalf("armed exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	// The nameless third record is held back by on_missing (SPEC §8).
	if len(bodies) != 2 {
		t.Fatalf("deliveries = %d, want 2", len(bodies))
	}
	joined := ""
	for _, b := range bodies {
		raw, _ := json.Marshal(b)
		joined += string(raw)
	}
	contains(t, joined, `"first_name":"Jane Doe"`, "delivered body")
	contains(t, joined, `"contact_email":"jane.doe@acme.com"`, "delivered body")

	// Re-run: idempotency holds; nothing re-delivers.
	res = h.runWithEnv(env, "", "run", "p.yaml")
	if res.code != 0 {
		t.Fatalf("re-run exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if len(bodies) != 2 {
		t.Errorf("re-run re-delivered: %d total calls, want 2", len(bodies))
	}

	// The required idempotency key: a plan without it fails naming the rule.
	h.write("p2.yaml", strings.Replace(readFile(t, filepath.Join(h.work, "p.yaml")),
		"  idempotency: email\n", "", 1))
	res = h.run("plan", "p2.yaml")
	if res.code != 2 {
		t.Fatalf("plan without idempotency exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	contains(t, res.stderr, "requires idempotency", "plan error")
}

func TestCSVDeliver(t *testing.T) {
	h := newHarness(t)
	h.write("contacts.csv", campaignZeroCSV)
	out := filepath.Join(h.work, "reviewed.csv")
	yaml := strings.Replace(outFloorYAML, "%s\n", "csv/deliver\n", 1)
	yaml = strings.Replace(yaml, "%s", `    path: `+out, 1)
	h.write("p.yaml", yaml)

	res := h.run("run", "p.yaml")
	if res.code != 0 {
		t.Fatalf("run exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	raw := readFile(t, out)
	lines := nonEmptyLines(raw)
	if len(lines) != 3 { // header + 2 rows (the nameless record is held back)
		t.Fatalf("csv lines = %d, want 3:\n%s", len(lines), raw)
	}
	if lines[0] != "identity_key,contact_email,first_name" {
		t.Errorf("header = %q (columns must be sorted behind identity_key)", lines[0])
	}
	contains(t, raw, "jane.doe@acme.com,jane.doe@acme.com,Jane Doe", "csv row")

	// Re-run: §8 idempotency means the review artifact gains nothing.
	res = h.run("run", "p.yaml")
	if res.code != 0 {
		t.Fatalf("re-run exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if n := len(nonEmptyLines(readFile(t, out))); n != 3 {
		t.Errorf("csv lines after re-run = %d, want 3", n)
	}
}
