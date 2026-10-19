# Proxy quality benchmark runbook

This runbook covers the three measurements requested for evaluating a residential/mobile proxy provider with Web Harvester (written against anyIP, whose username flags are documented below):

1. **A. US state-level success:** request a state, observe where the exit actually geolocates, and report success and state-match per state.
2. **B. Sticky vs. rotating:** compare latency, usable-result rate and exit-IP behavior under the two session modes, holding everything else constant.
3. **C. Failure and retry behavior:** first-attempt outcomes vs. eventual outcomes, retry overhead, recovery time, provider error codes, and whether a new session helps.

Nothing here is a result. Results belong in a separate, dated report that cites the commit and run settings each summary records.

## Provider facts this runner relies on

Checked against anyIP's public documentation on 2026-10-19. Re-check before each campaign; the account-specific availability can differ from the public catalogue.

| Fact | Source |
| --- | --- |
| Username format `user_ID,flag,flag…`; flags `session_<name>`, `sesstime_<min>` (sticky default up to 7 days), `sessreplace_false`, `country_<ISO>`, `region_<name>` (requires country), `city_<name>`, `asn_<n>`, `type_residential`, `type_mobile` | docs/guides/configuration |
| No session flag means "every request … through a fresh IP"; per-connection behavior is not specified | docs/guides/sessions-and-rotation |
| A sticky IP that drops is replaced in the same location unless `sessreplace_false`, which returns `peer_not_found` instead | docs/guides/sessions-and-rotation |
| Unsatisfiable targeting returns `no_peers_available` (502); the code is in the `X-Anyip-Error` response header | docs/guides/location-targeting, errors-reference |
| US region values come from `https://dashboard.anyip.io/api/data/region/US`: 44 states plus District of Columbia, lowercase with spaces removed. Alaska, Montana, North Dakota, South Dakota, Vermont and Wyoming are absent | regions API |
| Proxy usage can be read per proxy account (`consumption_bytes`) through the REST API with an `Api-Key` header | docs/api-reference/v1usage |

## What `cmd/proxybench` measures

Each sample is one optional geo probe plus one target request with up to `-retries` retries. Defaults are chosen so that results mean what they say:

- **Fresh tunnel per request** (`-keepalive=false`). Go otherwise reuses a CONNECT tunnel to the same host, and a reused tunnel keeps its exit IP, so a "rotating" run would partly measure the client, not the provider. `reused_connections` in the summary must be 0 for a rotating result to count.
- **Geo on the target request itself.** When `-geo-url` equals `-target-url` (the workflow default, both `https://ipinfo.io/json`), the observed state belongs to the exact request being measured. A separate geo probe is a different tunnel and, under rotation, a different exit; treat its state as an estimate only.
- **Usable vs. transport success.** `target_ok` is a 2xx. `target_usable` additionally requires `-expect-substring` in the body, so a 200 consent page, challenge or unrelated content is not counted as data.
- **Retries** apply to transport errors, provider errors, 429 and 5xx, with exponential backoff and a `Retry-After` of up to 30s honored. Auth and other 4xx are not retried. `-retry-new-session` (sticky only) retries on a new session ID and keeps it if it recovers.
- **Provider error codes** from `X-Anyip-Error` are captured on both refused CONNECT tunnels and plain-HTTP responses, and reported as `proxy_<code>` error classes.
- **Bytes.** `target_bytes` is the body; `wire_bytes` is everything exchanged with the proxy, including CONNECT, TLS and headers, attributed per attempt. Neither is the provider's billable figure; reconcile against the usage API or dashboard after each campaign.
- **Run metadata.** Each summary records start/finish time, commit (`+dirty` for an uncommitted build), and every setting that affects the result.
- **Small samples.** The summary reports a 95% Wilson interval for the usable rate and flags p95 latency computed from fewer than 20 successes.
- **Privacy.** `-hash-ips` stores exit IPs as salted hashes; the salt is never written, so distinct-IP and IP-change counts still work but the third-party residential/mobile addresses are not retained.

## Where to run it

- **Do not run a provider benchmark from the public repository's Actions** until the provider has agreed the results may be published. Logs and artifacts of a public repository are readable by any signed-in GitHub user. Use a private fork with its own secret, or run locally.
- Latency includes the client-to-gateway leg. GitHub-hosted runners are not pinned to a region, and a local run from outside the US adds a constant transatlantic leg. Run every arm of a comparison from the same place and record it.
- Use only targets you are authorized to request. `ipinfo.io/json` is a geolocation service used as intended. For the representative workload use a site you operate. Do not use this to bypass authentication, paywalls, CAPTCHAs or access controls, and do not induce failures against third-party sites.

## Local runner

```bash
go test ./...
go build -o bin/proxybench ./cmd/proxybench

# Paste the proxy URL from the provider dashboard into the prompt, so it
# stays out of shell history and process arguments.
read -rs HARVESTER_PROXY_URL && export HARVESTER_PROXY_URL

mkdir -p runs/phase0
./bin/proxybench -profile rotating -type residential -requests 5 -hash-ips \
  -target-url https://ipinfo.io/json -geo-url https://ipinfo.io/json \
  -out runs/phase0/rotating-residential.jsonl 2> runs/phase0/rotating-residential.summary.json
```

The summary goes to stderr, samples to `-out`. `runs/`, `*.jsonl` and `*.summary.json` are ignored by Git. In a non-interactive shell, where `read -rs` cannot prompt, load the URL from a file outside the repository instead of typing it into a command.

### Sticky session names

anyIP keys a sticky session by its name. A request that reuses a name resumes that session's exit for up to its `sesstime` (7 days for dashboard-generated profiles), even from a later run and even with a different `type_` flag. `proxybench` therefore picks a random session ID per run when `-session-id` is omitted, appends the type (`residential`, `mobile`) when `-type` is set, and records the base ID in the summary. Pass an explicit `-session-id` only to deliberately resume a session.

Session names must be 1 to 32 letters and digits (anyIP errors reference, `bad_attributes_session_name_invalid_character`). In phase 0 the gateway did not return that error for names containing underscores: it answered 200 and gave a new exit on every request, as if no session had been requested. `proxybench` refuses such names, including after it appends the type and retry suffixes.

### Reconciling bytes with the provider

anyIP reports consumption per team through its REST API (`https://dashboard.anyip.io`, `api-key` header). `GET /api/users/me` returns `default_team.id`. Record the usage before and after each run:

```bash
read -rs ANYIP_API_KEY && export ANYIP_API_KEY
team=$(curl -sS -H "api-key: $ANYIP_API_KEY" https://dashboard.anyip.io/api/users/me | jq -r .default_team.id)
used() { curl -sS -H "api-key: $ANYIP_API_KEY" "https://dashboard.anyip.io/api/v1/teams/$team/usage" | jq .consumption_bytes; }
before=$(used)
# ... run proxybench ...
sleep 60; echo "billed: $(( $(used) - before )) bytes"
```

Compare the billed difference with the summary's `wire_bytes`. The usage figure may lag; re-read it after a few minutes before drawing a conclusion. Running anything else on the same account during a run invalidates the comparison.

## Protocol

Run the phases in order and stop to review after each. Budget figures use about 7.5 KB of wire traffic per `ipinfo.io` request on a fresh tunnel (measured through a stand-in proxy, not the provider). Confirm the real figure from phase 0's `wire_bytes_per_sample` and the provider's usage figure before committing to later phases.

### Phase 0: smoke test (≈ 0.2 MB)

`rotating` and `sticky`, each with `residential` and `mobile`, 5 samples each. Pass criteria:

- requests succeed and `observed_country` is US;
- rotating shows `distinct_ips` close to `samples`, with `reused_connections` 0;
- sticky shows `distinct_ips` of 1;
- the two sticky arms do not share an exit: different `observed_org` or IP hash. A shared exit means the arms shared a session;
- record the network (`observed_org`) distribution per type. A mobile arm exiting on non-carrier networks is a failure. The converse proves nothing: the residential pool has also returned carrier networks (T-Mobile AS21928, Verizon AS6167, AT&T AS20057), which may be fixed wireless home internet or mobile devices. The network number cannot distinguish the two pools, so report pool composition as observed and leave classification to the provider.

Record usage through the API before and after each arm, not once per phase, so billed bytes can be attributed. Read the "after" figure at least two minutes after the last request.

### Phase 1, benchmark A: state coverage (≈ 20 MB)

`state-targeted`, target and geo `https://ipinfo.io/json`, 30 samples per state, for each of the 45 listed regions, residential first, then mobile. Then 100 samples for the states that matter commercially (for example California, Texas, New York, Florida, Illinois). Also request one unlisted state (for example Wyoming) with 5 samples to record how the provider fails: an error code, or a silent fallback to another state.

Use an ipinfo token (`https://ipinfo.io/json?token=…`) for this phase. Without one, ipinfo's own rate limit returns 429s that count as failures but measure ipinfo, not the proxy; phase 0 hit one after 10 requests. Error classes prefixed `proxy_` carry an anyIP code; a bare `http_429` or `http_5xx` came from the target. The token is redacted from recorded errors and the summary records only the host, but keep the full command out of shared logs.

Report per state and type: requested region value, samples, successes, state matches, the Wilson interval, median latency, and error codes. Geolocation databases disagree at state level, especially for mobile carrier ranges; name the database used and treat a mismatch as "observed elsewhere by ipinfo," not as proof of mis-routing. Satellite exits need the same care: Starlink (AS14593) addresses geolocate to the ground station or point of presence, which can be in a different state from the subscriber, so report how many samples per state exited on satellite networks. Cross-checking mismatches against a second geolocation source is a cheap follow-up.

### Phase 2, benchmark B: sticky vs. rotating (≈ 10 MB for geo, plus 1,200 × the representative page)

For each type, run `rotating` and `sticky` back to back with identical target, timeout, delay, retries and location, 100 samples each, repeated in three time windows on different days (300 per arm). Run once against `ipinfo.io/json` to measure exit-IP behavior, and once against the representative authorized page with `-expect-substring` set to text that only a correct page contains.

Compare usable rate with intervals, median and p95 latency (p95 needs at least 200 successes per arm to be stable), `distinct_ips` and `ip_changes`, and `wire_bytes_per_sample`. A sticky session's first request includes peer selection; report its latency separately from the steady state if it differs.

### Phase 3, benchmark C: failure and retry (≈ 10 MB)

1. **Natural failure rate:** rerun phase 2's representative arm with `-retries 2`. Compare `first_attempt_successes` with `usable_results`; report `recovered`, `median_recovery_ms`, `retry_attempts` and `failed_attempt_body_bytes`.
2. **Does a new session help?** Sticky with `-retries 2`, with and without `-retry-new-session`, same everything else.
3. **Provider error semantics:** an unsatisfiable target (the unlisted state from phase 1) and a deliberately short `-timeout` (for example 1s) to observe `no_peers_available`, `peer_timeout` and timeout recovery. This exercises the provider, not a third-party site.
4. **Engine retry logic** is covered deterministically and offline by `TestRetryBenchmark_*` and `TestPool_HonorsRateLimitRetryAfter` in `internal/worker`. Those prove the retry code; they say nothing about the provider.

### Sample-size guide

| Observed successes | 95% Wilson interval | Use |
| --- | --- | --- |
| 5 of 5 | 56.6% to 100% | smoke test only |
| 29 of 30 | 83.3% to 99.4% | screening states |
| 95 of 100 | 88.8% to 97.8% | comparing states or modes |
| 285 of 300 | 91.9% to 96.9% | supporting a recommendation |

Differences smaller than the overlap of two intervals are not findings.

## Evidence status

The repository implements proxy rotation, health tracking, cooldowns, `Retry-After` handling and worker retries, and `proxybench` implements the measurements above with offline tests against a scripted proxy. No live provider measurement has been recorded in this repository.
