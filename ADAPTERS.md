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
| `sql/transform` | enrich | runner-owned | derive fields with a read-only SELECT over the ledger — per-record or cross-record |
| `ai/filter` | filter | process (built-in) | LLM judgment → pass/fail verdicts with reasons |
| `sql/filter` | filter | runner-owned | deterministic verdicts from a SQL predicate |
| `ai/compose` | compose | process (built-in) | LLM writing → `first_line`, `ps_line`, or whatever the step's `provides:` declares |
| `instantly/add-to-campaign` | deliver | process (built-in) | add a lead to an Instantly campaign |
| `attio/assert` | deliver | **binding** | idempotent upsert of a person into Attio |
| `group/deliver` | deliver | runner-owned | hand records to a group — the next stage's source (ADR-032) |
| `http/deliver` | deliver | engine-inline | POST resolved variables to any URL |
| `csv/deliver` | deliver | process (built-in) | append delivered records to a reviewable CSV |
| `mock-enrich-py` | enrich | process (external, Python) | example proving the any-language adapter boundary |

## How adapters work

Every adapter — binding or process — presents the same three things to the
runner: a **contract** (`needs`/`provides` as JSON Schema over canonical
field names), a **config schema** (what its `with:` block accepts), and a
**role** (source, enrich, filter, verify, compose, deliver). That's all
`gtme plan` sees, which is why it can validate a whole pipeline without
caring how any step is implemented. At run time the runner projects
exactly the declared fields into the adapter, validates whatever comes
back against `provides` and the registry, and writes it to the ledger —
adapters never touch storage and never see more than their projection.

Most adapters are **bindings**: the entire implementation is a YAML
document the engine interprets. Here's a complete, working one — the
source adapter from the
[getting-started tutorial](https://www.elegantatomics.com/blog/getting-started-with-gtme),
annotated:

```yaml
id: jsonplaceholder/users     # how pipelines name it: use: jsonplaceholder/users
version: 1
role: source                  # source | enrich | deliver
entity_type: person

provides:                     # the contract — plan validates downstream
  type: object                # steps against exactly these fields
  additionalProperties: false
  properties:
    full_name: { type: string }
    email: { type: string }
    company_name: { type: string }
    company_domain: { type: string }
    jsonplaceholder.username: { type: string }   # vendor-namespaced: not canonical

config_schema:                # what `with:` accepts, validated at plan time
  type: object
  properties:
    limit: { type: integer, minimum: 1 }
    base_url: { type: string, default: "https://jsonplaceholder.typicode.com" }

request:                      # templated from config + (for enrich/deliver)
  method: GET                 # the record's own fields: {{record.email}} etc.
  url: "{{config.base_url}}/users"

extract:                      # response → canonical records
  records: "."                # dotted path to the record array
  fields:
    full_name: name                                    # plain path
    email: { path: email, transform: email }           # registry rule: lowercase + validate
    company_name: company.name                         # paths walk nested objects
    company_domain: { path: website, transform: domain } # rule: reduce to eTLD+1
    jsonplaceholder.username: username
```

Real vendors add the remaining primitives, declared the same way: `auth`
(where the credential goes and which env var holds it), `pagination`
(strategy, termination, page size), `errors` (status → verdict),
`idempotency: native | ledger` for deliver bindings, `cost`, and `retry`.
That's the whole vocabulary — about eight primitives, pinned by
[`spec/binding-schema.json`](spec/binding-schema.json). What a binding
deliberately *cannot* do is express logic: no conditionals, no
expressions, no multi-call flows. The `transform:` hook only accepts
named registry rules, so all judgment is frozen at authoring time and a
binding is safe to review by reading it.

The lifecycle: drop the file (plus `fixtures/conformance.json`, a saved
real response) into `~/.gtme/adapters/<name>/` and the id resolves
immediately — no build, no restart. The fixtures are the adapter's
conformance test *and* what `--simulate` serves, so a new adapter is
provable offline before its first live call.

When an integration genuinely needs logic — HarvestAPI's second
posts-call, Instantly's campaign-name resolution — it **graduates to a
process adapter**: an executable in any language reading NDJSON on stdin
and writing it on stdout (`OPEN` → `RECORD`s → `END`, spec'd in SPEC §5
with schemas in `spec/schemas/`), with a `manifest.json` declaring the
same contract surface. Same protocol as the built-ins, same conformance
bar, ~40 lines in Python for the shipped example
([`adapters/mock-enrich-py/`](adapters/mock-enrich-py/)).

## Universal per-step knobs

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

### `sql/transform`

Deterministic derivation in the ledger's own language. One read-only,
timeboxed SELECT per step; declared contracts (`uses:` and `provides:` in
config — never parsed from the SQL); results must include an
`identity_id` column and apply only to the run's records. Derived values
append like any adapter output, provenance `sql/transform @ <query-hash>`.

```yaml
- id: bucket
  use: sql/transform
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

A filter MAY also declare output fields with a step-level `provides:`
(ADR-033) — a list of names, or a map of name → `{type, enum}`:

```yaml
  - id: judge
    use: ai/filter
    uses: [title, company_name]
    provides:
      state: {enum: [now, later]}
      rationale: {}
    with:
      prompt: Decide when to work each contact, and why.
```

The required output shape in the prompt is generated from that schema;
the model's answer is validated against it (a value outside the enum is
retried once, then fails the batch — never stored); and the step emits
its VERDICT *and* a RECORD carrying the declared fields, for passing and
failing records alike, so the reasoning is queryable without a second
call. Declared fields land namespaced by pipeline — `qualify.state`,
`qualify.rationale` for a pipeline named `qualify` — so two campaigns'
judgments about one identity never collide; a later step reads them as
`uses: [qualify.state]`. A name written with a dot is kept as written.
To write a canonical field instead (global, shared across campaigns —
`first_line`, say, so a deliver step's `variables:` keep reaching it),
mark it `canonical: true`; the plan checks the name, type and domain
against the registry.
AI steps are entity-agnostic (`"entity_type": "*"` in the manifest — any
adapter may declare it): inside a company pipeline they plan and validate
against the company registry.

**Deferred, at half price (ADR-038).** `with: {deferred: true}` on an AI
step sends its batch to the Message Batches API (one request per record,
`custom_id` = identity key, the shared prompt cached across them) and
ends the run **`pending`** — the step must be the pipeline's last, so its
judgment lands in the `group:` terminus and a consumer pipeline pulls it.
The next `gtme run` of the pipeline collects (still processing → still
pending; run again later, from cron or by hand — nothing waits). Under
`--simulate`, or on `engine: claude-code` (no batch surface), the step
answers synchronously and says so. `gtme plan` warns when a judgment step
has nothing remembering its answers (add `exclude:` naming a group the
pipeline writes, or say `respend: true`).

**Prompt assembly (ADR-035).** The operator's prompt goes first, then the
batch — one compact JSON line per record, long lines wrapped at
structural breaks. Fields the pipeline *fetched* from the outside world
(`http/enrich` pages, provider bios; the runner knows from provenance)
leave the JSON line and arrive as a delimited block labelled in-band as
subject-supplied data, with any delimiter inside the page neutralised
first — so a homepage that says "ignore your instructions" reads as
evidence, not task. Default on; `with: {fence: false}` opts out. The
prompt/records split is exposed to the engine so a cache breakpoint sits
between them (the API engine caches the shared half).

Write queries against the vocabulary views — `current_values` (current
value per field, JSON unwrapped) and `group_membership` (membership by
`group_name`) — rather than the raw tables; `gtme plan` runs `EXPLAIN` so
an unknown column fails before anything runs, and annotates a query that
joins `relations` or membership as *cross-record* (it may read any
identity; only its results are run-scoped, and it recomputes every run).
Any value under any step's `with:` may be `{query: SQL}` or `{segment:
NAME}`, resolved read-only at plan (rows shown; zero rows is an error)
and recorded in the run. `gtme help --agent` carries the read surface and
the canonical query shapes.

### `sql/filter`

Same mechanism as `sql/transform`, producing verdicts: return a `pass`
column (with optional `reason`) to judge explicitly, or just return the
passing `identity_id`s — returned passes, absent fails, predicate named
in the reason. Closes the "has replied ever" / "3+ known contacts at this
company" cases without AI spend. Runs under `--simulate` (it's offline by
construction).

## Composers

### `ai/compose`

Batched LLM writing: provides `first_line` and `ps_line` by default, or
whatever the step's `provides:` declares (ADR-033 — same declaration,
same namespacing and validation as `ai/filter` above; a compose declaring
`provides: [subject, body]` in pipeline `outreach` writes
`outreach.subject` and `outreach.body` and nothing else; `provides:
{first_line: {canonical: true}, subject: {}}` writes canonical
`first_line` beside `outreach.subject`). Output is
validated against the schema with one retry on malformed output. `uses:`
declares what the prompt may reference — including fields `http/enrich`
fetched, which is how compose gets grounded in a prospect's actual
website.

## Deliverers

All deliver steps share the runner's guarantees: `variables:` egress
mapping, `on_missing` completeness (blank merge fields never send),
idempotency via the `deliveries` table, dry-run receipts, `record:` touch
scoping, and `suppress:` windows.

**A 2xx is not a delivery (ADR-036).** Every delivery lands `accepted` —
the provider took the request. `sent` is written only when a provider
attests execution (the `listen` verb, not built). An adapter whose
manifest declares `attests: true` re-reads what it just wrote and emits a
three-way verdict per record: `confirmed` (every non-blank field sent is
stored), `contradicted` (a stored value disagrees — the record fails; the
row is kept, marked, so nothing re-sends into a duplicate), or
`inconclusive` (the re-read failed or the shape was unrecognised — the
record advances, `accepted`, and the receipt names it). The receipt and
`gtme show` carry the status. Instantly is the first attesting adapter.

### `instantly/add-to-campaign`

Adds a lead to an Instantly campaign. Accepts a campaign *name* (resolved
to an id once per run — a deliberate process-adapter extra) or the id
itself. `variables:` targets matching Instantly's first-class lead fields
(`first_name`, `last_name`, `company_name`, `personalization`) map into
the lead body; anything else becomes a custom variable. Attests: after
the create it re-reads the lead (`GET /api/v2/leads/{id}`) and compares
every field it sent. Credential: `INSTANTLY_API_KEY`.

### `attio/assert` — binding

Asserts (upserts) a person into Attio by `matching_attribute` (default
`email_addresses`) — **idempotency is native**: re-delivering cannot
duplicate. `variables:` become attribute values on the record. Config:
`object` (default `people`). Credential: `ATTIO_API_KEY`. Pure YAML:
[spec/bindings/attio-assert/](spec/bindings/attio-assert/binding.yaml).

### `group/deliver` — the handoff (ADR-032)

Runner-owned, like the SQL steps: no adapter, no network. `use:
group/deliver` with `with: {group: <name>}` delivers each record *to a
group*, created on demand — the way one pipeline commits records to the
next stage under the same gate a send gets: `--dry-run` receipts the
resolved `variables:` per record for review, arming commits, delivery
idempotency (`group:<name>` is the target scope) means nothing is handed
off twice, and `suppress:`/`on_missing:`/`record:`/`require:`/`exclude:`
all apply. A pipeline may carry several. A group with no consumer is a
hold; release is `gtme groups add`, rejection `gtme groups remove --note
"why"`, review `gtme groups show`. Nothing runs the consumer — it pulls on
its own schedule, and a group source takes `limit: N` (oldest-added
first) to bound a day's work. One commit point per pipeline: `gtme plan`
warns when a handoff and a network-side deliver share one, because arming
approves both.

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
