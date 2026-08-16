package csvsource

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trevorfox/gtm/internal/adapters"
	"github.com/trevorfox/gtm/internal/protocol"
)

// drive runs the adapter over a pair of in-memory pipes, exactly as the runner
// would, and returns everything it emitted.
func drive(t *testing.T, config map[string]any) ([]protocol.Message, error) {
	t.Helper()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	go func() {
		w := protocol.NewWriter(inW)
		w.Write(protocol.Message{Type: protocol.TypeOpen, StepID: "source", RunID: "run1", Config: config})
		w.Write(protocol.End())
		inW.Close()
	}()

	runErr := make(chan error, 1)
	go func() {
		err := (&Adapter{}).Run(context.Background(), adapters.Ports{In: inR, Out: outW, Log: io.Discard})
		outW.CloseWithError(err)
		runErr <- err
	}()

	var msgs []protocol.Message
	r := protocol.NewReader(outR)
	for {
		m, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			break
		}
		msgs = append(msgs, m)
	}
	return msgs, <-runErr
}

func fixture(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("fixtures", "people.csv"))
	if err != nil {
		t.Fatalf("locating fixture: %v", err)
	}
	return path
}

func TestRunEmitsSchemaThenRecords(t *testing.T) {
	msgs, err := drive(t, map[string]any{"path": fixture(t)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("no messages")
	}
	if msgs[0].Type != protocol.TypeSchema {
		t.Fatalf("first message = %s, want SCHEMA", msgs[0].Type)
	}

	var schema struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(msgs[0].Provides, &schema); err != nil {
		t.Fatalf("decoding schema: %v", err)
	}
	// Header cells are normalized, and the trailing empty column gets a name
	// rather than colliding with a real field.
	for _, want := range []string{"email", "full_name", "company_domain", "linkedin_url", "title"} {
		if _, ok := schema.Properties[want]; !ok {
			t.Errorf("schema is missing %q: %v", want, keysOf(schema.Properties))
		}
	}

	var records []protocol.Message
	sawEnd := false
	for _, m := range msgs[1:] {
		switch m.Type {
		case protocol.TypeRecord:
			records = append(records, m)
		case protocol.TypeEnd:
			sawEnd = true
		}
	}
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3", len(records))
	}
	if !sawEnd {
		t.Error("adapter must end with END")
	}

	first := records[0].Fields
	// ADR-018: normalization per the registry happens at ingress, at this
	// adapter's own boundary — the mixed-case email is canonical by the time
	// it leaves the adapter (SPEC §10.1).
	if first["email"] != "jane.doe@acme.com" {
		t.Errorf("email = %#v, want registry-normalized (ADR-018)", first["email"])
	}
	if first["full_name"] != "Jane Doe" {
		t.Errorf("full_name = %#v", first["full_name"])
	}
	if got := records[1].Fields["company_domain"]; got != "globex.io" {
		t.Errorf("company_domain = %#v, want eTLD+1 per the registry", got)
	}
	if got := records[1].Fields["linkedin_url"]; got != "https://www.linkedin.com/in/bob-stone" {
		t.Errorf("linkedin_url = %#v, want the canonical public URL form", got)
	}
	if records[0].Key != nil {
		t.Error("a source must not invent identity keys; that is the runner's job")
	}
	// Empty cells carry nothing, so they are omitted rather than written as "".
	if _, ok := records[2].Fields["linkedin_url"]; ok {
		t.Errorf("empty cell should be omitted, got %#v", records[2].Fields["linkedin_url"])
	}
	if _, ok := records[2].Fields["title"]; ok {
		t.Error("empty title should be omitted")
	}
}

func TestRunHonoursLimit(t *testing.T) {
	msgs, err := drive(t, map[string]any{"path": fixture(t), "limit": 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	n := 0
	for _, m := range msgs {
		if m.Type == protocol.TypeRecord {
			n++
		}
	}
	if n != 2 {
		t.Errorf("records = %d, want 2", n)
	}
}

func TestRunErrors(t *testing.T) {
	if _, err := drive(t, map[string]any{}); err == nil {
		t.Error("want an error when path is missing")
	}
	if _, err := drive(t, map[string]any{"path": filepath.Join(t.TempDir(), "nope.csv")}); err == nil {
		t.Error("want an error for a missing file")
	}

	empty := filepath.Join(t.TempDir(), "empty.csv")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := drive(t, map[string]any{"path": empty}); err == nil {
		t.Error("want an error for an empty file")
	}
}

func TestProbeSchemaReadsHeaderOnly(t *testing.T) {
	raw, err := (&Adapter{}).ProbeSchema(map[string]any{"path": fixture(t)})
	if err != nil {
		t.Fatalf("ProbeSchema: %v", err)
	}
	if !strings.Contains(string(raw), `"company_domain"`) {
		t.Errorf("probed schema = %s", raw)
	}
	if adapters.Wildcard(raw) {
		t.Error("a probed header is exact, so the schema must be closed for the planner to be useful")
	}
	for _, want := range []string{"email", "full_name", "title"} {
		if !strings.Contains(string(raw), `"`+want+`"`) {
			t.Errorf("probed schema is missing %q: %s", want, raw)
		}
	}
}

// The ingress mapping (SPEC §10.1, ADR-018): explicit columns:, auto-map for
// canonical headers, csv.* namespacing for the rest, registry normalization of
// values, and per-record drops (never crashes) for invalid values.
func TestColumnsMapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contacts.csv")
	csv := "Work Email,Contact,Company Website,title,Notes\n" +
		"Jane.Doe@Acme.com,Jane Doe,https://www.Acme.com/about,VP Marketing,loves dogs\n"
	if err := os.WriteFile(path, []byte(csv), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"path": path, "columns": map[string]any{
		"email":          "Work Email",
		"full_name":      "Contact",
		"company_domain": "Company Website",
	}}

	msgs, err := drive(t, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var rec *protocol.Message
	for i := range msgs {
		if msgs[i].Type == protocol.TypeRecord {
			rec = &msgs[i]
			break
		}
	}
	if rec == nil {
		t.Fatal("no record emitted")
	}
	want := map[string]any{
		"email":          "jane.doe@acme.com", // mapped + normalized
		"full_name":      "Jane Doe",
		"company_domain": "acme.com",     // full URL reduced to eTLD+1
		"title":          "VP Marketing", // auto-mapped: header already canonical
		"csv.notes":      "loves dogs",   // leftover: kept, namespaced
	}
	for k, v := range want {
		if rec.Fields[k] != v {
			t.Errorf("%s = %#v, want %#v", k, rec.Fields[k], v)
		}
	}
	if _, ok := rec.Fields["notes"]; ok {
		t.Error("unmapped non-canonical header leaked in under its bare name")
	}

	// The probed schema sees the same names, so the planner plans against them.
	raw, err := (&Adapter{}).ProbeSchema(cfg)
	if err != nil {
		t.Fatalf("ProbeSchema: %v", err)
	}
	for _, wantName := range []string{`"email"`, `"csv.notes"`, `"title"`} {
		if !strings.Contains(string(raw), wantName) {
			t.Errorf("probed schema missing %s: %s", wantName, raw)
		}
	}
}

func TestColumnsMappingErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.csv")
	if err := os.WriteFile(path, []byte("Email\njane@acme.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A mapping to a column the file does not have fails at probe time — this
	// is what makes it a plan error, not a runtime surprise (SPEC §7).
	if _, err := (&Adapter{}).ProbeSchema(map[string]any{"path": path,
		"columns": map[string]any{"full_name": "Contact"}}); err == nil {
		t.Error("want an error for a columns: entry naming a missing header")
	}
	// A columns: target must be canonical or namespaced (SPEC §4a layer 1).
	_, err := (&Adapter{}).ProbeSchema(map[string]any{"path": path,
		"columns": map[string]any{"shoe_size": "Email"}})
	if err == nil || !strings.Contains(err.Error(), "not a canonical") {
		t.Errorf("want a registry validation error, got %v", err)
	}
}

func TestInvalidValueIsDroppedNotFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.csv")
	csv := "Email,LinkedIn URL,Company Employees\n" +
		"jane@acme.com,https://www.linkedin.com/sales/lead/ACwAAAbQ2xKB9abc,not-a-number\n"
	if err := os.WriteFile(path, []byte(csv), 0o644); err != nil {
		t.Fatal(err)
	}
	msgs, err := drive(t, map[string]any{"path": path})
	if err != nil {
		t.Fatalf("Run: %v (invalid values must never crash the run, SPEC §10.1)", err)
	}
	var rec *protocol.Message
	warns := 0
	for i := range msgs {
		switch msgs[i].Type {
		case protocol.TypeRecord:
			rec = &msgs[i]
		case protocol.TypeLog:
			if msgs[i].Level == "warn" {
				warns++
			}
		}
	}
	if rec == nil {
		t.Fatal("record with a valid email should still be emitted")
	}
	if rec.Fields["email"] != "jane@acme.com" {
		t.Errorf("email = %#v", rec.Fields["email"])
	}
	// A Sales Navigator URL is an invalid value for linkedin_url (ADR-020) and
	// a range/garbage string is invalid for an integer field — both dropped.
	if _, ok := rec.Fields["linkedin_url"]; ok {
		t.Error("non-public URL must not survive under linkedin_url")
	}
	if _, ok := rec.Fields["company_employees"]; ok {
		t.Error("non-integer must not survive under company_employees")
	}
	if warns != 2 {
		t.Errorf("want 2 dropped-field warnings, got %d", warns)
	}
}

func TestEntityTypeOverride(t *testing.T) {
	a := &Adapter{}
	if got := a.EntityType(map[string]any{"path": "x.csv"}); got != "" {
		t.Errorf("EntityType = %q, want empty so the manifest wins", got)
	}
	if got := a.EntityType(map[string]any{"path": "x.csv", "entity_type": "company"}); got != "company" {
		t.Errorf("EntityType = %q, want company", got)
	}
}

func TestNormalizeHeader(t *testing.T) {
	got := normalizeHeader([]string{" First Name ", "first-name", "E.Mail", "", "Company/Domain"})
	want := []string{"first_name", "first_name_2", "e_mail", "column_4", "company_domain"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("header %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestManifestIsRegistered(t *testing.T) {
	resolved, err := adapters.Resolve(ID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.External {
		t.Error("csv/source is built in")
	}
	if resolved.Manifest.Role != adapters.RoleSource {
		t.Errorf("role = %q", resolved.Manifest.Role)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
