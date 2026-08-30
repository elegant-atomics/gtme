# AUDIT.md — divergence audit, code vs. reconciled SPEC.md

Per HANDOFF.md Phase 3, run once against SPEC.md v0.2 (the Phase 2
reconciliation commit `aca71e4`). Every divergence found is classified (a)
code bug → fixed, (b) spec gap where the code was right → queued as a
proposed spec diff, or (c) dead code from a culled feature → deleted. This
report is the build backlog: `make check` is green as of the commits below,
but (b) and the deferred item under (a) remain open.

Commits this audit produced: `791f5bb` (category a), `b99f226` (category c).
`make check` (fmt, vet, test) is green after both.

---

## (a) Code bugs — fixed

1. **`current_fields` view didn't exist.** SPEC §3 (ADR-003) requires the
   current-value resolution rule to live in exactly one place, a SQL view;
   `internal/ledger/project.go` implemented it in Go instead, and `gtme
   query`'s own §8 examples referencing `current_fields` would have failed
   against a real ledger. Fixed: migration `0004_current_fields_view.sql`
   adds `field_value_ranks` (the ranking) and `current_fields` (rank 1,
   unwindowed); `project.go` reads the ranked view and applies its
   per-step freshness window in Go, since a window can't live in an
   unparameterized view. Discovered mid-fix: my first version of the view
   collapsed straight to one winning row per field, which silently broke
   the existing "fall through a stale top-ranked row to the next-best
   fresh one" behavior (`TestProjectPicksHighestConfidenceInWindow`
   caught it). SPEC.md and `spec/ledger.sql` were corrected in the same
   commit — see that commit's message for detail. Never shipped as a
   separate spec-visible change; it's a same-session fix to Phase 2's own
   output.

2. **`uses:` (ADR-004) was entirely unimplemented.** No `Uses` field on
   `pipeline.Step`, no planner handling — SPEC §9's own example pipeline
   didn't parse. Fixed: `pipeline.Step.Uses []string`; `planner.ResolveStep`
   treats a non-empty `Uses` as the step's `Needs`/`Required` (exactly like
   `needs.required`), overriding the needs-all wildcard, and rejects `uses:`
   on any step whose role isn't `filter`/`compose`. Caught along the way:
   SPEC §9's own example and `examples/apollo-to-instantly.yaml` both used
   `uses: [name, ...]`, but `apollo/search`'s real manifest provides
   `full_name`, not `name` — `TestHelpAgentExamplesPassPlan` (which actually
   runs `gtme plan` against generated examples, not just schema-validates
   them) caught this; both files corrected.

3. **`gtme show` (ADR-006) didn't exist.** Not wired into `cli.go`'s verb
   switch at all. Fixed: `internal/cli/show.go`, plus
   `ledger.FindByKey` (entity-type-agnostic identity lookup, since `gtme
   show <key>` takes no `entity_type` argument per SPEC §8).

4. **`gtme help --agent` (ADR-007) didn't exist.** Same situation. Fixed:
   `internal/cli/help_agent.go`, plus `adapters.Installed()` (built-ins +
   anything on the adapter search path, so the doc never drifts from what
   `gtme plan` can actually resolve). Its round-trip acceptance criterion
   (SPEC §8 — an agent given only this doc can write a pipeline that
   passes `gtme plan`) is asserted directly by
   `TestHelpAgentExamplesPassPlan`, which is what caught bug #2's example
   error above.

5. **`protocol.Message.AmountUSD` used to drop an explicit $0 COST.** A
   bare `float64` with `json:",omitempty"` is indistinguishable from "no
   amount sent" when the amount is exactly 0 — and SPEC §5 explicitly
   requires 0 to be a valid, sent COST (a free or unpriced call). Found by
   inspection while implementing #2 (both touch `internal/protocol`), not
   by a failing test — no existing test exercised the zero-cost path on
   the wire. Fixed: `AmountUSD` is now `*float64`, the same pattern already
   used for `Pass`, with an `Amount()` accessor. Not spec-visible (the
   observable COST message shape is unchanged; only a stored-zero's
   presence on the wire is corrected) — recorded here rather than as an
   ADR because it isn't a design decision, it's a bug.

## (c) Dead code from culled features — deleted

ADR-005 cuts pipe mode from v0 entirely; SPEC.md v0.2 no longer describes
it. Deleted along with it:

- `internal/runner/pipe.go`, `internal/cli/pipe.go` (whole files): the
  RUN-handshake protocol extension, `PipeOptions`/`RunPipe`,
  `checkIncoming`/`mergeSchema` contract-checking against an incoming
  stream, and the `gtme source|filter|enrich|compose|deliver` CLI
  subcommands.
- `test/e2e/pipe_test.go` (whole file): its acceptance test no longer
  applies. Its shared helpers (`writeAdapter`, `echoAdapterScript`,
  `needsLinkedInManifest`, `nonEmptyLines`) were reused by other test
  files and moved to `harness_test.go` rather than disappearing.
- `ledger.AppendRunConfigStep` and `ledger.SetRunConfig`: zero remaining
  callers — both existed only for pipe mode's incremental, concurrent
  config-snapshot accumulation. `gtme run` writes the whole resolved
  `Pipeline` atomically at `CreateRun` time and always has.
- `freeze.go`'s `frozenPipeline` no longer reorders steps by `StepIDs`
  (and the now-unused `stableSortByRank` is deleted): that reordering
  existed only because pipe-mode processes could append their step to a
  run's config concurrently and out of order. A `gtme run`-produced
  config snapshot is already in execution order (SPEC §9: steps execute
  strictly in order), so the reorder was provably a no-op on every
  remaining code path.
- Comments in `internal/ledger/ledger.go`, `runs.go`, and
  `internal/planner/planner.go` that justified behavior (WAL/`_txlock`
  choice, tx retry, `ResolveStep`'s reuse) by citing pipe mode
  specifically — reworded to the reasoning that still holds (concurrent
  `gtme` invocations against one ledger file remain possible without pipe
  mode) or removed where nothing still depends on the code being
  described.
- `README.md`: the shell-pipe quickstart example and "Two modes" section
  described a surface that no longer exists; removed, `gtme show`/`gtme
  help --agent` added to the commands table in their place.

## (b) Spec gaps where the code was right — approved and applied

Both items below were proposed here, approved by the human, and applied to
SPEC.md (§5, §6) in a follow-up commit after this report was first written.
Left in place as the record of what was found and why.

1. **Two manifest fields §6 doesn't name: `credentials_optional` and
   `batch`.** Both are load-bearing — `ai/filter`/`ai/compose` depend on
   `credentials_optional` (an AI step on the `claude-code` engine needs no
   API key; DECISIONS.md's 2026-08-13 "Optional credentials" entry
   originally judged this spec-invisible, but under ADR-010's own litmus —
   would a second clean-room implementation need this to interoperate? —
   a manifest author plainly would). `batch` marks an adapter the runner
   must feed in `batch_size`-sized invocations rather than the normal
   worker pool (SPEC §9 describes the *behavior* — "AI steps which
   process in batches" — without naming the manifest field that triggers
   it).
   **Applied:** both fields added to §6's manifest shape and prose;
   `spec/schemas/manifest.schema.json`'s descriptions un-flagged to match.

2. **§5's RECORD examples read as if `key` were always present**, but a
   source's outbound RECORD legitimately carries none (the runner
   canonicalizes the identity from `fields`, per §4) — `csv/source` and
   `apollo/search` both do this. Not a code bug (§4 already establishes
   the rule; the runner already implements it correctly, and
   `spec/schemas/msg-record-in.schema.json` vs. `msg-record-out.schema.json`
   already encode the asymmetry), just an editorial gap in §5's worked
   examples.
   **Applied:** §5 gained a one-line note that a source's RECORD `key` is
   OPTIONAL (matching `msg-record-out.schema.json`) plus a worked keyless
   example.

## (b) Spec gaps queued from the M14 step 1 build (ADR-033) — proposed, not applied

Found while building declared AI provides (DECISIONS.md, 2026-08-28 "M14
step 1 internals"). Both are spec-visible under ADR-010's litmus — a
second implementation would need the answer to interoperate — so the code
takes the conservative reading and the question is queued here.

1. **How does a step's config "map a name to a canonical field"?** §4a
   tier 3 and §7 both said declared AI outputs default to
   `<pipeline>.<field>` "unless the step's config maps a name to a
   canonical field", but neither §9 nor `spec/schemas/pipeline.schema.json`
   defined a mapping form — a `provides:` value could carry only `type`
   and `enum`. Name-matching was rejected (`state` is a canonical person
   field; a judgment must not land in a location).
   **Applied (approved 2026-08-28, SPEC v0.16):** `canonical: true` on the
   declaration — §7 states the rule (the name must be canonical for the
   pipeline's entity type; declared `type`/`enum` must agree with the
   registry), §9 lists the keyword, §4a points at it, the pipeline schema
   constrains a map value to null or `{type, enum, canonical}`. Every
   other bare name namespaces, and `gtme plan` notes a coincidence with a
   canonical field and names the opt-in.

2. **How does a manifest declare that it is entity-agnostic?** §10.3
   (items 3 and 5) said "the manifest is entity-agnostic: the step's
   entity type is the pipeline's", but §6 and
   `spec/schemas/manifest.schema.json` required `entity_type` (min length
   1) described as `'person' | 'company' (extensible)`, and the build
   initially applied the §10.3 behaviour by a planner rule keyed on the
   `ai/` id prefix with the two AI manifests still saying `person`.
   **Applied (approved 2026-08-28, SPEC v0.16):** `"entity_type": "*"` —
   §6 states the rule (steps take the pipeline's entity type; static
   schemas validate against it at plan; a source may not declare it), the
   manifest schema describes the sentinel, §10.3 points at §6, the AI
   manifests declare it, and the planner keys on the declaration rather
   than the id prefix — so external adapters can opt in.

## (b) Spec gap queued from the agent round-trip (2026-08-29) — approved and applied

3. **§8's `help --agent` contents omit the binding surface.** The doc
   MUST contain verbs, manifests, three examples, and the ledger read
   surface — nothing about bindings: that they exist, the
   `~/.gtme/adapters/<name>/binding.yaml` discovery path, or the
   `spec/binding-schema.json` contract. A capable agent (VALIDATION.md,
   2026-08-29) reached for `strings` on the binary to find it. The code is
   faithful to §8; §8 is short.
   **Proposed diff:** §8 gains a second agent surface, `gtme help
   --bindings` — the binding schema, the discovery path, and one reference
   binding as a worked example — kept separate from `help --agent` so the
   common pipeline doc does not carry a contract only adapter authors need;
   `help --agent` gains one sentence pointing at it. The verb table in §8
   lists both.
   **Applied (approved 2026-08-30, SPEC v0.23):** ADR-041 carries the
   diff — §8's verb table lists both surfaces and `help --agent` MUST
   point at `help --bindings`; the build is M18.

## Deferred (a) item — flagged, not executed: `webhook/source`

SPEC §10 (item 8, added by Phase 2's ADR-009 reconciliation) documents a
`webhook/source` adapter that does not exist in code at all — no
`internal/adapters/webhooksource/` package, no manifest, no fixtures. This
is a genuine category-(a) gap (the spec now says something the code
doesn't do), but building it — a new adapter with its own manifest, spool-
consumption semantics, fixtures, and unit tests, per SPEC §10's own
per-adapter requirements — is real *build* scope (SPEC §11 places it in
M5, "real adapters"), not a reconciliation-pass fix. Per DECISIONS.md
ADR-016, the priority after reconciliation is the validation campaign, not
completing every v0 adapter. Flagged here as the first item of the build
backlog rather than implemented inline; `internal/cli/help_agent.go`'s
canonical examples were written to avoid referencing it precisely because
an unimplemented adapter in a "this must pass `gtme plan`" example would be
self-contradicting (see `TestHelpAgentExamplesPassPlan`).

## Reviewed, not a divergence

Three ledger tables SPEC §3 doesn't name (`schema_migrations`,
`identity_aliases`, `saved_queries`) and the manifest-schema tightening on
a probed CSV header are all covered by existing DECISIONS.md entries that
judge them spec-invisible under ADR-010's litmus, and `test/conformance`'s
schema check allowlists them explicitly with that reasoning inline —
re-confirmed here, not re-litigated.

## (b) Spec gap queued from Campaign 1 (2026-08-30) — approved and applied (ADR-043, SPEC v0.26)

4. **Apollo's people search no longer serves API callers the §10.2 shape.**
   Observed live (VALIDATION.md, 2026-08-30): the endpoint the reference
   binding calls, `POST /api/v1/mixed_people/search`, returns HTTP 422
   `SEARCH.ROUTING.LEGACY_PEOPLE_SEARCH_DEPRECATED`; the designated
   replacement, `mixed_people/api_search`, returns obfuscated rows —
   `last_name_obfuscated`, `has_email`/`has_direct_phone` booleans in
   place of values, organization `name` + `has_*` only, no `pagination`
   object — and reveal moved to Apollo's per-credit match/enrichment
   surface. The shipped `apollo-search` binding's provides (email,
   last_name, linkedin_url, city/state/country, company fields) cannot be
   satisfied by the search call alone. The code is faithful to §10.2;
   the vendor changed the contract underneath it.
   **Proposed direction:** split the capability along the vendor's own
   line — `apollo/search` becomes an honest masked *source* (apollo.id,
   first_name, title, company_name, the `has_*` signals; $0, new
   pagination shape), and a new `apollo/enrich` binding wraps the match
   endpoint (needs apollo.id or name+company, provides the revealed
   fields, per-credit cost declared) — which is the composition the spec
   already prefers (fetch once, judge many; pay only past the filter).
   Spec impact: §10.2, the `apollo-search` reference binding + fixtures
   (re-recorded from the new shape), cost table. Queued for a session
   packet; not applied, per SPEC §12(a) — Campaign 1 stays blocked on
   this decision.

## Deferred (a) items — flagged 2026-08-30 (agent round-trip, round 2), not executed

- **`help --agent` omits the `sql/*` step adapters.** `gtme plan`
  resolves `sql/filter` and `sql/transform`, but the doc's adapter list
  (from `adapters.Installed()`) carries no `sql/` ids — §8 requires every
  resolvable adapter's manifest. Root cause not yet chased (they are
  likely registered outside the builtin registry the doc reads).
- **`internal/ai`'s `api` engine fails identity-linked Anthropic keys**:
  the Messages API demands an `anthropic-workspace-id` header for them
  and the engine never sends one. Recovery exists (`engine:
  claude-code`), so deferred; the fix is a config/env passthrough.
