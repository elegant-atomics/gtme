# VALIDATION.md — the validation campaigns

**Non-normative and living**, unlike the acceptance criteria these enact —
see SPEC.md's "Operator stories — acceptance criteria" section (ADR-012) for
the normative invariant + Given/When/Then each campaign below enacts in
miniature, against real data instead of fixtures.

**Human-gated. Do not execute without an explicit human go-ahead**, per
CLAUDE.md and SPEC.md §12: both campaigns below send real records to a real
Instantly campaign, and Campaign 1 spends real money on top of that (Apollo,
Harvest, Anthropic). Nothing in this document authorizes running either; it
is the script to run *when* a human says go.

Two campaigns, run in order:

- **Campaign zero** — the essence. `csv/source → instantly/add-to-campaign`,
  two adapters, zero enrichment spend. Per DECISIONS.md ADR-019: everything
  that makes a delivery pipeline trustworthy — identity, both edge mappings,
  dynamic needs, plan coherence, per-record verdicts, idempotent delivery, an
  armed gate — in the smallest shape that exercises all of it. **Runnable**
  (awaiting the human go this document never grants) — see below.
- **Campaign 1** — the fuller funnel (originally this file's only campaign,
  per ADR-016). Real Apollo pull, AI filter/compose, Harvest enrichment,
  real delivery. Runs today; exercises cache windows, real spend, and the
  stories campaign zero deliberately skips (Recover, Iterate, Segment).

## Campaign zero — the essence (ADR-019)

### Status: runnable (human go still required)

The mechanics this campaign depends on were built by the 2026-08-15
reconciliation-plus-build pass (SPEC.md v0.4, milestone M7 — `make check`
green including M7's offline acceptance tests, which run this exact
pipeline shape against the `mock/deliver` fixture in
`test/e2e/campaign_zero_test.go`):

- `csv/source` takes `columns:` mapping canonical field names → CSV headers,
  with auto-map for canonical headers, plan-time near-miss suggestions, and
  registry normalization at ingress (ADR-018, SPEC §10.1).
- `instantly/add-to-campaign` declares dynamic needs with an `email` floor;
  its merge fields derive entirely from the step-level `variables:` mapping
  (ADR-018/019, SPEC §10.6) — nothing is hard-coded.
- `needs: dynamic` is generalized (ADR-019, SPEC §6): a deliver step's
  needs derive from `variables:` exactly as an AI step's derive from
  `uses:`, validated at plan time.
- The runner enforces `on_missing: skip | fail` (default skip with a
  recorded verdict; blank merge fields never send) and `gtme run --dry-run`
  renders each record's resolved variables in the receipt — the artifact
  the "review before arming" gate below reviews (SPEC §8).

What remains is only what must remain: a human go, real ~10-record CSV,
and a controlled Instantly campaign.

### Pipeline

```yaml
name: campaign-zero
version: 1

source:
  use: csv/source
  with:
    path: contacts.csv
    columns:
      full_name: Full Name
      email: Email
      company_domain: Company Website

steps:
  - id: send            # ADR-031: a deliver adapter is an ordinary step
    use: instantly/add-to-campaign
    with:
      campaign: "gtm-campaign-zero-<date>"
    variables:
      first_name: full_name
    idempotency: email
```

### Enactment script

1. **Guard.** `gtme plan campaign-zero.yaml` against a CSV missing an email
   column entirely — confirm the plan fails (exit 2) before any row is
   read. With name columns still present the failure comes from the deliver
   adapter's `email` floor ("needs email, which no earlier step provides"),
   since a names-only CSV legitimately keys on the name-hash tier (plan
   *notes* the weak tier rather than blocking on it); strip the name
   columns too and ADR-018's "no identity-key path" rule fires at the
   source instead. Either way: a plan error, not a runtime surprise
   partway through.
2. **Dry run.** `gtme run campaign-zero.yaml --dry-run` against the real
   ~10-record CSV. Capture: the receipt renders each record's *resolved*
   `variables:` values (and lists any record held back by `on_missing`,
   with its reason) — this is the artifact a human actually reviews, per
   ADR-019, before anything sends. Confirm `deliveries` gained zero rows.
3. **Arm.** Review the dry-run receipt by eye. If it looks right, run for
   real (the "arm" step) against the same ~10 records into the controlled
   Instantly campaign.
4. **Re-run to prove no dupes.** Run `campaign-zero.yaml` again, unchanged,
   against the same CSV. Capture: zero new `deliveries` rows — idempotency
   holding with no enrichment/cache layer involved at all, the simplest
   possible version of the Top-up story.

## Campaign 1 — the fuller funnel

### Prerequisites

- `gtme secret set APOLLO_API_KEY`, `HARVEST_API_KEY`, `ANTHROPIC_API_KEY`,
  `INSTANTLY_API_KEY` — real keys, human-provided.
- A real Instantly campaign created ahead of time, scoped so a mistaken send
  is cheap and reversible (a throwaway/internal test campaign, not a real
  prospect list). `instantly/add-to-campaign` resolves the campaign by
  name (SPEC §10.6) — name it something unambiguous, e.g.
  `gtme-validation-<date>`.
- `make build`, so `./bin/gtme` is the binary under test (not `go run`).
- A query small enough to cap around 50 records: use `apollo/search`'s
  `limit` config, not a broad query then a manual trim, so the plan's cost
  estimate (§7) is honest about what will actually run.

### Campaign pipeline

```yaml
name: validation-campaign
version: 1

source:
  use: apollo/search
  with:
    query: "vp marketing, saas, 50-200 employees"
    limit: 50

steps:
  - id: icp-filter
    use: ai/filter
    uses: [first_name, title, company_name]   # masked fields only (ADR-043)
    with:
      prompt: >
        Keep only contacts likely to own outbound tooling decisions.
      batch_size: 25

  - id: reveal
    use: apollo/enrich
    when: icp-filter.passed
    cache: 30d

  - id: linkedin
    use: harvest/profile
    when: icp-filter.passed
    cache: 30d

  - id: personalize
    use: ai/compose
    uses: [recent_posts, role_history]
    when: icp-filter.passed
    with:
      prompt: >
        Write first_line and ps_line using recent_posts and role_history.
      batch_size: 25

  - id: send
    use: instantly/add-to-campaign
    with:
      campaign: "gtme-validation-<date>"
    idempotency: email
```

### Enactment script — the eight stories in miniature

Run in this order; each step names which story it enacts and what to
capture as evidence.

1. **Guard.** Before touching real data: `gtme plan validation-campaign.yaml`
   with one field deliberately mistyped in `uses:` (e.g. `full_nmae`).
   Confirm the error names the step and field, exit 2, zero network calls
   (no cost rows). Fix the typo. Run `gtme plan` again clean — confirm the
   printed cost estimate is `?` for the AI steps (unpriced without a real
   call) and a real number for Harvest, and that all four credentials
   resolve.

2. **Launch.** `gtme run validation-campaign.yaml`. Capture: the run id, the
   terminal receipt (records in/out per step, total cost), and that every
   sourced identity has an `identities` row with `field_values`
   (`gtme show --run last --limit 5` as a spot check).

3. **Interrogate.** Pick one delivered record's email from the run.
   `gtme show <email> --provenance`. Capture: every field has a real source
   adapter id, a plausible confidence, and a `run_id` matching this
   campaign's run.

4. **Recover.** Re-run a *second*, separate small campaign (a fresh 5-10
   record `limit`, different campaign name, so this doesn't touch the
   first run's delivered records) and kill the `gtme run` process (Ctrl-C or
   `kill`) partway through the `linkedin` step — after at least one Harvest
   call has completed but before the step finishes. `gtme run
   validation-campaign-2.yaml --resume last`. Capture: the receipt shows
   the already-done records were not re-processed (no duplicate Harvest
   cost for them — compare the `costs` table's per-identity rows before
   and after the kill), and the run reaches `status='done'`.

5. **Segment.** `gtme query --save validation-delivered "SELECT i.identity_key
   FROM identities i JOIN deliveries d ON d.identity_id = i.id
   WHERE d.target = 'instantly/add-to-campaign@1'"`. Capture: the row
   count matches the receipt's delivered count exactly.

6. **Iterate.** Edit the `personalize` step's prompt (a small wording
   change). `gtme plan` first — confirm it's still valid — then `gtme run`
   against a `limit: 3` subsample of the *same already-sourced* records
   (a new pipeline pointed at a `gtme query --records`-style narrow slice
   is a v1 idea per ROADMAP.md; for this campaign, simplest is a fresh
   tiny Apollo pull with `limit: 3` and the edited prompt) before deciding
   whether to run the change at full scope. Capture: the cost of the
   3-record test vs. what a 50-record re-run of the same change would have
   cost, as the concrete number behind "test small before scaling."

7. **Top-up (the Monday scenario).** Days later — or simulated by waiting
   past nothing, since the cache window is 30d and this is the same week —
   re-run `validation-campaign.yaml` with the *same* Apollo query. Capture:
   the receipt's cache-skip count and `$ avoided` for the `linkedin` step
   (everyone already enriched should skip), and — critically — zero new
   rows in `deliveries` for identities already delivered by the first run
   (`gtme query "SELECT count(*) FROM deliveries WHERE target =
   'instantly/add-to-campaign@1'"` before and after must match, unless
   the second pull surfaced genuinely new people).

8. **Report.** `gtme runs` (list) and `gtme runs <first run id>` (receipt).
   Capture: the receipt's per-step counts and total cost reconstruct
   correctly from the ledger alone, days after the run finished, with no
   other record of what happened.

## What to record when reality diverges

Real providers will disagree with the spec in ways fixtures can't catch —
a field Apollo/Harvest/Instantly's actual API returns differently than
§10 describes, a rate limit shaped differently than the exit-code table
assumes, a cost estimate that's off by an order of magnitude. When that
happens, for either campaign:

- **The implementation is wrong relative to a DECIDED section it
  misread**, and the spec is right → a code bug. Fix it, note it in
  DECISIONS.md if the fix involved an underspecified choice (SPEC §12).
- **The spec's assumption about a provider's real behavior was wrong**
  (this is the case §12(a) names explicitly: "an external API's real
  shape contradicts the spec in a way that changes a contract") → STOP,
  do not silently adapt the code around it. Write up the actual shape
  observed and propose a spec diff for human approval, the same way
  AUDIT.md's category (b) items are queued rather than applied.
- **Anything that would spend more money, send to a wider audience, or
  behave differently than an explicit human instruction for this
  campaign** → stop entirely; this is exactly SPEC §12(b)'s "never without
  an explicit human go."

Each finding, whichever bucket, gets appended to this file's own running
log below once a campaign actually runs (neither has one yet — this
document is the script, not the record).

## Campaign log

### 2026-08-15 — Campaign zero: ran clean, one finding

Human-authorized and armed the same day the M7 mechanics landed. Target:
Instantly campaign `gtm-campaign-zero-2026-08-15` (created as a shell — no
sending accounts attached, never activated, so lead creation was the only
real-world effect). Source: a 10-row CSV of operator-controlled plus-alias
addresses with three deliberate probes (a mixed-case email, a nameless row,
a caps-duplicate of row 1).

- **Guard:** email column stripped → `gtme plan` exit 2 at the source
  (`columns:` naming a missing header), zero rows read. ✅
- **Dry run:** 10 rows → 9 identities (caps-duplicate collapsed); receipt
  rendered 8 records' resolved `first_name` values, held the nameless row
  back (`on_missing: skip`, reason named); zero `deliveries` rows, zero
  adapter calls. Reviewed by the human before arming. ✅
- **Armed:** 8/8 delivered; leads verified in Instantly via a separate API
  read — payloads carried exactly the mapped `first_name` + `email`,
  nothing else. Mixed-case email keyed and delivered lowercase. ✅
- **Re-run, unchanged:** 8 skipped `already_delivered`, zero adapter calls
  for them, `deliveries` count unchanged at 8 — the Top-up invariant with
  no cache layer involved. ✅

**Finding (code bug, fixed):** SPEC §10.6 says the campaign name resolves
to an id *once per run*; the armed run resolved once per worker-pool
session (four times). Read-only calls, no behavioral harm, but a real
divergence from a DECIDED section. Fixed same day: a process-level
name→id cache in the instantly adapter, with a regression test
(`TestResolvesCampaignOncePerProcess`).

**Observation (no action):** Instantly populates its own `company_domain`
field on each lead, derived server-side from the lead's email — visible in
API reads, not something gtme sends. Noted so a future read of lead data
doesn't get attributed to the pipeline's mapping.

### 2026-08-15 — Increment: + ai/compose (campaign zero widened one step)

Per the incremental plan (one adapter at a time from campaign zero's base),
same dry→review→arm→re-run ritual, fresh operator-controlled aliases.

- First live Anthropic call: 8 records composed in one batch, $0.0129
  receipted from real token usage; `uses:` carried a vendor-namespaced
  `csv.*` field into the prompt cleanly; dry-run receipt rendered all three
  resolved merge fields per record. Armed 8/8; re-run delivered nothing
  twice.
- **Finding (code bug, fixed):** the API engine read `ANTHROPIC_API_KEY`
  from the *process* env, but a built-in adapter's credentials arrive via
  the runner-injected session env (SPEC §6) — a key stored with `gtme
  secret set` never reached the engine. Every prior test had exported the
  key or used the fixture engine, so only a live run could catch it. Fixed
  (engine resolution now takes the adapter's env view) with a regression
  test pinning the session-env path.
- **Observation (spec-conformant, worth knowing):** compose steps are not
  cache-skippable by design (§7), so a no-op re-run of a compose pipeline
  re-spends the AI call (~$0.0122 for 8 records here) even when every
  delivery skips. At scale, expensive compose + frequent re-runs is a cost
  pattern to watch.

### 2026-08-15 — Increment: + harvest/profile (first paid enrichment)

One record (the operator's own profile), csv → harvest → compose → deliver.
The richest finding-per-dollar run yet: four live-API shape divergences,
all in HarvestAPI responses vs our fixture-era decoding, each degrading or
failing per-record exactly as §5 requires (run continues, partial output
kept):

1. `startDate.month` arrives as a quoted string, not an int → decode
   tolerant (`flexInt`), fixture updated to carry the live shape.
2. `status` arrives as a number, not a string → decoded loosely (never
   consumed).
3. The posts endpoint's target param is `profile`/`profileId` — the
   `profileUrl` param the adapter sent doesn't exist ("No valid target
   provided"). Confirmed against HarvestAPI's current docs; the adapter
   now prefers `profileId` (their fast path) from the already-fetched
   profile.
4. `postedAt` arrives as an object, not a string → decoded loosely (never
   consumed).

After the fixes: enrichment grounded compose in real data (the generated
first_line referenced an actual recent post), and the armed run showed the
cache economics live — harvest skipped (`$0.0120 avoided` on the receipt),
compose $0.0068, one lead delivered. Total increment spend ≈ $0.07 across
the debug retries; the receipt's "avoided" line is the enrichment-ledger
story with real money for the first time.

### 2026-08-16 — Increment: + ai/filter (dry-run-validated; arm skipped)

Bare `ai/filter` on the per-step `model:` override (Haiku judging, Sonnet
composing, one pipeline) against an 8-row CSV with a checkable rubric:
keep warm favorite colors, fail cool ones.

- Filter: 8 in → 3 pass / 5 filtered, $0.0024, one batch. The split
  matched prediction exactly (burnt orange, goldenrod, crimson pass;
  mauve judged cool — the one defensible coin-flip). Verdict *reasons*
  reconstructed afterwards from `step_events` by SQL — the
  reasons-in-the-ledger story working as designed.
- Gating economics visible on the receipt: compose ran only on the 3
  passers ($0.0048 vs $0.0129 for 8 in the compose increment).
- Deliver held at the dry gate; arming was skipped deliberately — the
  arm/re-run mechanics were already proven three times in prior
  increments, and 3 more test leads add nothing.
- No findings: first increment with zero divergences, consistent with the
  AI path having been debugged in the compose increment.

This increment also seeded ADR-021 (groups): the design conversation it
triggered — filter nondeterminism, verdict scope, campaign as a concept —
is recorded there.

### 2026-08-16 — Increment: stack-proof (the M8–M12 infrastructure, live)

Every piece of the binding-era stack in one pipeline against the real
ledger and the live Instantly test shell: csv/source → http/enrich
(markdown mode) → sql/transform → ai/compose → the instantly deliver
BINDING, installed the operator way (`~/.gtme/adapters/instantly-add-lead/
binding.yaml`, id rewritten, fixtures alongside). Run up the full ladder:
simulate → plan → dry → arm → re-run.

- **Simulate ($0):** sql/transform ran for real (offline by construction),
  AI answered synthetically-marked, http/enrich surfaced as a counted
  gap, delivery held with all four variables resolved. The agent-rung
  works: behavior validated before any key was exercised.
- **Plan ($0):** the external YAML binding resolved with credentials
  from ~/.gtme/secrets; the M9 touch scope printed
  (`record: touched → stack-proof`); vendor-coupling notes flagged
  `web.homepage` and `sql.shout`.
- **Dry ($0.0053):** the grounding loop proved itself — ai/compose's
  lines cite the actually-fetched page ("classic IANA documentation
  example domain", the "Learn more" link): http/enrich → ledger →
  `uses:` → model, live. Three text/html payloads retained (ADR-030,
  90d TTL).
- **Armed ($0.0054):** 3 leads landed in the never-activated campaign
  shell via the YAML binding — first_name first-class, first_line/
  ps_line/shout as custom variables (the `$variables` splice against the
  live API). fetch: 3 cached — the armed run reused the dry run's
  fetches inside the 7d window; fetch-once economics on a receipt with
  real money. `stack-proof` group: 3 touched events, created on demand.
- **Re-run ($0.0050):** deliver 0 in, 3 cached (already_delivered) —
  idempotency held. Note, not a finding: ai/compose re-ran (composes are
  cheap-to-rerun by design, uncached); the qualify/send decomposition or
  an `exclude:` gate is the pattern when even that re-spend matters.

Zero divergences. Total spend for proving five milestones live: $0.016.

### 2026-08-29 — Agent round-trip: a CRM list → filter → AI summary → CSV, from the CLI alone

The §8 round-trip criterion tested for real rather than with the doc's own
examples: a fresh agent session in a sealed directory — the `gtme` binary
(main `34685b1`, M17), an initialised ledger, two secrets stored by the
human beforehand (a CRM private-app token and a model key), no repo, no
README, no SPEC — given a task in campaign terms: read every contact in a
named CRM list, keep those with an engagement score above 20, write an AI
summary of each one's profile and product usage, deliver email + summary
to a CSV. Real API, real model, no send surface (`csv/deliver` only).

Outcome, from the ledger: 16 runs, ~10 minutes. Fourteen were probes —
the agent used gtme itself as its exploration harness (`probe-properties`,
`probe-list`, `probe-search`×3 failed on the vendor's own 400s,
`probe-search-wide`) before the real source. It **authored a source
binding** for the CRM's search API (33 mapped properties, cursor
pagination, retry rules, payloads retained), reproduced the list's own
membership predicate server-side and checked the count against the list's
reported size (561), snapshotted the >20 cut into a group by SQL
(`groups add --query`) so the summarised set was a frozen decision,
declared `provides: {summary: {}}` on `ai/compose` (landed namespaced),
dry-ran, reviewed, edited the prompt, armed. 558 identities (three
duplicate emails across contact records, noted by the agent), 7 above the
cut, 7 rows delivered, $0.47 — all of it compose, every CRM read free.

Findings:

- **`help --agent` is silent on bindings** — the one route to an API gtme
  has no adapter for. The agent found the contract by pulling the embedded
  `spec/binding-schema.json` out of the binary with `strings` and wrote a
  working binding from it blind. Not a code bug: §8 does not list the
  binding surface among the doc's required contents. **Spec gap → AUDIT.md
  (b)**, queued: §8 names a second surface, `gtme help --bindings` (the
  schema, the discovery path, one reference binding), and `help --agent`
  points at it.
- **The unknown-adapter error named only `manifest.json`** though the
  loader accepts `binding.yaml` in the same directory; the agent said that
  line taught it the load path, and it pointed the wrong way. Code, fixed
  in this commit: the message names both shapes and points at
  `help --bindings`.
- **Dry run → prompt edit → arm paid the judgments twice** ($0.24 + $0.23).
  By design (ADR-039: a changed prompt is a different question) and worth
  stating in the dry-run guidance: review the *judgments* on the dry run,
  and expect a prompt edit to re-buy them.
- The vendor-side facts an agent had to discover by probing (the
  memberships endpoint returns only ids; search cannot filter on list
  membership) are the argument for a shipped reference binding and for
  the OpenAPI→binding codegen skill (ROADMAP); the binding the agent wrote
  is the seed for the first.

Zero divergences against a DECIDED section. The rest of the score sheet
lives outside this repo.


### 2026-08-30 — Increment: campaign zero re-armed on the M19 binary — preflight and attestation, live

Campaign zero's enactment script re-run end to end on main `3aa6005`
(M14–M19 in the binary), same controlled campaign, ten fresh
self-owned records (`+cz01..cz10`). Guard: a CSV without the email column
fails `plan` at exit 2 naming the missing column. Dry run: receipt renders
resolved variables for review; zero `deliveries` rows written. Arm: 2 new
deliveries, both **ATTEST confirmed** (M14, first live firing). Re-run:
10/10 cache-skipped, zero new rows. Instantly's lead list confirms exactly
the two new addresses.

Findings:

- **M17's preflight fired live for the first time and blocked correctly**:
  `send: preflight BLOCKED — campaign is not Active (✗ campaign active)`
  against the real campaign, which is deliberately a draft. The dry run
  reported the block and continued; an armed run would have failed before
  any record moved. For a validation target that never sends, the ADR-040
  opt-out (`preflight: false` in step config, with a comment saying why)
  is the documented route, and the receipt shows it. Working as specified;
  recorded because it is the exact class of failure ADR-040 was built for,
  observed against the real API.
- **Idempotency is ledger-scoped, deliberately, and now visibly.** Eight
  of the ten records were skipped as already-delivered from the 08-15
  enactment — but those leads no longer exist in the remote campaign
  (only `+s1..s3` remained). `deliveries` UNIQUE(target, idempotency)
  answers "has gtme ever delivered this record to this scope", not "is it
  there now". Right default (replays can't duplicate), worth a line here
  because an operator who deletes leads remotely and re-runs will get
  nothing re-added until the touch scope changes. A `reconcile` surface,
  if ever wanted, is a ROADMAP conversation, not a bug.

### 2026-08-30 — Campaign 1: STOPPED at the source — Apollo withdrew the search shape from API callers

Story 1 (Guard) passed: the mistyped `uses:` field failed `plan` at
exit 2, naming the step, the field, and the nearest canonical name, with
zero network. Story 2 (Launch) stopped at the source with $0 spent:

- `POST /api/v1/mixed_people/search` → HTTP 422
  `SEARCH.ROUTING.LEGACY_PEOPLE_SEARCH_DEPRECATED` ("This endpoint is
  deprecated for API callers. Please use the new mixed_people/api_search
  endpoint").
- The replacement `mixed_people/api_search` returns **obfuscated rows**:
  person keys are `id, first_name, last_name_obfuscated, title,
  has_email, has_direct_phone, has_city, has_state, has_country,
  last_refreshed_at, organization`; the organization carries `name` plus
  `has_*` booleans only. No email, no linkedin_url, no city/state values,
  and no `pagination` object (top level is `total_entries` + `people`).

This is the "external API's real shape contradicts the spec in a way that
changes a contract" bucket, verbatim: `apollo/search`'s declared provides
(email, last_name, linkedin_url, …) cannot be satisfied by search alone
any more — Apollo moved reveal behind its per-credit match/enrichment
surface. Stopped without adapting; the observed shape and a proposed
direction are queued as AUDIT.md (b) item 4 for human approval. Campaign 1
is blocked on that decision. Two adjacent notes: the run failed clean at
the source with `$0 spent` on the receipt (the failure mode costs
nothing), and the registry's CI runs fixtures offline, so a live
withdrawal like this is invisible to it between fixture re-recordings —
which is precisely what a maintained tier is for.

### 2026-08-30 — Agent round-trip, round 2: the registry closes the loop the first round exposed

Round 1's box re-run on main `3aa6005` (M18+M19 in the binary): fresh
agent session, fresh ledger, same TASK.md, same sealed conditions (no
repo, no README, no SPEC; secrets stored beforehand; the human answered
nothing — zero help turns). Same correct outcome: 562 list members
sourced, 7 above the engagement cut, 7 summaries, 7-row CSV. The deltas
are the story:

- **The `strings` move is gone.** Round 1's agent dug the binding schema
  out of the binary's string table. Round 2's agent, needing a source
  gtme doesn't ship, went to the **registry**: found the verified
  `hubspot/contact-search` entry, read it (its comments carry the
  reproduce-the-list-via-search pattern — vendor knowledge travelling in
  a binding), forked it into its own `hubspot/list-contacts` (score +
  usage properties added), and ran **`gtme adapters verify`** on its own
  work before using it. AUDIT (b) item 3's fix, observed doing its job
  end to end, one day after it merged.
- **6 runs vs 16.** Round 1 burned fourteen probe runs; round 2 used two
  free `http/enrich` probe pipelines and read the raw responses back out
  of the ledger's `payloads` table (ADR-030's cache tier as a discovery
  instrument — an emergent pattern worth naming: the agent probed an
  unknown API *through* gtme, so the token was injected, never seen).
- **$0.2254 vs $0.47, paid once.** Round 1 paid its judgments at dry-run
  and again at arm — the finding that produced ADR-039. Round 2's armed
  run served all 7 summaries from the judgment cache: dry-run spent,
  arm cost $0. M16's first live proof, closing round 1's finding #3.

New surface gaps, verified and queued:

- **`help --agent` lists no `sql/*` adapters** (checked against the
  binary: zero `sql/` ids among the 10 listed) though `gtme plan`
  resolves `sql/filter`/`sql/transform` in a pipeline; the agent found
  them only through the ledger notes section. §8 says the doc carries
  every adapter surface `plan` resolves. → AUDIT, deferred (a).
- **The `api` AI engine can't use identity-linked Anthropic keys** (400:
  `anthropic-workspace-id is required when authenticating with an
  identity-linked API key`); the agent recovered via `engine:
  claude-code`. → AUDIT, deferred (a): send the workspace header when
  configured.
- The per-(adapter, identity) enrich cache silently served a stale probe
  response until the agent switched seed identities — by design
  (freshness is the read gate), logged as operator-experience note, not
  a gap.

### 2026-08-30 — Campaign 1: the eight stories, live, on the split economics (M20)

Unblocked by ADR-043/M20 the same day it stopped. Total spend for the
whole enactment ≈ $1.87. By story:

1. **Guard** ✓ (logged above): the mistyped `uses:` failed at exit 2
   naming the fix, zero network.
2. **Launch** ✓ — run `01M1AE2R0DDP8E0BGWQYN1J6D2`, and the receipt *is*
   the split economics: 24 masked rows $0 → filter dropped 5 before
   anyone paid → 19 reveals $0.19 → harvest $0.228 → compose $0.0927 →
   14 delivered, all ATTEST-confirmed; $0.5606 total. Finding: the new
   `api_search` treats a long qualified phrase ("vp marketing, saas,
   50-200 employees") as a literal and returns zero — the run completed
   cleanly through every step at $0; use short keywords plus the
   structured config filters. Second finding: 5 records revealed without
   an email failed the deliver's needs floor per record (reason recorded
   in `step_events`), and the run continued — correct, though "failed"
   next to `on_missing: skip` reads ambiguously; an operator-experience
   note, not a gap.
3. **Interrogate** ✓ — every field on a delivered record shows a real
   `source` adapter@version, confidence, and this run's id; deliveries
   show `confirmed`. Note for operators: a masked-sourced identity keeps
   its `nh:` name-hash key after reveal (§4 keys at the source and never
   re-keys); the email is a field on it, not its name.
4. **Recover** — honestly partial. Three timed kills (45s/30s/80s, up to
   40 records) all landed after completion: the funnel is too fast at
   validation scale to snipe (40 records through five steps in under
   80s). What was proven live: `--resume` of a completed run is a $0
   no-op, twice, and the per-(identity, step) cost invariant held at
   max 1 across every attempt. The mid-step kill itself remains covered
   by the offline resume acceptance.
5. **Segment** ✓ — `--save validation-delivered`; count matches the
   receipt (14). `deliveries.target` is the adapter id, as the script
   said; see AUDIT (b) 5 for the scope question this surfaced.
6. **Iterate** ✓ — the concrete number behind "test small": a compose
   prompt edit re-ran against 3 records in 8 seconds for **$0.0131** —
   only the edited step re-paid (its cache signature changed), while
   filter verdicts, reveals and profiles all served from cache. The same
   edit at the first run's scope would have cost ~$0.08 in compose alone,
   plus ~$0.42 of re-fetches without the cache.
7. **Top-up** ✓ — the identical re-run: **$0 spent, $0.418+ avoided, 95
   records skipped, zero new deliveries, 0.4 seconds.** The savings
   counter next to the spend line is the receipt working as designed.
8. **Report** ✓ — `gtme runs <id>` reconstructs the per-step receipt and
   total from the ledger alone.

Cross-run bonus observed in 4: filter judgments and reveals from earlier
runs served later runs' overlapping records from cache — M16's signature
cache and the enrich freshness window compounding across pipelines.
