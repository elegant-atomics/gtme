# qualify → group → send

The two-pipeline campaign: a cheap pipeline you run often decides who is
worth writing to and leaves them in a group; a deliberate one you arm by
hand takes the group, writes the lines, and sends. The boundary between
them is the review gate — `2-send` has its own dry-run receipt, and nobody
sends by re-running the qualifier.

```
1-qualify/   csv/source → demo/enrich (scored, $0.01 pretend) → sql/filter (score ≥ 50) → ai/filter (title) ⇒ group "qualified"
2-send/      group "qualified" → ai/compose (first_line, ps_line) → instantly/add-to-campaign ⇒ group "sent"
```

## Run it

```sh
cd bundles/qualify-group-send/1-qualify
gtme run . --simulate         # $0: 8 in, 5 scored high enough, the judge answers synthetically
gtme run .                    # armed, once: the pretend $0.08 lands in the ledger; needs ANTHROPIC_API_KEY
cd ../2-send
gtme run . --simulate         # $0: the 5 members sourced, lines written synthetically, delivery held
```

`2-send` sources a group, and a group lives in the ledger, not the bundle:
until `1-qualify` has run armed, `2-send` refuses to plan and says so
(`group "qualified" does not exist`). That refusal is the point — a
consumer bundle moved to a clean ledger fails loudly rather than running
ungated.

What the receipts show, in `1-qualify/receipt.txt` and `2-send/receipt.txt`:

- `score: 8 in, 8 out … $0.0800` — the enrichment is priced, so the second
  armed run prints `8 cached` and `$0.0800 avoided`; that line is why
  `demo/enrich` is here instead of a free lookup.
- `worth-a-model-call: 8 in, 5 out, 3 filtered` — the SQL gate spends
  nothing and keeps the model call for the records worth it.
- `fit: 5 in, 5 out` — under `--simulate` the judge answers synthetically
  and says so in provenance; armed, it is one batched model call.
- `2-send`: `send: 5 in, 0 out, 5 held` and the resolved variables per
  person — the receipt a human reads before arming.

## The rungs after simulate

```sh
gtme secret set ANTHROPIC_API_KEY
gtme secret set INSTANTLY_API_KEY
cd 2-send
gtme plan .                   # $0: contracts and credentials
gtme run . --dry-run          # spends on the model; reads the campaign; sends nothing
gtme run .                    # armed — a human runs this line, after reading the dry-run receipt
```

The campaign in `2-send/pipeline.yaml` is a placeholder id. A bundle
cannot be edited in place (its manifest would refuse it), so copy
`pipeline.yaml` out, set `campaign:` to your campaign's id or name (the
built-in adapter resolves names once per run), and run the copy with the
same commands. Re-runs deliver nothing twice: delivery is keyed on email.

## Make it yours

Replace `1-qualify/people.csv` under the same name — the manifest lists the
frozen files only, so the bundle still verifies — or point a copied
`pipeline.yaml` at your export and map its headers under `columns:`. The
judge's prompt and the score threshold are the two lines you will actually
tune; both live in `1-qualify/pipeline.yaml`.
