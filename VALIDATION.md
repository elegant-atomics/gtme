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
  recorded verdict; blank merge fields never send) and `gtm run --dry-run`
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

deliver:
  use: instantly/add-to-campaign
  with:
    campaign: "gtm-campaign-zero-<date>"
  variables:
    first_name: full_name
  idempotency: email
```

### Enactment script

1. **Guard.** `gtm plan campaign-zero.yaml` against a CSV missing an email
   column entirely — confirm the plan fails (exit 2) before any row is
   read. With name columns still present the failure comes from the deliver
   adapter's `email` floor ("needs email, which no earlier step provides"),
   since a names-only CSV legitimately keys on the name-hash tier (plan
   *notes* the weak tier rather than blocking on it); strip the name
   columns too and ADR-018's "no identity-key path" rule fires at the
   source instead. Either way: a plan error, not a runtime surprise
   partway through.
2. **Dry run.** `gtm run campaign-zero.yaml --dry-run` against the real
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

- `gtm secret set APOLLO_API_KEY`, `HARVEST_API_KEY`, `ANTHROPIC_API_KEY`,
  `INSTANTLY_API_KEY` — real keys, human-provided.
- A real Instantly campaign created ahead of time, scoped so a mistaken send
  is cheap and reversible (a throwaway/internal test campaign, not a real
  prospect list). `instantly/add-to-campaign` resolves the campaign by
  name (SPEC §10.6) — name it something unambiguous, e.g.
  `gtm-validation-<date>`.
- `make build`, so `./bin/gtm` is the binary under test (not `go run`).
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
    uses: [full_name, title, company_domain]
    with:
      prompt: >
        Keep only contacts likely to own outbound tooling decisions.
      batch_size: 25

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

deliver:
  use: instantly/add-to-campaign
  with:
    campaign: "gtm-validation-<date>"
  idempotency: email
```

### Enactment script — the eight stories in miniature

Run in this order; each step names which story it enacts and what to
capture as evidence.

1. **Guard.** Before touching real data: `gtm plan validation-campaign.yaml`
   with one field deliberately mistyped in `uses:` (e.g. `full_nmae`).
   Confirm the error names the step and field, exit 2, zero network calls
   (no cost rows). Fix the typo. Run `gtm plan` again clean — confirm the
   printed cost estimate is `?` for the AI steps (unpriced without a real
   call) and a real number for Harvest, and that all four credentials
   resolve.

2. **Launch.** `gtm run validation-campaign.yaml`. Capture: the run id, the
   terminal receipt (records in/out per step, total cost), and that every
   sourced identity has an `identities` row with `field_values`
   (`gtm show --run last --limit 5` as a spot check).

3. **Interrogate.** Pick one delivered record's email from the run.
   `gtm show <email> --provenance`. Capture: every field has a real source
   adapter id, a plausible confidence, and a `run_id` matching this
   campaign's run.

4. **Recover.** Re-run a *second*, separate small campaign (a fresh 5-10
   record `limit`, different campaign name, so this doesn't touch the
   first run's delivered records) and kill the `gtm run` process (Ctrl-C or
   `kill`) partway through the `linkedin` step — after at least one Harvest
   call has completed but before the step finishes. `gtm run
   validation-campaign-2.yaml --resume last`. Capture: the receipt shows
   the already-done records were not re-processed (no duplicate Harvest
   cost for them — compare the `costs` table's per-identity rows before
   and after the kill), and the run reaches `status='done'`.

5. **Segment.** `gtm query --save validation-delivered "SELECT i.identity_key
   FROM identities i JOIN deliveries d ON d.identity_id = i.id
   WHERE d.target = 'instantly/add-to-campaign@1'"`. Capture: the row
   count matches the receipt's delivered count exactly.

6. **Iterate.** Edit the `personalize` step's prompt (a small wording
   change). `gtm plan` first — confirm it's still valid — then `gtm run`
   against a `limit: 3` subsample of the *same already-sourced* records
   (a new pipeline pointed at a `gtm query --records`-style narrow slice
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
   (`gtm query "SELECT count(*) FROM deliveries WHERE target =
   'instantly/add-to-campaign@1'"` before and after must match, unless
   the second pull surfaced genuinely new people).

8. **Report.** `gtm runs` (list) and `gtm runs <first run id>` (receipt).
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

- **Guard:** email column stripped → `gtm plan` exit 2 at the source
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
API reads, not something gtm sends. Noted so a future read of lead data
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
  the runner-injected session env (SPEC §6) — a key stored with `gtm
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
(markdown mode) → sql/enrich → ai/compose → the instantly deliver
BINDING, installed the operator way (`~/.gtm/adapters/instantly-add-lead/
binding.yaml`, id rewritten, fixtures alongside). Run up the full ladder:
simulate → plan → dry → arm → re-run.

- **Simulate ($0):** sql/enrich ran for real (offline by construction),
  AI answered synthetically-marked, http/enrich surfaced as a counted
  gap, delivery held with all four variables resolved. The agent-rung
  works: behavior validated before any key was exercised.
- **Plan ($0):** the external YAML binding resolved with credentials
  from ~/.gtm/secrets; the M9 touch scope printed
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
