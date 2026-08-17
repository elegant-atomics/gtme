# CLAUDE.md

`gtme` is built from **SPEC.md**. It is canonical for all observable behavior
(CLI surface, exit codes, schemas, wire protocol, ledger DDL, acceptance
criteria). Sections marked **DECIDED** are constraints, not suggestions.
Read it fully before any change. Code that diverges from spec is a bug even
if it works.

**Spec-visible change** (anything a second clean-room implementation would
need to interoperate — see DECISIONS.md ADR-010's litmus): STOP. Propose a
spec diff for human approval. Do not write code against your own proposal
before it's approved.

**Spec-invisible change** (internals: packages, naming, file layout, test
structure, an underspecified detail SPEC.md §12 hands you): decide and keep
building. Record it in `DECISIONS.md` — date, question, choice, why.

**Stop and ask, always, no exceptions:**
- Anything that spends real money or sends a real email/message. M6 live
  runs and the validation campaign are human-gated.
- An external API's real shape contradicting the spec in a way that changes
  a contract or the ledger schema.
- A DECIDED section that appears internally inconsistent.
- A dependency beyond §2's list, without first recording why in
  `DECISIONS.md`.

Run `make check` (fmt, vet, test) before considering any milestone done.

Pointers: `SPEC.md` (what's true, normative) · `DECISIONS.md` (why, ADRs +
implementation decisions) · `PROCESS.md` (how chat and repo relate) ·
`ROADMAP.md` (non-normative, not yet decided) · `AUDIT.md` (current
code/spec divergence backlog) · `VALIDATION.md` (the campaign, human-gated).
