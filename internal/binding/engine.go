package binding

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/httpx"
	"github.com/elegant-atomics/gtme/internal/identity"
	"github.com/elegant-atomics/gtme/internal/protocol"
	"github.com/elegant-atomics/gtme/internal/registry"
	"github.com/elegant-atomics/gtme/internal/ulid"
)

// MaxPayloadBytes is the engine's payload size cap (SPEC §10a): an oversized
// response is not retained (dropped with a warning), never truncated silently.
const MaxPayloadBytes = 256 << 10

// SimulateEnv is the environment key the runner injects to put a binding
// engine into fixture-served mode (SPEC §8 simulation gate). It rides in
// Ports.Env like a credential, so the protocol stays untouched.
const SimulateEnv = "GTME_SIMULATE"

// Engine interprets one binding deterministically (SPEC §10a). It is an
// ordinary built-in adapter: same protocol boundary, same Session transport.
type Engine struct {
	B *Binding
	// HTTP is the live seam; tests stub it.
	HTTP httpx.Doer
	// Fixtures are the binding's conformance fixtures, nil when it ships none.
	Fixtures *FixtureSet
}

// Run implements adapters.Adapter.
func (e *Engine) Run(ctx context.Context, p adapters.Ports) error {
	r := protocol.NewReader(p.In)
	w := protocol.NewWriter(p.Out)

	simulate := p.Getenv(SimulateEnv) != ""
	doer := e.HTTP
	if simulate {
		if e.Fixtures == nil {
			// A binding without fixtures is a simulation gap (SPEC §8): say so and
			// serve nothing, rather than silently passing or touching the network.
			return e.runGap(r, w)
		}
		doer = e.Fixtures.Doer()
	}

	var (
		cfg     map[string]any
		opened  bool
		session string
	)

	for {
		m, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		switch m.Type {
		case protocol.TypeOpen:
			cfg = e.configWithDefaults(m.Config)
			if e.B.Auth != nil && !simulate {
				if p.Getenv(e.B.Auth.Env) == "" {
					return &httpx.Error{Kind: httpx.KindAuth, Provider: e.B.Provider(),
						Msg: e.B.Auth.Env + " is not set"}
				}
			}
			if e.B.Session != nil {
				session = ulid.New()
			}
			opened = true
			if err := w.Write(protocol.Schema(e.providesSchema())); err != nil {
				return err
			}
			if e.B.Role == adapters.RoleSource {
				if err := e.runSource(ctx, w, p, doer, cfg, session); err != nil {
					return err
				}
			}

		case protocol.TypeRecord:
			if !opened {
				return fmt.Errorf("%s: received a record before OPEN", e.B.ID)
			}
			if m.Key == nil {
				return fmt.Errorf("%s: received a record with no key", e.B.ID)
			}
			switch e.B.Role {
			case adapters.RoleEnrich:
				if err := e.enrichRecord(ctx, w, p, doer, cfg, session, *m.Key, m.Fields); err != nil {
					return err
				}
			case adapters.RoleDeliver:
				if err := e.deliverRecord(ctx, w, p, doer, cfg, session, *m.Key, m.Fields); err != nil {
					return err
				}
			}

		case protocol.TypeEnd:
			// Input complete; keep reading until EOF.
		}
	}
	if !opened {
		return fmt.Errorf("%s: stream ended before OPEN", e.B.ID)
	}
	return w.Write(protocol.End())
}

// runGap answers a fixtureless simulated session: one warning, no records.
func (e *Engine) runGap(r *protocol.Reader, w *protocol.Writer) error {
	for {
		m, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if m.Type == protocol.TypeOpen {
			if err := w.Write(protocol.Schema(e.providesSchema())); err != nil {
				return err
			}
			if err := w.Write(protocol.Log("warn",
				fmt.Sprintf("%s: simulation gap — this binding ships no conformance fixtures", e.B.ID))); err != nil {
				return err
			}
		}
	}
	return w.Write(protocol.End())
}

// runSource pages through the API, emitting records until termination or the
// config record limit (SPEC §10a source role: pagination + cursor/STATE).
func (e *Engine) runSource(ctx context.Context, w *protocol.Writer, p adapters.Ports, doer httpx.Doer, cfg map[string]any, session string) error {
	// limit is the engine's (ADR-047): read it, and unless the binding
	// declares it too, keep it out of the templates' sight.
	limit := intConfig(cfg, "limit")
	if !e.B.declaresConfig("limit") {
		cfg = without(cfg, "limit")
	}
	tctx := tmplContext{Config: cfg, Session: session}
	pageSize := e.pageSize(tctx)

	emitted, pages := 0, 0
	pageNum, offset, cursor := 1, 0, ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := e.buildRequest(tctx, p, pageNum, offset, cursor, pageSize)
		if err != nil {
			return err
		}
		var doc any
		if err := e.do(ctx, doer, req, &doc); err != nil {
			return e.mapError(w, nil, err)
		}
		pages++

		records := e.extractRecords(doc)
		for _, rec := range records {
			fields := e.extractFields(rec, nil)
			if len(fields) == 0 {
				continue
			}
			// The per-record slice rides along for ADR-030 retention (the runner
			// decides whether it is kept).
			if err := w.Write(protocol.Message{Type: protocol.TypeRecord, Fields: fields,
				Payload: e.payloadFor(w, cfg, rec)}); err != nil {
				return err
			}
			emitted++
			if limit > 0 && emitted >= limit {
				break
			}
		}

		if err := w.Write(protocol.Message{Type: protocol.TypeState,
			Cursor: map[string]any{"page": pageNum, "offset": offset, "cursor": cursor}}); err != nil {
			return err
		}
		if limit > 0 && emitted >= limit {
			break
		}
		stop, nextCursor := e.terminated(doc, len(records), pageNum, pages, pageSize)
		if stop {
			break
		}
		pageNum++
		offset += len(records)
		cursor = nextCursor
	}

	if e.B.Cost != nil {
		rate, _ := e.B.Cost.rate(tctx) // unresolved: $0, and the plan said `unset`
		amount := 0.0
		switch e.B.Cost.Per {
		case "record":
			amount = rate * float64(emitted)
		case "request":
			amount = rate * float64(pages)
		}
		if err := w.Write(protocol.Cost(nil, e.B.Provider(), amount,
			map[string]any{"records": emitted, "requests": pages})); err != nil {
			return err
		}
	}
	return w.Write(protocol.Log("info", fmt.Sprintf("%s: %d records", e.B.ID, emitted)))
}

// enrichRecord performs the per-record request and emits what was learned.
func (e *Engine) enrichRecord(ctx context.Context, w *protocol.Writer, p adapters.Ports, doer httpx.Doer, cfg map[string]any, session string, key protocol.Key, in map[string]any) error {
	tctx := tmplContext{Config: cfg, Record: in, Session: session}
	req, err := e.buildRequest(tctx, p, 0, 0, "", 0)
	if err != nil {
		return err
	}
	var doc any
	if err := e.do(ctx, doer, req, &doc); err != nil {
		return e.mapError(w, &key, err)
	}

	target := doc
	if recs := e.extractRecords(doc); len(recs) > 0 {
		target = recs[0]
	}
	learned := e.extractFields(target, in)

	if e.B.Cost != nil && e.B.Cost.Per == "record" {
		rate, _ := e.B.Cost.rate(tctx)
		if err := w.Write(protocol.Cost(&key, e.B.Provider(), rate,
			map[string]any{"records": 1})); err != nil {
			return err
		}
	}
	if len(learned) == 0 {
		return w.Write(protocol.Log("warn", e.B.ID+": nothing returned for "+key.IdentityKey))
	}
	msg := protocol.Record(key, learned, nil)
	msg.Payload = e.payloadFor(w, cfg, doc)
	return w.Write(msg)
}

// payloadFor renders the raw response as an ADR-030 attachment, or nil when
// retention is off or the body exceeds the cap. httpx already decoded the
// response, so the body is a canonical re-encoding of the same JSON —
// recorded as such in DECISIONS.md.
func (e *Engine) payloadFor(w *protocol.Writer, cfg map[string]any, doc any) *protocol.Payload {
	keep := e.B.KeepPayloads == nil || *e.B.KeepPayloads
	if v, ok := cfg["keep_payloads"].(bool); ok {
		keep = v
	}
	if !keep || doc == nil {
		return nil
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil
	}
	if len(raw) > MaxPayloadBytes {
		_ = w.Write(protocol.Log("warn", fmt.Sprintf(
			"%s: response is %d bytes, over the %d-byte payload cap — not retained", e.B.ID, len(raw), MaxPayloadBytes)))
		return nil
	}
	return &protocol.Payload{ContentType: "application/json", Body: string(raw)}
}

// deliverRecord performs the per-record delivery request and acknowledges it.
func (e *Engine) deliverRecord(ctx context.Context, w *protocol.Writer, p adapters.Ports, doer httpx.Doer, cfg map[string]any, session string, key protocol.Key, in map[string]any) error {
	tctx := tmplContext{Config: cfg, Record: in, Session: session,
		Variables: resolveVariables(cfg, in)}
	req, err := e.buildRequest(tctx, p, 0, 0, "", 0)
	if err != nil {
		return err
	}
	if err := e.do(ctx, doer, req, nil); err != nil {
		return e.mapError(w, &key, err)
	}
	// An empty RECORD is the acknowledgement: delivered, nothing learned.
	if err := w.Write(protocol.Record(key, map[string]any{}, nil)); err != nil {
		return err
	}
	if e.B.Cost != nil && e.B.Cost.Per == "record" {
		rate, _ := e.B.Cost.rate(tctx)
		return w.Write(protocol.Cost(&key, e.B.Provider(), rate,
			map[string]any{"records": 1}))
	}
	return nil
}

// resolveVariables renders the deliver variables: mapping (target → ledger
// field, injected by the runner per ADR-018) against a record's fields.
func resolveVariables(cfg map[string]any, fields map[string]any) map[string]string {
	raw, ok := cfg["variables"].(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]string{}
	for target, fieldName := range raw {
		f, ok := fieldName.(string)
		if !ok {
			continue
		}
		if v := strings.TrimSpace(stringifyTemplate(orEmpty(fields[f]))); v != "" {
			out[target] = v
		}
	}
	return out
}

func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// buildRequest templates one HTTP request, layering auth, session and
// pagination params in their declared places.
func (e *Engine) buildRequest(tctx tmplContext, p adapters.Ports, page, offset int, cursor string, pageSize int) (httpx.Request, error) {
	b := e.B
	url := tctx.renderString(b.Request.URL)
	if url == "" {
		return httpx.Request{}, fmt.Errorf("%s: request url template %q resolved empty", b.ID, b.Request.URL)
	}

	req := httpx.Request{
		Method:   b.Request.Method,
		URL:      url,
		Provider: b.Provider(),
		Headers:  map[string]string{},
		Query:    map[string]string{},
	}
	if b.Retry != nil && b.Retry.MaxAttempts > 0 {
		req.Attempts = b.Retry.MaxAttempts
	}
	for k, v := range b.Request.Headers {
		if s := tctx.renderString(v); s != "" {
			req.Headers[k] = s
		}
	}
	for k, v := range b.Request.Query {
		if s := tctx.renderString(v); s != "" {
			req.Query[k] = s
		}
	}

	var body map[string]any
	if b.Request.Body != nil {
		if resolved := tctx.resolveBody(b.Request.Body); resolved != nil {
			if m, ok := resolved.(map[string]any); ok {
				body = m
			}
		}
		if body == nil {
			body = map[string]any{}
		}
	}

	// Pagination params ride in the body or the query, as declared.
	if pg := b.Pagination; pg != nil && page > 0 {
		set := func(name string, value any) {
			if name == "" {
				return
			}
			if pg.In == "body" && body != nil {
				body[name] = value
			} else {
				req.Query[name] = stringifyTemplate(value)
			}
		}
		switch pg.Strategy {
		case "page":
			set(pg.Param, page)
		case "offset":
			set(pg.Param, offset)
		case "cursor":
			if cursor != "" {
				set(pg.Param, cursor)
			}
		}
		if pageSize > 0 {
			set(pg.SizeParam, pageSize)
		}
	}

	if s := b.Session; s != nil && tctx.Session != "" && s.Param != "" {
		switch s.In {
		case "header":
			req.Headers[s.Param] = tctx.Session
		case "body":
			if body != nil {
				body[s.Param] = tctx.Session
			}
		default:
			req.Query[s.Param] = tctx.Session
		}
	}

	if a := b.Auth; a != nil {
		cred := p.Getenv(a.Env)
		switch a.Type {
		case "header":
			req.Headers[a.Name] = a.Prefix + cred
		case "query":
			req.Query[a.Name] = cred
		case "bearer":
			req.Headers["Authorization"] = "Bearer " + cred
		case "basic":
			req.Headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(cred))
		}
	}

	if body != nil {
		req.Body = body
	}
	return req, nil
}

func (e *Engine) do(ctx context.Context, doer httpx.Doer, req httpx.Request, out *any) error {
	if out == nil {
		var sink any
		return httpx.JSON(ctx, doer, req, &sink)
	}
	return httpx.JSON(ctx, doer, req, out)
}

// mapError applies primitive 5 (error → verdict). Statuses the map does not
// name keep the engine default: the classified error fails the run, which the
// runner turns into failed records and the right exit code (SPEC §8).
func (e *Engine) mapError(w *protocol.Writer, key *protocol.Key, err error) error {
	var herr *httpx.Error
	if errors.As(err, &herr) && herr.Status != 0 {
		rule, ok := e.errorRule(herr.Status)
		if ok {
			switch rule.Verdict {
			case "skip", "fail_record":
				reason := rule.Reason
				if reason == "" {
					reason = err.Error()
				}
				who := ""
				if key != nil {
					who = " for " + key.IdentityKey
				}
				return w.Write(protocol.Log("warn", e.B.ID+": "+rule.Verdict+who+": "+reason))
			}
		}
	}
	return err
}

func (e *Engine) errorRule(status int) (ErrorRule, bool) {
	if rule, ok := e.B.Errors[fmt.Sprint(status)]; ok {
		return rule, true
	}
	class := fmt.Sprintf("%dxx", status/100)
	if rule, ok := e.B.Errors[class]; ok {
		return rule, true
	}
	rule, ok := e.B.Errors["default"]
	return rule, ok
}

// extractRecords applies the records path.
func (e *Engine) extractRecords(doc any) []any {
	v := atPaths(doc, e.B.Extract.Records)
	switch t := v.(type) {
	case []any:
		return t
	case map[string]any:
		return []any{t}
	default:
		return nil
	}
}

// extractFields maps one raw record onto canonical fields: paths waterfall,
// sentinel absents, registry transform, and the skip_if_input dedupe. This is
// the adapter boundary of SPEC §4a, executed by the engine.
func (e *Engine) extractFields(rec any, input map[string]any) map[string]any {
	out := map[string]any{}
	for name, rule := range e.B.Extract.Fields {
		v := atPaths(rec, rule.Paths)
		if v == nil || isAbsentValue(v, rule.Absent) {
			continue
		}
		if rule.SkipIfInput && input != nil && !isEmpty(input[name]) {
			continue
		}
		switch rule.Transform {
		case "", "none":
			if s, ok := v.(string); ok {
				v = strings.TrimSpace(s)
			}
		case "linkedin":
			// The §4 classify-and-route rule, engine-owned: the value lands under
			// the field matching its shape, never reinterpreted.
			s, _ := v.(string)
			field, routed := routeLinkedIn(s)
			if routed == "" {
				continue
			}
			if rule.SkipIfInput && input != nil && !isEmpty(input[field]) {
				continue
			}
			out[field] = routed
			continue
		default:
			s, _ := v.(string)
			normalized, err := registry.ApplyRule(rule.Transform, s)
			if err != nil || normalized == "" {
				// An unknown rule cannot exist past Parse; an empty result means the
				// value is invalid for the rule — dropped, mirroring §10.1 ingress.
				continue
			}
			v = normalized
		}
		if isEmpty(v) {
			continue
		}
		out[name] = v
	}
	return out
}

// routeLinkedIn classifies a LinkedIn-URL-shaped value (SPEC §4, ADR-020) and
// returns the canonical field it belongs in plus the stored form.
func routeLinkedIn(s string) (field, value string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	switch identity.ClassifyLinkedIn(s) {
	case identity.LinkedInPublic:
		return "linkedin_url", identity.NormalizeLinkedInURL(s)
	case identity.LinkedInInternal:
		return "linkedin_internal_url", s
	case identity.LinkedInSalesNav:
		return "linkedin_sales_nav_url", s
	default:
		return "", ""
	}
}

func isAbsentValue(v any, absent []any) bool {
	for _, a := range absent {
		if fmt.Sprint(v) == fmt.Sprint(a) {
			return true
		}
	}
	return false
}

// pageSize resolves the declared page size (int or template).
func (e *Engine) pageSize(tctx tmplContext) int {
	pg := e.B.Pagination
	if pg == nil || pg.PageSize == nil {
		return 0
	}
	switch t := pg.PageSize.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		v, ok := tctx.resolveString(t)
		if !ok {
			return 0
		}
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case string:
			var i int
			fmt.Sscanf(n, "%d", &i)
			return i
		}
	}
	return 0
}

// terminated decides whether pagination stops after this page, and carries the
// next cursor forward.
func (e *Engine) terminated(doc any, got, page, pages, pageSize int) (bool, string) {
	pg := e.B.Pagination
	if pg == nil {
		return true, ""
	}
	if got == 0 {
		return true, "" // the always-on empty-page stop
	}
	if pg.Max > 0 && pages >= pg.Max {
		return true, ""
	}
	t := pg.Termination
	if t != nil && t.TotalPagesPath != "" {
		if v := atPaths(doc, []string{t.TotalPagesPath}); v != nil {
			if total, ok := v.(float64); ok && page >= int(total) {
				return true, ""
			}
		}
	}
	if t != nil && t.ShortPage && pageSize > 0 && got < pageSize {
		return true, ""
	}
	next := ""
	if pg.Strategy == "cursor" {
		if v := atPaths(doc, []string{pg.CursorPath}); v != nil {
			next = stringifyTemplate(v)
		}
		if next == "" {
			return true, ""
		}
	}
	return false, next
}

// configWithDefaults fills config keys the config_schema declares defaults
// for, so a binding can rely on them the way a Go adapter's parseConfig does.
func (e *Engine) configWithDefaults(config map[string]any) map[string]any {
	return e.B.configWithDefaults(config)
}

func (b *Binding) configWithDefaults(config map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range config {
		out[k] = v
	}
	var doc struct {
		Properties map[string]struct {
			Default any `json:"default"`
		} `json:"properties"`
	}
	if len(b.ConfigSchema) > 0 && json.Unmarshal(b.ConfigSchema, &doc) == nil {
		for name, p := range doc.Properties {
			if p.Default == nil {
				continue
			}
			if _, set := out[name]; !set {
				out[name] = p.Default
			}
		}
	}
	return out
}

func (e *Engine) providesSchema() []byte {
	if len(e.B.Provides) > 0 {
		return e.B.Provides
	}
	return []byte(`{"type":"object","properties":{}}`)
}

// declaresConfig reports whether the binding's config_schema names key.
func (b *Binding) declaresConfig(key string) bool {
	var doc struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if len(b.ConfigSchema) == 0 || json.Unmarshal(b.ConfigSchema, &doc) != nil {
		return false
	}
	_, ok := doc.Properties[key]
	return ok
}

// without copies cfg minus one key.
func without(cfg map[string]any, key string) map[string]any {
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if k != key {
			out[k] = v
		}
	}
	return out
}

func intConfig(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}
