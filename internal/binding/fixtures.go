package binding

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
)

// FixtureSet is a binding's conformance fixtures: canned responses matched by
// "METHOD path" substring. The same file serves the conformance kit (fixture
// payloads in → canonical records out, SPEC §4a/§10a) and `--simulate`
// (SPEC §8), which is exactly the double duty ADR-028 wants fixtures to do.
type FixtureSet struct {
	Responses []FixtureResponse `json:"responses"`
}

// FixtureResponse is one canned reply.
type FixtureResponse struct {
	Match  string `json:"match"` // substring of "METHOD path"
	Status int    `json:"status,omitempty"`
	Body   any    `json:"body"`
}

// FixtureFile is where a binding's fixtures live, next to binding.yaml.
const FixtureFile = "fixtures/conformance.json"

// LoadFixtures reads a binding's fixture set from its directory (an fs.FS
// rooted at the binding's dir). A missing file returns (nil, nil): the binding
// has no fixtures, which `--simulate` must surface as a gap, not an error.
func LoadFixtures(dir fs.FS) (*FixtureSet, error) {
	raw, err := fs.ReadFile(dir, FixtureFile)
	if err != nil {
		return nil, nil
	}
	var set FixtureSet
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("binding: parsing %s: %w", FixtureFile, err)
	}
	if len(set.Responses) == 0 {
		return nil, nil
	}
	return &set, nil
}

// Doer serves the fixtures as an httpx.Doer, so fixture-served execution runs
// through the exact same engine path as live execution.
func (s *FixtureSet) Doer() *fixtureDoer { return &fixtureDoer{set: s} }

type fixtureDoer struct{ set *FixtureSet }

func (d *fixtureDoer) Do(req *http.Request) (*http.Response, error) {
	key := req.Method + " " + req.URL.Path
	for _, r := range d.set.Responses {
		if strings.Contains(key, r.Match) || strings.Contains(req.URL.String(), r.Match) {
			status := r.Status
			if status == 0 {
				status = 200
			}
			raw, err := json.Marshal(r.Body)
			if err != nil {
				return nil, err
			}
			header := http.Header{}
			header.Set("Content-Type", "application/json")
			return &http.Response{
				StatusCode: status,
				Status:     http.StatusText(status),
				Header:     header,
				Body:       io.NopCloser(strings.NewReader(string(raw))),
				Request:    req,
			}, nil
		}
	}
	return &http.Response{
		StatusCode: 404,
		Status:     http.StatusText(404),
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":"no fixture matches ` + key + `"}`)),
		Request:    req,
	}, nil
}
