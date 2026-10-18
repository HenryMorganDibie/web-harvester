package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HenryMorganDibie/web-harvester/internal/fetcher"
	"github.com/HenryMorganDibie/web-harvester/internal/metrics"
	"github.com/HenryMorganDibie/web-harvester/internal/parser"
	"github.com/HenryMorganDibie/web-harvester/internal/proxy"
	"github.com/HenryMorganDibie/web-harvester/internal/queue"
	"github.com/HenryMorganDibie/web-harvester/internal/ratelimit"
)

// retrySequenceFetcher deterministically injects transient network failures,
// then returns a parser-compatible fixture. It never contacts an external host.
type retrySequenceFetcher struct {
	failuresRemaining atomic.Int64
	attempts          atomic.Int64
	body              []byte
}

func (f *retrySequenceFetcher) Fetch(ctx context.Context, req fetcher.Request) (*fetcher.Response, error) {
	f.attempts.Add(1)
	for {
		remaining := f.failuresRemaining.Load()
		if remaining <= 0 {
			return &fetcher.Response{StatusCode: 200, Body: f.body}, nil
		}
		if f.failuresRemaining.CompareAndSwap(remaining, remaining-1) {
			return nil, errors.New("controlled transient network failure")
		}
	}
}

func retryBenchmarkPool(f fetcher.Fetcher, counters *metrics.Counters, sink *countingSink, retries int) *Pool {
	return &Pool{
		Concurrency: 1,
		MaxRetries:  retries,
		Fetcher:     f,
		ProxyPool:   proxy.NewPool(nil, 1000, time.Hour),
		Limiter:     ratelimit.New(100000, 1000),
		Parser:      parser.New(),
		Sink:        sink,
		Metrics:     counters,
	}
}

func TestRetryBenchmark_TransientFailuresRecoverWithinBudget(t *testing.T) {
	body := []byte(`<html><body><div class="serp-organic-results"><div class="organic-result" data-position="1"><a class="organic-link" href="https://example.com/1"><span class="organic-title">result</span></a><div class="organic-snippet">snippet</div></div></div></body></html>`)
	f := &retrySequenceFetcher{body: body}
	f.failuresRemaining.Store(2)
	counters := &metrics.Counters{}
	sink := &countingSink{}
	pool := retryBenchmarkPool(f, counters, sink, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool.Run(ctx, queue.New([]string{"controlled-recovery"}, 1))

	snap := counters.Snapshot()
	if got := f.attempts.Load(); got != 3 {
		t.Fatalf("expected 3 attempts (2 failures then success), got %d", got)
	}
	if snap.Retried != 2 {
		t.Errorf("expected 2 retries, got %d", snap.Retried)
	}
	if snap.Success != 1 || snap.Dropped != 0 {
		t.Errorf("expected one recovered job and no drops, got success=%d dropped=%d", snap.Success, snap.Dropped)
	}
	if sink.Count() != 1 {
		t.Errorf("expected exactly one sink write after recovery, got %d", sink.Count())
	}
}

func TestRetryBenchmark_ExhaustedRetriesDropExactlyOnce(t *testing.T) {
	body := []byte(`<html><body><div class="serp-organic-results"><div class="organic-result" data-position="1"><a class="organic-link" href="https://example.com/1"><span class="organic-title">result</span></a><div class="organic-snippet">snippet</div></div></div></body></html>`)
	f := &retrySequenceFetcher{body: body}
	f.failuresRemaining.Store(10)
	counters := &metrics.Counters{}
	sink := &countingSink{}
	pool := retryBenchmarkPool(f, counters, sink, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool.Run(ctx, queue.New([]string{"controlled-exhaustion"}, 1))

	snap := counters.Snapshot()
	if got := f.attempts.Load(); got != 3 {
		t.Fatalf("expected initial attempt plus 2 retries, got %d", got)
	}
	if snap.Retried != 2 {
		t.Errorf("expected 2 retries, got %d", snap.Retried)
	}
	if snap.Success != 0 || snap.Dropped != 1 {
		t.Errorf("expected zero successes and exactly one dropped job, got success=%d dropped=%d", snap.Success, snap.Dropped)
	}
	if sink.Count() != 0 {
		t.Errorf("exhausted job must not be written to sink, got %d writes", sink.Count())
	}
}
