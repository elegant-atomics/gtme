package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/elegant-atomics/gtme/internal/identity"
)

// TestPayloadEviction: eviction removes exactly the expired — TTL-less
// payloads and facts stay (SPEC §3/§8, ADR-030).
func TestPayloadEviction(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t)
	jane, err := l.UpsertIdentity(ctx, identity.Person, map[string]any{"email": "jane@acme.com"}, Provenance{})
	if err != nil {
		t.Fatal(err)
	}
	id := jane.Identity.ID

	now := time.Now()
	l.SetNow(func() time.Time { return now })
	if err := l.WritePayload(ctx, id, "harvest/profile", "run1", "application/json", `{"a":1}`, 30); err != nil {
		t.Fatal(err)
	}
	if err := l.WritePayload(ctx, id, "http/enrich", "run1", "text/html", `<p>hi</p>`, 0); err != nil {
		t.Fatal(err) // ttl 0 = no expiry
	}

	// Nothing expires yet.
	if n, err := l.PurgeExpiredPayloads(ctx); err != nil || n != 0 {
		t.Fatalf("early purge = %d, %v", n, err)
	}

	// 31 days later the 30-day payload goes; the no-expiry one stays.
	l.SetNow(func() time.Time { return now.Add(31 * 24 * time.Hour) })
	n, err := l.PurgeExpiredPayloads(ctx)
	if err != nil || n != 1 {
		t.Fatalf("purge = %d, %v (want 1)", n, err)
	}
	left, err := l.PayloadCount(ctx, id, "")
	if err != nil || left != 1 {
		t.Fatalf("remaining payloads = %d, %v (want 1)", left, err)
	}
	if left, _ := l.PayloadCount(ctx, id, "http/enrich"); left != 1 {
		t.Errorf("the no-expiry payload should survive")
	}
	// The fact layer untouched — eviction deletes payloads and nothing else.
	var idents int
	if err := l.DB().QueryRow(`SELECT count(*) FROM identities`).Scan(&idents); err != nil || idents != 1 {
		t.Errorf("identity layer disturbed: %d, %v", idents, err)
	}
}
