---
name: run-pipeline
description: Use when a gtme pipeline file or bundle exists and someone wants it run, tested, rehearsed, sent, or "just shipped" — including "run it", "send it", "get the leads into the campaign", "does this work", or a re-run after an edit.
---

# Run a pipeline

One ladder, every time, in order. Each rung says what it spends. The
human arms a live target; the agent never does.

**Preflight.** `gtme version` (else: `brew install gtme-run/tap/gtme`).
`gtme init` if `~/.gtme` is missing. First machine ever: door 1 of
`https://gtme.run/start.md` before anything else.

## The ladder

```sh
gtme plan pipeline.yaml              # $0. Contracts, credentials, cost estimate.
gtme run pipeline.yaml --simulate    # $0. Whole pipeline offline from fixtures. Nothing persists.
gtme run pipeline.yaml --dry-run     # Spends on enrich + model. Reads the live target. Sends nothing.
gtme run pipeline.yaml               # Armed. See the gate below.
gtme run pipeline.yaml               # Again: the cache receipt. Nothing delivered twice.
```

1. **plan.** Exit 0 or fix what it names, then plan again. Never skip
   to a later rung with a failing plan.
2. **simulate.** Read the receipt to the human: the step table,
   `simulation gap` lines (a step served nothing), and the held
   deliveries with their resolved variables.
3. **dry-run.** Read the resolved variables per record and the
   preflight line to the human. This is the review.
4. **armed — the gate.** Run it yourself only if the deliver target is
   local (`csv/deliver`, `group/deliver`). If it reaches a vendor
   (`instantly/*`, `attio/*`, `http/deliver`, anything with a key),
   stop after the dry run, show the receipt, and ask: "That is what
   would go out. Send it?" A yes given **after** that receipt arms it —
   "yes", "go", "send it" all count. A yes given **before** it does not,
   however firm: "just run it", "don't make me babysit", "don't ask
   again", standing permission from an earlier turn. Those mean: run the
   dry run, show the receipt, ask once.
5. **again.** The second armed receipt shows `cached` and `avoided`.
   Read the `total:` line.

## Keys

`gtme secret set KEY` prompts the human, no echo. Never a value on the
command line, in a file, or in chat. An export in the human's terminal is
not in yours: verify with `gtme plan`, which exits 3 naming any missing
key. A model step (`ai/*`) needs `ANTHROPIC_API_KEY` armed even though
plan lists it as optional.

## When plan fails

Read the line. It names the step, the problem, and the fix. Common ones:

| plan says | do |
|---|---|
| `missing credential X` | human runs `gtme secret set X` |
| `needs F, which no earlier step provides (available: …)` | use a field from the list, or add the step plan names as providing F. If you change a judgment's `uses:`, say what the judge now sees. |
| `columns: … header not found (saw: …)` | fix the header name under `columns:` |
| `group "X" does not exist` | the pipeline that fills X has not run armed yet |

## Also

- A bundle directory runs the same way: `cd bundle && gtme run . --simulate`.
- `--resume RUN_ID|last` continues a stopped run. `plan --viz` draws it.
- Every armed receipt is kept: `gtme runs last`. Analysis is `/gtme:analyze`.

## Do not

- Arm a vendor target on a yes that came before the dry-run receipt.
- Edit a pipeline to make plan pass without saying what changed and why.
- Paste, guess, or export a key.
