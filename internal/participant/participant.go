// Package participant is the one implementation behind the human/* and
// agent/* adapters (SPEC §8 "People and agents answer", ADR-049): the answer
// contract a pending step exposes — what a participant is shown, what it may
// answer — the validation `gtme answer` and the in-run walk both apply, and
// the interactive walk itself. The runner and the CLI share it so a person
// at a terminal and an agent answering later are held to exactly the same
// contract.
package participant

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/elegant-atomics/gtme/internal/adapters"
)

// Field is one declared output a participant answers.
type Field struct {
	Name string
	// Type is a JSON-Schema primitive type, or "" when untyped (free text).
	Type string
	// Enum is the declared value domain: the menu.
	Enum []string
}

// Contract is what a pending step accepts as an answer (SPEC §8): for a
// filter, `pass` and `reason` plus any declared fields; for a compose or
// review, the declared (or default) fields — every one required.
type Contract struct {
	Role   string
	Fields []Field
}

// ContractFor derives the contract from a step's role and its effective
// provides schema (the declared provides: schema, or the manifest's static
// one; nil for a filter that declares nothing).
func ContractFor(role string, provides json.RawMessage) (Contract, error) {
	c := Contract{Role: role}
	if len(provides) == 0 {
		return c, nil
	}
	var doc struct {
		Properties map[string]struct {
			Type string   `json:"type"`
			Enum []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(provides, &doc); err != nil {
		return c, fmt.Errorf("participant: not a provides schema: %w", err)
	}
	order := append([]string(nil), doc.Required...)
	seen := map[string]bool{}
	for _, n := range order {
		seen[n] = true
	}
	var rest []string
	for n := range doc.Properties {
		if !seen[n] {
			rest = append(rest, n)
		}
	}
	sort.Strings(rest)
	for _, n := range append(order, rest...) {
		p, ok := doc.Properties[n]
		if !ok {
			continue
		}
		c.Fields = append(c.Fields, Field{Name: n, Type: p.Type, Enum: p.Enum})
	}
	return c, nil
}

// Outputs lists what the contract accepts, for a menu or an error message:
// pass/reason first for a filter, then the fields with their domains.
func (c Contract) Outputs() []string {
	var out []string
	if c.Role == adapters.RoleFilter {
		out = append(out, "pass=true|false", "reason=<text>")
	}
	for _, f := range c.Fields {
		out = append(out, f.Name+"="+hint(f))
	}
	return out
}

func hint(f Field) string {
	if len(f.Enum) > 0 {
		return strings.Join(f.Enum, "|")
	}
	switch f.Type {
	case "integer":
		return "<integer>"
	case "number":
		return "<number>"
	case "boolean":
		return "true|false"
	case "array":
		return "<json array>"
	default:
		return "<text>"
	}
}

// Answer is a validated answer: the verdict for a filter, and the fields to
// write (never pass/reason — those are the verdict, not facts).
type Answer struct {
	Pass   bool
	Reason string
	Fields map[string]any
}

// Wire is the answer as an `answered` event carries it (SPEC §3): the
// fields, plus pass and reason for a filter.
func (a Answer) Wire(role string) map[string]any {
	out := make(map[string]any, len(a.Fields)+2)
	for k, v := range a.Fields {
		out[k] = v
	}
	if role == adapters.RoleFilter {
		out["pass"] = a.Pass
		out["reason"] = a.Reason
	}
	return out
}

// Parse validates the string form of an answer — `--set field=value` — and
// types it (SPEC §8): a filter accepts only pass and reason beside its
// declared fields; a value outside an enum is refused naming the allowed
// values; a missing field is refused naming it; an unknown key is refused
// naming what the step declares.
func (c Contract) Parse(set map[string]string) (Answer, error) {
	typed := make(map[string]any, len(set))
	for k, v := range set {
		typed[k] = v
	}
	return c.validate(typed, true)
}

// Validate checks an already-typed answer — one read back from an
// `answered` event — against the contract.
func (c Contract) Validate(fields map[string]any) (Answer, error) {
	return c.validate(fields, false)
}

func (c Contract) validate(raw map[string]any, fromStrings bool) (Answer, error) {
	a := Answer{Fields: map[string]any{}}
	declared := map[string]Field{}
	for _, f := range c.Fields {
		declared[f.Name] = f
	}
	var unknown []string
	for k, v := range raw {
		switch {
		case c.Role == adapters.RoleFilter && k == "pass":
			pass, ok := parseBool(v)
			if !ok {
				return a, fmt.Errorf("pass must be true or false (got %v)", v)
			}
			a.Pass = pass
		case c.Role == adapters.RoleFilter && k == "reason":
			a.Reason = strings.TrimSpace(fmt.Sprint(v))
		default:
			f, ok := c.resolve(declared, k)
			if !ok {
				unknown = append(unknown, k)
				continue
			}
			value, err := coerce(f, v, fromStrings)
			if err != nil {
				return a, err
			}
			a.Fields[f.Name] = value
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return a, fmt.Errorf("%s: not an output of this step — it takes %s", strings.Join(unknown, ", "), strings.Join(c.Outputs(), ", "))
	}
	if c.Role == adapters.RoleFilter {
		if _, ok := raw["pass"]; !ok {
			return a, fmt.Errorf("a filter takes pass=true|false (and reason=<text>)")
		}
	}
	var missing []string
	for _, f := range c.Fields {
		if _, ok := a.Fields[f.Name]; !ok {
			missing = append(missing, f.Name)
		}
	}
	if len(missing) > 0 {
		return a, fmt.Errorf("missing %s — the step declares %s", strings.Join(missing, ", "), strings.Join(c.Outputs(), ", "))
	}
	return a, nil
}

// resolve finds the declared output an answer names. A declared provides:
// lands namespaced (`<pipeline>.<name>`, ADR-033) but the operator wrote the
// bare name in the pipeline file, so `--set grade=B` answers `review.grade`
// — as long as exactly one declared output ends in it. Two fields sharing a
// bare name are ambiguous, so only the full name will do.
func (c Contract) resolve(declared map[string]Field, name string) (Field, bool) {
	if f, ok := declared[name]; ok {
		return f, true
	}
	var found Field
	n := 0
	for _, f := range c.Fields {
		if strings.HasSuffix(f.Name, "."+name) {
			found, n = f, n+1
		}
	}
	return found, n == 1
}

func parseBool(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "yes", "y", "pass":
			return true, true
		case "false", "no", "n", "fail":
			return false, true
		}
	}
	return false, false
}

// coerce types one value: from a string (the --set and terminal forms) by
// the declared type, or as already typed; enums are checked either way.
func coerce(f Field, v any, fromString bool) (any, error) {
	if s, ok := v.(string); ok {
		s = strings.TrimSpace(s)
		if len(f.Enum) > 0 {
			for _, e := range f.Enum {
				if s == e {
					return e, nil
				}
			}
			return nil, fmt.Errorf("%s must be one of %s (got %q)", f.Name, strings.Join(f.Enum, ", "), s)
		}
		if !fromString {
			if f.Type != "" && f.Type != "string" {
				return nil, fmt.Errorf("%s must be a %s (got %q)", f.Name, f.Type, s)
			}
			return s, nil
		}
		switch f.Type {
		case "integer":
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%s must be an integer (got %q)", f.Name, s)
			}
			return float64(n), nil
		case "number":
			n, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return nil, fmt.Errorf("%s must be a number (got %q)", f.Name, s)
			}
			return n, nil
		case "boolean":
			b, ok := parseBool(s)
			if !ok {
				return nil, fmt.Errorf("%s must be true or false (got %q)", f.Name, s)
			}
			return b, nil
		case "array":
			var list []any
			if err := json.Unmarshal([]byte(s), &list); err != nil {
				return nil, fmt.Errorf("%s must be a JSON array (got %q)", f.Name, s)
			}
			return list, nil
		default:
			if s == "" {
				return nil, fmt.Errorf("%s must not be empty", f.Name)
			}
			return s, nil
		}
	}
	if len(f.Enum) > 0 {
		return nil, fmt.Errorf("%s must be one of %s (got %v)", f.Name, strings.Join(f.Enum, ", "), v)
	}
	switch f.Type {
	case "integer":
		n, ok := v.(float64)
		if !ok || n != float64(int64(n)) {
			return nil, fmt.Errorf("%s must be an integer (got %v)", f.Name, v)
		}
	case "number":
		if _, ok := v.(float64); !ok {
			return nil, fmt.Errorf("%s must be a number (got %v)", f.Name, v)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return nil, fmt.Errorf("%s must be true or false (got %v)", f.Name, v)
		}
	case "array":
		if _, ok := v.([]any); !ok {
			return nil, fmt.Errorf("%s must be an array (got %v)", f.Name, v)
		}
	case "string":
		return nil, fmt.Errorf("%s must be a string (got %v)", f.Name, v)
	}
	if v == nil {
		return nil, fmt.Errorf("%s must not be null", f.Name)
	}
	return v, nil
}

// Surface is what a participant is shown per record (SPEC §8, ADR-049):
// the render: fields, a template, or — by default — the uses: fields, or the
// of: value alone.
type Surface struct {
	Fields   []string
	Template string
	Of       string
	Uses     []string
}

var placeholder = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.\-]+)\s*\}\}`)

// Render renders one record's surface as text: the template with its
// placeholders filled, or one `field: value` line per shown field (the
// referent first, marked), every value JSON-encoded so a string reads as a
// string.
func (s Surface) Render(fields map[string]any) string {
	if strings.TrimSpace(s.Template) != "" {
		return placeholder.ReplaceAllStringFunc(s.Template, func(m string) string {
			name := placeholder.FindStringSubmatch(m)[1]
			if v, ok := fields[name]; ok {
				if str, ok := v.(string); ok {
					return str
				}
				return encode(v)
			}
			return "(no " + name + ")"
		})
	}
	var b strings.Builder
	if s.Of != "" {
		fmt.Fprintf(&b, "%s (the value under review): %s\n", s.Of, encodeOr(fields, s.Of))
	}
	for _, name := range s.Shown() {
		if name == s.Of {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", name, encodeOr(fields, name))
	}
	return b.String()
}

// Shown lists the field names the surface presents, in order: render.fields
// when declared, else uses:, else the referent alone; with none of those,
// every field, sorted, is shown by Render's caller passing them as Uses.
func (s Surface) Shown() []string {
	switch {
	case len(s.Fields) > 0:
		return s.Fields
	case len(s.Uses) > 0:
		return s.Uses
	case s.Of != "":
		return []string{s.Of}
	}
	return nil
}

func encodeOr(fields map[string]any, name string) string {
	v, ok := fields[name]
	if !ok {
		return "(no value)"
	}
	return encode(v)
}

func encode(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(raw)
}

// DefaultName is the participant name `--as` defaults to: the OS user.
func DefaultName() string {
	if u, err := user.Current(); err == nil && strings.TrimSpace(u.Username) != "" {
		return u.Username
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	return "operator"
}

// Qualify prefixes a participant name by the adapter it answers under
// (SPEC §10a): human/<name> or agent/<name>.
func Qualify(adapterID, name string) string {
	kind := adapters.ParticipantKind(adapterID)
	if kind == "" {
		kind = adapters.KindHuman
	}
	return kind + "/" + strings.TrimSpace(name)
}

// Bare strips the kind prefix a qualified participant carries: the ledger's
// `answered` event names who answered as `human/<name>` or `agent/<name>`,
// while provenance (SPEC §10a) puts the adapter id before the `@` and the
// participant's own name after it — `human/review @ trevor#<sig>`.
func Bare(qualified string) string {
	if _, name, ok := strings.Cut(qualified, "/"); ok {
		return name
	}
	return qualified
}

// ErrInterrupted is the walk's answer to Ctrl-C or end of input: the rest
// stays pending (SPEC §8).
var ErrInterrupted = errors.New("participant: interrupted — the remaining records stay pending")

// Pending is one record awaiting an answer, as the walk presents it.
type Pending struct {
	IdentityKey string
	Fields      map[string]any
}

// Walker is the interactive walk (SPEC §8, ADR-049): for each pending
// record, the rendered surface, then the declared outputs as a menu (an
// enum) or a field to fill, validated on the spot. The same code drives the
// in-run walk and `gtme answer` with no key.
type Walker struct {
	In       io.Reader
	Out      io.Writer
	Contract Contract
	Surface  Surface
	// StepID and Adapter label the walk.
	StepID  string
	Adapter string
}

// Walk asks about every record in turn, handing each validated answer to
// record. It returns how many were answered; ErrInterrupted when the
// context is cancelled (Ctrl-C) or the input ends before the last record.
func (w *Walker) Walk(ctx context.Context, records []Pending, record func(Pending, Answer) error) (int, error) {
	lines := readLines(w.In)
	fmt.Fprintf(w.Out, "%s (%s): %d record(s) to answer — Ctrl-C leaves the rest pending\n", w.StepID, w.Adapter, len(records))
	answered := 0
	for i, rec := range records {
		fmt.Fprintf(w.Out, "\n[%d/%d] %s\n", i+1, len(records), rec.IdentityKey)
		surface := w.Surface
		if len(surface.Shown()) == 0 {
			surface.Uses = sortedKeys(rec.Fields)
		}
		fmt.Fprint(w.Out, indent(surface.Render(rec.Fields)))
		set := map[string]string{}
		if w.Contract.Role == adapters.RoleFilter {
			v, err := w.ask(ctx, lines, "pass [y/n]", false)
			if err != nil {
				return answered, err
			}
			set["pass"] = v
			reason, err := w.ask(ctx, lines, "reason", true)
			if err != nil {
				return answered, err
			}
			set["reason"] = reason
		}
		for _, f := range w.Contract.Fields {
			for {
				v, err := w.ask(ctx, lines, f.Name+" ["+hint(f)+"]", false)
				if err != nil {
					return answered, err
				}
				set[f.Name] = v
				if _, err := coerce(f, v, true); err == nil {
					break
				} else {
					fmt.Fprintf(w.Out, "  %v\n", err)
				}
			}
		}
		a, err := w.Contract.Parse(set)
		if err != nil {
			// The field prompts validated each value; what is left is a
			// contract-level refusal, which a person can act on next time.
			fmt.Fprintf(w.Out, "  refused: %v\n", err)
			continue
		}
		if err := record(rec, a); err != nil {
			return answered, err
		}
		answered++
	}
	return answered, nil
}

func (w *Walker) ask(ctx context.Context, lines <-chan string, label string, optional bool) (string, error) {
	for {
		fmt.Fprintf(w.Out, "  %s: ", label)
		select {
		case <-ctx.Done():
			fmt.Fprintln(w.Out)
			return "", ErrInterrupted
		case line, ok := <-lines:
			if !ok {
				fmt.Fprintln(w.Out)
				return "", ErrInterrupted
			}
			line = strings.TrimSpace(line)
			if line == "" && !optional {
				fmt.Fprintln(w.Out, "  a value is needed")
				continue
			}
			return line, nil
		}
	}
}

// readLines feeds input lines through a channel so a blocked read never
// outlives the context: on Ctrl-C the walk returns and the reader is left
// to the process exit.
func readLines(in io.Reader) <-chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(in)
		for sc.Scan() {
			ch <- sc.Text()
		}
	}()
	return ch
}

func indent(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
