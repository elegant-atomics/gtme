package ledger

import (
	"context"
	"testing"
)

// ADR-048: a value written about another value records that value's id as
// its referent, and the projection reads both back.
func TestReferentIsRecordedAndProjected(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t)
	res, err := l.UpsertIdentity(ctx, "person", map[string]any{"email": "jane@acme.com"}, Provenance{})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Identity.ID
	if _, err := l.WriteFieldMap(ctx, id, "ai/compose@1", Provenance{}, map[string]any{"first_line": "Hi Jane"}, nil); err != nil {
		t.Fatal(err)
	}
	rec, err := l.Project(ctx, id, Projection{})
	if err != nil {
		t.Fatal(err)
	}
	draft := rec.Values["first_line"]
	if draft.ID == "" || draft.Referent != "" {
		t.Fatalf("draft = %+v, want an id and no referent", draft)
	}
	if _, err := l.WriteFieldMapAbout(ctx, id, "human/review @ trevor#abc", Provenance{}, map[string]any{"review.grade": "B"}, nil, draft.ID); err != nil {
		t.Fatal(err)
	}
	rec, err = l.Project(ctx, id, Projection{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Values["review.grade"].Referent; got != draft.ID {
		t.Errorf("grade referent = %q, want %q", got, draft.ID)
	}
}

// ADR-049: answers are append-only and the latest per record wins.
func TestAnswersLatestWins(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t)
	run, err := l.CreateRun(ctx, "review", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	cost := 0.01
	if err := l.RecordAnswer(ctx, run.ID, "grade", "id-1", Answer{Fields: map[string]any{"grade": "C"}, Participant: "human/trevor", Token: run.ID + "/grade"}); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordAnswer(ctx, run.ID, "grade", "id-1", Answer{Fields: map[string]any{"grade": "B"}, Participant: "agent/claude-code", Note: "second look", Cost: &cost, Measured: true}); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordAnswer(ctx, run.ID, "grade", "id-2", Answer{Fields: map[string]any{"grade": "A"}, Participant: "human/trevor"}); err != nil {
		t.Fatal(err)
	}
	answers, err := l.Answers(ctx, run.ID, "grade")
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 2 {
		t.Fatalf("answers = %d, want 2", len(answers))
	}
	a := answers["id-1"]
	if a.Fields["grade"] != "B" || a.Participant != "agent/claude-code" || a.Note != "second look" || a.Cost == nil || *a.Cost != 0.01 || !a.Measured {
		t.Errorf("latest answer = %+v, want the agent's B with note and measured cost", a)
	}
	if n := countEvents(t, l, run.ID, EventAnswered); n != 3 {
		t.Errorf("answered events = %d, want 3 (append-only)", n)
	}
	if err := l.RecordAnswer(ctx, run.ID, "grade", "", Answer{}); err == nil {
		t.Error("an answer with no record must be refused")
	}
}

func countEvents(t *testing.T, l *Ledger, runID, event string) int {
	t.Helper()
	var n int
	if err := l.DB().QueryRow(`SELECT count(*) FROM step_events WHERE run_id = ? AND event = ?`, runID, event).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
