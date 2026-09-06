package registry

// The adapter–type contract (SPEC §4a, ADR-054): for every manifest naming
// an entity_type, three checks with no network and no spend, run by `gtme
// plan` and `gtme adapters verify` alike — (a) the name resolves to exactly
// one type file; (b) every static provides property is canonical for that
// type or vendor-namespaced; (c) a source or a traverse covers at least one
// key tier, so every emitted record can be keyed (#27, generalized).

import (
	"fmt"
	"strings"

	"github.com/elegant-atomics/gtme/internal/adapters"
)

// ContractProblem is one failed check, worded for the operator.
type ContractProblem struct {
	Check string // "type" | "provides" | "key"
	Msg   string
}

func (p ContractProblem) Error() string { return p.Msg }

// Contract runs the three checks for a manifest against entityType (its
// resolved type — a config-specific type wins over the static one) and the
// provides names it will emit. An entity-agnostic manifest ("*") has no type
// to check until a pipeline gives it one; with entityType "" nothing is
// checked. Weak reports a source whose only coverage is the hash fallback,
// which is a note, not a problem (SPEC §7).
func (r *Registry) Contract(m *adapters.Manifest, entityType string, provides []string, wildcard bool) (problems []ContractProblem, weak bool) {
	if entityType == "" || entityType == adapters.EntityAny {
		return nil, false
	}
	t, err := r.Resolve(entityType)
	if err != nil {
		return []ContractProblem{{Check: "type", Msg: err.Error()}}, false
	}
	for _, name := range provides {
		if err := r.ValidateName(entityType, name); err != nil {
			problems = append(problems, ContractProblem{Check: "provides", Msg: "provides: " + err.Error()})
		}
	}
	if m.Role != adapters.RoleSource && m.Role != adapters.RoleTraverse {
		return problems, false
	}
	if wildcard || len(provides) == 0 {
		// An open provides schema (a probed source before its config is known)
		// is judged when the shape is: the planner runs this with the probed
		// names and the probed shape's openness.
		return problems, false
	}
	strong, weakOnly := r.Coverage(entityType, provides)
	if !strong && !weakOnly {
		problems = append(problems, ContractProblem{Check: "key", Msg: fmt.Sprintf(
			"no identity-key path: none of the fields it provides (%s) can key a %s (SPEC §4 tiers: %s) — a %s that emits unkeyable records is billed and yields nothing",
			strings.Join(provides, ", "), entityType, strings.Join(t.TierNames(), "; "), m.Role)})
	}
	return problems, !strong && weakOnly
}
