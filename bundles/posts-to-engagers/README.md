# Posts to engagers, via a traverse

Start from people you know, find what they posted, find who reacted, and
keep the reactors worth a look. Two traverses (SPEC §6, ADR-054): the run
opens on `person`, crosses to `post`, and crosses back to `person` — a
different set of people, related to the first by the posts between them.
The ledger ends up holding the graph: `authored_by` from each post to its
author, `engaged_with` from each reactor to the post.

```
csv/source (3 people)
  → sql/filter               (has-profile: a LinkedIn URL to read)
  → harvest/profile-posts    (traverse person → post, limit 2 per person, authored_by)
  → harvest/post-reactions   (traverse post → person, engaged_with)
  → ai/filter                (worth-a-look: runs marketing, growth, demand gen or RevOps)
  ⇒ group "warm-engagers"    (typed person)
```

## Run it

```sh
cd bundles/posts-to-engagers
gtme run . --simulate         # $0: both traverses served from the fixtures in the bundle
```

`receipt.txt` is that run. Read the two traverse lines:

- `posts: 2 parent(s) in, 2 out, 0 empty — 2 traversed (post), 1
  coalesced` — Jane has three posts and `limit: 2` keeps two; Bob's one
  post is a repost of Jane's second, so it reaches the run already known
  and coalesces instead of minting twice.
- `engagers: 2 parent(s) in, 2 out, 0 empty — 4 traversed (person), 2
  coalesced` — six reactions across the two posts: Bob reacted to Jane's
  post and is already in the run (a source person, now also an engager);
  Dave reacted to both and is minted once. Four new people.
- `worth-a-look: 5 in, 5 out` — under `--simulate` the judge passes
  synthetically; armed, the recruiter drops.
- `group "warm-engagers": 5 record(s) would be added` — the terminus
  takes the last segment's type, so the group holds people, and only
  people who completed the run.

Then look at the graph it would have built (an armed run keeps it):

```sh
gtme groups show warm-engagers
gtme query "SELECT relation, count(*) FROM relations GROUP BY relation"
gtme show https://www.linkedin.com/posts/jane-doe_outbound-activity-7123456789 --provenance
```

## The bindings, and why their fixtures are hand-written

`harvest/profile-posts` and `harvest/post-reactions` are traverse bindings
shaped from HarvestAPI's documented endpoints (`GET /linkedin/profile-posts`,
`GET /linkedin/post-reactions`). No shipped binding had `role: traverse`
with fixtures, so these were written for this pattern, and their fixtures
are **written to the documented response shape, not recorded from the
API** — each `fixtures/conformance.json` says so in its `note`. The
reactor profile URLs come in the id form the docs describe
(`/in/ACoAAB…`), which the registry's `linkedin` rule sorts into
`linkedin_internal_url`; that is the key those people carry until a
`harvest/profile` step fills in the rest.

Before an armed run, record one real, sanitized response per endpoint
over the hand-written ones and re-verify:

```sh
gtme secret set HARVEST_API_KEY
gtme adapters verify harvest/profile-posts     # fixtures in, records out, the hosts it would call
gtme adapters verify harvest/post-reactions
gtme plan .                                    # $0: person → post → person, with the relations
gtme run . --dry-run                           # spends on HarvestAPI; the traverses execute; nothing sends
```

A traverse is spend as at a source: `--dry-run` crosses for real. Nothing
here delivers — the group is the output, and a send is its own pipeline
(`qualify-group-send/2-send` is the shape) sourcing `warm-engagers`.

## Make it yours

`people.csv` under the same name, with a `LinkedIn URL` column. Raise
`limit:` on the posts step to read further back, and rewrite the judge's
prompt for the people you want to meet.
