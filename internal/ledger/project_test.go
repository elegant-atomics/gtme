package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/elegant-atomics/gtme/internal/identity"
)

// seed writes one field value at a specific time with a specific confidence.
func seed(t *testing.T, l *Ledger, clock *time.Time, id, field string, value any, conf float64, source string, at time.Time) {
	t.Helper()
	*clock = at
	if _, err := l.WriteFields(context.Background(), id, source, Provenance{RunID: "run1"},
		[]FieldWrite{{Field: field, Value: value, Confidence: conf}}); err != nil {
		t.Fatalf("WriteFields: %v", err)
	}
}

func TestProjectPicksHighestConfidenceInWindow(t *testing.T) {
	ctx := context.Background()
	l, clock := openTest(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	res, err := l.UpsertIdentity(ctx, identity.Person, map[string]any{"email": "jane@acme.com"}, Provenance{})
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	id := res.Identity.ID

	// Fresh but low confidence, stale but high confidence, and the winner:
	// the highest confidence *inside* the window.
	seed(t, l, clock, id, "title", "Head of Growth", 0.4, "guess/one@1", now.Add(-1*time.Hour))
	seed(t, l, clock, id, "title", "VP Marketing (old)", 0.99, "stale/source@1", now.Add(-90*24*time.Hour))
	seed(t, l, clock, id, "title", "VP Marketing", 0.9, "apollo/search@1", now.Add(-10*24*time.Hour))

	rec, err := l.Project(ctx, id, Projection{DefaultFreshness: 30 * 24 * time.Hour, AsOf: now})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	got := rec.Values["title"]
	if got.Any() != "VP Marketing" {
		t.Errorf("title = %v (source %s), want %q", got.Any(), got.Source, "VP Marketing")
	}

	// Without a window the stale high-confidence row wins.
	rec, err = l.Project(ctx, id, Projection{AsOf: now})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if v := rec.Values["title"].Any(); v != "VP Marketing (old)" {
		t.Errorf("unbounded window title = %v, want %q", v, "VP Marketing (old)")
	}
}

func TestProjectBreaksConfidenceTiesByNewest(t *testing.T) {
	ctx := context.Background()
	l, clock := openTest(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	res, err := l.UpsertIdentity(ctx, identity.Person, map[string]any{"email": "jane@acme.com"}, Provenance{})
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	id := res.Identity.ID

	seed(t, l, clock, id, "headline", "older", 0.7, "a@1", now.Add(-48*time.Hour))
	seed(t, l, clock, id, "headline", "newer", 0.7, "b@1", now.Add(-1*time.Hour))

	rec, err := l.Project(ctx, id, Projection{AsOf: now})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if v := rec.Values["headline"].Any(); v != "newer" {
		t.Errorf("headline = %v, want %q", v, "newer")
	}
}

func TestProjectPerFieldFreshnessAndFieldRestriction(t *testing.T) {
	ctx := context.Background()
	l, clock := openTest(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	res, err := l.UpsertIdentity(ctx, identity.Person, map[string]any{"email": "jane@acme.com"}, Provenance{})
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	id := res.Identity.ID

	seed(t, l, clock, id, "email", "jane@acme.com", 1.0, "csv/source@1", now.Add(-200*24*time.Hour))
	seed(t, l, clock, id, "headline", "VP Marketing", 1.0, "harvest/profile@1", now.Add(-45*24*time.Hour))

	p := Projection{
		Fields:           []string{"headline"},
		Freshness:        map[string]time.Duration{"headline": 30 * 24 * time.Hour},
		DefaultFreshness: 0,
		AsOf:             now,
	}
	rec, err := l.Project(ctx, id, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if len(rec.Values) != 0 {
		t.Errorf("values = %+v, want none (headline is stale, email not requested)", rec.Values)
	}
	if rec.Has("headline") {
		t.Error("Has must be false for a stale field")
	}

	// Widen just that field's window; email stays out of the projection.
	p.Freshness["headline"] = 60 * 24 * time.Hour
	rec, err = l.Project(ctx, id, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if !rec.Has("headline") {
		t.Fatal("headline should be in window at 60d")
	}
	if _, ok := rec.Values["email"]; ok {
		t.Error("email must not appear when Fields restricts the projection")
	}

	// An unrestricted projection sees both; email has no window.
	rec, err = l.Project(ctx, id, Projection{Freshness: map[string]time.Duration{"headline": 60 * 24 * time.Hour}, AsOf: now})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if !rec.Has("email", "headline") {
		t.Errorf("values = %+v, want both email and headline", rec.Values)
	}
	if rec.Identity.IdentityKey != "jane@acme.com" {
		t.Errorf("identity key = %q", rec.Identity.IdentityKey)
	}
}

func TestProjectUnknownIdentity(t *testing.T) {
	l, _ := openTest(t)
	if _, err := l.Project(context.Background(), "nope", Projection{}); err != ErrNotFound {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestProjectEmptyIdentityHasNoValues(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t)
	res, err := l.UpsertIdentity(ctx, identity.Person, map[string]any{"email": "jane@acme.com"}, Provenance{})
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	rec, err := l.Project(ctx, res.Identity.ID, Projection{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if len(rec.Values) != 0 {
		t.Errorf("values = %+v, want none", rec.Values)
	}
	if len(rec.Fields()) != 0 {
		t.Errorf("fields = %+v, want none", rec.Fields())
	}
}
