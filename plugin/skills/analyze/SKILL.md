---
name: analyze
description: Use when someone asks what gtme did, what it cost, what the cache saved, why a record was dropped or not contacted, what is known about a person or company and where each fact came from, who is in a group, or wants a number from past runs.
---

# Analyze

Every answer is in the ledger. Read it; do not infer from a receipt.
`gtme query` is read-only by construction, so nothing here can spend or
send.

## Which question, which command

| question | command |
|---|---|
| what ran, when, status | `gtme runs` |
| what one run did and cost | `gtme runs last` (or a run id) |
| what is true about one record, who wrote each fact, when | `gtme show <key> --provenance` |
| which records a run touched, and how far each got | `gtme show --run last` (NDJSON on stdout) |
| why one record stopped | `step_events` for that identity: the `done` row of the filter carries `pass` and `reason` |
| who is in a group, who wrote it | `gtme groups`, `gtme groups show <group>` |
| the exact pipeline a run used | `gtme freeze <run>` |
| anything across records: spend by provider, who was contacted, the graph | `gtme query` |

```sh
gtme runs
gtme runs last
gtme groups
gtme query "SELECT provider, round(sum(amount_usd),4) AS usd, count(*) AS n FROM costs GROUP BY provider" --format table
gtme query "SELECT relation, count(*) AS n FROM relations GROUP BY relation" --format table
```

## The model

`gtme help --agent` prints one JSON line; the `ledger` key has the
tables, the views and a query shape per question. Read it with a parser,
not by eye:

```sh
gtme help --agent | jq .ledger
```

Views to reach for: `current_values` (field, value per identity),
`relations` (`works_at`, `authored_by`, `engaged_with`),
`group_membership`, `deliveries`, `costs`, `step_events`, `run_records`
(state and verdicts per record per run).

## Facts that trip people

- Fields are namespaced: a CSV header that is not canonical is
  `csv.<header>`; a judgment's declared outputs are `<pipeline>.<field>`.
- `avoided` dollars print on the live receipt's `total:` line. A
  reconstructed receipt (`gtme runs <id>`) shows `cached` counts only; the
  dollars are `skipped_cache` events × that step's per-record estimate.
- Costs carry a `basis`: `estimated` or `measured`. `provider = 'demo'`
  is pretend by design. A `fixture` engine row is a rehearsal, not a
  judgment.
- Keep a useful query: `gtme query "..." --save NAME`; `gtme query
  --list` shows the saved ones, and a saved name works as a
  `{segment: NAME}` config value in a pipeline.

## Do not

- Answer a "why" from the step table alone. The reason is in `step_events`.
- Run a pipeline to find out what it did. The last run is already in the ledger.
