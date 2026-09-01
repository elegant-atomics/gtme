package ledger

import (
	"context"
	"testing"
)

// ADR-046: every cost row carries its basis, and a run's totals keep measured
// and estimated dollars apart.
func TestCostsCarryTheirBasis(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t)
	run, err := l.CreateRun(ctx, "p", nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := l.RecordCost(ctx, run.ID, "enrich", "", "vendor", 0.25, BasisMeasured, nil); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordCost(ctx, run.ID, "enrich", "", "vendor", 0.5, BasisEstimated, nil); err != nil {
		t.Fatal(err)
	}
	// An unlabeled amount is a guess until proven otherwise.
	if err := l.RecordCost(ctx, run.ID, "judge", "", "anthropic", 0.01, "", nil); err != nil {
		t.Fatal(err)
	}

	bases := map[string]int{}
	rows, err := l.DB().Query(`SELECT basis, count(*) FROM costs WHERE run_id = ? GROUP BY basis`, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var basis string
		var n int
		if err := rows.Scan(&basis, &n); err != nil {
			t.Fatal(err)
		}
		bases[basis] = n
	}
	if bases[BasisMeasured] != 1 || bases[BasisEstimated] != 2 {
		t.Errorf("basis counts = %v, want measured:1 estimated:2", bases)
	}

	totals, err := l.CostsByStep(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := totals["enrich"]; got.Measured != 0.25 || got.Estimated != 0.5 || got.Estimates != 1 {
		t.Errorf("enrich totals = %+v, want measured 0.25 estimated 0.5 (1 row)", got)
	}
	if got := totals["judge"]; got.Measured != 0 || got.Estimated != 0.01 || got.Estimates != 1 {
		t.Errorf("judge totals = %+v, want estimated 0.01 (1 row)", got)
	}
	if got := totals["enrich"].Total(); got != 0.75 {
		t.Errorf("enrich total = %v, want 0.75", got)
	}
}
