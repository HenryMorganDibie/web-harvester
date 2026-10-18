# Proxy quality benchmark runbook

This benchmark covers the three measurements requested for Web Harvester:

1. **US state-level success:** compare a vendor-configured state-targeted proxy profile with the state observed by the IP geolocation probe. Report each state's sample size, successful target responses, and state-match rate separately.
2. **Sticky vs. rotating:** compare the same authorized target under separately configured sticky and rotating proxy profiles, holding request count, timeout, delay, runner region, and target constant.
3. **Failure/retry behavior:** distinguish first-attempt outcomes from eventual job outcomes, retry count, recovery time, and exhausted retries. Use deterministic local fixtures for injected failures; do not induce errors against a third-party site.

The runner is provider-agnostic. It deliberately does not guess AnyIP username parameters, sticky-session syntax, or state-targeting options. Configure the proxy URL using the provider's current documentation/dashboard before each profile run. The profile label is a report label; it does not itself change provider behavior.

## Safety and prerequisites

- Rotate any proxy credentials previously pasted into chats or tickets. Do not commit credentials or put them in command-line arguments.
- Store each configured proxy URL in a GitHub Actions secret. The workflow defaults to `HARVESTER_PROXY_URL`, and the `proxy_secret_name` input can select another secret by name.
- A profile secret must actually be configured for the named behavior. For example, a secret called `HARVESTER_PROXY_URL_STICKY` should contain a vendor-configured sticky-session endpoint; renaming a rotating endpoint is not a sticky-session test.
- Choose a target you are authorized to access. The default target, `https://httpbin.org/status/200`, is a small connectivity check, not a representative production target.
- The default geo probe, `https://ipinfo.io/json`, is a separate request through the same configured proxy. With rotating sessions, its observed state may not be the exact egress state used by the subsequent target request. Geolocation is an estimate, not ground truth.
- Start with five samples per profile and check provider bandwidth/usage before expanding. Do not use this benchmark to bypass authentication, paywalls, CAPTCHAs, or access controls.

## Run the live proxy benchmark in GitHub Actions

Open **Actions → anyIP Proxy Benchmark → Run workflow** on the branch containing this workflow. Choose:

- `profile`: `rotating`, `sticky`, or `state-targeted`
- `proxy_secret_name`: the name of the GitHub Actions secret containing that provider-configured proxy URL
- `expected_state`: required for state-targeted trials (for example, `Texas`)
- `requests`: start at 5; use 10 or 20 only after the smoke test succeeds

The workflow runs `go vet ./...` and `go test ./...` before sending low-volume live traffic. It uploads only the aggregate JSON summary for 14 days. Per-sample JSONL records, including observed IP addresses, remain on the ephemeral runner and are not uploaded. The workflow fails closed if the selected secret is missing or empty.

For state coverage, configure a provider-supported state-targeted profile in the secret, then run a trial for each supported state you need to evaluate. Do not infer coverage from the profile label. Keep a results ledger with state, profile/secret name (not secret value), sample count, target successes, state-match count, median/p95 latency, and run URL. Compare only states that the provider actually supports and record the geolocation source used.

## Local runner

From the repository root:

```bash
go test ./...
go build -o bin/proxybench ./cmd/proxybench

export HARVESTER_PROXY_URL='http://REPLACEMENT_USER:URL_ENCODED_PASSWORD@portal.anyip.io:1080'

./bin/proxybench -profile rotating -requests 5 -delay 1s -out results-rotating.jsonl
./bin/proxybench -profile sticky -requests 5 -delay 1s -out results-sticky.jsonl
./bin/proxybench -profile state-targeted-TX -expected-state Texas -requests 5 -delay 1s -out results-state-TX.jsonl
```

Do not run examples unchanged with placeholder credentials. Keep local output files private; they contain observed IP addresses.

Each JSONL row records a UTC timestamp, profile label, target hostname/status/latency, geo-probe status/latency, observed IP/region/country, optional expected-state match, and an error category. The summary includes target success rate, median/p95 latency, observed states, and state matches. Target success currently means a 2xx status, not that the body contains useful scraped data. A production crawl should also validate result quality.

## Sticky vs. rotating protocol

1. Use the same target, runner, timeout, request count, delay, and approximate time window.
2. Run an initial 5-sample smoke test for each profile.
3. If healthy, run at least three rounds of 20 samples per profile; keep the raw per-sample files private.
4. Compare success rate, median and p95 latency, state distribution, and provider-reported bandwidth. Record the sample sizes and run links.
5. If the geo probe's egress changes between requests, describe the state-match metric as an observed estimate; do not claim the target request's exact exit IP from the separate probe.

## Failure and retry protocol

The live proxy runner records each request attempt but does not itself retry. Retry behavior belongs to the Web Harvester worker pool and must be tested separately. The existing suite includes `TestPool_HonorsRateLimitRetryAfter`, plus `TestRetryBenchmark_TransientFailuresRecoverWithinBudget` and `TestRetryBenchmark_ExhaustedRetriesDropExactlyOnce`. The latter two inject deterministic transient network failures locally and verify recovery within the retry budget, retry counts, exhausted-job accounting, and exactly-once/no sink writes.

For a fuller failure matrix, use the local integration fixture proxy (no third-party traffic) to exercise:
- one transient 429 with `Retry-After`, followed by success;
- repeated 5xx/network errors up to the configured retry limit;
- a timeout followed by recovery;
- a persistent block/challenge that should cool the proxy and stop retrying that same egress;
- all proxies cooling, followed by recovery when the earliest cooldown expires;
- exhausted retries and final job accounting, including no duplicate sink writes.

For each case, capture attempts, final outcome, elapsed recovery time, cooldown/failover decisions, and dropped/exhausted job count. A deterministic fixture result is evidence of retry logic, not a measurement of real-provider reliability. Do not manufacture failures against Google or another third-party site.

## Evidence status

Repository code already implements proxy rotation, health tracking, escalating cooldowns, `Retry-After` handling, and worker retries. The live proxy benchmark can measure connectivity/latency/geolocation, but a profile label alone does not enable sticky sessions or state targeting. Real state-level coverage and sticky-vs-rotating performance remain unmeasured until the workflow is run with correctly configured provider profiles. Provider bandwidth/cost must be reconciled against the provider dashboard; this runner does not prove billed bytes.
