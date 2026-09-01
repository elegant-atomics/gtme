package runner

import (
	"bytes"
	"strings"
	"testing"

	"github.com/elegant-atomics/gtme/internal/ledger"
)

// ADR-046: the receipt's total carries its basis — bare when every dollar
// was measured, `(estimated)` when every dollar was arithmetic, and split
// when a run mixed the two. A run that spent nothing and estimated nothing
// prints bare.
func TestReceiptTotalCarriesBasis(t *testing.T) {
	cases := []struct {
		name string
		cost ledger.CostTotal
		want string
	}{
		{"measured", ledger.CostTotal{Measured: 0.5}, "total: $0.5000 spent"},
		{"estimated", ledger.CostTotal{Estimated: 0.5, Estimates: 2}, "total: $0.5000 (estimated) spent"},
		{"mixed", ledger.CostTotal{Measured: 0.25, Estimated: 0.5, Estimates: 1}, "total: $0.7500 ($0.2500 measured + $0.5000 estimated) spent"},
		{"nothing", ledger.CostTotal{}, "total: $0 spent"},
		// An unset rate ran at $0 and said so: the guess is visible.
		{"zero estimate", ledger.CostTotal{Estimates: 3}, "total: $0 (estimated) spent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			PrintReceipt(&buf, &Result{RunID: "r", Status: "ok", Steps: []StepStat{{ID: "s", Use: "x", Cost: tc.cost}}})
			last := ""
			for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				last = line
			}
			if last != tc.want {
				t.Errorf("total line = %q, want %q", last, tc.want)
			}
		})
	}
}

func TestFormatCost(t *testing.T) {
	if got := FormatCost(ledger.CostTotal{Measured: 0.01, Estimated: 0.02, Estimates: 1}); got != "$0.0300 ($0.0100 measured + $0.0200 estimated)" {
		t.Errorf("FormatCost mixed = %q", got)
	}
	if got := FormatCost(ledger.CostTotal{Estimated: 0.02, Estimates: 1}); got != "$0.0200 (estimated)" {
		t.Errorf("FormatCost estimated = %q", got)
	}
	if got := FormatCost(ledger.CostTotal{Measured: 0.02}); got != "$0.0200" {
		t.Errorf("FormatCost measured = %q", got)
	}
}
