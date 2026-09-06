# The account shape

Account-based outbound from shipped atoms and no special role: companies
are judged, people are gated by the company they work at, each account is
summarised from its selected people, and the people are written to one
seat at a time. Four pipelines, four groups; every step is one the
`README.md` tour already names. The cross-type moves are relations the
ledger holds (`works_at`, written when a person row carries a
`company_domain`) read by SQL.

```
1-qualify/    companies.csv (company entity) → sql/filter (< 200 people) → ai/filter (tier a|b, rationale) ⇒ group "qualified-accounts"
2-select/     people.csv → sql/filter (works at a qualified account) → ai/filter (owns reporting or its budget) → group/deliver ⇒ "selected-people"
3-brief/      group "qualified-accounts" → sql/transform (the committee, fanned in over selected-people) → ai/compose (brief) → group/deliver ⇒ "briefed"
4-outreach/   group "selected-people" (limit 10) → ai/compose (first_line, ps_line) → instantly/add-to-campaign ⇒ group "contacted"
```

## Run it, in order

```sh
cd bundles/account-shape/1-qualify
gtme run . --simulate         # $0: 4 companies in, 3 small enough, judged synthetically
gtme run .                    # armed: $0 here, but the judge needs ANTHROPIC_API_KEY (or GTME_AI_ENGINE=fixture)
cd ../2-select
gtme run . --simulate         # $0: 8 people in, 6 work at a qualified account, 6 handed off (held)
gtme run .                    # armed: the handoff — a delivery to a group, keyed on email
cd ../3-brief
gtme run . --simulate         # $0: 3 accounts, each carrying its committee as acct.committee
gtme run .                    # armed
cd ../4-outreach
gtme run . --simulate         # $0: 6 people sourced (limit 10), lines written, 6 held
```

Groups live in your ledger. A later bundle simulated before the earlier
one has run armed either finds nothing (`2-select`'s SQL gate passes
nobody) or refuses to plan (`3-brief`, `4-outreach`: `group … does not
exist`). Run them in order; `gtme groups` lists the four as they appear.

What the receipts show:

- `1-qualify`: `small-enough: 4 in, 3 out, 1 filtered` (Globex, 450
  people, never reaches the model); `judge: 3 in, 3 out` with `tier` and
  `rationale` landing as `accounts-qualify.tier` / `.rationale` — declared
  provides on a company judgment.
- `2-select`: `at-qualified-account: 8 in, 6 out, 2 filtered` — the two
  Globex people drop because their company did; the plan annotates the
  query `cross-record`.
- `3-brief`: `committee: 3 in, 3 out` — a fan-in: each company carries a
  JSON array of its selected people and a count, provenance
  `sql/transform @ <query hash>`; `handoff` holds the briefs.
- `4-outreach`: `source: sourced 6 members of group "selected-people"
  (limit 10, oldest first)` and the resolved variables per seat — the
  dry-run receipt a human reads.

## The rungs after simulate

```sh
gtme secret set ANTHROPIC_API_KEY
gtme secret set INSTANTLY_API_KEY
cd 4-outreach
gtme plan .                   # $0
gtme run . --dry-run          # spends on the model; reads the campaign; sends nothing
gtme run .                    # armed — a human runs this, after reading the dry-run receipt
```

The campaign id in `4-outreach/pipeline.yaml` is a placeholder; copy the
file out and set yours. Re-running `4-outreach` contacts nobody twice, and
`limit: 10` makes it a bounded consumer you can run on a schedule until
the group is drained.

## Make it yours

The two CSVs are the inputs — a company list and a people export whose
rows carry `company_domain`, which is what writes `works_at`. Swap them in
under the same names. The judgments to tune are the three prompts; the
shape needs no change until you want a fifth pipeline.
