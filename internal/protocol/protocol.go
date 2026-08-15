// Package protocol is the NDJSON wire format spoken between the runner and
// adapters, in both directions (SPEC §5). Unknown message types are ignored by
// readers, which is what keeps the format forward compatible.
package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Message types. Runner → adapter: OPEN, RECORD, END. Adapter → runner: SCHEMA,
// RECORD, VERDICT, COST, STATE, LOG, END (SPEC §5).
const (
	TypeOpen   = "OPEN"
	TypeRecord = "RECORD"
	TypeEnd    = "END"

	TypeSchema  = "SCHEMA"
	TypeVerdict = "VERDICT"
	TypeCost    = "COST"
	TypeState   = "STATE"
	TypeLog     = "LOG"
)

// Key identifies a record. The runner canonicalizes keys (SPEC §4); adapters
// echo back whatever key they were handed.
type Key struct {
	EntityType  string `json:"entity_type"`
	IdentityKey string `json:"identity_key"`
}

func (k Key) String() string { return k.EntityType + ":" + k.IdentityKey }

// Zero reports whether the key is unset.
func (k Key) Zero() bool { return k.EntityType == "" && k.IdentityKey == "" }

// Message is one line of the protocol. A single struct serves both directions:
// the type tag says which fields are meaningful, and everything else stays
// omitted on the wire.
type Message struct {
	Type string `json:"type"`

	// OPEN
	StepID string         `json:"step_id,omitempty"`
	RunID  string         `json:"run_id,omitempty"`
	Config map[string]any `json:"config,omitempty"`

	// RECORD, VERDICT, COST
	Key        *Key               `json:"key,omitempty"`
	Fields     map[string]any     `json:"fields,omitempty"`
	Confidence map[string]float64 `json:"confidence,omitempty"`

	// SCHEMA
	Provides json.RawMessage `json:"provides,omitempty"`

	// VERDICT
	Pass   *bool  `json:"pass,omitempty"`
	Reason string `json:"reason,omitempty"`

	// COST. AmountUSD is a pointer, like Pass above and for the same reason: 0 is
	// an explicitly allowed cost (SPEC §5) — a free or unpriced call — and must
	// stay distinguishable from "no COST sent at all". A bare float64 with
	// omitempty would drop a real $0 COST from the wire.
	Provider  string         `json:"provider,omitempty"`
	AmountUSD *float64       `json:"amount_usd,omitempty"`
	Detail    map[string]any `json:"detail,omitempty"`

	// STATE
	Cursor map[string]any `json:"cursor,omitempty"`

	// LOG
	Level string `json:"level,omitempty"`
	Msg   string `json:"msg,omitempty"`
}

// Passed reports a verdict's outcome; a VERDICT with no explicit pass is a fail,
// because a filter that cannot say "keep this" should not silently keep it.
func (m Message) Passed() bool { return m.Pass != nil && *m.Pass }

// Amount reports a COST message's amount; a message with none (should not
// happen for a well-formed COST, but reading one should never panic) is $0.
func (m Message) Amount() float64 {
	if m.AmountUSD == nil {
		return 0
	}
	return *m.AmountUSD
}

// Writer serializes messages as NDJSON. It is safe for concurrent use so an
// adapter can emit LOG and COST lines from several goroutines.
type Writer struct {
	mu  sync.Mutex
	w   io.Writer
	buf *bufio.Writer
}

// NewWriter wraps w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w, buf: bufio.NewWriterSize(w, 64*1024)}
}

// Write emits one message followed by a newline and flushes, so the reader on
// the other side of the pipe sees it immediately.
func (w *Writer) Write(m Message) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("protocol: encoding %s: %w", m.Type, err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.buf.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("protocol: writing %s: %w", m.Type, err)
	}
	if err := w.buf.Flush(); err != nil {
		return fmt.Errorf("protocol: flushing %s: %w", m.Type, err)
	}
	return nil
}

// Convenience constructors for the messages adapters send most.

// Record builds a RECORD message.
func Record(key Key, fields map[string]any, confidence map[string]float64) Message {
	return Message{Type: TypeRecord, Key: &key, Fields: fields, Confidence: confidence}
}

// Verdict builds a VERDICT message.
func Verdict(key Key, pass bool, reason string) Message {
	return Message{Type: TypeVerdict, Key: &key, Pass: &pass, Reason: reason}
}

// Cost builds a COST message. key may be nil for step-level costs.
func Cost(key *Key, provider string, amountUSD float64, detail map[string]any) Message {
	return Message{Type: TypeCost, Key: key, Provider: provider, AmountUSD: &amountUSD, Detail: detail}
}

// Log builds a LOG message.
func Log(level, msg string) Message { return Message{Type: TypeLog, Level: level, Msg: msg} }

// Schema builds a SCHEMA message.
func Schema(provides json.RawMessage) Message {
	return Message{Type: TypeSchema, Provides: provides}
}

// End builds an END message.
func End() Message { return Message{Type: TypeEnd} }

// MaxLineBytes bounds a single NDJSON line. Batched AI responses and scraped
// profiles are the large cases; 8 MiB is far above either.
const MaxLineBytes = 8 << 20

// Reader parses NDJSON messages. Blank lines are skipped; malformed lines are
// an error, because silently dropping a record would lose data.
type Reader struct {
	sc *bufio.Scanner
}

// NewReader wraps r.
func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)
	return &Reader{sc: sc}
}

// Next returns the next message, or io.EOF when the stream ends.
func (r *Reader) Next() (Message, error) {
	for r.sc.Scan() {
		line := r.sc.Bytes()
		if len(trimSpace(line)) == 0 {
			continue
		}
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			return Message{}, fmt.Errorf("protocol: bad NDJSON line: %w", err)
		}
		if m.Type == "" {
			return Message{}, fmt.Errorf("protocol: line has no type: %s", truncate(line))
		}
		return m, nil
	}
	if err := r.sc.Err(); err != nil {
		return Message{}, fmt.Errorf("protocol: reading stream: %w", err)
	}
	return Message{}, io.EOF
}

func trimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && (b[start] == ' ' || b[start] == '\t' || b[start] == '\r' || b[start] == '\n') {
		start++
	}
	end := len(b)
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\r' || b[end-1] == '\n') {
		end--
	}
	return b[start:end]
}

func truncate(b []byte) string {
	const max = 120
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}
