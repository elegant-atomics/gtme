package ulid

import (
	"sort"
	"strings"
	"testing"
	"time"
)

func TestNewShapeAndAlphabet(t *testing.T) {
	id := New()
	if len(id) != 26 {
		t.Fatalf("len(%q) = %d, want 26", id, len(id))
	}
	for _, r := range id {
		if !strings.ContainsRune(encoding, r) {
			t.Fatalf("id %q contains %q, outside the Crockford alphabet", id, r)
		}
	}
}

func TestNewIsUniqueAndMonotonic(t *testing.T) {
	const n = 5000
	ids := make([]string, n)
	seen := map[string]bool{}
	for i := range ids {
		ids[i] = New()
		if seen[ids[i]] {
			t.Fatalf("duplicate id %q at %d", ids[i], i)
		}
		seen[ids[i]] = true
	}
	if !sort.SliceIsSorted(ids, func(a, b int) bool { return ids[a] < ids[b] }) {
		t.Error("ids minted in sequence must sort in creation order")
	}
}

func TestTimestampOrderingAcrossMilliseconds(t *testing.T) {
	early := newAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	late := newAt(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if !(early < late) {
		t.Errorf("%q should sort before %q", early, late)
	}
}

func TestClockGoingBackwardsKeepsOrder(t *testing.T) {
	forward := newAt(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	backward := newAt(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	if !(forward < backward) {
		t.Errorf("a backwards clock must not break ordering: %q, %q", forward, backward)
	}
}
