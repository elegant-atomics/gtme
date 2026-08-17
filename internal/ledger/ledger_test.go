package ledger

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/elegant-atomics/gtme/internal/identity"
)

// openTest returns a ledger in a temp dir with a controllable clock.
func openTest(t *testing.T) (*Ledger, *time.Time) {
	t.Helper()
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l, err := Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	l.SetNow(func() time.Time { return clock })
	return l, &clock
}

func TestOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested", "ledger.db")

	l, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := l.UpsertIdentity(ctx, identity.Person, map[string]any{"email": "a@b.com"}, Provenance{}); err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	l.Close()

	l2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer l2.Close()

	var n int
	if err := l2.DB().QueryRow(`SELECT count(*) FROM identities`).Scan(&n); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	if n != 1 {
		t.Errorf("identities = %d, want 1 (migrations must not re-run destructively)", n)
	}

	names, err := migrationNames()
	if err != nil {
		t.Fatalf("migrationNames: %v", err)
	}
	var applied int
	if err := l2.DB().QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied != len(names) {
		t.Errorf("schema_migrations = %d, want %d (each migration applied exactly once)", applied, len(names))
	}
}

func TestUpsertIdentityIsStableAcrossEquivalentInput(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t)

	first, err := l.UpsertIdentity(ctx, identity.Person, map[string]any{"email": "Jane@Example.com"}, Provenance{})
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	if !first.Created {
		t.Error("first upsert should create")
	}

	again, err := l.UpsertIdentity(ctx, identity.Person, map[string]any{"email": " jane@example.COM "}, Provenance{})
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	if again.Created {
		t.Error("equivalent email should not create a second identity")
	}
	if again.Identity.ID != first.Identity.ID {
		t.Errorf("id = %q, want %q", again.Identity.ID, first.Identity.ID)
	}
}

func TestUpsertIdentityUpgradesWeakKeyInPlace(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t)

	// Sourced by name only.
	weak, err := l.UpsertIdentity(ctx, identity.Person,
		map[string]any{"full_name": "Jane Doe", "company_domain": "acme.com"},
		Provenance{RunID: "run1", StepID: "source"})
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	if got := weak.Identity.IdentityKey; got[:3] != "nh:" {
		t.Fatalf("expected name-hash key, got %q", got)
	}

	// Later the same record gains a LinkedIn URL, then an email.
	slug, err := l.UpsertIdentity(ctx, identity.Person, map[string]any{
		"full_name": "Jane Doe", "company_domain": "acme.com",
		"linkedin_url": "https://www.linkedin.com/in/jane-doe/",
	}, Provenance{RunID: "run1", StepID: "enrich"})
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	if slug.Identity.ID != weak.Identity.ID {
		t.Fatalf("upgrade created a duplicate identity: %q != %q", slug.Identity.ID, weak.Identity.ID)
	}
	if !slug.Upgraded {
		t.Error("gaining a linkedin_url should upgrade the key")
	}
	if slug.Identity.IdentityKey != "in/jane-doe" {
		t.Errorf("key = %q, want %q", slug.Identity.IdentityKey, "in/jane-doe")
	}

	strong, err := l.UpsertIdentity(ctx, identity.Person, map[string]any{
		"full_name": "Jane Doe", "company_domain": "acme.com",
		"linkedin_url": "https://www.linkedin.com/in/jane-doe/",
		"email":        "jane@acme.com",
	}, Provenance{RunID: "run1", StepID: "verify"})
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	if strong.Identity.ID != weak.Identity.ID {
		t.Fatalf("second upgrade created a duplicate identity")
	}
	if strong.Identity.IdentityKey != "jane@acme.com" {
		t.Errorf("key = %q, want %q", strong.Identity.IdentityKey, "jane@acme.com")
	}

	var n int
	if err := l.DB().QueryRow(`SELECT count(*) FROM identities`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("identities = %d, want 1", n)
	}

	var events int
	if err := l.DB().QueryRow(
		`SELECT count(*) FROM step_events WHERE event = 'identity_upgraded' AND identity_id = ?`,
		weak.Identity.ID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 2 {
		t.Errorf("identity_upgraded events = %d, want 2", events)
	}

	// A weaker key alone still resolves to the same identity.
	byName, err := l.UpsertIdentity(ctx, identity.Person,
		map[string]any{"full_name": "Jane Doe", "company_domain": "acme.com"}, Provenance{})
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	if byName.Identity.ID != weak.Identity.ID {
		t.Error("the original weak key must keep resolving to the upgraded identity")
	}
	if byName.Upgraded {
		t.Error("a weaker key must not trigger an upgrade")
	}
}

func TestUpsertIdentityResolvesEveryKeyARecordHasCarried(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t)

	// A CSV row with three candidate keys; the identity is stored under the email.
	full := map[string]any{
		"email":        "jane@acme.com",
		"linkedin_url": "https://www.linkedin.com/in/jane-doe/",
		"full_name":    "Jane Doe",
	}
	first, err := l.UpsertIdentity(ctx, identity.Person, full, Provenance{RunID: "run1"})
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}

	// Each weaker key on its own must land on the same identity — this is what
	// makes the cross-run cache hit on the second run of a pipeline.
	for _, fields := range []map[string]any{
		{"linkedin_url": "linkedin.com/in/jane-doe"},
		{"full_name": "  jane   doe "},
	} {
		res, err := l.UpsertIdentity(ctx, identity.Person, fields, Provenance{RunID: "run2"})
		if err != nil {
			t.Fatalf("UpsertIdentity(%v): %v", fields, err)
		}
		if res.Identity.ID != first.Identity.ID {
			t.Errorf("%v resolved to %q, want %q", fields, res.Identity.ID, first.Identity.ID)
		}
		if res.Created {
			t.Errorf("%v created a duplicate identity", fields)
		}
	}

	// Aliases never repoint: the identity keeps its strongest key.
	got, err := l.IdentityByID(ctx, first.Identity.ID)
	if err != nil {
		t.Fatalf("IdentityByID: %v", err)
	}
	if got.IdentityKey != "jane@acme.com" {
		t.Errorf("identity_key = %q, want %q", got.IdentityKey, "jane@acme.com")
	}
	if byAlias, err := l.IdentityByKey(ctx, identity.Person, "in/jane-doe"); err != nil {
		t.Errorf("IdentityByKey(alias): %v", err)
	} else if byAlias.ID != first.Identity.ID {
		t.Errorf("IdentityByKey(alias) = %q, want %q", byAlias.ID, first.Identity.ID)
	}
}

func TestUpsertIdentityDoesNotStealAnExistingStrongKey(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t)

	// Two identities that both claim the same email once merged; v0 does not merge.
	byEmail, err := l.UpsertIdentity(ctx, identity.Person, map[string]any{"email": "jane@acme.com"}, Provenance{})
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	byName, err := l.UpsertIdentity(ctx, identity.Person, map[string]any{"full_name": "Jane Doe"}, Provenance{})
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}

	res, err := l.UpsertIdentity(ctx, identity.Person,
		map[string]any{"full_name": "Jane Doe", "email": "jane@acme.com"}, Provenance{})
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	if res.Identity.ID != byEmail.Identity.ID {
		t.Errorf("strongest matching candidate should win: got %q, want %q", res.Identity.ID, byEmail.Identity.ID)
	}
	if res.Upgraded {
		t.Error("nothing to upgrade when the strongest key already matched")
	}

	stillWeak, err := l.IdentityByID(ctx, byName.Identity.ID)
	if err != nil {
		t.Fatalf("IdentityByID: %v", err)
	}
	if stillWeak.IdentityKey == "jane@acme.com" {
		t.Error("the name-hash identity must not have taken the email key")
	}
}

func TestUpsertIdentityRejectsUnidentifiableRecord(t *testing.T) {
	l, _ := openTest(t)
	if _, err := l.UpsertIdentity(context.Background(), identity.Person,
		map[string]any{"title": "VP Marketing"}, Provenance{}); err == nil {
		t.Fatal("want error for a record with no identity key")
	}
}

func TestWriteFieldsAppendsWithProvenance(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t)

	res, err := l.UpsertIdentity(ctx, identity.Person, map[string]any{"email": "jane@acme.com"}, Provenance{RunID: "run1"})
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	id := res.Identity.ID

	n, err := l.WriteFieldMap(ctx, id, "csv/source@1", Provenance{RunID: "run1", StepID: "source"},
		map[string]any{
			"email":        "jane@acme.com",
			"headline":     "VP Marketing",
			"recent_posts": []string{"a", "b"},
			"score":        7,
			"nothing":      nil, // skipped: nothing learned
		},
		map[string]float64{"headline": 0.8})
	if err != nil {
		t.Fatalf("WriteFieldMap: %v", err)
	}
	if n != 4 {
		t.Errorf("wrote %d fields, want 4", n)
	}

	rec, err := l.Project(ctx, id, Projection{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if got := rec.Values["headline"]; got.Confidence != 0.8 || got.Source != "csv/source@1" || got.RunID != "run1" {
		t.Errorf("headline provenance = %+v", got)
	}
	if got := rec.Values["email"].Confidence; got != 1.0 {
		t.Errorf("default confidence = %v, want 1.0", got)
	}
	var posts []string
	if err := rec.Values["recent_posts"].Decode(&posts); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(posts) != 2 || posts[0] != "a" {
		t.Errorf("recent_posts = %v", posts)
	}
	if _, ok := rec.Values["nothing"]; ok {
		t.Error("nil values must not be written")
	}
	if fields := rec.Fields(); fields["score"] != float64(7) {
		t.Errorf("score = %#v, want 7", fields["score"])
	}
}

func TestRelateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t)

	person, err := l.UpsertIdentity(ctx, identity.Person, map[string]any{"email": "jane@acme.com"}, Provenance{})
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	company, err := l.UpsertIdentity(ctx, identity.Company, map[string]any{"domain": "acme.com"}, Provenance{})
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := l.Relate(ctx, person.Identity.ID, "works_at", company.Identity.ID); err != nil {
			t.Fatalf("Relate: %v", err)
		}
	}
	var n int
	if err := l.DB().QueryRow(`SELECT count(*) FROM relations`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("relations = %d, want 1", n)
	}
}

func TestIdentityLookupsMiss(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t)
	if _, err := l.IdentityByID(ctx, "nope"); err != ErrNotFound {
		t.Errorf("IdentityByID error = %v, want ErrNotFound", err)
	}
	if _, err := l.IdentityByKey(ctx, identity.Person, "nope@nope.com"); err != ErrNotFound {
		t.Errorf("IdentityByKey error = %v, want ErrNotFound", err)
	}
}
