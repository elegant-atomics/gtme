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
  armed gate — in the smallest shape that exercises all of it. **Currently
  blocked** — see below.
- **Campaign 1** — the fuller funnel (originally this file's only campaign,
  per ADR-016). Real Apollo pull, AI filter/compose, Harvest enrichment,
  real delivery. Runs today; exercises cache windows, real spend, and the
  stories campaign zero deliberately skips (Recover, Iterate, Segment).

## Campaign zero — the essence (ADR-019)

### Status: blocked, not runnable yet

Campaign zero depends on mechanics DECISIONS.md ADR-017/018/019 describe but
that do not exist in code or SPEC.md yet (scoped deliberately as a
docs-only pass on 2026-08-15 — see the "Session packet, 2026-08-15" section
of DECISIONS.md). Specifically, before this campaign can run for real:

- `csv/source` needs a `columns:` config mapping CSV headers → canonical
  field names (ADR-018) — today it only auto-detects a fixed set of
  identity-key aliases (DECISIONS.md's 2026-08-12 "Name-hash key inputs"
  entry), not arbitrary header mapping.
- `instantly/add-to-campaign` needs a `variables:` config mapping ledger
  fields → Instantly merge-field names (ADR-018) — today its merge fields
  are hard-coded (`first_line`, `ps_line`; SPEC §10.6).
- The manifest schema needs `needs: dynamic` generalized beyond AI steps
  (ADR-019) so a deliver step's needs derive from `variables:`, the same
  way an AI step's derive from `uses:` (ADR-004) today.
- The runner needs the `on_missing: skip | fail` policy (default skip with
  a verdict) and a dry-run mode that renders resolved variables per record
  — the "review before arming" gate below has nothing to review without it.

None of that is built. This section documents the *target* shape so it
isn't lost, per the scope note in DECISIONS.md — building it is separate,
future work.

### Target pipeline

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

### Target enactment script (once unblocked)

1. **Guard.** `gtm plan campaign-zero.yaml` against a CSV missing an email
   column entirely — confirm ADR-018's rule fires ("no identity-key path")
   as a plan error, not a runtime surprise partway through.
2. **Dry run.** `gtm run campaign-zero.yaml --dry-run` (or equivalent, once
   the gate exists) against the real ~10-record CSV. Capture: the receipt
   renders each record's *resolved* `variables:` values — this is the
   artifact a human actually reviews, per ADR-019, before anything sends.
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

*(empty — filled in after a human authorizes and runs a campaign above)*
