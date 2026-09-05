package pipeline

import (
	"strings"
	"testing"
	"time"
)

const specExample = `name: apollo-to-instantly
version: 1

source:
  use: apollo/search
  with:
    query: "vp marketing, saas, 50-200 employees"
    limit: 500

steps:
  - id: icp-filter
    use: ai/filter
    with:
      prompt: >
        Keep only contacts likely to own outbound tooling decisions.
      batch_size: 25

  - id: linkedin
    use: harvest/profile
    when: icp-filter.passed
    cache: 30d

  - id: personalize
    use: ai/compose
    with:
      prompt: >
        Write first_line and ps_line using recent_posts and role_history.
      batch_size: 25

  - id: send
    use: instantly/add-to-campaign
    with:
      campaign: "Q3 VP Marketing"
    idempotency: email
`

func TestParseSpecExample(t *testing.T) {
	p, err := Parse([]byte(specExample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Name != "apollo-to-instantly" || p.Version != 1 {
		t.Errorf("name/version = %q/%d", p.Name, p.Version)
	}
	if p.Source.ID != DefaultSourceID || p.Source.Use != "apollo/search" {
		t.Errorf("source = %+v", p.Source)
	}
	if p.Source.With["limit"] != 500 {
		t.Errorf("source limit = %#v", p.Source.With["limit"])
	}
	if len(p.Steps) != 4 {
		t.Fatalf("steps = %d, want 4", len(p.Steps))
	}
	if p.Steps[1].WhenStep() != "icp-filter" {
		t.Errorf("when step = %q", p.Steps[1].WhenStep())
	}
	// A deliver adapter is an ordinary step (ADR-031).
	if send := p.Steps[3]; send.ID != "send" || send.Idempotency != "email" {
		t.Errorf("send step = %+v", send)
	}

	d, err := ParseCache(p.Steps[1].Cache)
	if err != nil {
		t.Fatalf("ParseCache: %v", err)
	}
	if d != 30*24*time.Hour {
		t.Errorf("cache = %v, want 720h", d)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "waterfall is reserved, not usable",
			yaml: "name: x\nsource:\n  use: a/b\nwaterfall:\n  - use: c/d\n",
			want: "not implemented in v0",
		},
		{
			name: "waterfall inside a step is reserved too",
			yaml: "name: x\nsource:\n  use: a/b\nsteps:\n  - id: s\n    waterfall:\n      - use: c/d\n",
			want: "not implemented in v0",
		},
		{
			name: "unknown field is a typo, not an extension point",
			yaml: "name: x\nsource:\n  use: a/b\n  wth:\n    path: p\n",
			want: "field wth not found",
		},
		{
			name: "top-level deliver block is gone (ADR-031)",
			yaml: "name: x\nsource:\n  use: a/b\ndeliver:\n  use: c/d\n",
			want: "deliver adapters are ordinary steps: entries",
		},
		{
			name: "name is required",
			yaml: "source:\n  use: a/b\n",
			want: "name is required",
		},
		{
			name: "source is required",
			yaml: "name: x\nsteps:\n  - id: s\n    use: a/b\n",
			want: "source is required",
		},
		{
			name: "use is required",
			yaml: "name: x\nsource:\n  with:\n    path: p\n",
			want: "use is required",
		},
		{
			name: "duplicate step ids",
			yaml: "name: x\nsource:\n  id: s\n  use: a/b\nsteps:\n  - id: s\n    use: c/d\n",
			want: "duplicate step id",
		},
		{
			name: "when must be <step>.passed",
			yaml: "name: x\nsource:\n  use: a/b\nsteps:\n  - id: s\n    use: c/d\n    when: other.failed\n",
			want: "when must look like",
		},
		{
			name: "when cannot reference a later step",
			yaml: "name: x\nsource:\n  use: a/b\nsteps:\n  - id: s\n    use: c/d\n    when: later.passed\n  - id: later\n    use: e/f\n",
			want: "unknown or later step",
		},
		{
			name: "cache must be Nd",
			yaml: "name: x\nsource:\n  use: a/b\nsteps:\n  - id: s\n    use: c/d\n    cache: 12h\n",
			want: "cache must look like",
		},
		{
			name: "unsupported version",
			yaml: "name: x\nversion: 2\nsource:\n  use: a/b\n",
			want: "unsupported version",
		},
		{
			name: "on_missing vocabulary is run|skip|fail (ADR-053)",
			yaml: "name: x\nsource:\n  use: a/b\nsteps:\n  - id: s\n    use: ai/compose\n    on_missing: maybe\n",
			want: "on_missing must be \"run\", \"skip\" or \"fail\"",
		},
		{
			name: "once: is a group-source key (ADR-052)",
			yaml: "name: x\nsource:\n  use: a/b\n  once: true\n",
			want: "once: is only valid on a group source",
		},
		{
			name: "once: on an interior step is refused too",
			yaml: "name: x\nsource:\n  group: g\nsteps:\n  - id: s\n    use: c/d\n    once: true\n",
			want: "once: is only valid on a group source",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestStepIDsDefaultByPosition(t *testing.T) {
	p, err := Parse([]byte("name: x\nsource:\n  use: a/b\nsteps:\n  - use: c/d\n  - use: e/f\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := []string{p.Source.ID, p.Steps[0].ID, p.Steps[1].ID}
	want := []string{"source", "step-1", "step-2"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("id %d = %q, want %q", i, got[i], want[i])
		}
	}
	if n := len(p.AllSteps()); n != 3 {
		t.Errorf("AllSteps = %d, want 3", n)
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	p, err := Parse([]byte(specExample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	raw, err := Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	again, err := Parse(raw)
	if err != nil {
		t.Fatalf("re-parsing marshalled pipeline: %v\n%s", err, raw)
	}
	if again.Name != p.Name || len(again.Steps) != len(p.Steps) || again.Steps[3].Idempotency != "email" {
		t.Errorf("round trip changed the pipeline:\n%s", raw)
	}
	if again.Steps[1].Cache != "30d" {
		t.Errorf("cache survived as %q", again.Steps[1].Cache)
	}
}

func TestFormatCache(t *testing.T) {
	if got := FormatCache(90 * 24 * time.Hour); got != "90d" {
		t.Errorf("FormatCache = %q, want 90d", got)
	}
}

// TestProvidesDeclarationShapes covers the two provides: forms (ADR-033) and
// the shape errors: a list of names, or a map of name → {type, enum}.
func TestProvidesDeclarationShapes(t *testing.T) {
	parse := func(t *testing.T, provides string) (Step, error) {
		t.Helper()
		p, err := Parse([]byte("name: q\nsource:\n  use: csv/source\nsteps:\n  - id: judge\n    use: ai/filter\n    provides:\n" + provides))
		if err != nil {
			return Step{}, err
		}
		return p.Steps[0], nil
	}

	s, err := parse(t, "      [state, rationale]\n")
	if err != nil {
		t.Fatal(err)
	}
	fields, err := s.ProvidesFields()
	if err != nil || len(fields) != 2 || fields[0].Name != "state" || fields[1].Name != "rationale" {
		t.Errorf("list form = %+v, %v", fields, err)
	}

	s, err = parse(t, "      state: {enum: [now, later]}\n      rationale: {}\n      score: {type: integer}\n      note:\n")
	if err != nil {
		t.Fatal(err)
	}
	fields, err = s.ProvidesFields()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ProvidesField{}
	for _, f := range fields {
		got[f.Name] = f
	}
	if len(fields) != 4 || got["state"].Type != "" || len(got["state"].Enum) != 2 || got["score"].Type != "integer" ||
		got["rationale"].Type != "" || got["note"].Type != "" {
		t.Errorf("map form = %+v", fields)
	}
	// Map form is sorted by name, so the derived order is deterministic.
	if fields[0].Name != "note" || fields[3].Name != "state" {
		t.Errorf("map form order = %+v", fields)
	}

	s, err = parse(t, "      first_line: {canonical: true, type: string}\n")
	if err != nil {
		t.Fatal(err)
	}
	fields, err = s.ProvidesFields()
	if err != nil || len(fields) != 1 || !fields[0].Canonical || fields[0].Type != "string" {
		t.Errorf("canonical form = %+v, %v", fields, err)
	}

	for _, tc := range []struct{ name, yaml, want string }{
		{"empty list", "      []\n", "at least one field"},
		{"blank name", `      [""]` + "\n", "non-empty field names"},
		{"repeat", "      [a, a]\n", "declared twice"},
		{"scalar", "      just-a-string\n", "must be a list of field names or a map"},
		{"bad type", "      x: {type: date}\n", "type must be one of"},
		{"empty enum", "      x: {enum: []}\n", "enum must be a non-empty list"},
		{"enum repeat", "      x: {enum: [a, a]}\n", "repeats"},
		{"unknown keyword", "      x: {description: hi}\n", `unknown keyword "description"`},
		{"enum vs type", "      x: {type: integer, enum: [a]}\n", "contradicts"},
		{"canonical not bool", "      x: {canonical: yes}\n", "canonical must be true or false"},
		{"canonical with dot", "      v.x: {canonical: true}\n", "must not contain a dot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(t, tc.yaml)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
			if err != nil && !strings.Contains(err.Error(), "judge") {
				t.Errorf("error should name the step: %v", err)
			}
		})
	}
}

// TestOnceParsesOnAGroupSource: `once: true` is a group-source key (SPEC §9,
// ADR-052); absent, it is false and nothing about the source changes.
func TestOnceParsesOnAGroupSource(t *testing.T) {
	p, err := Parse([]byte("name: x\nsource:\n  group: todo\n  limit: 2\n  once: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Source.Once || p.Source.Limit != 2 || p.Source.Group != "todo" {
		t.Errorf("source = %+v, want once on a limited group source", p.Source)
	}
	p, err = Parse([]byte("name: x\nsource:\n  group: todo\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Source.Once {
		t.Error("once should default to false")
	}
}
