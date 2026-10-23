# web-harvester

Self-hosted web data acquisition in Go, with two front ends on one
acquisition stack:

- **A general-purpose crawler for any website** (`cmd/crawl`): start URLs,
  link following within a scope, generic page data (metadata, headings,
  OpenGraph, JSON-LD, text, links) plus CSS-selector extraction rules per
  site, plain HTTP or headless Chromium, robots.txt obeyed by default. See
  [Crawling any website](#crawling-any-website).
- **A large-scale Google SERP pipeline** (`cmd/harvester`): structured
  extraction of organic results, featured snippets, "people also ask" and AI
  Overviews, direct or through a third-party SERP provider, with drift
  detection when the page layout stops matching.

Shared underneath: a concurrent worker pool, proxy health tracking with
pluggable rotation strategies and escalating cooldowns, adaptive rate
limiting that honors `Retry-After`, block classification (CAPTCHAs,
challenges, rate limits; detected and routed around, never bypassed), a
distributed queue (Redis Streams) for scaling across processes and hosts,
PostgreSQL or JSON-Lines persistence, and Prometheus metrics with a Grafana
dashboard.

Formerly `serp-harvester`. Runtime identifiers keep that name so existing
deployments, queues and dashboards carry on unchanged: the
`serp_harvester_*` metric prefix, the `serp-harvester:queries` Redis stream,
the systemd units and `/opt/serp-harvester` paths, and the Docker Compose
service names.

It runs end to end, offline, with `go run ./cmd/harvester` — no API keys, no
proxies, no network access required.

## At a glance

- **SERP pipeline**: Google SERP collection (organic, featured snippet,
  "people also ask", AI Overview including its page-token follow-up),
  direct-to-Google (plain HTTP or headless Chromium) or via a third-party
  provider, behind one interface.
- **Any website**: `cmd/crawl` crawls arbitrary sites with the same proxy
  pool, adaptive per-host rate limiting, block classification and
  HTTP/Chromium fetchers, extracting title, metadata, headings, OpenGraph,
  JSON-LD, text and links from every page plus per-site CSS-selector fields
  and item lists. See [Crawling any website](#crawling-any-website).
- **Deployment**: self-hosted. `docker compose -f deploy/docker-compose.yml up -d`
  runs the full stack (harvester + Redis + Prometheus + Grafana) on your own
  infrastructure. No Apify dependency.
- **Scaling**: Redis Streams distributed queue, horizontal worker scaling,
  per-proxy rate limiting, retry/backoff, Prometheus metrics.
- **Acquisition resilience** (self-hosted, no SERP API needed): every fetch
  result is classified (rate limit, CAPTCHA, consent wall, JS check,
  timeout, network, 4xx/5xx) and drives adaptive per-proxy throttling,
  health-based proxy cooldowns, failover to another proxy or to the browser,
  and retries. Blocks are detected and routed around, never bypassed. See
  [Acquisition resilience](#acquisition-resilience).
- **Proxy provider measurement**: `cmd/proxybench` measures a residential or
  mobile proxy provider the way a scraper experiences it: rotation, sticky
  sessions, state-level targeting, provider error codes, retries and billed
  bytes. See [Measuring proxy providers](#measuring-proxy-providers).
- **Reproducible testing**: `make integration` / `make e2e` run the suite and
  the deployed Playwright image against real Redis, PostgreSQL and Chromium
  in Docker; the same tests run without Docker against any local services.
- **Job submission**: an HTTP API (`cmd/api`) for `POST /jobs` +
  `GET /jobs/{run_id}`, plus real cron-based scheduled harvesting — both as
  pure queue producers — see [Job/API layer](#jobapi-layer).
- **Capacity targets**: plan for **1 million queries/day at initial
  launch**, with a scale target of **10 million queries/day**. These are
  engineering targets, not a claim that live third-party acquisition has
  already been validated at those volumes. See [Load testing](#load-testing)
  for measured orchestration results and their limits.
- **Production history**: none yet — this is a new build. See
  [CAPABILITIES.md](CAPABILITIES.md) for the direct, no-spin answer to that
  question and the other four this project needs to answer.
- **Limitations**: documented in full, not glossed over — see
  [Honest limitations](#honest-limitations).

- **Recent search evaluation**: a 50-query mixed-difficulty run returned at least one mapped organic result for 49 queries (98%); median latency was 1.39s and p95 was 4.37s. See [Recent search evaluation](#recent-search-evaluation) for evaluation details.

## US proxy benchmark

A two-phase benchmark run on 19–20 October 2026 used Web Harvester to measure US state targeting and session behaviour across residential and mobile proxy pools.

### State targeting

| Measure | Residential | Mobile |
|---|---:|---:|
| Requests tested | 1,530 | 1,530 |
| Exits geolocated to the requested state | 1,502 (98.2%) | 1,312 (85.8%) |

The mobile result should be interpreted cautiously: mobile carriers often route traffic through regional gateways, so IP geolocation may place an exit in a different state from the device or session's intended location. State-level results are a measurement of observed exit geolocation, not a guarantee of physical device location.

### Sticky sessions vs. rotation

The protected-page test used 100 requests per arm across four alternating blocks, with residential and mobile results measured separately.

| Measure | Result |
|---|---:|
| Rotating sessions: usable responses | 1,175/1,200 (97.9%) |
| Sticky sessions, renewed every 15 requests: usable responses | 600/600 (100%) |
| Sticky-session latency advantage | About 100–230 ms across the six comparisons |

In these runs, renewed sticky sessions were faster in all six comparisons. A single long-lived sticky session performed substantially worse: only 16–49 of 100 responses were usable per run before the target returned HTTP 4xx responses. This reflects the tested target's protection behaviour, not a universal proxy-provider result.

### Method

Requests ran from GitHub-hosted runners in US Azure regions. Residential and mobile pools were evaluated separately; each request used a fresh tunnel, no retries were used, and failures were counted rather than hidden. IP addresses were hashed in the output, and credentials were excluded. Geolocation was checked against ipinfo.io.

These are dated, workload-specific observations, not guarantees for other sites, proxy networks, locations or traffic patterns. The benchmark demonstrates Web Harvester's measurement workflow as well as the results.

## Recent search evaluation

A recent 50-query evaluation covered 25 general, mixed-topic searches and 25 more difficult flight-related searches, including a Shanghai-to-Brussels query.

| Measure | Result |
|---|---:|
| Queries tested | 50 |
| Queries with at least one mapped organic result | 49/50 (98%) |
| General / mixed-topic queries with results | 25/25 (100%) |
| Difficult flight-related queries with results | 24/25 (96%) |
| Median request latency | 1.39 s |
| 95th-percentile request latency | 4.37 s |

The evaluation covered both broad everyday searches and more demanding flight-related queries. These results provide an initial view of search-result availability and response latency across a mixed query set.

Proxy-provider benchmarks are evaluated and reported separately. They measure proxy behaviour such as state targeting, sticky versus rotating sessions, latency, retries and billed traffic; they are not combined with search-result availability or latency figures.

## Third-party deal-discovery integration pilot

On 22 October 2026, Web Harvester completed a successful third-party
integration run for [Deal Intelligence](https://deal-intelligence-app.vercel.app).
The workflow passed its offline regression tests, validated its configuration,
ran external search, fetched source pages through the configured acquisition
path, compared the results with the live canonical dataset, and uploaded a
review artifact.

| Measure | Observed result |
|---|---:|
| External search queries completed successfully | 8/8 |
| Unique result URLs discovered | 74 |
| URLs with recent publisher dates | 65 |
| Source pages attempted | 30 |
| Source pages fetched successfully | 22/30 |
| Existing Deal Intelligence records loaded for comparison | 4,506 |


**What this demonstrates:** a third-party search provider was connected to
Web Harvester, search results were collected, source pages were fetched through
the configured proxy, and the resulting discovery set was compared with
Deal Intelligence's visible canonical dataset. The automated workflow and
regression checks passed, and a review artifact was generated. The run was
review-only and made no production database writes.

This is a bounded integration validation, not a claim of comprehensive market
coverage or analyst-verified deal discovery. It verifies that the third-party
search-to-acquisition-to-dataset-comparison workflow runs end to end. Human
review of candidate quality remains a separate step; no candidate-count metric
is presented here as a measure of success.

[View the successful GitHub Actions run and review artifact](https://github.com/HenryMorganDibie/web-harvester/actions/runs/39872441752).

Full document index: [CAPABILITIES.md](CAPABILITIES.md) (answers to the five
questions this was built for) · [PRODUCTION_READINESS.md](PRODUCTION_READINESS.md)
(architecture-to-operations checklist) · [RUNBOOK.md](RUNBOOK.md) (what to
do when a metric looks wrong) · [deploy/](deploy/) (Docker Compose / systemd).

## Why this exists

The operating target is **1 million queries/day at initial launch**,
scaling toward **10 million queries/day** as deployment capacity grows. The
queueing, concurrency, proxy rotation, rate limiting, structured extraction,
and observability are the foundation for that scale. These are target volumes,
not a claim that live Google collection has already been load-tested at those
levels. Real throughput depends on deployment size, provider limits, proxy
capacity, target-site policies and the workload. See
[Honest limitations](#honest-limitations) for what is built and what still
needs validation at production volume.

## Quickstart

```bash
go test ./...
go run ./cmd/harvester -config configs/config.example.yaml -mode mock
```

This runs the full pipeline — queue, worker pool, proxy rotation, rate
limiter, fetch, parse, sink — against bundled fixture HTML, and prints
JSON-Lines results to stdout and periodic throughput metrics to stderr:

```
[metrics] elapsed=2s success=5 failure=0 retried=0 dropped=0 completed=5 req/s=2.50 projected/day=216000
```

## Crawling any website

`cmd/crawl` applies the harvester's acquisition stack to arbitrary sites.
Each target in a config (see `configs/crawl.example.yaml`) says where to
start, how far to go, how to fetch and what to extract:

```bash
# Ad hoc: the given pages and everything they link to on the same host
go run ./cmd/crawl -url https://example.com/ -depth 1 -i-have-reviewed-tos

# Configured targets, results to pages.jsonl (or sink_backend: postgres)
go run ./cmd/crawl -config configs/crawl.example.yaml -i-have-reviewed-tos
```

What every page yields (`model.Page`, one JSON line or one row in the
`pages` table): URL, final URL after redirects, depth, status, content type,
latency and proxy; title, meta description, canonical URL, language, h1-h3
headings, OpenGraph tags, every JSON-LD block (schema.org products,
articles, events, organizations), visible text (capped) and absolute links.
On top of that a target can define:

```yaml
extract:
  fields:                                  # one value, or a list with all: true
    category: { selector: "h1", required: true }
  items:                                   # one object per repeated element
    selector: ".product-card"
    required: true
    fields:
      name:  { selector: ".title" }
      price: { selector: ".price" }
      url:   { selector: "a", attr: href }  # href/src resolved to absolute URLs
```

`required` works like the SERP parser's drift detection: a page where a
required field or the item selector matches nothing is flagged
(`extraction.missing`) with its HTML kept, instead of being stored as a
quietly empty result. Selectors are validated at startup.

Crawling: `start_urls`, `max_depth` (0 = only the start URLs), `max_pages`
(default 100), `allowed_domains` (default: the start hosts; subdomains
included), `follow` (a CSS selector for which links to follow, e.g. only
pagination), and `include` / `exclude` regular expressions. URLs are
normalized and fetched once; non-HTML responses are recorded but not
parsed or followed, and links to binaries (images, archives, documents)
are not followed.

Fetching (`render`): `http` (default); `browser`, headless Chromium for
every page; or `auto`, plain HTTP with Chromium only for pages that are
client-side app shells (executable scripts, almost no server-rendered
text), which is usually a small share of a site.

Politeness and blocks:

- **robots.txt is obeyed by default** (RFC 9309: longest match, `*` and
  `$` patterns, 4xx = no rules, 5xx or unreachable = disallow all), and
  `Crawl-delay` is honored per host. `respect_robots: false` exists for
  sites you own or have permission to crawl.
- `rate_per_host_rps` is the rate to one host **across all proxies**
  (default 1 req/s); adaptive throttling lowers it for a host that answers
  429 or blocks. Each target has its own proxy pool, so a block on one
  site cools an egress IP for that site only.
- Bot-protection pages on arbitrary sites are classified, not interacted
  with: Cloudflare challenges, DataDome and PerimeterX walls are outcome
  `challenge`; a reCAPTCHA/hCaptcha/Turnstile widget counts as `captcha`
  only on an error response (the same widgets on an ordinary contact or
  login form are not a block). A challenged page is never retried in the
  browser; its egress cools and the URL is recorded as failed.
- The crawler identifies itself (`user_agent`, matched against robots.txt
  via `robots_agent`) and refuses to run without `-i-have-reviewed-tos`.

What is verified: the crawler, extraction, robots.txt handling and the
`pages` table are tested against local sites and real PostgreSQL, and
`render: auto` against real Chromium (`internal/crawl`, `internal/extract`,
`internal/robots`, `internal/store`). It has not been run against third-party
sites at volume, and how a given site responds (rate limits, bot
protection) is only known by measuring it at a low rate first.

## Architecture

```
jobs (query+run_id+locale+device)
  │
  ▼
queue.Source ──▶ worker.Pool (N goroutines)
(Memory or           │
 Redis Streams)       ├─▶ proxy.Pool.Next()      (health-tracked rotation: round-robin, random, or weighted-by-success-rate)
                       ├─▶ ratelimit.Limiter.Wait (token bucket, keyed per proxy; honors provider Retry-After on 429)
                       ├─▶ fetcher.Fetcher.Fetch  (mock fixtures, live HTTP, headless Chromium via Playwright, HTTP->browser hybrid, or a third-party provider)
                       ├─▶ worker.Parser.Parse    (HTML or provider-JSON, structured extraction + drift detection)
                       └─▶ store.Sink.Write       (JSON-Lines, or PostgreSQL with JSONB columns)

metrics.Counters ──▶ periodic req/s + projected-daily-volume reporting, and
                      a /metrics + /healthz endpoint for Prometheus (-metrics-addr)
```

Every stage is an interface (`Fetcher`, `worker.Parser`, `Sink`, `queue.Source`)
or a small struct (`proxy.Pool`, `ratelimit.Limiter`) so each one is
independently swappable and independently testable. Note what this diagram
does *not* have: the API/scheduler layer (see "Job/API layer" and
"Scheduled harvesting" below, where they exist) only ever produces jobs onto
`queue.Source` — it never calls `Fetcher` or `Parser` itself. That boundary
is deliberate: it's what lets the acquisition backend change (direct HTTP vs.
provider) without the job-submission layer knowing or caring. Going from
here to a 10M+/day production system is a matter of:

| This repo today                                       | Production                                                        |
|---------------------------------------------------------|----------------------------------------------------------------------|
| `queue.MemorySource` or `queue.RedisStreamSource`        | Already the same interface Kafka/SQS would sit behind                |
| JSON-Lines sink, or PostgreSQL with JSONB columns        | Already real; add a warehouse-streaming `Sink` if that's preferred   |
| Proxy pool with health tracking + 3 rotation strategies  | Already real; point `proxy.Pool.Reload` at a vendor's proxy-list API |
| Plain `net/http`, headless Chromium (Playwright), or a third-party provider fetch | Rendering is built (`mode: playwright`); direct-to-Google still needs a compliant answer to CAPTCHAs, blocks and selector maintenance, or just more provider budget |
| Single process, N goroutines                             | N processes across M hosts, all pointed at the same Redis stream (or Kafka/SQS) |
| Selectors tuned to fixture HTML                           | Selectors tuned to live Google markup, versioned and monitored for drift (see below) |
| `println` + Prometheus counters, no alerting              | Alerting rules on top of the same `/metrics` (see `deploy/prometheus/alerts.yml` where present) |

The reason this table exists instead of the repo pretending to already be
the production system: the remaining right-hand items depend on decisions
and resources (proxy budget, legal sign-off, target markets/locales) that
belong to the client, not to a codebase alone.

## Handling selector drift

Google's SERP markup is obfuscated and changes without notice. A scraper
that silently returns an empty result when its selectors stop matching is a
liability — it fails quietly, and nobody notices until a report downstream
is wrong. This pipeline treats "expected block not found" as a distinct
outcome from "found and empty":

```go
if len(result.Organic) == 0 {
    missing = append(missing, "organic_results")
}
if len(missing) > 0 {
    result.Calibration = &model.CalibrationNote{MissingBlocks: missing}
}
```

See `internal/parser/testdata/uncalibrated.html` and the corresponding test
for a worked example: a page whose organic-results wrapper class changed
still parses without error, but comes back flagged for review instead of
silently empty. In production, calibration-flagged results route to a
review queue (and would typically be paired with archiving the raw HTML for
whoever is retuning selectors that week) instead of being written to the
same sink as trusted results.

## Distributed queue (Redis Streams)

`internal/queue.RedisStreamSource` consumes jobs from a Redis Stream via a
consumer group, so any number of harvester processes — on one host or
spread across many — share one durable queue instead of each needing its
own list of queries. This is the concrete mechanism behind "10M/day is many
processes across many hosts": they all point at the same stream and group,
and Redis divides the work.

```yaml
queue_backend: redis
redis_addr: "localhost:6379"
redis_stream: "serp-harvester:queries"
redis_group: "harvesters"
redis_consumer: "worker-1"   # unique per process
redis_seed_queue: true        # local demo only: push `queries` before consuming
```

Run two consumers against the same stream (different `redis_consumer`
values, or the same via `docker-compose up redis` for local testing) and
Redis hands each message to exactly one of them, re-delivering to another
consumer if one dies mid-job without acknowledging — see
`internal/queue/redis_stream_test.go` (run against an in-memory `miniredis`,
no real Redis server needed for `go test`).

## Job/API layer

`cmd/api` submits work over HTTP instead of a fixed query list in a config
file — `POST /jobs` enqueues one query per item onto the same Redis Stream
`cmd/harvester` consumes from, and `GET /jobs/{run_id}` reports progress.

```bash
curl -X POST localhost:8080/jobs -d '{"queries": ["best laptops 2026"], "country": "US", "language": "en", "device": "mobile"}'
# {"run_id": "run-19ef9a1e45ab0f9d", "queries_accepted": 1}

curl localhost:8080/jobs/run-19ef9a1e45ab0f9d
# {"run_id": "...", "submitted": 1, "completed": 1, "completed_known": true, "submitted_known": true}
```

This is strictly a **producer**: `internal/api.Server` depends only on
`queue.Producer` (`PushJob`) — it never imports `Fetcher` or `Parser`. That
boundary is deliberate (see "Architecture" above): the job-submission layer
can't accidentally grow into doing acquisition work itself, which is what
keeps direct-HTTP vs. provider swappable underneath it without the API
knowing or caring. `completed` comes from `store.PostgresSink.CountByRunID`
when `sink_backend: postgres` is configured on the harvester side and the
API is pointed at the same database; without Postgres, completion is
honestly reported as unknown (`completed: -1, completed_known: false`)
rather than a fabricated number.

**Tested end-to-end against real infrastructure, not just unit tests:** a
real Redis and a real Postgres were both stood up, `cmd/api` and
`cmd/harvester` run as separate processes against them, a job posted via
curl, and the result verified by querying Postgres directly — `run_id`,
`locale` (`US-en`), and `device` (`mobile`) all landed correctly on the row
the harvester wrote after consuming the job the API pushed. `internal/api/server_test.go`
covers the handler logic in isolation (8 tests: enqueue-per-query, producer
failure, unknown run ID, no-counter-configured, etc.) with a fake producer,
so CI doesn't need real infrastructure to verify the logic — the real-infra
run was a one-time manual confirmation against local Docker containers, not
something re-run automatically on every commit today.

### Scheduled harvesting

`cmd/api -schedule your-schedule.yaml` also runs recurring harvests —
`internal/scheduler` wraps a real cron parser (`robfig/cron`), so "query set
A at 10:00, query set B at 10:15" is an actual 5-field cron expression, not
a bespoke interval format:

```yaml
harvests:
  - name: "morning-set-a"
    cron: "0 10 * * *"
    queries: ["best noise cancelling headphones 2026", "best laptops for students 2026"]
    country: "US"
    language: "en"
```

Every tick gets a fresh `run_id` and pushes through the same
`queue.Producer` the job/API layer uses — the scheduler is a producer too,
never a fetcher. **Tested for real, not just unit tests:** a schedule with a
3-second interval was run against the real Redis container for 10+ seconds
and the stream's length was confirmed growing on schedule (4 → 14 → 16
entries), not just that `AddFunc` was called. `internal/scheduler/scheduler_test.go`
covers cron-expression validation, per-tick job pushing, and fresh-run-ID-per-tick
using a fast `@every 100ms` schedule so CI stays quick.

## Proxy management

`internal/proxy.Pool` tracks per-proxy health and cools proxies down based
on what their requests ran into (`Pool.ReportOutcome`):

| Outcome through a proxy                  | Effect |
|-------------------------------------------|--------|
| success                                   | resets the failure streak and cooldown escalation |
| timeout, network error, 5xx               | counts toward `proxy_ban_fails` in a row, then cooldown |
| CAPTCHA / "unusual traffic" / JS-check page | cooldown immediately |
| 429 with `Retry-After`                    | cooldown for exactly `Retry-After`, no escalation |
| 429 without `Retry-After`                 | cooldown immediately |
| consent wall, 4xx, cancellation           | nothing: not the proxy's fault |

Repeat cooldowns double (`proxy_ban_cooldown`, then 2x, 4x ... up to
`proxy_ban_cooldown_max`) until the proxy succeeds again. Each proxy also
keeps a recent-health score (an exponentially weighted success average), and
there are three rotation strategies:

```yaml
proxy_strategy: round_robin   # default: predictable, fair
# proxy_strategy: random               # avoids synchronized patterns across many processes
# proxy_strategy: weighted_success_rate # biases toward proxies with better *recent* health
```

When every proxy is cooling, a job waits for the soonest one to recover (up
to `max_proxy_wait`) instead of burning its retries.

`Pool.Reload(urls)` swaps the proxy list at runtime — the seam for pointing
at a vendor's proxy-list API or a file watcher instead of a static config
list — and preserves accumulated health stats for any URL that stays in the
list. `Pool.AllStats()` returns a per-proxy snapshot (success/failure
counts, ban state, last-used time) for an operator dashboard or a health
endpoint. Authenticated proxies (`http://user:pass@host:port`) work with no
special handling — Go's `net/http` applies the Basic auth automatically, for
both plain HTTP proxying and HTTPS `CONNECT` tunneling, confirmed by
`TestHTTPFetcher_AuthenticatedProxy` against a fake proxy server that
verifies the header actually arrives. See `internal/proxy/pool_test.go`
(16 tests, including two real bugs the suite caught: `Reload` originally
lost health stats for retained proxies due to a map that was only populated
on the empty-list code path; and a proxy that kept failing after its first
cooldown was never banned again, because the ban only triggered when the
failure count *equalled* the threshold. `TestPool_RebansAfterCooldownExpires`
fails against the old code).

## Measuring proxy providers

Before routing a workload through a residential or mobile proxy provider,
it is worth knowing how that provider behaves from the scraper's side, not
from its marketing page. `cmd/proxybench` is a small, auditable runner for
exactly that, and `.github/workflows/anyip-proxy-benchmark.yml` runs it from
GitHub Actions with the proxy URL held in a repository secret.

What it measures per request, written as one JSON line each plus a summary:

| Question | How |
|----------|-----|
| Does rotating mode really rotate? | A fresh tunnel per request by default, because Go reuses a kept-alive `CONNECT` tunnel and its exit IP; exit IPs and tunnel reuse are recorded |
| Does a sticky session hold its exit? | Distinct exit IPs and IP changes per session; `-session-renew N` caps how many requests one exit carries |
| Does state targeting land in the requested state? | With the geolocation service as the target, the state is read from the measured request itself; a separate probe would use a different exit under rotation |
| Is the response usable, not just a 200? | `-expect-substring` requires text that only a correct page contains, so challenge and consent pages count as failures |
| Why did a request fail? | The provider's machine-readable error header (for example `no_peers_available`) is captured from refused tunnels; transport errors, timeouts and target status codes are classified separately |
| Do retries recover? | `-retries`, optionally on a new sticky session, with first-attempt versus eventual success, recovery time and bytes spent on failed attempts |
| What does it cost? | Body bytes and every byte exchanged with the proxy, to reconcile with the provider's own usage figures |

Summaries record the commit, every setting, Wilson confidence intervals for
small samples, and a note when a p95 rests on too few requests to be stable.
Exit IPs can be stored as salted hashes (`-hash-ips`), credentials and API
tokens are redacted from every recorded error, and the runner refuses to send
traffic without a proxy configured.

```bash
go build -o bin/proxybench ./cmd/proxybench
read -rs HARVESTER_PROXY_URL && export HARVESTER_PROXY_URL   # paste; nothing is echoed

./bin/proxybench -profile rotating -type residential -requests 30 -hash-ips \
  -target-url https://ipinfo.io/json -geo-url https://ipinfo.io/json -out rotating.jsonl
```

Session, network-type and US-state options are written in anyIP's documented
username syntax, the provider this was first used with; the measurement
itself is provider-neutral. The full protocol (phases, sample sizes, budget,
and what each result can and cannot show) is in
[docs/anyip-benchmark-runbook.md](docs/anyip-benchmark-runbook.md).

What this does not do: it measures a provider, it does not certify one, and
it does not get around anti-bot protection. A target that blocks an exit is
recorded as blocked. Results depend on time of day, client location and the
geolocation database used, so they belong in a dated report with their
settings, not in this README.

## Persistent storage (PostgreSQL)

JSON-Lines (`store.JSONLSink`) is fine for the pipeline itself; for a client
who wants to query results — by run, by query, by date range, by whether AI
Overview was present — `store.PostgresSink` stores each result as a row with
real columns (`run_id`, `query`, `locale`, `device`, `fetched_at`,
`latency_ms`, `proxy_used`) plus the parsed SERP fields as native JSONB
(`organic`, `featured_snippet`, `ai_overview`, `people_also_ask`,
`calibration`), queryable with Postgres's own JSON operators instead of
being an opaque blob:

```yaml
sink_backend: postgres
postgres_dsn_env: POSTGRES_DSN   # read from the environment, never from this file
```

```bash
export POSTGRES_DSN="postgres://user@host:5432/dbname?sslmode=disable"
export PGPASSWORD="..."   # pgx reads the password from here; keep it out of the DSN
go run ./cmd/harvester -config your-config.yaml
```

Schema (`CREATE TABLE IF NOT EXISTS`) is applied automatically on startup —
safe to run on every deploy, no separate migration tool for this scope. When
a result is calibration-flagged (selector drift), the raw pre-parse response
body is stored in `raw_response` for whoever is retuning selectors —
otherwise that column stays NULL, keeping normal rows small.

**Tested against a real Postgres, not just mocked:** `internal/store/postgres_test.go`
runs against an actual PostgreSQL instance (gated behind `POSTGRES_TEST_DSN`,
skipped in normal CI the same way `tests/live` is) — write-then-read-back of
every JSONB column, schema idempotency across repeated startups, and a
fast-fail on an invalid DSN. Beyond the automated tests, the full CLI was
run end-to-end against a real local Postgres container (`mode: mock`,
`sink_backend: postgres`) and the resulting rows were queried directly:
organic results, AI Overview presence, and calibration+raw-body capture all
landed correctly.

## Third-party provider fetcher

`internal/fetcher.ProviderFetcher` calls a SerpApi-compatible JSON API
(`GET {base_url}?engine=google&q=...&api_key=...`) instead of requesting
Google directly, and `internal/parser.JSONParser` maps that provider's
structured JSON onto the same `model.SerpResult` the HTML parser produces —
so the queue, worker pool, retries, proxy rotation, metrics, and sink are
identical regardless of which fetch path is active.

```yaml
mode: provider
provider_base_url: "https://serpapi.com/search"
provider_api_key_env: "SERPAPI_KEY"   # read from the environment, never from this file
provider_engine: "google"
```

```bash
export SERPAPI_KEY=...
go run ./cmd/harvester -config your-config.yaml -mode provider
```

This is the honest answer to "handle CAPTCHAs and blocks": a licensed SERP
data provider has already solved consent walls, CAPTCHAs, and proxy scale
as their product. Plugging into one means the pipeline being delivered here
(queue, worker pool, retries, parsing, metrics, ops) is real and owned by
this codebase, while the legally risky edge — automated querying of Google
directly — is a paid vendor's product, not code written here to defeat it.

**AI Overview's documented two-step fetch is handled.** SerpApi documents
that AI Overview content sometimes comes back as only a `page_token` (valid
~4 minutes) rather than inline `text_blocks`, requiring a follow-up request
with `engine=google_ai_overview&page_token=...`. `ProviderFetcher.Fetch`
detects this automatically, makes the follow-up request, and merges the
resolved content back in before `JSONParser` ever sees the body — so the
two-step flow is invisible to everything downstream. If the follow-up fails
(expired token, network error) the original body is still returned
unmodified: organic results and everything else still parse, and that
query's result just comes back with no AI Overview, the same as if the SERP
genuinely had none. See `TestProviderFetcher_ResolvesAIOverviewPageToken`,
`_InlineAIOverviewSkipsFollowUp`, and `_AIOverviewFollowUpFailureIsNonFatal`
in `internal/fetcher/provider_test.go` — verified against a synthetic
two-request fixture, per the same no-real-key caveat below.

**Rate limits are handled per the provider's own signal, not guessed.** A
429 response is parsed into a `*fetcher.RateLimitError` carrying whatever
`Retry-After` the provider sent (seconds or HTTP-date form, per RFC 9110),
and `worker.Pool` waits out that exact duration before retrying instead of
applying its generic exponential backoff —
`TestPool_HonorsRateLimitRetryAfter` confirms this by timing an actual
retry cycle. Three more tests
(`TestProviderFetcher_RateLimitWithRetryAfterSeconds` /
`_HTTPDate` / `_WithoutRetryAfter`) cover both `Retry-After` formats and the
case where none is given.

**No API key yet? Nothing else in this repo needs one.** Mock mode requires
none, `go test ./...` never touches the network, and
`internal/fetcher/provider_test.go` / `internal/parser/json_provider_test.go`
verify the full request/response and JSON-mapping cycle against a local
`httptest` server and a realistic fixture, so this path is fully tested
without a real key. What hasn't been verified yet, precisely because no key
exists to test with, is a real provider's actual response matching this
schema exactly — copy `.env.example` to `.env`, set whichever variable name
`provider_api_key_env` points at once a trial or paid key exists, and it
works with zero code changes. Worth a quick real-key smoke test before
relying on this path for a client, since a vendor's exact field names are
usually close but not guaranteed identical to what `json_provider.go`
currently expects.

## Observability (Prometheus metrics)

```yaml
metrics_addr: ":9090"
```

```bash
curl localhost:9090/metrics
# serp_harvester_success_total 42
# serp_harvester_failure_total 3
# serp_harvester_dropped_total 0
# serp_harvester_retried_total 5
# serp_harvester_fetch_outcomes_total{outcome="captcha"} 1   # every attempt, by outcome (incl. crawl: challenge)
# serp_harvester_proxy_cooldowns_total{reason="blocked"} 1   # failures | blocked | rate_limited
# serp_harvester_proxies_available 2 / serp_harvester_proxies_cooling 1 / serp_harvester_proxies_throttled 1
# serp_harvester_ratelimit_decreases_total 3                 # adaptive throttling steps
# serp_harvester_fetch_failovers_total 12                    # hybrid: HTTP -> browser
# serp_harvester_http_consent_handled_total 1
# serp_harvester_robots_disallowed_total 4                   # crawl: URLs skipped for robots.txt
# mode: playwright/hybrid add serp_harvester_browser_* (launches, launch
# failures, disconnects, page crashes, navigation timeouts, consent handled,
# blocked{reason}, sessions open/in use)
```

The provisioned Grafana dashboard (`deploy/grafana/provisioning/dashboards/`)
charts these alongside the original panels: fetch outcomes, cooldowns by
reason, proxy availability/cooling/throttling, blocked share, throttling and
failover rates, and the browser row (sessions, launches, crashes, blocked
pages by reason).

`internal/metrics.PrometheusCollector` reads the same atomic counters the
`println` reporter uses, on every scrape — so a client's existing
Prometheus/Grafana stack can alert on failure rate, dropped-job rate, and
throughput without this repo needing to know anything about their
dashboards. See `internal/metrics/prometheus_test.go`.

## Load testing

`cmd/loadtest` measures the pipeline's own orchestration overhead —
queue → worker pool → proxy pool → rate limiter → mock fetch → parse →
discard sink — at high concurrency, entirely offline. It answers one
specific question: does this codebase's own plumbing bottleneck before a
proxy budget or target site would? It does **not** measure real network
throughput against Google or any other live target. Three modes:

```bash
go run ./cmd/loadtest -n 100000 -concurrency 800            # fixed-count benchmark
go run ./cmd/loadtest -sweep 1,4,8,16,50,200,800 -n 3000     # concurrency sweep, one table
go run ./cmd/loadtest -duration 90m -sample-interval 2m      # soak test
```

### Concurrency sweep

Real numbers measured on the development machine this was built on (mock
fetcher, simulated 20-80ms latency per fetch, zero network calls). The
first three rows are from `-sweep`; the last three are earlier fixed-count
runs from before per-request latency tracking existed, kept here rather
than re-run purely for a prettier table — the `req/s` and `0 drops` figures
are directly comparable either way, only the latency columns differ:

| Concurrency | Queries | Wall time | Throughput | p50 | p95 | p99 | Failures/Drops |
|-------------|---------|-----------|------------|-----|-----|-----|----------------|
| 1           | 2,000   | 1m39.7s   | ~20 req/s  | 49ms | 78ms | 80ms | 0 / 0 |
| 4           | 2,000   | 25.9s     | ~77 req/s  | 52ms | 78ms | 90ms | 0 / 0 |
| 8           | 2,000   | 12.6s     | ~159 req/s | 50ms | 78ms | 80ms | 0 / 0 |
| 50          | 20,000  | 20.2s     | ~990 req/s | n/a (pre-latency-tracking run) | | | 0 / 0 |
| 200         | 50,000  | 12.7s     | ~3,945 req/s | n/a | | | 0 / 0 |
| 800         | 100,000 | 6.5s      | ~15,290 req/s | n/a | | | 0 / 0 |

Throughput scales roughly linearly with concurrency here because the
bottleneck is the mock fetcher's simulated per-request latency, not
pipeline overhead — exactly the property you want: the orchestration layer
gets out of the way, and real-world throughput becomes a function of proxy
count and target-site latency, not this codebase. Note concurrency=8
(~159 req/s) already clears the ~116 req/s that 10M/day works out to, in
this orchestration-only benchmark.

### Soak test

A sustained run tracks throughput, latency percentiles, memory, and
goroutine count over time via `-duration`, so stability can be checked
directly rather than assumed from a short benchmark:

```text
[soak 10s] success=19486 failure=0 dropped=0 interval_rps=1949 p50=51ms p95=77ms p99=81ms alloc=2MB goroutines=103
[soak 20s] success=39019 failure=0 dropped=0 interval_rps=1953 p50=50ms p95=78ms p99=81ms alloc=2MB goroutines=103
[soak 30s] success=58691 failure=97 dropped=0 interval_rps=1967 p50=51ms p95=77ms p99=80ms alloc=3MB goroutines=100
```

(30-second smoke test shown above, concurrency=100 — stable throughput,
stable memory, stable goroutine count across samples; the 97 failures at
the 30s mark are jobs in flight exactly at the shutdown cutoff hitting
context cancellation, not a fetch failure — see `PRODUCTION_READINESS.md`
§5 for the accounting fix this run surfaced.)

**Full 90-minute run, completed:**

```text
actual elapsed: 1h30m0s (requested: 1h30m0s)
total success=20756754 failure=186 dropped=0 retried=186
ai_overview=6918916 calibration_flagged=6918920 proxy_banned=0
latency (last 200000 samples): min=20ms p50=50ms p95=77ms p99=79ms max=200ms
average throughput: 3844 req/s => 332098180/day if sustained
final memory: alloc=5MB sys=61MB goroutines=1
```

Concurrency 200, sampled every 2 minutes for the full run: throughput held
at 3,700-4,000 req/s the entire 90 minutes with no degradation, `alloc`
oscillated between 3-7MB the whole time with no upward trend (no memory
leak), and goroutine count stayed exactly at 203 across all 45 samples
before winding down to 1 at shutdown (no goroutine leak). `dropped=0` across
20.76 million requests processed — the 186 `failure`/`retried` count is
first-attempt failures exactly at the process's hard 90-minute cutoff that
were then retried successfully, not lost work; nothing was dropped.
`ai_overview` and `calibration_flagged` landing within 4 of each other
(6,918,916 vs. 6,918,920) is the expected internal consistency check given
the 3-fixture rotation the mock fetcher cycles through.

This is still orchestration-only evidence (mock fetcher, zero network), not
a live-throughput claim — but it's real, sustained, and reproducible:
`go run ./cmd/loadtest -duration 90m -sample-interval 2m -concurrency 200`.

## Live mode

```bash
go run ./cmd/harvester -config configs/config.example.yaml -mode live -i-have-reviewed-tos
```

Live mode issues real HTTP requests directly to the configured endpoint
(default: Google Search) via a plain `net/http` client. It deliberately:

- requires an explicit `-i-have-reviewed-tos` flag (the tool refuses to run
  otherwise, and prints why),
- defaults to a conservative rate (`rate_per_proxy_rps: 1` in the example
  config),
- carries a cookie jar so a consent decision persists across requests within
  a run (`internal/fetcher/http.go`), the same way a browser remembers you
  clicked "I agree", and submits a consent page's own "reject all" form when
  one appears,
- reports what came back precisely instead of handing every page to the
  parser: 429 (with `Retry-After`), CAPTCHA, JS-check shell, consent wall and
  4xx/5xx are distinct, classified errors (see
  [Acquisition resilience](#acquisition-resilience)),
- does **not** implement headless rendering, CAPTCHA solving, or browser
  fingerprint spoofing.

That last point is a deliberate scope boundary, not an oversight — see
[Honest limitations](#honest-limitations) below, and use
[the provider fetcher](#third-party-provider-fetcher) instead for anything
beyond light, occasional direct queries.

**Tested result:** even with the cookie jar and a pre-seeded consent cookie,
live requests against `https://www.google.com/search` from this environment
never reached a results page — confirmed by inspecting the raw response
each time. Two different outcomes were observed across separate attempts,
neither of which is real search results:

1. The "Before you continue to Google Search" consent interstitial (status
   200, page title matches, no results markup).
2. A JavaScript-execution check (`/httpservice/retry/enablejs`, "click here
   if you are not redirected") — no consent form present at all, no cookie
   or header fixes this one, since it requires an actual JS engine to
   execute and follow through, which is browser-automation territory, not a
   fetch-layer change.

Persisting whatever cookies Google's own responses set (`SEARCH_SAMESITE`,
`AEC`, `__Secure-ENID`) across repeated requests — ordinary cookie-jar
behavior, not fabricating anything — did not change either outcome. That's
reported here rather than glossed over: this isn't a single small gap with
one fix, it's Google's layered defenses doing what they're designed to do,
and getting a plain, undisguised HTTP client past them reliably is already
nontrivial before CAPTCHAs, rate limiting, or volume enter the picture at
all.

## Browser mode (Playwright)

```bash
make playwright-install   # Playwright driver + Chromium (scripts/install-playwright.sh), matched to go.mod
go run ./cmd/harvester -config your-config.yaml -mode playwright -i-have-reviewed-tos
```

`mode: playwright` (`internal/fetcher/playwright.go`) renders
`live_endpoint`'s results page in headless Chromium and returns the
rendered DOM, which goes through the same HTML parser as live mode. It is
an **acquisition backend only**: it lets content that Google renders with
JavaScript reach the parser, and it gets past the JS-execution check that
stops a plain HTTP client (see [Live mode](#live-mode)). It does not make
direct-to-Google scraping production-ready. Google can and does still
answer a real browser with CAPTCHAs, "unusual traffic" pages, consent
walls, rate limits and other blocks, and this mode does not try to get
past any of them.

What it does:

- **Same pipeline.** It implements `fetcher.Fetcher`; the worker pool,
  proxy pool, rate limiter, retries, parser, sinks and metrics are
  unchanged. Each fetch uses the proxy the pool handed out (authenticated
  proxies included) and waits on the same per-proxy rate limit. There is no
  second proxy or rate-limit implementation.
- **Same request semantics.** `q`/`hl`/`gl` are sent exactly as in live
  mode. The job's locale becomes the browser locale (and so
  `Accept-Language`), and its device (`desktop`, `mobile`, `tablet`)
  becomes viewport emulation. The User-Agent is the worker pool's honest
  `web-harvester` string, unchanged. `request_timeout` bounds each fetch,
  and cancellation (shutdown, SIGTERM) aborts the in-flight navigation.
- **Bounded browser pool.** One Chromium process is shared. Each fetch
  borrows a session (a browser context plus a page) from a pool capped at
  `browser_pool_size`, keyed by proxy, device, locale and User-Agent, since
  those are fixed per context. Sessions are reused, so cookies persist like
  live mode's cookie jar, and are recycled after `browser_max_session_uses`
  fetches or any failure. A crashed browser is relaunched on the next fetch.
  Shutdown closes pages, contexts, the browser and the driver.
- **Consent pages.** The same `CONSENT` cookie live mode sends is
  pre-seeded. If a consent page still appears, the fetcher submits that
  page's own **"reject all"** form, the choice any visitor can make, and
  continues. A consent page without such a form is reported as blocked.
- **Blocks are reported, not bypassed.** A CAPTCHA or "unusual traffic"
  page, a consent page it can't dismiss, or a JS-check interstitial that
  persists after rendering returns a `*fetcher.BlockedError`. That counts
  as a failed request against the proxy's health (so a challenged proxy
  goes into cooldown) and increments `serp_harvester_browser_blocked_total`.
  A plain 429 returns the usual `RateLimitError`, so `Retry-After` is
  honored.
- **Metrics.** Launches, unexpected disconnects, navigation timeouts,
  consent pages handled, blocked pages by reason, and open/in-use sessions,
  alongside the existing counters (see [Observability](#observability-prometheus-metrics)).

What it deliberately does not do: CAPTCHA solving, stealth plugins,
fingerprint spoofing or randomization, patching automation markers, or any
other anti-bot evasion.

Settings (all optional; existing configs are unaffected):

```yaml
mode: playwright
request_timeout: 30s          # per fetch: page load + consent step + wait selector
browser_pool_size: 4          # max open sessions; workers beyond it wait
browser_max_session_uses: 50
browser_wait_selector: "#search"   # awaited after load; if absent the page is still returned and drift-flagged
browser_executable_path: ""   # empty = Playwright's headless shell (recommended)
browser_headful: false        # local debugging only
```

Deployment: `deploy/playwright.Dockerfile` (official Playwright base image
with Chromium preinstalled) and an opt-in `harvester-playwright` Compose service
(`--profile playwright`); see [deploy/README.md](deploy/README.md).

Tests (`internal/fetcher/playwright_*_test.go`,
`internal/worker/playwright_pipeline_test.go`) drive real Chromium against
local `httptest` fixtures only: JS rendering into the parser, query/locale/
device/User-Agent passthrough, consent reject-all, CAPTCHA reported and never
submitted, 429 with `Retry-After`, navigation timeout, cancellation, an
authenticating proxy, the bounded pool, crash recovery, and shutdown. They
need Chromium, so they run with `SERP_HARVESTER_PLAYWRIGHT=1`
(`make test-playwright`); CI installs Chromium and runs them.

**Not yet verified against live Google.** Everything above is tested
against local fixtures. Whether a real Google results page renders into
results, which blocks appear, and at what rate, depends on egress IP,
proxies, locale and volume, and has not been measured.
`HARVESTER_LIVE=true go test ./tests/live/... -run Playwright -v` records
what actually comes back, the same way the live-mode test does.

Known limitations of this mode:

- The parser's selectors still target the bundled fixtures, not live Google
  markup (see [Honest limitations](#honest-limitations) item 1). Rendered
  Google pages will be drift-flagged until the selectors are retargeted.
- A browser session costs roughly 100 to 300MB of memory and far more CPU
  than an HTTP request; plan capacity per session, not per request.
- Use Playwright's headless shell (the default). A full Chrome/Chromium
  build (`browser_executable_path` pointing at one, or `browser_headful`)
  makes its own background requests to Google services that bypass the
  per-context proxy and the rate limiter.
- One browser per process: a Chromium crash fails that process's in-flight
  fetches (they are retried) before the relaunch.

## Acquisition resilience

The self-hosted path (`live`, `playwright`, `hybrid`) needs no paid SERP API.
What makes it robust is not getting past blocks, which it deliberately does
not try to do, but reacting to them correctly. Every fetch result is
classified once (`internal/fetcher/classify.go`) and that outcome drives
everything else in the worker (`internal/worker/pool.go`):

| Outcome | Detected by | Retry | Proxy | Throttle |
|---------|-------------|-------|-------|----------|
| `rate_limited` | HTTP 429 | on another proxy at once; waits `Retry-After` if none | cooldown = `Retry-After` | halve that proxy's rate |
| `captcha` | `/sorry/` URL, CAPTCHA form, reCAPTCHA markers | on another proxy at once | cooldown now, escalating | halve |
| `interstitial` | JS-check redirect or `<noscript>` enablejs shell | another proxy; in `hybrid`, the browser renders it | cooldown now, escalating | halve |
| `consent` | consent host or form, when no "reject all" form works | yes; in `hybrid`, the browser | none | none |
| `timeout`, `transport`, `http_5xx` | net errors, `net::ERR_*`, status | yes, with backoff | counts toward `proxy_ban_fails` | none |
| `http_4xx` | status | no: the job ends | none | none |

- **Adaptive per-proxy throttling** (`adaptive_rate`, default on): the
  per-proxy token bucket is halved on a rate limit or block (down to
  `rate_min_rps`) and recovers by 5% of the configured rate per success.
- **Consent**: both fetchers pre-seed the same consent cookie, and when a
  consent page still appears they submit its **"reject all"** form: a POST
  in the HTTP fetcher, a click in the browser. A consent page without such a
  form is reported, not guessed at.
- **Failover** (`mode: hybrid`): plain HTTP first, and the browser only for a
  JS-check page or a consent wall HTTP couldn't dismiss
  (`fetcher.FailoverFetcher`). CAPTCHA and rate-limit responses are never
  retried in the browser; a browser doesn't make them go away.
- **Browser recovery** (`playwright`, `hybrid`): a crashed Chromium is
  relaunched on the next fetch, with exponential launch backoff (1s to 30s)
  if the launch itself fails; a crashed page (renderer) or a closed page is
  discarded and replaced; sessions are recycled after
  `browser_max_session_uses` or any failure.
- **Retries** stop early for errors a retry can't fix (4xx), and every wait
  (backoff, cooldown, rate limit) ends immediately on shutdown.

Not implemented, on purpose: CAPTCHA solving, fingerprint spoofing or
randomisation, stealth patches, or anything else meant to defeat bot
detection.

**What is verified, and where.** Unit tests cover classification, the HTTP
fetcher against scripted local servers, cooldown escalation, AIMD
throttling and the worker's failover decisions. `tests/integration` runs the
whole pipeline in `hybrid` mode against real Redis, PostgreSQL and Chromium,
through three scripted local proxies (one always CAPTCHA'd, one
rate-limited, one good), and checks that every result lands in PostgreSQL,
that the CAPTCHA'd proxy is used exactly once, and that blocks, cooldowns,
throttling and failovers appear in the metrics. `make e2e` does the same
with the deployed `harvester-playwright` image. All of it uses a fictional
site (`tests/integration/fixture`); **none of it has been run against live
Google**, so the real block rate, and how well these reactions hold up
against it, are unmeasured.

### Running the tests with or without Docker

```bash
go test ./...                     # unit tests; browser/DB tests skip without their services
make integration                  # everything, in Docker: real Redis, PostgreSQL, Chromium
make e2e                          # the harvester-playwright image end to end, against the fixture site
```

Docker is only a convenience for these runs; the application itself never
needs it.

### Measuring against Google

Two gated live tests are the tools for tuning against the real target (see
`tests/live/README.md`): `TestGoogleCapture_SavesPages` saves the pages
Google actually returns (results or block pages, via HTTP and Chromium) as
the material for retargeting the parser, and
`TestGoogleCalibration_MeasuresPushback` sends a capped number of queries at
a fixed pace and reports when pushback starts, any `Retry-After`, and how
long until success resumes, with suggested `rate_per_proxy_rps` and
`proxy_ban_cooldown` values.

First live run (2026-09-25, from a cloud datacenter egress IP, no proxy):

| Fetcher | Requests | What Google returned | Classified as |
|---|---|---|---|
| HTTP | 3, 10s apart | JS-check shell (`<noscript>` refresh to `/httpservice/retry/enablejs`) | `interstitial` |
| Chromium | 3, 10s apart | `/sorry/` "unusual traffic" CAPTCHA page | `captcha` |
| Chromium (calibration) | 20, 30s apart over 9.5 min | `/sorry/` CAPTCHA on every request | `captcha` (20/20) |

Second run (2026-09-25, from a residential/consumer connection, no proxy):

| Fetcher | Requests | What Google returned | Classified as |
|---|---|---|---|
| HTTP | 3, 10s apart | JS-check shell, as from the datacenter IP | `interstitial` |
| Chromium (headless, in Docker) | 3, 10s apart | consent page (dismissed via reject-all), then `/sorry/` CAPTCHA on the first request | `captcha` (3/3) |

What that establishes: the block and interstitial classifiers match Google's
real markup on both paths (previously only synthetic fixtures), and from that
egress Google blocks the first request, so the block is IP-reputation-driven,
not rate-driven, and had not cleared after 9.5 minutes at 2 req/min. What it
does not establish: no results page was served, so the parser is still not
validated against live markup, and no rate or cooldown value can be derived
from this egress. Defaults are unchanged; the observation is only consistent
with keeping `proxy_ban_cooldown_max` at 10m or more. Repeating the capture
through egress Google does not block on sight (`SERP_LIVE_PROXY`, or a run
from a residential connection) was the next step, and was tried; see
below.

What the second run adds: plain HTTP gets the JS-check page regardless of
egress, so direct collection needs a browser; and headless Chromium was
refused on its first request from a consumer connection as well as a
datacenter one. Two egress types are not enough to separate IP reputation
from automated-browser detection, but they are enough to conclude that
direct Google collection is not dependable with this stack, which by design
does no fingerprint masking or CAPTCHA solving. For Google results, `mode:
provider` (a third-party SERP API, see "Third-party provider fetcher") is
the dependable path; the crawler (`cmd/crawl`) covers other sites.

The integration suite (not live Google) runs against any local services:

```bash
make playwright-install
INTEGRATION=1 REDIS_ADDR=localhost:6379 \
  POSTGRES_TEST_DSN=postgres://user:pass@localhost:5432/db?sslmode=disable \
  SERP_HARVESTER_PLAYWRIGHT=1 go test ./...
```

## Honest limitations

Overselling readiness here would be a worse outcome than being precise about
where the real work is — especially once this is running against a real
client's volume, not just a pitch:

1. **Parser selectors target the bundled fixtures, not live Google markup.**
   Google's actual DOM is obfuscated and changes on its own schedule.
   Retargeting selectors to current production markup — and keeping them
   correct as Google ships layout changes — is real, ongoing work. The
   fixtures here (`internal/parser/testdata/*.html`) are deliberately
   generic, clearly-labeled synthetic HTML, not captured Google pages —
   redistributing real scraped Google markup in this repo isn't something
   it does. What's demonstrated instead is the extraction *architecture*:
   structured output, optional-block handling, and drift detection, which
   the real selectors plug into unchanged. Using the
   [provider fetcher](#third-party-provider-fetcher) instead sidesteps this
   entirely, since the provider returns already-structured JSON.

2. **AI Overview content is frequently rendered client-side.** A
   request/response HTTP fetch (what `HTTPFetcher` does) won't see content
   that Google's frontend renders via JS after the initial page load in
   some surfaces. [Browser mode](#browser-mode-playwright) now renders
   those pages in headless Chromium, but only as acquisition: it ships no
   evasion toolkit, the AI Overview selectors are still fixture-targeted,
   and rendering against live Google is not yet verified. The provider
   fetcher sidesteps this too: providers that support AI Overview
   extraction return it as structured JSON already (see
   `internal/parser/json_provider.go`).

3. **Direct Google collection was tested, but did not return usable results.**
   Live requests returned JavaScript checks and CAPTCHA challenges; browser
   mode reports those pages as blocked and stops. No CAPTCHA solving or
   anti-detection/evasion measures are implemented. A separate live integration
   pilot subsequently validated the third-party search-provider path end to end:
   8/8 queries completed, 74 unique result URLs were collected, 22/30 source
   pages were fetched, and results were compared with 4,506 existing Deal
   Intelligence records to generate a review artifact. This was review-only and
   made no production database writes. This pilot validated the integration workflow rather than measuring net-new deal yield.
   Candidate verification and broader coverage can be evaluated in follow-up runs. Proxy rotation and adaptive throttling are
   resilience mechanisms, not a guarantee of access and not a way to defeat
   access controls.

4. **Resilience and proxy behaviour have real-network measurements, but are workload-specific.**
   The 19–20 October 2026 US proxy benchmark measured 98.2% state-target
   geolocation for residential exits (1,502/1,530), 85.8% for mobile exits
   (1,312/1,530), 97.9% usable responses with rotating sessions (1,175/1,200),
   and 600/600 usable responses when sticky sessions were renewed every 15
   requests. Renewed sticky sessions were 100–230 ms faster across six
   comparisons. A single long-lived sticky session degraded to 16–49 usable
   responses per 100-request run before target-side 4xx responses. These are
   measured outcomes for the tested pools, target, locations and dates, not
   universal provider guarantees. Mobile IP geolocation is not a reliable
   proxy for a device's physical state, and long-lived sticky sessions can
   still be blocked. The benchmark measures observed behaviour; it does not
   establish that the same cooldowns or rates will work on every target.

5. **Google's Terms of Service restrict automated querying.** Whether and
   how to operate at volume against Google directly (vs. via a licensed
   data provider) is a legal/business decision for the client, made with
   their counsel — this repo surfaces that decision point (the
   `-i-have-reviewed-tos` flag, required for both live and playwright
   modes) rather than deciding it for you.

## Scaling to 10M+ requests/day

10M requests/day is ~116 sustained requests/second. Nothing about that
number requires a different architecture from what's here — it requires:

- enough proxy/egress capacity (direct mode) or provider request quota
  (provider mode) that the sustained rate reaches ~116 req/s,
- enough worker concurrency per process, and enough `RedisStreamSource`
  consumer processes across enough hosts once a single process's ceiling is
  reached — the load-test numbers above show this codebase's own
  orchestration sustaining well past that number in isolation,
- a queue and sink that can sustain that throughput (Redis Streams is
  already wired in; Kafka/SQS are the same `queue.Source`/`store.Sink`
  interface if preferred),
- for direct-to-Google mode specifically: the live-mode gaps above closed
  for the client's actual target markup and locales. Rendering now exists
  (`mode: playwright`), but CAPTCHA/block handling and selector
  maintenance do not, and a browser session costs far more than an HTTP
  request, so ~116 req/s rendered means a large Chromium fleet. More
  realistically at this volume: the provider fetcher instead.

## Layout

```
cmd/harvester/          CLI entrypoint and wiring (the worker/consumer side)
cmd/api/                Job-submission HTTP layer: POST /jobs, GET /jobs/:run_id (the producer side)
cmd/loadtest/           Offline orchestration measurement: fixed-count, concurrency sweep, and soak-test modes
cmd/crawl/              General-purpose web crawler for any website (see "Crawling any website")
cmd/proxybench/         Proxy provider measurement: rotation, sticky sessions, state targeting, errors, retries, bytes
internal/crawl/         Crawl targets: scope, frontier, robots.txt and Crawl-delay, per-host rate keys
internal/extract/       Any HTML page -> model.Page: generic page data + per-site CSS-selector fields/items
internal/robots/        robots.txt (RFC 9309) parsing, matching and per-origin caching
internal/model/         SerpResult (SERP) and Page (crawler) with their sub-structures
internal/api/           HTTP handlers for cmd/api — depends only on queue.Producer, never Fetcher/Parser
internal/scheduler/     Cron-based recurring harvests — also a pure queue.Producer, wired into cmd/api
internal/queue/         Job sources/producer: in-memory, and Redis Streams for multi-process/host scaling
internal/proxy/         Proxy pool: health tracking, dynamic reload, 3 rotation strategies
internal/ratelimit/     Per-key token-bucket rate limiter
internal/fetcher/       Fetcher interface + Mock, direct HTTP, headless Chromium (playwright.go),
                        HTTP->browser failover (failover.go) and third-party provider implementations,
                        result classification (classify.go) and RateLimitError/Retry-After parsing (errors.go)
internal/parser/        HTML and provider-JSON → SerpResult extraction, with drift detection
internal/worker/        The pool tying fetch → parse → sink together with retry/backoff
internal/metrics/       Run counters, latency percentile tracking, periodic reporting,
                        and a Prometheus /metrics + /healthz endpoint
internal/store/         Sink/PageSink interfaces + JSON-Lines and PostgreSQL (serp_results, pages) implementations
internal/config/        YAML config loading
configs/                Example configs (harvester, schedule, crawl)
docs/                   Proxy benchmark runbook and proxy smoke test
deploy/                 Docker Compose (harvester+Redis+Prometheus+Grafana, opt-in Playwright harvester),
                        systemd units, Dockerfiles (Alpine default, official Playwright image for mode: playwright)
tests/integration/      Pipeline test against real Redis/PostgreSQL/Chromium + the fixture site and proxies
tests/live/             Real-network integration tests, gated behind HARVESTER_LIVE=true (never run in CI)
scripts/                Playwright driver installer; compose-test.sh for Docker integration/e2e runs
```

## Contact

Built by Henry Morgan Dibie. Open to discussing how this maps onto a real
collection engagement, including the production gaps called out above.