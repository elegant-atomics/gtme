// Package httpx is the shared HTTP layer for provider adapters: one interface to
// stub in tests, JSON helpers, retry with backoff, and error classification that
// maps onto gtm's exit codes (SPEC §8).
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Doer is the seam every adapter's HTTP calls go through, so fixture tests never
// touch the network (SPEC §10).
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// RetryBase is the first backoff interval; each retry doubles it. Tests set it
// to zero so classified-error cases do not sleep.
var RetryBase = time.Second

// DefaultClient is a sensible client for provider APIs.
func DefaultClient() *http.Client {
	return &http.Client{Timeout: 45 * time.Second}
}

// Error kinds, which map onto exit codes.
const (
	KindAuth      = "auth"
	KindRateLimit = "rate_limit"
	KindNetwork   = "network"
	KindProvider  = "provider"
	KindRequest   = "request"
)

// Error is a classified provider failure.
type Error struct {
	Kind     string
	Status   int
	Provider string
	Msg      string
	Body     string
	// RetryAfter is the provider's requested wait, when it sent one.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", e.Provider, e.Msg)
	if e.Status != 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.Status)
	}
	if e.Body != "" {
		fmt.Fprintf(&b, ": %s", truncate(e.Body, 300))
	}
	return b.String()
}

// ExitCode maps a provider failure onto the process exit code the CLI should use
// (SPEC §8: 3 auth, 4 rate-limited, 5 network).
func (e *Error) ExitCode() int {
	switch e.Kind {
	case KindAuth:
		return 3
	case KindRateLimit:
		return 4
	case KindNetwork:
		return 5
	default:
		return 1
	}
}

// Retryable reports whether another attempt could plausibly succeed.
func (e *Error) Retryable() bool {
	switch e.Kind {
	case KindRateLimit, KindNetwork:
		return true
	case KindProvider:
		return e.Status >= 500
	default:
		return false
	}
}

// ExitCoder is implemented by errors that know which exit code they deserve.
type ExitCoder interface {
	ExitCode() int
}

// ExitCodeFor returns the exit code an error asks for, or 0 when it has no
// opinion.
func ExitCodeFor(err error) int {
	var ec ExitCoder
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return 0
}

// Request describes one JSON call.
type Request struct {
	Method  string
	URL     string
	Headers map[string]string
	Query   map[string]string
	// Body is marshalled as JSON when non-nil.
	Body any
	// Provider names the service, for error messages.
	Provider string
	// Attempts bounds tries including the first (default 3).
	Attempts int
}

// JSON performs the request and decodes the response body into out. It retries
// rate limits, timeouts and 5xx with backoff, honouring Retry-After.
func JSON(ctx context.Context, client Doer, r Request, out any) error {
	if client == nil {
		client = DefaultClient()
	}
	attempts := r.Attempts
	if attempts <= 0 {
		attempts = 3
	}

	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			wait := backoff(attempt, last)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}

		err := once(ctx, client, r, out)
		if err == nil {
			return nil
		}
		last = err
		var perr *Error
		if !errors.As(err, &perr) || !perr.Retryable() {
			return err
		}
	}
	return last
}

func once(ctx context.Context, client Doer, r Request, out any) error {
	var body io.Reader
	if r.Body != nil {
		raw, err := json.Marshal(r.Body)
		if err != nil {
			return &Error{Kind: KindRequest, Provider: r.Provider, Msg: "encoding request body: " + err.Error()}
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, body)
	if err != nil {
		return &Error{Kind: KindRequest, Provider: r.Provider, Msg: err.Error()}
	}
	if len(r.Query) > 0 {
		q := req.URL.Query()
		for k, v := range r.Query {
			if v != "" {
				q.Set(k, v)
			}
		}
		req.URL.RawQuery = q.Encode()
	}
	req.Header.Set("Accept", "application/json")
	if r.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return &Error{Kind: KindNetwork, Provider: r.Provider, Msg: err.Error()}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return &Error{Kind: KindNetwork, Provider: r.Provider, Msg: "reading response: " + err.Error()}
	}

	if resp.StatusCode >= 400 {
		e := &Error{Status: resp.StatusCode, Provider: r.Provider, Body: string(raw)}
		switch {
		case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
			e.Kind, e.Msg = KindAuth, "credentials were rejected"
		case resp.StatusCode == http.StatusTooManyRequests:
			e.Kind, e.Msg = KindRateLimit, "rate limited"
			e.RetryAfter = retryAfter(resp.Header.Get("Retry-After"))
		default:
			e.Kind, e.Msg = KindProvider, "request failed"
		}
		return e
	}

	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &Error{Kind: KindProvider, Status: resp.StatusCode, Provider: r.Provider,
			Msg: "response was not the JSON we expected: " + err.Error(), Body: string(raw)}
	}
	return nil
}

func backoff(attempt int, last error) time.Duration {
	var perr *Error
	if errors.As(last, &perr) && perr.RetryAfter > 0 {
		return perr.RetryAfter
	}
	d := time.Duration(1<<uint(attempt-1)) * RetryBase
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

func retryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(header); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func truncate(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
