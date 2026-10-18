// Package worker runs a pool of concurrent goroutines that pull jobs off a
// queue, fetch, parse, and sink the result — with per-proxy rate limiting,
// retry with backoff, and proxy health tracking wired in on every request.
//
// Every fetch result is classified (fetcher.Classify) and fed back: to the
// proxy pool (health, cooldowns), to the rate limiter (adaptive per-proxy
// throttling) and to metrics. A rate limit or block page fails the attempt
// over to another proxy immediately; errors that retrying cannot fix (4xx)
// end the job early. Blocks are detected and routed around, never bypassed.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	"github.com/HenryMorganDibie/web-harvester/internal/fetcher"
	"github.com/HenryMorganDibie/web-harvester/internal/metrics"
	"github.com/HenryMorganDibie/web-harvester/internal/model"
	"github.com/HenryMorganDibie/web-harvester/internal/proxy"
	"github.com/HenryMorganDibie/web-harvester/internal/queue"
	"github.com/HenryMorganDibie/web-harvester/internal/ratelimit"
	"github.com/HenryMorganDibie/web-harvester/internal/store"
)

// Parser extracts a SerpResult from a fetcher's raw response body. Both the
// HTML parser (internal/parser.Parser) and the JSON parser for structured
// third-party provider responses (internal/parser.JSONParser) satisfy this,
// so the worker pool never needs to know which fetch path is active.
type Parser interface {
	Parse(query string, body []byte) (*model.SerpResult, error)
}

// userAgents is a small, honest rotation pool. This is standard practice for
// identifying different concurrent clients — not fingerprint spoofing.
var userAgents = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) web-harvester/0.1",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) web-harvester/0.1",
	"Mozilla/5.0 (X11; Linux x86_64) web-harvester/0.1",
}

// Pool wires together every stage of the pipeline.
type Pool struct {
	Concurrency int
	MaxRetries  int

	Fetcher   fetcher.Fetcher
	ProxyPool *proxy.Pool
	Limiter   *ratelimit.Limiter
	Parser    Parser
	Sink      store.Sink
	Metrics   *metrics.Counters

	// Latencies, if set, records fetch latency for every successful fetch —
	// used for percentile reporting (see cmd/loadtest). Nil is safe: no
	// latency tracking occurs.
	Latencies *metrics.LatencyRecorder

	// MaxProxyWait bounds how long an attempt waits for a proxy to leave
	// cooldown when every proxy is cooling. Zero means 2 minutes.
	MaxProxyWait time.Duration

	// UserAgent, if set, is sent on every request instead of rotating
	// through the built-in list (the web crawler sets its configured one,
	// which is also what robots.txt rules are matched against).
	UserAgent string
}

// Run starts Concurrency workers consuming jobs, and blocks until jobs is
// closed and every in-flight job has been processed.
func (p *Pool) Run(ctx context.Context, jobs <-chan queue.Job) {
	done := make(chan struct{})
	for i := 0; i < p.Concurrency; i++ {
		go func() {
			p.runWorker(ctx, jobs)
			done <- struct{}{}
		}()
	}
	for i := 0; i < p.Concurrency; i++ {
		<-done
	}
}

func (p *Pool) runWorker(ctx context.Context, jobs <-chan queue.Job) {
	for {
		select {
		case job, ok := <-jobs:
			if !ok {
				return
			}
			p.process(ctx, job)
		case <-ctx.Done():
			return
		}
	}
}

func (p *Pool) process(ctx context.Context, job queue.Job) {
	build := func(px *proxy.Proxy) fetcher.Request {
		return fetcher.Request{
			Query:     job.Query,
			UserAgent: p.userAgent(),
			ProxyURL:  px.URL,
			Language:  localeLanguage(job.Locale),
			Country:   localeCountry(job.Locale),
			Device:    job.Device,
		}
	}
	var result *model.SerpResult
	use := func(resp *fetcher.Response, px *proxy.Proxy) error {
		r, err := p.Parser.Parse(job.Query, resp.Body)
		if err != nil {
			return err
		}
		r.LatencyMS = resp.Latency.Milliseconds()
		r.ProxyUsed = px.DisplayID()
		r.FetchedAt = time.Now().UTC()
		r.RunID = job.RunID
		r.Locale = job.Locale
		r.Device = job.Device
		if r.Calibration != nil {
			// Archive the raw pre-parse body only on drift — see README
			// "Handling selector drift": this is what a selector-retuning
			// pass would want to inspect, and keeping it off normal results
			// keeps JSONLSink output small at volume.
			r.RawBody = resp.Body
		}
		result = r
		return nil
	}

	if err := p.Acquire(ctx, build, nil, use); err != nil {
		p.Metrics.IncDropped()
		log.Printf("worker: job %q %v", job.Query, err)
		return
	}

	if err := p.Sink.Write(result); err != nil {
		log.Printf("worker: sink write failed for %q: %v", job.Query, err)
	}
	p.Metrics.IncSuccess()
	if result.AIOverview != nil {
		p.Metrics.IncAIOverviewPresent()
	}
	if result.Calibration != nil {
		p.Metrics.IncCalibrationFlagged()
	}
}

// DroppedError reports why Acquire gave up on a job.
type DroppedError struct {
	// Attempts is the number of fetches actually made.
	Attempts int
	// Canceled is set when the run shut down (ctx ended) mid-job.
	Canceled bool
	Err      error
}

func (e *DroppedError) Error() string {
	if e.Canceled {
		return fmt.Sprintf("dropped: %v", e.Err)
	}
	return fmt.Sprintf("dropped after %d fetch attempts: %v", e.Attempts, e.Err)
}

func (e *DroppedError) Unwrap() error { return e.Err }

// Acquire runs the fetch loop every job shares: pick a proxy (waiting out
// cooldowns up to MaxProxyWait), wait for the rate limiter, fetch, classify
// the result and feed it back to the proxy pool, limiter and metrics, and
// retry with backoff (or fail over at once after a rate limit or block)
// up to MaxRetries. build makes the request for the chosen proxy. rateKey,
// if set, picks the rate-limiter bucket for that proxy and request (nil
// means one bucket per proxy). use is called with each successful response;
// an error from it counts as a failed attempt and is retried.
//
// It returns nil once use succeeds, or a *DroppedError.
func (p *Pool) Acquire(ctx context.Context, build func(*proxy.Proxy) fetcher.Request, rateKey func(*proxy.Proxy, fetcher.Request) string, use func(*fetcher.Response, *proxy.Proxy) error) error {
	var lastErr error
	attempts := 0
	// failover is set when the last attempt was rate-limited or blocked:
	// that proxy is now cooling, so the retry goes straight to another one
	// (or waits for the cooldown if none is available) instead of backing
	// off first.
	failover := false
	canceled := func(err error) error { return &DroppedError{Attempts: attempts, Canceled: true, Err: err} }

	for attempt := 0; attempt <= p.MaxRetries; attempt++ {
		if attempt > 0 {
			p.Metrics.IncRetried()
			if !failover && !sleepCtx(ctx, backoffDuration(attempt)) {
				return canceled(ctx.Err())
			}
		}
		failover = false

		px, err := p.nextProxy(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return canceled(ctx.Err())
			}
			lastErr = err
			continue
		}

		req := build(px)
		key := px.DisplayID()
		if rateKey != nil {
			key = rateKey(px, req)
		}
		if err := p.Limiter.Wait(ctx, key); err != nil {
			// Context cancelled (shutdown/timeout) while waiting for a rate
			// limit slot. Counted as dropped rather than silently
			// unaccounted for, so success+dropped stays a complete count of
			// every job actually attempted — found via soak testing, where
			// jobs in flight at the exact cutoff need to land somewhere.
			return canceled(err)
		}

		attempts++
		resp, err := p.Fetcher.Fetch(ctx, req)
		outcome := fetcher.Classify(err)
		if err != nil && ctx.Err() != nil {
			outcome = fetcher.OutcomeCanceled // shutdown, not the proxy's fault
		}
		p.Metrics.IncOutcome(outcome.String())
		p.recordOutcome(px, key, outcome, err)
		if err != nil {
			if outcome == fetcher.OutcomeCanceled {
				return canceled(err)
			}
			lastErr = err
			p.Metrics.IncFailure()
			if !outcome.Retryable() {
				break
			}
			failover = outcome == fetcher.OutcomeRateLimited || outcome.Blocked()
			continue
		}
		if p.Latencies != nil {
			p.Latencies.Record(resp.Latency)
		}

		if err := use(resp, px); err != nil {
			lastErr = err
			p.Metrics.IncFailure()
			continue
		}
		return nil
	}
	return &DroppedError{Attempts: attempts, Err: lastErr}
}

func (p *Pool) userAgent() string {
	if p.UserAgent != "" {
		return p.UserAgent
	}
	return userAgents[rand.Intn(len(userAgents))]
}

// nextProxy returns a proxy that is not cooling. When every proxy is
// cooling it waits for the soonest one to recover, up to MaxProxyWait,
// rather than burning retries against ErrAllBanned.
func (p *Pool) nextProxy(ctx context.Context) (*proxy.Proxy, error) {
	maxWait := p.MaxProxyWait
	if maxWait <= 0 {
		maxWait = 2 * time.Minute
	}
	deadline := time.Now().Add(maxWait)
	for {
		px, err := p.ProxyPool.Next()
		if !errors.Is(err, proxy.ErrAllBanned) {
			return px, err
		}
		at := p.ProxyPool.NextAvailableAt()
		if at.IsZero() {
			continue // one just recovered
		}
		if at.After(deadline) {
			return nil, err
		}
		if !sleepCtx(ctx, time.Until(at)) {
			return nil, ctx.Err()
		}
	}
}

// recordOutcome feeds a classified fetch result back to the proxy pool
// (health and cooldowns) and the rate limiter bucket rateKey (adaptive
// throttling).
func (p *Pool) recordOutcome(px *proxy.Proxy, rateKey string, outcome fetcher.Outcome, err error) {
	var retryAfter time.Duration
	var rl *fetcher.RateLimitError
	if errors.As(err, &rl) {
		retryAfter = rl.RetryAfter
	}
	if p.ProxyPool.ReportOutcome(px, proxyOutcome(outcome), retryAfter) {
		p.Metrics.IncProxyBanned()
		p.Metrics.IncProxyCooldown(cooldownReason(outcome))
	}
	switch {
	case outcome == fetcher.OutcomeSuccess:
		p.Limiter.Reward(rateKey)
	case outcome == fetcher.OutcomeRateLimited || outcome.Blocked():
		if p.Limiter.Penalize(rateKey) {
			p.Metrics.IncRateDecrease()
		}
	}
}

// proxyOutcome maps a fetch outcome to what it says about the proxy.
// Consent walls and client errors are about the request or region, not the
// proxy, so they leave its health alone.
func proxyOutcome(o fetcher.Outcome) proxy.Outcome {
	switch o {
	case fetcher.OutcomeSuccess:
		return proxy.Success
	case fetcher.OutcomeRateLimited:
		return proxy.RateLimited
	case fetcher.OutcomeCaptcha, fetcher.OutcomeInterstitial, fetcher.OutcomeChallenge:
		return proxy.Blocked
	case fetcher.OutcomeConsent, fetcher.OutcomeClientError, fetcher.OutcomeCanceled:
		return proxy.Neutral
	default:
		return proxy.Failure
	}
}

func cooldownReason(o fetcher.Outcome) string {
	switch {
	case o == fetcher.OutcomeRateLimited:
		return "rate_limited"
	case o.Blocked():
		return "blocked"
	default:
		return "failures"
	}
}

// localeLanguage and localeCountry split a "COUNTRY-language" locale hint
// (e.g. "US-en") into its two parts. An empty or malformed locale yields
// empty strings for both, which fetchers treat as "use the target/provider
// default."
func localeCountry(locale string) string {
	country, _, ok := strings.Cut(locale, "-")
	if !ok {
		return ""
	}
	return country
}

func localeLanguage(locale string) string {
	_, lang, ok := strings.Cut(locale, "-")
	if !ok {
		return ""
	}
	return lang
}

// backoffDuration is an exponential delay with jitter, capped at 2s. This
// is deliberately short since mock-mode runs should stay fast; pacing
// against a live target comes from the rate limiter and proxy cooldowns,
// not from this retry delay.
func backoffDuration(attempt int) time.Duration {
	base := time.Duration(1<<uint(attempt)) * 50 * time.Millisecond
	if base > 2*time.Second {
		base = 2 * time.Second
	}
	jitter := time.Duration(rand.Int63n(int64(base) / 2))
	return base + jitter
}

// sleepCtx sleeps for d or until ctx is done, reporting false if ctx ended
// first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
