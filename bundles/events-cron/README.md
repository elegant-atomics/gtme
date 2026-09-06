# Events via CSV + cron

Run a pipeline when something happens, with no daemon (SPEC §8, ADR-009):
a receiver you already have appends each event as one CSV row, and cron
runs the pipeline over the file. Rows are re-sourced every run; identity
coalescing, the judgment cache and delivery idempotency make that cheap and
safe, so the receiver may deliver the same event twice and the campaign
still gets each person once.

```
csv/source (events.csv — a row per event)
  → sql/filter        (paid-trial: event = trial_started AND plan IN team, business)
  → ai/compose        (first_line, ps_line — a welcome note)
  → instantly/add-to-campaign  (keyed on email)
  ⇒ group "welcomed"
```

## Run it

```sh
cd bundles/events-cron
gtme run . --simulate         # $0: 5 rows, 4 people, 2 paid trials, 2 held
```

`receipt.txt` is that run. `source: sourced 4 records (1 coalesced into
known identities)` is the receiver's retry: the file holds Jane's
`trial_started` twice and the ledger holds Jane once. `paid-trial: 4 in, 2
out, 2 filtered` drops the free trial and the teammate invite. `welcome: 2
in, 0 out, 2 held` with the resolved variables is what an armed run would
send.

## Wire it up

**The receiver.** Anything that can append a line to a file: a Cloudflare
Worker writing to a mounted volume, a Zapier or Make webhook action with a
"append row" step, a GitHub Action, a five-line script behind a URL. One
row per event, the header once:

```
email,full_name,company_domain,event,plan,occurred_at
```

Headers that match canonical names map themselves; the rest arrive as
`csv.<header>` (here `csv.event`, `csv.plan`, `csv.occurred_at`) and are
what the filter reads.

**The schedule.** Copy `pipeline.yaml` out beside the file the receiver
writes, set the campaign id, then:

```sh
gtme secret set ANTHROPIC_API_KEY
gtme secret set INSTANTLY_API_KEY
gtme plan trial-welcome.yaml            # $0
gtme run  trial-welcome.yaml --dry-run  # the first real batch, held: read it
```

and once the dry-run receipt reads right, the cron line — every fifteen
minutes, say:

```
*/15 * * * *  cd /srv/events && gtme run trial-welcome.yaml >> gtme.log 2>&1
```

Every run leaves a receipt in the log and a row in `gtme runs`; a run
where nothing new arrived reads `welcome: N in, 0 out, N cached`. Nothing
is delivered twice because the `deliveries` table is unique per target and
email, and nothing is re-judged because the compose's judgment is cached
per record and prompt.

## What it is not

Per-event latency: the pipeline runs on the schedule, not on the event.
And the file grows; when re-sourcing the whole history stops being cheap,
rotate it — the ledger, not the file, is the record of who was welcomed.
