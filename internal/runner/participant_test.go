package runner

// The in-run walk's gate (SPEC §8, ADR-049). Whether a step asks is decided
// here and nowhere else: a person at a terminal is asked, an agent never is,
// `prompt: never` opts out, and a rehearsal asks nobody because a simulated
// human step is a gap. The walk itself is covered in internal/participant;
// what this pins is the decision that reaches it.

import (
	"testing"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/planner"
)

func TestCanAskOnlyWhenThereIsSomeoneToAsk(t *testing.T) {
	human := func() *planner.Step {
		return &planner.Step{ID: "grade", Participant: adapters.KindHuman, Prompt: "tty"}
	}

	for _, tc := range []struct {
		name        string
		step        *planner.Step
		interactive bool
		simulate    bool
		want        bool
	}{
		{"a person at a terminal is asked", human(), true, false, true},
		{"no terminal, so the records wait", human(), false, false, false},
		{"prompt: never opts out of asking",
			&planner.Step{ID: "grade", Participant: adapters.KindHuman, Prompt: "never"}, true, false, false},
		{"an agent is never prompted",
			&planner.Step{ID: "grade", Participant: adapters.KindAgent, Prompt: "tty"}, true, false, false},
		{"a rehearsal asks nobody", human(), true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &runner{interactive: tc.interactive, simulate: tc.simulate}
			if got := r.canAsk(tc.step); got != tc.want {
				t.Errorf("canAsk = %v, want %v", got, tc.want)
			}
		})
	}
}
