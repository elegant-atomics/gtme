package ledger

import (
	"context"
	"testing"

	"github.com/elegant-atomics/gtme/internal/identity"
)

// TestPipelineRecordsReadTerminality: every run record of a named pipeline
// comes back with the final step its run's snapshot declared — what a
// `once:` group source judges "finished" against (SPEC §8, ADR-052). Runs
// of other pipelines are not part of the answer; a snapshot without steps
// finishes at 'sourced'.
func TestPipelineRecordsReadTerminality(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t)

	ids := map[string]string{}
	for _, email := range []string{"x@acme.com", "y@acme.com", "z@acme.com"} {
		res, err := l.UpsertIdentity(ctx, identity.Person, map[string]any{"email": email}, Provenance{})
		if err != nil {
			t.Fatal(err)
		}
		ids[email] = res.Identity.ID
	}

	type step struct {
		ID string `json:"id"`
	}
	snapshot := map[string]any{"name": "drain", "steps": []step{{ID: "gate"}, {ID: "send"}}}
	a, err := l.CreateRun(ctx, "drain", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(l.AddRunRecord(ctx, a.ID, ids["x@acme.com"], StateSourced))
	must(l.SetRunRecordState(ctx, a.ID, ids["x@acme.com"], "send"))
	must(l.AddRunRecord(ctx, a.ID, ids["y@acme.com"], StateSourced))
	must(l.SetVerdict(ctx, a.ID, ids["y@acme.com"], "gate", false))
	must(l.AddRunRecord(ctx, a.ID, ids["z@acme.com"], StateSourced))
	must(l.FinishRun(ctx, a.ID, StatusDone))

	// Another pipeline's run over the same people is not this pipeline's history.
	other, err := l.CreateRun(ctx, "elsewhere", snapshot)
	must(err)
	must(l.AddRunRecord(ctx, other.ID, ids["x@acme.com"], StateSourced))

	// A source-only run finishes at 'sourced'.
	b, err := l.CreateRun(ctx, "drain", map[string]any{"name": "drain"})
	must(err)
	must(l.AddRunRecord(ctx, b.ID, ids["z@acme.com"], StateSourced))

	recs, err := l.PipelineRecords(ctx, "drain")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 4 {
		t.Fatalf("got %d records, want 4: %+v", len(recs), recs)
	}
	byKey := map[string]PipelineRecord{}
	for _, r := range recs {
		byKey[r.RunID+"/"+r.IdentityID] = r
	}
	x := byKey[a.ID+"/"+ids["x@acme.com"]]
	if x.FinalStep != "send" || x.State != "send" || x.RunStatus != StatusDone {
		t.Errorf("x = %+v, want final step send, state send, run done", x)
	}
	y := byKey[a.ID+"/"+ids["y@acme.com"]]
	if y.Verdicts["gate"] != "fail" || y.State != StateSourced {
		t.Errorf("y = %+v, want a gate fail verdict at sourced", y)
	}
	z := byKey[b.ID+"/"+ids["z@acme.com"]]
	if z.FinalStep != StateSourced || z.RunStatus != StatusRunning {
		t.Errorf("z in the source-only run = %+v, want final step 'sourced', run running", z)
	}
	if _, ok := byKey[other.ID+"/"+ids["x@acme.com"]]; ok {
		t.Error("another pipeline's run leaked into the answer")
	}
}
