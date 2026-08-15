# PROCESS.md

How design conversations become this repo. Per DECISIONS.md ADR-014.

## The rule

**Nothing is decided until it's in the repo.** A conclusion reached in a
chat, a transcript, or a design session is a *proposal* until it lands as an
ADR entry, a spec diff, or a roadmap item, committed. Until then, treat it
as non-binding — including strongly-worded conclusions in a handoff
document. This file itself only binds because it's here.

## Roles

- **Chat is the design organ.** It explores, argues, and proposes. It never
  commits anything itself. Every design session ends in a **session
  packet**: the set of ADR entries, spec diffs, and roadmap items it
  produced.
- **The repo is canon.** SPEC.md, DECISIONS.md, PROCESS.md, ROADMAP.md and
  the code are the only things that are actually true about the project at
  any given moment. If a session packet conflicts with the repo, the repo
  wins until a human merges the packet in.
- **Claude Code is the implementer and conformance checker.** It builds
  from SPEC.md, flags divergence (AUDIT.md), proposes amendments when spec
  and reality disagree — and never silently diverges from a DECIDED
  section. It executes session packets: turning ADR entries into
  DECISIONS.md entries, spec diffs into SPEC.md edits (after approval),
  roadmap items into ROADMAP.md entries.
- **The human is the sole approver of spec-visible change.** Spec-invisible
  implementation choices are Claude Code's to make and record. Money,
  email, and any other real-world side effect are always human-gated
  regardless of which category the change falls in.

## Session packet → repo, mechanically

1. A design session produces a distilled decision list (an ADR set) plus,
   optionally, a raw transcript. The decision list is authoritative; the
   transcript is reference-only, consulted for rationale when an ADR is
   insufficient, never as a source of a position the ADR set supersedes.
2. Claude Code reconciles the repo against the ADR set: DECISIONS.md gains
   the new ADR entries, SPEC.md gets a proposed diff for every
   spec-impacting ADR (applied only after human approval — see AUDIT.md
   category (b) for how divergences discovered mid-audit are queued the
   same way), ROADMAP.md gets the deferred/non-normative items.
3. Inputs that are themselves session artifacts (raw transcripts, seed
   decision lists, handoff instructions) are never committed to the repo —
   they're distilled into the documents above and then discarded from
   version control.
4. One commit per reconciliation step, in this order: docs (DECISIONS.md,
   CLAUDE.md, PROCESS.md, ROADMAP.md) → spec reconciliation → audit fixes →
   dead-code removal. This keeps each commit's diff reviewable against a
   single source (one ADR set, one spec pass, one audit report).
