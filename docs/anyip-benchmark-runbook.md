# anyIP benchmark runbook

This benchmark is designed around the three measurements Lukas requested:

1. **US state-level results:** record the observed exit-IP region/country and compare it with the state requested in the provider's supported configuration.
2. **Sticky vs rotating:** run separate, clearly labelled trials and compare target latency and usable HTTP responses.
3. **Failure behavior:** preserve per-sample status/error classes and target latency so timeouts, proxy-connect failures, HTTP 429s, and server errors are visible.

The runner is intentionally provider-agnostic. It does not guess anyIP's username parameters, sticky-session syntax, or state-targeting options. Use Lukas's workload-specific recommendation doc or current anyIP dashboard/docs to configure each profile before running it.

## Safety and prerequisites

- Rotate any proxy credentials previously pasted into chats or tickets. Do not commit credentials or put them in command-line arguments.
- Use a replacement proxy URL in the local `HARVESTER_PROXY_URL` environment variable. The runner refuses to send target traffic directly when this variable is absent.
- Choose a target you are authorized to access. The default target, `https://httpbin.org/status/200`, is a small connectivity check, not a representative production target.
- The default geo probe is `https://ipinfo.io/json`. This is a separate request through the same configured proxy. With rotating sessions, its observed state may not be the exact egress state used by the subsequent target request.
- Start with five samples per profile. Check the anyIP dashboard's bandwidth usage before expanding. Do not use this benchmark to bypass authentication, paywalls, CAPTCHAs, or access controls.

## Build

From the repository root:

```bash
go test ./...
go build -o bin/proxybench ./cmd/proxybench
```

## Run a controlled profile

```bash
export HARVESTER_PROXY_URL='http://REPLACEMENT_USER:URL_ENCODED_PASSWORD@portal.anyip.io:1080'

# Configure the provider's rotating profile using the vendor-documented method first.
./bin/proxybench -profile rotating -requests 5 -delay 1s -out results-rotating.jsonl

# Reconfigure the provider for a sticky session, using the documented method.
./bin/proxybench -profile sticky -requests 5 -delay 1s -out results-sticky.jsonl

# For each supported state-targeted profile, configure the state with the vendor
# and compare its observed exit region; never assume a label forces a state.
./bin/proxybench -profile state-targeted-TX -expected-state Texas -requests 5 -delay 1s -out results-state-TX.jsonl
```

Do not run the examples unchanged with placeholder credentials. Keep output files local; they contain observed IP addresses and should not be committed or publicly shared without review.

Each JSONL row records a UTC timestamp, profile label, target hostname/status/latency, geo-probe status/latency, observed IP/region/country, optional expected-state match, and an error category. A JSON summary is printed to stderr with target success rate, median/p95 latency, observed states, and state matches. A target response is counted successful only when its final HTTP status is 2xx. The geo probe is a separate request and does not prove the target request used the same exit IP under rotating behavior.

## Compare profiles fairly

- Keep the target URL, sample count, timeout, delay, and machine constant between runs.
- Run at least three rounds per profile if the initial five-sample smoke test is healthy. Report sample sizes; do not claim state coverage from profile labels alone.
- Report target success rate and latency separately from geo-state match rate. Treat geolocation as an estimate, not ground truth.
- Track 429, 4xx, 5xx, proxy-connect, DNS, TLS, and timeout outcomes separately. HTTP 200 alone is not sufficient for a production crawl: inspect the response body and whether it contains useful data.
- Compare results with the anyIP dashboard's usage/bandwidth figures. This tool does not estimate cost or prove billed bytes.
- This runner records each attempt but does not implement retries. Test the Web Harvester's retry/backoff behavior separately against a controlled, authorized endpoint that can deliberately return 429/5xx or delay responses; don't manufacture failures against a third-party site.

## Current evidence status

No live anyIP measurements are embedded in this repository. Until the runner is executed with replacement credentials and vendor-supported profile settings, success rates, state coverage, latency, and bandwidth remain **unmeasured**.
