# Adapters

Every adapter that ships with gtme, what it does, and the config that
matters. Two always-true companions to this page: `gtme help --agent`
regenerates the full surface (every manifest, every flag) from the live
registry, and each adapter's contract lives in its manifest or binding —
this page is the human-readable tour, not a second source of truth.

Three kinds appear below:

- **binding** — pure YAML interpreted by the generic engine
  (`spec/bindings/`); cannot execute code; ships conformance fixtures that
  `--simulate` serves.
- **process** — a Go (or any-language) executable speaking NDJSON; used
  where an integration needs real logic.
- **runner-owned** — not an adapter at all: the runner executes it
  directly against the ledger (SQL steps, group sources). Listed here
  because you `use:` them the same way.

| id | role | kind | what it does |
|---|---|---|---|
| `csv/source` | source | process (built-in) | rows from a CSV, with `columns:` ingress mapping |
| `webhook/source` | source | process (built-in) | drains a spool file written by any webhook receiver |
| `source: {group: …}` | source | runner-owned | a group's current members, projected from the ledger |
| `apollo/search` | source | **binding** | Apollo people search, paginated |
| `harvest/profile` | enrich | process (built-in) | LinkedIn profile via HarvestAPI |
| `http/enrich` | enrich | engine-inline | fetch any URL per record → markdown field or JSON extraction |
| `sql/enrich` | enrich | runner-owned | derive fields with a read-only SELECT over the ledger |
| `ai/filter` | filter | process (built-in) | LLM judgment → pass/fail verdicts with reasons |
| `sql/filter` | filter | runner-owned | deterministic verdicts from a SQL predicate |
| `ai/compose` | compose | process (built-in) | LLM writing → `first_line`, `ps_line` |
| `instantly/add-to-campaign` | deliver | process (built-in) | add a lead to an Instantly campaign |
| `attio/assert` | deliver | **binding** | idempotent upsert of a person into Attio |
| `http/deliver` | deliver | engine-inline | POST resolved variables to any URL |
| `csv/deliver` | deliver | process (built-in) | append delivered records to a reviewable CSV |
| `mock-enrich-py` | enrich | process (external, Python) | example proving the any-language adapter boundary |

Universal knobs that work on (nearly) every step, regardless of adapter:
`cache: Nd` overrides the freshness window; `when: <step>.passed` gates on
a filter; `require:`/`exclude:` gate on group membership; deliver steps
take `variables:` (egress mapping), `idempotency:`, `on_missing:
skip|fail`, `record:` (touch scope), and `suppress: {group, within}`.

---

## Sources

### `csv/source`

Reads a CSV; the header row becomes field names. `columns:` maps canonical
names → your headers (`full_name: "Full Name"`); headers that already
match canonical names auto-map; near-misses are suggested at plan time,
never guessed. Unmapped headers are kept, namespaced `csv.<header>`.
Values are normalized at ingress (emails lowercased, domains reduced to
eTLD+1); a value that fails its rule is dropped from that record with the
reason recorded — never a crash. A mapping that yields no identity-key
path (person: no email, no LinkedIn URL, no name) is a plan error.

```yaml
source:
  use: csv/source
  with:
    path: contacts.csv
    columns: { full_name: Full Name, email: Email, company_domain: Company Website }
```

### `webhook/source`

The no-daemon answer to events: any commodity receiver (a Cloudflare
Worker, Zapier, a GitHub Action) appends JSON payloads to a spool file;
this adapter drains it like a CSV, marking consumed lines so a re-run
never re-sources them. Config: `spool_path`. Pair with cron for
event-driven pipelines.

### Group as a source

```yaml
source:
  group: q3-qualified
```

No adapter, no `use:` — the runner projects the group's current members
(people and companies alike) straight from the ledger. The consuming half
of the qualify/send decomposition. The group must exist at plan time.

### `apollo/search` — binding

Apollo `mixed_people/search`, paginated, mapped to canonical fields —
including the judgment calls: LinkedIn URLs routed by shape
(public/internal/sales-nav), Apollo's locked-email placeholder treated as
absent (never an identity key), domain fallback from `primary_domain` to
`website_url`. Config: `query` or `titles`/`seniorities`/`locations`/
`domains`, plus `limit`. Credential: `APOLLO_API_KEY`. The whole adapter
is [~150 lines of YAML](spec/bindings/apollo-search/binding.yaml).

## Enrichers

### `harvest/profile`

LinkedIn profile lookup via HarvestAPI. Needs *any one* LinkedIn URL shape
(public, internal, or Sales Navigator); when the lookup starts from a
non-public shape it also returns the resolved public `linkedin_url`, which
upgrades the identity key automatically. Provides headline, about,
location, role history, current role/company, and (config `posts_limit`)
recent posts. Credential: `HARVEST_API_KEY`; ~$0.012/profile; 30-day
freshness by default. Stays a process adapter on purpose: the posts call
and role-history formatting are logic a binding refuses to hold.

### `http/enrich`

The generic fetch enricher — the binding engine invoked inline. Two modes:
`markdown: true` + `field:` fetches a page and stores it as markdown under
your declared field; `extract:` maps a JSON response by dotted paths.
`freshness_days` is **required** (web content rots) and doubles as the
cache window — N AI steps across M runs reuse one fetch. `{{record.x}}`
placeholders in the URL are the step's plan-checked needs. 256 KB response
cap (oversized = dropped, never truncated); no-JS fetching only; raw
responses retained as payloads under the retention declaration.

```yaml
- id: fetch
  use: http/enrich
  with:
    url: "https://{{record.company_domain}}"
    markdown: true
    field: web.homepage
    freshness_days: 7
```

### `sql/enrich`

Deterministic derivation in the ledger's own language. One read-only,
timeboxed SELECT per step; declared contracts (`uses:` and `provides:` in
config — never parsed from the SQL); results must include an
`identity_id` column and apply only to the run's records. Derived values
append like any adapter output, provenance `sql/enrich @ <query-hash>`.

```yaml
- id: bucket
  use: sql/enrich
  with:
    uses: [title]
    provides: [sql.seniority_bucket]
    query: >
      SELECT identity_id, CASE WHEN ... END AS "sql.seniority_bucket"
      FROM current_fields WHERE field = 'title'
```

## Filters

### `ai/filter`

Batches records into one model call (default 25/batch) and returns
per-record verdicts with reasons — which land in the ledger, so prompt
tuning is a SQL query over what the model actually decided. Declare the
fields the prompt reads with `uses:`; they're plan-checked. Engine is
config (`engine: api | claude-code`), model overridable per step;
provenance records the model id. Credential: `ANTHROPIC_API_KEY`
(optional — the `claude-code` engine needs none).

### `sql/filter`

Same mechanism as `sql/enrich`, producing verdicts: return a `pass`
column (with optional `reason`) to judge explicitly, or just return the
passing `identity_id`s — returned passes, absent fails, predicate named
in the reason. Closes the "has replied ever" / "3+ known contacts at this
company" cases without AI spend. Runs under `--simulate` (it's offline by
construction).

## Composers

### `ai/compose`

Batched LLM writing: provides `first_line` and `ps_line`, output
validated against schema with one retry on malformed output. `uses:`
declares what the prompt may reference — including fields `http/enrich`
fetched, which is how compose gets grounded in a prospect's actual
website.

## Deliverers

All deliver steps share the runner's guarantees: `variables:` egress
mapping, `on_missing` completeness (blank merge fields never send),
idempotency via the `deliveries` table, dry-run receipts, `record:` touch
scoping, and `suppress:` windows.

### `instantly/add-to-campaign`

Adds a lead to an Instantly campaign. Accepts a campaign *name* (resolved
to an id once per run — a deliberate process-adapter extra) or the id
itself. `variables:` targets matching Instantly's first-class lead fields
(`first_name`, `last_name`, `company_name`, `personalization`) map into
the lead body; anything else becomes a custom variable. Credential:
`INSTANTLY_API_KEY`.

### `attio/assert` — binding

Asserts (upserts) a person into Attio by `matching_attribute` (default
`email_addresses`) — **idempotency is native**: re-delivering cannot
duplicate. `variables:` become attribute values on the record. Config:
`object` (default `people`). Credential: `ATTIO_API_KEY`. Pure YAML:
[spec/bindings/attio-assert/](spec/bindings/attio-assert/binding.yaml).

### `http/deliver`

POST the resolved variables to any URL — the universal Out. The default
body is the variables object; a `body:` template overrides; `auth:` in
config resolves through the same credential machinery as everything else.
The step-level `idempotency:` key is **required** — a generic target
cannot infer delivery semantics, so you must say what makes a delivery
"the same one."

### `csv/deliver`

Writes delivered records to a CSV: `identity_key` plus the `variables:`
targets as columns (sorted, header written once, rows appended across
runs). Universal output to anything with an import button, and the
natural human-review artifact. Re-runs append nothing — idempotency holds
records back before the adapter is invoked.

## The example external adapter

### `mock-enrich-py`

A ~40-line Python script proving the process-adapter boundary is real:
reads the NDJSON protocol on stdin, adds a `mock.score` field, exits.
Installed to `~/.gtme/adapters/` by `install.sh`. If you're writing a
process adapter in any language, start by reading it.

---

## Adding your own

Drop a `binding.yaml` (plus `fixtures/conformance.json`) into
`~/.gtme/adapters/<name>/` and the id resolves immediately — no build, no
restart. The [getting-started tutorial](https://www.elegantatomics.com/blog/getting-started-with-gtme)
walks authoring one against a live API; [CONTRIBUTING.md](CONTRIBUTING.md)
has the checklist for bindings worth sharing.
