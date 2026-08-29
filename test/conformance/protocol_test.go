package conformance

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/elegant-atomics/gtme/internal/protocol"
)

// transcriptLine is one line of a spec/wire/*.ndjson golden transcript. See
// spec/wire/README.md for the envelope's meaning.
type transcriptLine struct {
	Stream  string          `json:"stream"`
	Dir     string          `json:"dir"`
	Adapter string          `json:"adapter"`
	Msg     json.RawMessage `json:"msg"`
}

const (
	dirIn  = "runner->adapter"
	dirOut = "adapter->runner"
)

// schemaFor maps a (direction, message type) pair onto its schema file, exactly
// as the table in spec/wire/README.md does.
func schemaFor(dir, msgType string) (string, error) {
	switch dir {
	case dirIn:
		switch msgType {
		case protocol.TypeOpen:
			return "msg-open.schema.json", nil
		case protocol.TypeRecord:
			return "msg-record-in.schema.json", nil
		case protocol.TypeEnd:
			return "msg-end.schema.json", nil
		}
	case dirOut:
		switch msgType {
		case protocol.TypeSchema:
			return "msg-schema.schema.json", nil
		case protocol.TypeRecord:
			return "msg-record-out.schema.json", nil
		case protocol.TypeVerdict:
			return "msg-verdict.schema.json", nil
		case protocol.TypeAttest:
			return "msg-attest.schema.json", nil
		case protocol.TypePending:
			return "msg-pending.schema.json", nil
		case protocol.TypeCost:
			return "msg-cost.schema.json", nil
		case protocol.TypeState:
			return "msg-state.schema.json", nil
		case protocol.TypeLog:
			return "msg-log.schema.json", nil
		case protocol.TypeEnd:
			return "msg-end.schema.json", nil
		}
	default:
		return "", fmt.Errorf("unknown direction %q (expected %q or %q)", dir, dirIn, dirOut)
	}
	return "", fmt.Errorf("no schema in spec/schemas/ for a %s message going %s", msgType, dir)
}

// loadTranscript reads a golden transcript from spec/wire/.
func loadTranscript(t *testing.T, name string) []transcriptLine {
	t.Helper()
	path := specPath("wire", name)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	defer f.Close()

	var out []transcriptLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), protocol.MaxLineBytes)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var tl transcriptLine
		if err := json.Unmarshal([]byte(line), &tl); err != nil {
			t.Fatalf("%s:%d: not a transcript envelope: %v", name, n, err)
		}
		if tl.Dir == "" || len(tl.Msg) == 0 {
			t.Fatalf("%s:%d: envelope needs both `dir` and `msg`", name, n)
		}
		out = append(out, tl)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if len(out) == 0 {
		t.Fatalf("%s is empty", path)
	}
	return out
}

// TestWireTranscriptValidatesAgainstSchemas is the ADR-010 protocol-conformance
// test: every message in every golden transcript must validate against the
// spec/schemas/ file for its type and direction.
func TestWireTranscriptValidatesAgainstSchemas(t *testing.T) {
	entries, err := os.ReadDir(specPath("wire"))
	if err != nil {
		t.Fatalf("reading spec/wire: %v", err)
	}
	var transcripts []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".ndjson") {
			transcripts = append(transcripts, e.Name())
		}
	}
	if len(transcripts) == 0 {
		t.Fatal("spec/wire holds no .ndjson golden transcripts")
	}

	compiled := map[string]*jsonschema.Schema{}
	seen := map[string]int{} // "dir type" → count, for the coverage check below

	for _, name := range transcripts {
		t.Run(name, func(t *testing.T) {
			for i, line := range loadTranscript(t, name) {
				var envelope struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(line.Msg, &envelope); err != nil {
					t.Errorf("%s line %d: msg is not an object: %v", name, i+1, err)
					continue
				}
				if envelope.Type == "" {
					t.Errorf("%s line %d: msg has no `type` (SPEC §5: every message is {\"type\": ...})", name, i+1)
					continue
				}
				seen[line.Dir+" "+envelope.Type]++

				schemaFile, err := schemaFor(line.Dir, envelope.Type)
				if err != nil {
					t.Errorf("%s line %d: %v", name, i+1, err)
					continue
				}
				s, ok := compiled[schemaFile]
				if !ok {
					s = compileSchema(t, schemaFile)
					compiled[schemaFile] = s
				}
				if err := s.Validate(asJSONValue(t, line.Msg)); err != nil {
					t.Errorf("%s line %d (%s, %s, adapter %s) fails spec/schemas/%s:\n%v\n  message: %s",
						name, i+1, envelope.Type, line.Dir, line.Adapter, schemaFile, err, line.Msg)
				}
			}
		})
	}

	// The transcripts are only worth loading if they actually exercise the
	// message types SPEC §11 M2 calls out.
	t.Run("covers the required message types", func(t *testing.T) {
		required := []string{
			dirIn + " " + protocol.TypeOpen,
			dirIn + " " + protocol.TypeRecord,
			dirIn + " " + protocol.TypeEnd,
			dirOut + " " + protocol.TypeRecord,
			dirOut + " " + protocol.TypeEnd,
		}
		var missing []string
		for _, want := range required {
			if seen[want] == 0 {
				missing = append(missing, want)
			}
		}
		if len(missing) > 0 {
			t.Errorf("no golden transcript in spec/wire exercises: %s", strings.Join(missing, "; "))
		}
	})
}

// TestTranscriptRoundTripsThroughProtocol checks the other half of the contract:
// the golden lines are not just schema-valid, they are what internal/protocol
// actually reads and writes. A message that changes shape on the way through is
// a serialization divergence.
func TestTranscriptRoundTripsThroughProtocol(t *testing.T) {
	for _, line := range loadTranscript(t, "basic-run.ndjson") {
		var m protocol.Message
		if err := json.Unmarshal(line.Msg, &m); err != nil {
			t.Errorf("internal/protocol cannot decode a golden line: %v\n  %s", err, line.Msg)
			continue
		}
		if m.Type == "" {
			t.Errorf("internal/protocol decoded a golden line to an empty type: %s", line.Msg)
			continue
		}
		reencoded, err := json.Marshal(m)
		if err != nil {
			t.Errorf("internal/protocol cannot re-encode %s: %v", m.Type, err)
			continue
		}
		// Re-encoding must still satisfy the schema; the values may be reordered
		// or drop zero-valued omitempty fields, which the schemas allow for.
		schemaFile, err := schemaFor(line.Dir, m.Type)
		if err != nil {
			t.Errorf("%v", err)
			continue
		}
		if err := compileSchema(t, schemaFile).Validate(asJSONValue(t, reencoded)); err != nil {
			t.Errorf("a %s message re-encoded by internal/protocol no longer satisfies spec/schemas/%s:\n%v\n"+
				"  golden:     %s\n  re-encoded: %s", m.Type, schemaFile, err, line.Msg, reencoded)
		}
	}
}

// TestEveryProtocolSchemaIsCompilable guards the corpus itself: a schema nobody
// happens to exercise still has to be loadable draft-07.
func TestEveryProtocolSchemaIsCompilable(t *testing.T) {
	entries, err := os.ReadDir(specPath("schemas"))
	if err != nil {
		t.Fatalf("reading spec/schemas: %v", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".schema.json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("spec/schemas holds no *.schema.json files")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) { compileSchema(t, name) })
	}
}
