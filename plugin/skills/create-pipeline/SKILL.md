---
name: create-pipeline
description: Use when someone describes an outreach or go-to-market goal in their own words and wants it built with gtme — a list to work, people to find or qualify, notes to write, a campaign or CRM to fill — before any pipeline file exists.
---

# Create a pipeline

The human knows their goal, not gtme. You translate. They hear plain
words and see receipts; they never hear "enrich", "role", or "terminus"
unless they ask.

**Preflight.** `gtme version` (else `brew install gtme-run/tap/gtme`).
`gtme init` if `~/.gtme` is missing. First machine ever: door 1 of
`https://gtme.run/start.md` first.

## 1. Talk first

A few turns, plain questions, until you can play the plan back. Skip
what the first message already answered.

- Who are you trying to reach, and where does the list come from today?
  (a file, a tool like Apollo, a group from an earlier run)
- What makes one of them worth your time? What rules someone out?
- What do you want to happen to the good ones? (a campaign, a CRM row, a
  spreadsheet, a list to look at first)
- What do you want to say, and what would you need to know about someone
  to say it?
- Which tools and accounts do you have? (the vendor names; never the keys)
- Roughly how many, how often, and what is one person worth getting right?

## 2. Play it back

One paragraph in their words: source, the keep rule, what gets looked
up, what gets written, where it lands, what a first run costs, what a
re-run costs. Get a yes before building.

## 3. Options, when there are options

If the answers admit more than one honest shape, give two or three, one
line each, in their words, with the cost and the trade: what it knows
about each person, what it spends, whether a human reads before it sends.
Recommend one and say why. If there is only one sensible shape, say so
and move on.

Shapes to draw from, each a runnable bundle in the repo's `bundles/`
(read its README; copy its `pipeline.yaml` out as the start):
qualify → group → send; email waterfall; the account shape; events via
CSV + cron; posts to engagers. `gtme help --agent` has every adapter's
contract and four example pipelines.

## 4. Build one step at a time

Start from the nearest bundle's `pipeline.yaml` or from the source alone.
Add one step. `gtme plan pipeline.yaml`. Fix what it names. Next step.
Each plan error is then about one thing.

```sh
gtme plan pipeline.yaml
```

Rules of thumb the plan will not tell you:

- Gate paid steps behind a filter: judge on what the source already
  shows, pay to look up only the ones that passed.
- Deliver last, keyed on email (`idempotency: email`), and `on_missing:
  skip` so nobody is sent without the fields the copy needs.
- A judgment's `uses:` is what the model sees. Say so when you change it.
- A model step needs `ANTHROPIC_API_KEY` armed even though plan lists it
  as optional. Say which keys the human will set, by name only.

## 5. Hand off

When plan is clean, or fails only on `missing credential` (exit 3) for
keys the human will set: name those keys, then "Built. Next is a
rehearsal that spends nothing." Then `/gtme:run-pipeline`, which owns
simulate, dry-run, the arm gate, and the re-run.

## Do not

- Ask the human to read or write YAML. Show plans and receipts.
- Build the whole pipeline before the first `gtme plan`.
- Pick a shape for them when two would honestly do; recommend, then let them choose.
