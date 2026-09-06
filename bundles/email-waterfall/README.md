# Email waterfall

Two finders and a verifier, and no waterfall syntax: N enrich steps that
each provide `email` are a waterfall by construction, because a step whose
provides are already current cache-skips (SPEC §7). Finder B is only ever
paid for the people finder A missed; the verifier only sees people with an
address; the CSV at the end holds only addresses that verified.

```
csv/source (3 people, no emails)
  → finder-a/email   ($0.005/record; 404 = nothing on file, skip)
  → finder-b/email   ($0.010/record; cache-skips anyone who already has an email)
  → sql/filter       (found: has an email)
  → verifier/email-status  ($0.003/record, writes email_status)
  → sql/filter       (deliverable: email_status = 'valid')
  → csv/deliver      (out.csv, keyed on email)
```

## Run it

```sh
cd bundles/email-waterfall
gtme run . --simulate         # $0: served from the fixtures inside the bundle
```

`receipt.txt` is that run. The lines that matter:

- `finder-a: 3 in, 1 out, 2 empty … $0.0050` — Jane found; Bob and Carol
  answered 404, which the binding maps to `skip`, so they continue with
  nothing written.
- `finder-b: 3 in, 1 out, 1 empty, 1 cached … $0.0100 avoided` — Jane's
  email is already current, so finder B is not asked and its price is
  counted as avoided; Bob found; Carol still nothing.
- `found: 3 in, 2 out, 1 filtered` — Carol stops here, with a reason,
  instead of failing the verifier's `needs`.
- `verify: 2 in, 2 out` — Jane `valid`, Bob `catch_all`; `deliverable`
  keeps one; `out` holds it with `email`, `name` and `status` resolved.

The people are keyed without an email — Jane by her LinkedIn URL, Carol by
the name hash `nh:…` — and the address a finder writes is a fact on that
identity, so a later run that sources the same rows coalesces onto it.

## The one thing the cache does not do

A miss is not cached: a 404 writes no field, so the next run asks finder A
about Bob and Carol again. That is the honest default — a finder may know
them next month — and the fix, when you want one, is a `sql/filter` ahead
of the finder over whatever your vendor returns on a miss.

## Make it yours

The three vendors here are fictional (reserved `.example` hosts) and their
fixtures are hand-written to the shape most finders share: a GET per
person, the address in the response, 404 for nothing. Your finder is a
binding of the same role dropped into `~/.gtme/adapters/<vendor>-email/`
— `gtme help --bindings` prints the contract, door 4 in `START.md` walks
it — with `use:` changed in a copy of `pipeline.yaml`. Put your finders in
the order you want to pay for them. The verifier's status values are the
vendor's; the `deliverable` filter names the ones you accept.

Armed, this bundle needs `FINDER_A_API_KEY`, `FINDER_B_API_KEY` and
`VERIFIER_API_KEY`, which no vendor issues: the armed run exists once the
stand-ins are yours.
