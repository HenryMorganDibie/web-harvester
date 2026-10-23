# Capabilities

A direct answer to the five questions this project was built to answer.
Every claim here links to the code, test, or measurement that backs it —
nothing here is asserted without something to point at.

## Which working scrapers do you currently have, built/operated by you?

My current working SERP collection system is this distributed Go pipeline:
self-hosted worker pool, Redis Streams for horizontal scaling, a proxy pool
with health tracking and three rotation strategies, rate limiting that
honors a provider's own `Retry-After` signal, structured extraction with
drift detection, PostgreSQL or JSON-Lines persistence with run/locale/device
metadata, and Prometheus observability — running end to end today. See
[PRODUCTION_READINESS.md](PRODUCTION_READINESS.md) for the full
architecture-to-operations breakdown. What it hasn't done yet is operate
against your specific volume in production — see the volume question below
for exactly what that means and doesn't mean.

The same stack also runs a general-purpose crawler for any website
(`cmd/crawl`, README "Crawling any website"): configurable scope and depth,
generic page data plus per-site CSS-selector extraction, HTTP or headless
Chromium, robots.txt obeyed by default, bot-protection pages classified and
never interacted with. Tested against local sites, real PostgreSQL and real
Chromium; not yet run against third-party sites at volume.

## Do you have a Google SERP / AI Overviews solution? Direct from Google, or third-party?

Both, behind one interface (`internal/fetcher.Fetcher`):

| Capability                | Direct Google (`mode: live`) | Third-party provider (`mode: provider`) |
|----------------------------|------------------------------|-------------------------------------------|
| Organic results            | Built; parser verified against fixtures, not live markup | Built and structurally verified against a real, public API schema |
| Featured snippet / PAA     | Built (fixture-verified)     | Built (fixture-verified) |
| AI Overview (inline)       | Built (fixture-verified)     | Built (fixture-verified) |
| AI Overview (page-token follow-up) | Not applicable — this is a provider-specific async mechanism | Built and tested (`internal/fetcher/provider_test.go`) |
| Consent/session handling   | Cookie jar + "reject all" form submission; the plain-HTTP live result is **still blocked** (JS check) — see below | Not applicable — provider handles this |
| CAPTCHA / bot-detection handling | Detected and classified (CAPTCHA, JS check, consent wall), then routed around via proxy cooldown and failover; never solved or evaded (deliberately — see [README "Honest limitations"](README.md#honest-limitations)) | Handled by the provider as their product |
| Proxy rotation & rate limiting | Built: health-tracked pool, 3 rotation strategies, dynamic reload (`internal/proxy`, `internal/ratelimit`) | Built, same code path |
| Rate-limit (429) handling  | Built: `Retry-After` cools that proxy for exactly that long, work fails over to other proxies, and adaptive throttling halves that proxy's rate | Built: parses `Retry-After`, worker waits the exact duration instead of guessing |
| Locale/device targeting    | Built: `gl`/`hl` params passed through | Built: `gl`/`hl`/`device` params passed through |
| Persistent storage          | PostgreSQL (JSONB columns) or JSON-Lines, either fetch path | Same |
| Self-hostable               | Yes | Yes |
| Third-party dependency      | None | The provider itself |
| Who owns the ToS/legal risk | You, if used at volume | The provider, priced into their product |

**Tested, not assumed:** direct-to-Google requests were run against the live
site. Two distinct blocking responses were observed on different attempts —
a consent interstitial and a JavaScript-execution check — neither is real
search results, and neither was bypassed (no CAPTCHA solving or fingerprint
spoofing was added; see README for why). This is reproducible via
`HARVESTER_LIVE=true go test ./tests/live/... -run GoogleDirect -v`
(`tests/live/google_direct_test.go`).

**Direct Google, rendered (`mode: playwright`).** A third fetch path renders
the results page in headless Chromium through the same pipeline (same proxy
pool, rate limiter, parser, sinks, metrics). Compared with `mode: live` it
adds JavaScript rendering, which gets past the JS-execution check a plain
HTTP client stops at; consent-page dismissal via the page's own "reject
all" form; `Retry-After` handling on 429; and explicit classification of
CAPTCHA and other block pages, which are reported as failures and never
interacted with. It adds no CAPTCHA solving, fingerprint spoofing or other
evasion. Verified against local fixtures in real Chromium
(`internal/fetcher/playwright_browser_test.go`,
`internal/worker/playwright_pipeline_test.go`). **Run against live Google
(2026-09-25) from a datacenter IP and from a consumer connection**: every
Chromium request (26 in total) got the `/sorry/` CAPTCHA page, from the
first request on both, correctly classified as `captcha`; no results page
was served. Direct Google collection is therefore not dependable with this
stack, which does no fingerprint masking or CAPTCHA solving; the provider
path is the dependable route for Google (README "Measuring against
Google"). `HARVESTER_LIVE=true go test ./tests/live/... -run
Playwright -v` is the test that measures it. Rendering does not make direct
scraping production-ready: Google still challenges real browsers, and the
parser's selectors are still fixture-targeted.

**Self-hosted resilience, without a SERP API.** All direct paths (`live`,
`playwright`, and `hybrid`, which uses HTTP first and the browser only for
pages that need JavaScript) share one result classification that drives
adaptive per-proxy throttling, outcome-weighted proxy cooldowns, failover
and retries; in browser modes, Chromium and crashed pages recover on their
own. This is verified end to end against real Redis, PostgreSQL and
Chromium with a fictional site and scripted proxies, in Go
(`tests/integration`) and with the deployed Docker image (`make e2e`). It
has **not** been run against live Google, so it shows the system reacts
correctly to rate limits and blocks, not that it avoids or survives them at
any particular volume. See
[README "Acquisition resilience"](README.md#acquisition-resilience).

**Recommended path for real volume:** the provider fetch path. The
third-party integration was subsequently validated in a live bounded run on
22 October 2026: all 8 search queries completed, 74 unique result URLs were
collected, 22 of 30 source pages were fetched through the configured proxy,
and the output was compared with Deal Intelligence's existing dataset to
generate a review artifact. This confirms the provider-to-acquisition
integration beyond fixture-only testing. It does not establish sustained
production throughput or comprehensive market coverage. The pilot was not designed to
quantify incremental new-deal yield, so it is not evidence for or against finding new deals;
the run made no production database writes.

## What sustained daily volume have you handled, particularly for Google, and over what period?

**Honestly: none yet, in production.** This is a new build, not an existing
operation with a track record. What exists instead:

- **Offline orchestration benchmark** (measures this codebase's own
  queue/worker/proxy/rate-limit/parse pipeline, mock fetcher, zero network):
  see [README "Load testing"](README.md#load-testing) for the concurrency
  sweep table with real measured numbers, currently up to ~15,000 req/s at
  concurrency 800 — well past the ~116 req/s that 10M/day requires.
- **Soak test**: a completed 90-minute sustained run at concurrency 200 —
  20.76 million requests processed, throughput steady at ~3,700-4,000 req/s
  the entire run with no degradation, memory stable (3-7MB, no leak),
  goroutine count exactly flat at 203 across all 45 samples, and zero jobs
  dropped. Full numbers and methodology in
  [README "Load testing"](README.md#load-testing) — reproducible via
  `go run ./cmd/loadtest -duration 90m -sample-interval 2m -concurrency 200`.
- **Production history**: none to report. Once this runs against real
  traffic (via the provider path), this section gets updated with actual
  dates, actual daily volume, and actual success rates — not before.

## Could you deploy and maintain the solution on your servers, rather than Apify or an external hosted API?

Yes, concretely: [`deploy/`](deploy/) contains a working Docker Compose
stack (harvester + Redis + Prometheus + Grafana, plus an opt-in
headless-Chromium harvester image for `mode: playwright`) and a systemd unit
for bare-metal/VM deployment, both running entirely on infrastructure you
control. `docker compose -f deploy/docker-compose.yml up -d` is the whole
deployment. See [`deploy/README.md`](deploy/README.md).

Result storage is your choice: JSON-Lines for simplicity, or PostgreSQL
(`sink_backend: postgres`) if you want to query results directly — by run,
by query, by date range, by whether AI Overview was present — with the
parsed fields stored as native JSONB columns, not an opaque blob. Verified
against a real Postgres instance, not just mocked (`internal/store/postgres_test.go`,
plus an end-to-end CLI run whose output was queried directly).

The only external dependency, if you choose `mode: provider`, is API calls
to the SERP data vendor you pick — the same shape as calling any other paid
API from your own backend, not a hosted scraping platform sitting in front
of your data.

Job submission is also self-hosted: `cmd/api` exposes `POST /jobs` and
`GET /jobs/{run_id}` over your own infrastructure — your team (or another
system of yours) submits query batches over HTTP instead of editing a config
file, and the API server only ever enqueues work onto the same Redis Stream
your workers consume; it never fetches or parses anything itself. Verified
end-to-end against real Redis and Postgres containers, not just unit tests:
a job posted via curl was consumed by a separate harvester process and the
resulting rows confirmed directly in Postgres.

## What would your deployment timeline and indicative setup/monthly support costs be?

Rough shape, to be refined once scope (query mix, target locales, required
provider) is agreed:

- **Week 1**: wire the pipeline to your chosen provider and target
  markets/locales, deploy to your infrastructure, first data flowing.
- **Week 2**: calibrate for your actual query mix, scale workers/Redis
  consumers if a single host isn't enough, get the Grafana dashboard and
  alerting tuned to your thresholds.
- **Ongoing**: selector/schema maintenance as the provider or Google's
  markup changes, monitoring, support.

Setup and monthly costs mostly track the provider's request pricing at your
target volume, plus engineering time — I don't have real numbers to quote
until we know your query mix and provider choice, and I'd rather say that
than guess.
