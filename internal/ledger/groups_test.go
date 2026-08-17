package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/elegant-atomics/gtme/internal/identity"
)

// TestGroupMembershipDerivation: current membership is the newest
// added/removed event per (group, identity) — last event wins, and touched
// events never affect membership (SPEC §3, ADR-021).
func TestGroupMembershipDerivation(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t)

	jane, err := l.UpsertIdentity(ctx, identity.Person, map[string]any{"email": "jane@acme.com"}, Provenance{})
	if err != nil {
		t.Fatal(err)
	}
	id := jane.Identity.ID

	g, err := l.EnsureGroup(ctx, "q3")
	if err != nil {
		t.Fatal(err)
	}
	again, err := l.EnsureGroup(ctx, "q3")
	if err != nil || again.ID != g.ID {
		t.Fatalf("EnsureGroup not idempotent: %v %v", again, err)
	}

	member := func(want bool, when string) {
		t.Helper()
		set, err := l.GroupMembership(ctx, g.ID)
		if err != nil {
			t.Fatal(err)
		}
		if set[id] != want {
			t.Errorf("%s: member = %v, want %v", when, set[id], want)
		}
	}

	// The clock must advance between events: derivation orders by created_at.
	now := time.Now()
	tick := func() { now = now.Add(time.Second); l.SetNow(func() time.Time { return now }) }

	tick()
	member(false, "before any event")
	if err := l.AddGroupEvent(ctx, g.ID, id, GroupAdded, nil, ""); err != nil {
		t.Fatal(err)
	}
	member(true, "after added")

	tick()
	if err := l.AddGroupEvent(ctx, g.ID, id, GroupTouched, nil, "run1"); err != nil {
		t.Fatal(err)
	}
	member(true, "after touched (never affects membership)")

	tick()
	if err := l.AddGroupEvent(ctx, g.ID, id, GroupRemoved, nil, ""); err != nil {
		t.Fatal(err)
	}
	member(false, "after removed")

	tick()
	if err := l.AddGroupEvent(ctx, g.ID, id, GroupAdded, nil, ""); err != nil {
		t.Fatal(err)
	}
	member(true, "after re-added")

	last, ok, err := l.LastTouched(ctx, g.ID, id)
	if err != nil || !ok {
		t.Fatalf("LastTouched: %v ok=%v", err, ok)
	}
	if last.IsZero() {
		t.Error("LastTouched returned a zero time")
	}

	infos, err := l.Groups(ctx)
	if err != nil || len(infos) != 1 {
		t.Fatalf("Groups: %v %v", infos, err)
	}
	gi := infos[0]
	if gi.Members != 1 || gi.Added != 2 || gi.Removed != 1 || gi.Touched != 1 {
		t.Errorf("derived character = members %d added %d removed %d touched %d",
			gi.Members, gi.Added, gi.Removed, gi.Touched)
	}

	if err := l.AddGroupEvent(ctx, g.ID, id, "promoted", nil, ""); err == nil {
		t.Error("an unknown event kind must be rejected (exactly three kinds, SPEC §3)")
	}
}

// TestIdentityIDsFromSQL: the snapshot contract — a read-only SELECT that
// yields an identity_id column (SPEC §8).
func TestIdentityIDsFromSQL(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t)
	jane, err := l.UpsertIdentity(ctx, identity.Person, map[string]any{"email": "jane@acme.com"}, Provenance{})
	if err != nil {
		t.Fatal(err)
	}

	ids, err := l.IdentityIDsFromSQL(ctx, `SELECT id AS identity_id FROM identities`)
	if err != nil || len(ids) != 1 || ids[0] != jane.Identity.ID {
		t.Fatalf("ids = %v, err %v", ids, err)
	}
	if _, err := l.IdentityIDsFromSQL(ctx, `SELECT identity_key FROM identities`); err == nil {
		t.Error("a query without an identity_id column must be rejected")
	}
	if _, err := l.IdentityIDsFromSQL(ctx, `DELETE FROM identities`); err == nil {
		t.Error("a mutating statement must be rejected (read-only)")
	}
}
