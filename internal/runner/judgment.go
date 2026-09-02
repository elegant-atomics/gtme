package runner

// The judgment cache (SPEC §7, ADR-039): a participant step's answer is
// reused when the question and the facts are unchanged. The question is the
// judgment signature — adapter, model, operator prompt, output shape, uses:,
// of: for an AI step; adapter, render:, output shape, uses:, of: for a
// human/agent step (never the participant's name: the cache is checked at
// dispatch, before anyone has answered, ADR-049) — and the facts are the
// input hash over the fields the judgment reads, the referent's value
// included. Both are recorded on the `done` event that carries the
// judgment, and the signature rides in provenance (`ai/<op> @
// <model>#<signature>`, `human/<op> @ <name>#<signature>`), so the ledger
// can tell two prompts' outputs apart.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/elegant-atomics/gtme/internal/ai"
	"github.com/elegant-atomics/gtme/internal/planner"
)

// signatureLen is the hex prefix length of a signature or input hash — the
// same twelve characters SQL provenance uses (SPEC §10a).
const signatureLen = 12

// judgmentSignature hashes the question a participant step asks. For an AI
// step the model is the one the ARMED run would use (credentials only —
// never the simulate override), so a rehearsal skips exactly what an armed
// run would skip. For a human/agent step it is the step declaration alone.
func (r *runner) judgmentSignature(st *planner.Step) string {
	if !isParticipant(st) {
		return ""
	}
	r.mu.Lock()
	if sig, ok := r.signatures[st.ID]; ok {
		r.mu.Unlock()
		return sig
	}
	r.mu.Unlock()

	var uses []string
	if !st.NeedsAll {
		uses = append([]string(nil), st.Needs...)
		sort.Strings(uses)
	}
	question := map[string]any{
		"adapter": st.Manifest.ID,
		"shape":   json.RawMessage(st.ProvidesSchema),
		"uses":    uses,
	}
	if st.Of != "" {
		question["of"] = st.Of
	}
	if isAIStep(st) {
		model, _ := st.Config["model"].(string)
		prompt, _ := st.Config["prompt"].(string)
		question["model"] = ai.ProvenanceModel(model, func(k string) string { return st.Credentials[k] })
		question["prompt"] = strings.TrimSpace(prompt)
	} else {
		question["render"] = map[string]any{"fields": st.RenderFields, "template": st.RenderTemplate}
	}
	sig := digest(question)
	r.mu.Lock()
	r.signatures[st.ID] = sig
	r.mu.Unlock()
	return sig
}

// inputHash hashes the facts a judgment reads: the uses: fields when
// declared, else the projection minus the step's own provides and every
// field namespaced by this pipeline — so a needs-all step never sees its
// own last answer as a changed input (ADR-039). The referent's value is
// always in (ADR-048): a rewritten draft is re-reviewed, an unchanged one
// is not.
func inputHash(st *planner.Step, pipeline string, fields map[string]any) string {
	subset := map[string]any{}
	if !st.NeedsAll {
		for _, name := range st.Needs {
			if v, ok := fields[name]; ok {
				subset[name] = v
			}
		}
	} else {
		own := map[string]bool{}
		for _, p := range st.Provides {
			own[p] = true
		}
		for name, v := range fields {
			if own[name] || strings.HasPrefix(name, pipeline+".") {
				continue
			}
			subset[name] = v
		}
	}
	if st.Of != "" {
		if v, ok := fields[st.Of]; ok {
			subset[st.Of] = v
		}
	}
	return digest(subset)
}

// digest is the canonical hash: JSON with sorted keys, sha256, twelve hex.
// encoding/json sorts map keys, which is the canonical form we need.
func digest(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:signatureLen]
}

// judgmentDetail adds the cache keys to a step event's detail.
func (it *item) judgmentDetail(detail map[string]any) map[string]any {
	if it.signature == "" {
		return detail
	}
	out := make(map[string]any, len(detail)+2)
	for k, v := range detail {
		out[k] = v
	}
	out["signature"] = it.signature
	out["input"] = it.input
	return out
}
