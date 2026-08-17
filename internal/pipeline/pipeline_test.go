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
