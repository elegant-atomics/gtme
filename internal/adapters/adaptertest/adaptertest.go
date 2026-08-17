// Package adaptertest drives an adapter over the wire protocol with a stubbed
// HTTP client, so provider adapters are tested against fixtures and never touch
// the network (SPEC §10).
package adaptertest

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/protocol"
)

// Call is one HTTP request the adapter made, captured for assertions.
type Call struct {
	Method string
	URL    string
	Header http.Header
	Body   string
}

// Stub answers requests from a routing table. Keys are matched as substrings of
// "METHOD path", so a route like "POST /api/v1/mixed_people/search" is readable
// and precise enough.
type Stub struct {
	Routes map[string]Response
	Calls  []Call
	// Fallback answers anything the routes miss; zero value is a 404.
	Fallback *Response
}

// Response is a canned reply.
type Response struct {
	Status int
	Body   string
	Header http.Header
	Err    error
}

// Do implements httpx.Doer.
func (s *Stub) Do(req *http.Request) (*http.Response, error) {
	body := ""
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		body = string(raw)
	}
	s.Calls = append(s.Calls, Call{
		Method: req.Method,
		URL:    req.URL.String(),
		Header: req.Header.Clone(),
		Body:   body,
	})

	key := req.Method + " " + req.URL.Path
	res, ok := s.Routes[key]
	if !ok {
		for route, candidate := range s.Routes {
			if strings.HasPrefix(key, route) || strings.Contains(key, route) {
				res, ok = candidate, true
				break
			}
		}
	}
	if !ok {
		if s.Fallback != nil {
			res = *s.Fallback
		} else {
			res = Response{Status: 404, Body: `{"error":"no stub route for ` + key + `"}`}
		}
	}
	if res.Err != nil {
		return nil, res.Err
	}
	status := res.Status
	if status == 0 {
		status = 200
	}
	header := res.Header
	if header == nil {
		header = http.Header{}
	}
	header.Set("Content-Type", "application/json")

	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(res.Body)),
		Request:    req,
	}, nil
}

// CallsTo counts requests whose "METHOD path" contains want.
func (s *Stub) CallsTo(want string) int {
	n := 0
	for _, c := range s.Calls {
		if strings.Contains(c.Method+" "+c.URL, want) {
			n++
		}
	}
	return n
}

// Fixture reads a file from the caller package's fixtures directory.
func Fixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("fixtures", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return string(raw)
}

// Input is what the runner sends an adapter: a config, then records.
type Input struct {
	Config  map[string]any
	Records []protocol.Message
	Env     map[string]string
}

// Record builds a RECORD message for an input.
func Record(identityKey string, fields map[string]any) protocol.Message {
	return protocol.Record(protocol.Key{EntityType: "person", IdentityKey: identityKey}, fields, nil)
}

// Run drives an adapter to completion and returns everything it emitted.
func Run(t *testing.T, a adapters.Adapter, in Input) ([]protocol.Message, error) {
	t.Helper()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	go func() {
		w := protocol.NewWriter(inW)
		w.Write(protocol.Message{Type: protocol.TypeOpen, StepID: "step", RunID: "run1", Config: in.Config})
		for _, rec := range in.Records {
			w.Write(rec)
		}
		w.Write(protocol.End())
		inW.Close()
	}()

	runErr := make(chan error, 1)
	go func() {
		err := a.Run(context.Background(), adapters.Ports{In: inR, Out: outW, Log: io.Discard, Env: in.Env})
		outW.CloseWithError(err)
		runErr <- err
	}()

	var msgs []protocol.Message
	r := protocol.NewReader(outR)
	for {
		m, err := r.Next()
		if err != nil {
			break
		}
		msgs = append(msgs, m)
	}
	return msgs, <-runErr
}

// Records returns just the RECORD messages.
func Records(msgs []protocol.Message) []protocol.Message {
	var out []protocol.Message
	for _, m := range msgs {
		if m.Type == protocol.TypeRecord {
			out = append(out, m)
		}
	}
	return out
}

// Costs returns just the COST messages.
func Costs(msgs []protocol.Message) []protocol.Message {
	var out []protocol.Message
	for _, m := range msgs {
		if m.Type == protocol.TypeCost {
			out = append(out, m)
		}
	}
	return out
}

// Logs joins every LOG message, for asserting on warnings.
func Logs(msgs []protocol.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Type == protocol.TypeLog {
			b.WriteString(m.Level + ": " + m.Msg + "\n")
		}
	}
	return b.String()
}
