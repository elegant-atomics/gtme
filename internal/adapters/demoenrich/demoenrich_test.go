package demoenrich

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/gtme-run/gtme/internal/adapters"
	"github.com/gtme-run/gtme/internal/protocol"
)

// drive runs the adapter over in-memory pipes with one OPEN, the given
// records, and END, and returns everything it emitted.
func drive(t *testing.T, config map[string]any, keys ...string) []protocol.Message {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		w := protocol.NewWriter(inW)
		w.Write(protocol.Message{Type: protocol.TypeOpen, StepID: "score", RunID: "run1", Config: config})
		for _, k := range keys {
			key := protocol.Key{IdentityKey: k}
			w.Write(protocol.Record(key, map[string]any{"email": k}, nil))
		}
		w.Write(protocol.End())
		inW.Close()
	}()
	go func() {
		err := (&Adapter{}).Run(context.Background(), adapters.Ports{In: inR, Out: outW, Log: io.Discard})
		outW.CloseWithError(err)
	}()
	var msgs []protocol.Message
	r := protocol.NewReader(outR)
	for {
		m, err := r.Next()
		if errors.Is(err, io.EOF) || err != nil {
			break
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func TestDeterministicScoreAndPretendCost(t *testing.T) {
	msgs := drive(t, nil, "jane.doe@acme.com", "bob@globex.io")
	var records, costs int
	for _, m := range msgs {
		switch m.Type {
		case protocol.TypeRecord:
			records++
			score, ok := m.Fields["demo.score"].(float64)
			if !ok || score < 0 || score > 100 {
				t.Errorf("demo.score = %#v, want an integer in 0..100", m.Fields["demo.score"])
			}
			if int(score) != Score(m.Key.IdentityKey) {
				t.Errorf("score for %s = %v, want %d", m.Key.IdentityKey, score, Score(m.Key.IdentityKey))
			}
			if m.Fields["demo.note"] != Note {
				t.Errorf("demo.note = %#v", m.Fields["demo.note"])
			}
		case protocol.TypeCost:
			costs++
			if m.Provider != "demo" || m.AmountUSD == nil || *m.AmountUSD != DefaultCostPerRecord {
				t.Errorf("cost = %s %v, want demo %v", m.Provider, m.AmountUSD, DefaultCostPerRecord)
			}
		}
	}
	if records != 2 || costs != 2 {
		t.Fatalf("records = %d, costs = %d, want 2 and 2", records, costs)
	}
	if Score("jane.doe@acme.com") != Score("jane.doe@acme.com") {
		t.Error("Score is not deterministic")
	}
}

func TestOperatorPrice(t *testing.T) {
	msgs := drive(t, map[string]any{"cost_per_record_usd": 0.25}, "carol@initech.dev")
	for _, m := range msgs {
		if m.Type == protocol.TypeCost && (m.AmountUSD == nil || *m.AmountUSD != 0.25) {
			t.Errorf("cost = %v, want the operator's 0.25", m.AmountUSD)
		}
	}
	if rate, ok := costRateForTest(map[string]any{"cost_per_record_usd": 0.25}); !ok || rate != 0.25 {
		t.Errorf("plan rate = %v %v, want 0.25 true", rate, ok)
	}
}

func costRateForTest(config map[string]any) (float64, bool) { return costPerRecord(config), true }
