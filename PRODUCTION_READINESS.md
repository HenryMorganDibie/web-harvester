# Production Readiness

A structured answer to "is this ready for production," section by section.
Where something is already covered in depth elsewhere, this points there
instead of duplicating it — this document is the index, not a second copy.

## 1. Architecture

Queue → worker pool → proxy pool → rate limiter → fetcher → parser → sink,
every stage an interface or a small struct. Job submission (`cmd/api`) and
the worker/consumer (`cmd/harvester`) are separate binaries that only share
the queue: the API only ever calls `queue.Producer.PushJob`, never `Fetcher`
or `Parser`, so acquisition logic can't leak into the submission layer. See
[README "Architecture"](README.md#architecture) and
[README "Job/API layer"](README.md#jobapi-layer).

## 2. Deployment

Docker Compose (full stack: harvester + Redis + Prometheus + Grafana) or
systemd, both running entirely on infrastructure you control. See
[`deploy/README.md`](deploy/README.md).

## 3. Horizontal scaling

`queue.RedisStreamSource` lets any number of harvester processes share one
durable queue via a consumer group. Measured orchestration throughput
scaling by concurrency is in [README "Load testing"](README.md#load-testing)
— real numbers, not projections, from `cmd/loadtest -sweep`.

## 4. Queue durability

Redis Streams with consumer groups: unacknowledged entries (a worker that
crashes mid-job) stay claimable by another consumer rather than being lost.
See [RUNBOOK.md "Queue backlog growing"](RUNBOOK.md#queue-backlog-growing-redis-streams-mode)
for the operational side of this.

## 5. Retry semantics

Configurable `max_retries` with exponential backoff and jitter
(`internal/worker/pool.go`). Every fetch result is classified
(`fetcher.Classify`): a rate limit or block page retries at once on another
proxy (that one is now cooling), a 4xx ends the job without retrying, and
when every proxy is cooling the job waits for the soonest to recover (up to
`max_proxy_wait`). All waits end immediately on shutdown. See
[README "Acquisition resilience"](README.md#acquisition-resilience). A job
that exhausts retries, or is
still in flight when the run shuts down, is counted as dropped
(`serp_harvester_dropped_total`), never silently unaccounted for —
success+dropped is a complete count of every job actually attempted. This
specific accounting edge case (in-flight jobs at shutdown) was found and
fixed during the soak test run in [README "Load testing"](README.md#load-testing),
not designed in from the start — worth knowing when judging how this repo's
test coverage was actually built.

## 6. Proxy management

Health-tracked pool (`internal/proxy`): per-proxy success/failure counts, a
recent-health score, and outcome-weighted cooldowns: consecutive failures
past a threshold, a block page at once, a 429 for exactly its `Retry-After`,
with repeat cooldowns doubling up to `proxy_ban_cooldown_max`. Consent walls
and 4xx don't count against a proxy. Three rotation strategies (round-robin,
random, weighted by recent health). A bug where a proxy was never re-banned
after its first cooldown was found and fixed with a regression test
(`TestPool_RebansAfterCooldownExpires`). `Pool.Reload(urls)`
reconfigures the proxy list at runtime without dropping accumulated health
stats for URLs that stay — the seam for pointing at a vendor's proxy-list
API instead of static config. Authenticated proxies
(`http://user:pass@host:port`) work with no special handling; confirmed by
an integration test that checks the header actually arrives at a fake proxy
server, not just that Go's docs say it should
(`TestHTTPFetcher_AuthenticatedProxy`). `serp_harvester_proxy_banned_total`
tracks cooldown events (by reason in `serp_harvester_proxy_cooldowns_total`),
and `serp_harvester_proxies_available`/`_cooling` feed the
`ProxyPoolExhausted` alert. What's not included: automatically sourcing
proxies from a vendor's API — `Reload` is the integration point, but nothing
calls it on a schedule yet.

## 7. Rate limiting

Two layers: a per-key (per-proxy, or per-direct-egress-IP) token bucket
(`internal/ratelimit`) for self-imposed pacing, and provider-signaled
rate-limit handling for the provider fetch path — a 429 is parsed into a
`*fetcher.RateLimitError` carrying the provider's own `Retry-After` value,
and the worker pool waits that exact duration before retrying instead of
guessing with generic backoff (`TestPool_HonorsRateLimitRetryAfter`). The
same applies to the HTTP and browser fetchers. With `adaptive_rate` (default
on), the per-proxy bucket also adapts: a rate limit or block halves that
proxy's rate down to `rate_min_rps`, and successes recover it gradually
(`internal/ratelimit`, AIMD). A rate-limited proxy is cooled for its
`Retry-After`, so work moves to other proxies instead of waiting.

## 8. Google response handling

Three live fetch paths with very different maturity — see
[README "Honest limitations"](README.md#honest-limitations) and
[README "Live mode"](README.md#live-mode) for the tested, specific findings
on direct-to-Google requests (both a consent interstitial and a
JS-execution check were observed, neither bypassed). The provider fetch
path is the one built for real reliability here.

`mode: playwright` renders the page in headless Chromium: it executes the
JS check, dismisses consent pages via their own "reject all" form, honors
`Retry-After` on 429, and classifies CAPTCHA / "unusual traffic" /
persistent interstitial pages as `BlockedError` failures that count against
proxy health. It never solves or interacts with a CAPTCHA and does no
fingerprint spoofing. It is verified against local fixtures only, not live
Google, so its real block rate is unknown; it is an acquisition option, not
a production-ready direct-to-Google path. See
[README "Browser mode"](README.md#browser-mode-playwright).

The plain HTTP fetcher now reports the same outcomes (429 with
`Retry-After`, CAPTCHA, JS-check shell, consent wall, 4xx/5xx) instead of
passing block pages to the parser, and dismisses consent pages by
submitting their "reject all" form. `mode: hybrid` uses HTTP first and the
browser only for JS-check pages and undismissed consent walls. All of this
is verified end to end against a fictional site and scripted proxies
(`tests/integration`, `make e2e`), not against live Google.

## 9. AI Overview extraction

Handled on both fetch paths differently: the HTML parser looks for an
inline block (fixture-verified, not live-Google-verified — see the first
item in "Honest limitations" below); the provider path handles the documented two-step
`page_token` → `engine=google_ai_overview` follow-up automatically,
including graceful degradation if the follow-up fails. See
[README "Third-party provider fetcher"](README.md#third-party-provider-fetcher).

## 10. Observability

Prometheus `/metrics` (throughput, failure/retry/drop counts, AI Overview
hit rate, parser drift rate, proxy ban rate, latency p50/p95/p99) plus a
`/healthz` liveness endpoint. `mode: playwright` adds browser metrics
(launches, unexpected disconnects, navigation timeouts, consent pages
handled, blocked pages by reason, open/in-use sessions) and two alert rules
(`BrowserBlockedPagesHigh`, `BrowserDisconnectsHigh`). Every mode reports
fetch outcomes by type, proxy cooldowns by reason, proxy availability and
throttling gauges, adaptive rate decreases and hybrid failovers, with
`ProxyPoolExhausted` and `TargetPushingBack` alerts. The pre-built Grafana
dashboard (`deploy/grafana/provisioning/`) charts all of them, in
"Acquisition resilience" and "Browser" rows; every panel query was checked
through Grafana's API against the Docker test stack. Grafana's memory limit
was raised from 256M to 768M after the dashboard OOM-killed it. See
[README "Observability"](README.md#observability-prometheus-metrics).

## 11. Failure recovery

Per-job retry with backoff and failover to another proxy; proxy cooldown
and automatic recovery after an escalating cooldown; in browser modes,
Chromium relaunch with launch backoff, and replacement of crashed pages; a failed AI Overview follow-up degrades to "no AI
Overview" rather than failing the whole result; a failed metrics-server
start doesn't crash the harvest. Process-level recovery (systemd
`Restart=on-failure`, or Docker's restart policy) is configured in
`deploy/`.

## 12. Capacity planning

10M requests/day = ~116 req/s sustained. See
[README "Scaling to 10M+ requests/day"](README.md#scaling-to-10m-requests-day)
for the model, and [README "Load testing"](README.md#load-testing) for
measured orchestration throughput well past that number — the real
constraint at that volume is proxy/provider capacity, not this codebase.
`mode: playwright` is the exception: each fetch holds a Chromium session
(roughly 100 to 300MB and seconds of CPU per page), so its capacity is set
by browser sessions per host, and it has not been load-tested.

## 13. Security

- Provider API keys and the Postgres DSN are both read from environment
  variables (`provider_api_key_env`, `postgres_dsn_env`), never written to
  config files or committed — see `.env.example`.
- The systemd unit runs as a dedicated non-root user with
  `NoNewPrivileges`/`ProtectSystem` hardening.
- No secrets are baked into the Docker image (`deploy/Dockerfile`).
- Not yet addressed: TLS termination in front of `/metrics` and `/healthz`
  (currently plain HTTP, intended for an internal network / behind a
  reverse proxy, not public internet exposure).

## 14. Data retention

Two sinks, two answers. `JSONLSink` has no retention logic of its own — it's
whatever file/log rotation you put around it. `PostgresSink` stores every
result as a row with a `created_at` timestamp, so retention is a matter of a
scheduled `DELETE ... WHERE created_at < ...` or a partition-based policy —
neither is included here, since retention windows are a client policy
decision, not something to hardcode. The `store.Sink` interface remains the
integration point for either a custom retention-aware sink or a
warehouse-streaming one.

## 15. Disaster recovery

Redis Streams persistence configuration (AOF/RDB) is Redis's own concern,
not configured by this repo — a production deployment should enable
whichever persistence mode matches the durability the client needs, and
that decision belongs with whoever operates the Redis instance. No backup
automation is included here.

## 16. Operational runbook

[`RUNBOOK.md`](RUNBOOK.md) — success-rate drops, parser drift, queue
backlog, AI Overview anomalies, metrics endpoint failures, all keyed to
metrics this codebase actually emits.

## 17. Known limitations

The full list is in [README "Honest limitations"](README.md#honest-limitations).
Direct-to-Google fetching remains unreliable: live requests returned
JavaScript checks and CAPTCHA challenges, which are classified and stopped;
CAPTCHA solving and anti-detection bypass are deliberately out of scope.
The third-party provider path has moved beyond fixture-only verification: a
bounded live integration run on 22 October 2026 completed 8/8 queries, fetched
22/30 attempted source pages through the configured proxy, compared results
with 4,506 existing Deal Intelligence records and generated a review artifact.
That is integration evidence, not production history or sustained-volume proof;
the run was review-only and made no production database writes.

The anyIP proxy benchmark also recorded live, workload-specific results:
residential state targeting matched 98.2%, mobile state targeting matched
85.8%, rotating sessions returned 97.9% usable responses, and sticky sessions
renewed every 15 requests returned 100% usable responses on the tested
protected-page workload. Long-lived sticky sessions performed materially worse,
and mobile IP geolocation does not guarantee physical state. See the README
benchmark tables and runbook for methodology and caveats. No number in this
repository claims sustained production operating history; see the README's
top summary.

## 18. Job submission API

`cmd/api` (`POST /jobs`, `GET /jobs/{run_id}`) — see
[README "Job/API layer"](README.md#jobapi-layer) for the full description
and the real end-to-end verification (real Redis + real Postgres, a job
submitted via curl, consumed by a separate harvester process, and confirmed
by querying Postgres directly). Handler logic is unit-tested in CI
(`internal/api/server_test.go`, 8 tests) with a fake producer; the
real-infrastructure run was a one-time manual confirmation against local
Docker containers, not something CI re-verifies on every commit. Not yet
built: authentication/authorization on the API endpoints (currently open —
fine behind an internal network or a reverse proxy that handles auth, not
fine exposed directly to the internet), and the in-memory `submitted` count
resets if the API process restarts (a run submitted before a restart still
shows `submitted_known: false` after one, even though its results are still
in Postgres and countable).

## 19. Scheduled harvesting

`cmd/api -schedule` (`internal/scheduler`, real cron expressions via
`robfig/cron`) — see [README "Scheduled harvesting"](README.md#scheduled-harvesting).
Verified against a real Redis container with a fast interval (queue length
confirmed growing on schedule, not just that the timer fired), plus 3 unit
tests covering cron validation and per-tick behavior. Not yet built:
persisting scheduled-harvest definitions anywhere other than the YAML file
loaded at process start — adding/removing a schedule means restarting
`cmd/api` with an updated file, not a dynamic API for managing schedules.
