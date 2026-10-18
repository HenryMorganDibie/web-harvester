// Package metrics tracks throughput counters for the harvest run. In
// production these would be exported as Prometheus counters/histograms;
// here they are printed periodically so the demo shows the same signals
// (success rate, req/sec, dropped jobs) an on-call engineer would watch.
package metrics

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

// Counters holds run-wide atomic counters.
type Counters struct {
	Success uint64
	Failure uint64
	Dropped uint64
	Retried uint64

	// AIOverviewPresent counts successful results whose SerpResult had a
	// non-nil AIOverview. Compare against Success to get an AI Overview
	// "hit rate" for the query mix being run.
	AIOverviewPresent uint64

	// CalibrationFlagged counts successful parses that still came back with
	// a CalibrationNote (an expected block, usually organic_results,
	// wasn't found) — the parser-drift signal from README "Handling
	// selector drift".
	CalibrationFlagged uint64

	// ProxyBanned counts how many times a proxy transitioned into cooldown,
	// for any reason (see Cooldowns for the breakdown).
	ProxyBanned uint64

	// RateDecreases, Failovers, HTTPConsentHandled and RobotsDisallowed:
	// see acquisition.go.
	RateDecreases      uint64
	Failovers          uint64
	HTTPConsentHandled uint64
	RobotsDisallowed   uint64

	outcomes  labeledCounter
	cooldowns labeledCounter

	// ProxyGauges, if set, is read on every scrape for the proxy
	// availability and throttling gauges.
	ProxyGauges func() ProxyGauges

	// Browser holds headless-browser counters when mode: playwright is
	// active, and is nil otherwise. Set it before starting the reporter or
	// the metrics server.
	Browser *BrowserCounters
}

// IncSuccess records one successful fetch+parse.
func (c *Counters) IncSuccess() { atomic.AddUint64(&c.Success, 1) }

// IncFailure records one failed HTTP/fetch attempt across all pool users.
// This includes auxiliary requests such as robots.txt; it does not mean a
// crawl page failed. Use crawl.Stats for page-level outcomes.
func (c *Counters) IncFailure() { atomic.AddUint64(&c.Failure, 1) }

// IncDropped records one job that never succeeded — either it exhausted all
// retries, or it was still in flight when the run's context was cancelled
// (shutdown/timeout). Either way, success+dropped is a complete count of
// every job actually attempted.
func (c *Counters) IncDropped() { atomic.AddUint64(&c.Dropped, 1) }

// IncRetried records one retry attempt.
func (c *Counters) IncRetried() { atomic.AddUint64(&c.Retried, 1) }

// IncAIOverviewPresent records one successful result whose AIOverview was
// populated.
func (c *Counters) IncAIOverviewPresent() { atomic.AddUint64(&c.AIOverviewPresent, 1) }

// IncCalibrationFlagged records one successful parse that still came back
// calibration-flagged (an expected block was missing).
func (c *Counters) IncCalibrationFlagged() { atomic.AddUint64(&c.CalibrationFlagged, 1) }

// IncProxyBanned records one proxy transitioning into cooldown.
func (c *Counters) IncProxyBanned() { atomic.AddUint64(&c.ProxyBanned, 1) }

// Snapshot is a point-in-time read of all counters.
type Snapshot struct {
	Success, Failure, Dropped, Retried                 uint64
	AIOverviewPresent, CalibrationFlagged, ProxyBanned uint64
}

// Snapshot reads the current counter values.
func (c *Counters) Snapshot() Snapshot {
	return Snapshot{
		Success:            atomic.LoadUint64(&c.Success),
		Failure:            atomic.LoadUint64(&c.Failure),
		Dropped:            atomic.LoadUint64(&c.Dropped),
		Retried:            atomic.LoadUint64(&c.Retried),
		AIOverviewPresent:  atomic.LoadUint64(&c.AIOverviewPresent),
		CalibrationFlagged: atomic.LoadUint64(&c.CalibrationFlagged),
		ProxyBanned:        atomic.LoadUint64(&c.ProxyBanned),
	}
}

// StartReporter prints a throughput line to stderr every interval until
// ctx is done (stdout is left to results, e.g. JSONL output "-"),
// returning a channel that is closed once the reporter has stopped (so
// callers can flush a final line after cancellation).
func (c *Counters) StartReporter(ctx context.Context, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		start := time.Now()
		for {
			select {
			case <-ctx.Done():
				c.report(time.Since(start))
				return
			case <-ticker.C:
				c.report(time.Since(start))
			}
		}
	}()
	return done
}

func (c *Counters) report(elapsed time.Duration) {
	s := c.Snapshot()
	total := s.Success + s.Dropped
	rps := float64(0)
	if elapsed.Seconds() > 0 {
		rps = float64(s.Success) / elapsed.Seconds()
	}
	dailyProjection := rps * 86400
	fmt.Fprintf(os.Stderr,
		"[metrics] elapsed=%s success=%d failed_attempts=%d retried=%d dropped=%d completed=%d req/s=%.2f projected/day=%.0f ai_overview=%d calibration_flagged=%d proxy_banned=%d\n",
		elapsed.Round(time.Second), s.Success, s.Failure, s.Retried, s.Dropped, total, rps, dailyProjection,
		s.AIOverviewPresent, s.CalibrationFlagged, s.ProxyBanned,
	)
	if c.ProxyGauges != nil {
		g := c.ProxyGauges()
		fmt.Fprintf(os.Stderr, "[metrics] proxies available=%d cooling=%d throttled=%d failovers=%d\n",
			g.Available, g.Cooling, g.Throttled, atomic.LoadUint64(&c.Failovers))
	}
	if c.Browser != nil {
		b := c.Browser.Snapshot()
		fmt.Fprintf(os.Stderr,
			"[metrics] browser launches=%d disconnects=%d nav_timeouts=%d consent_handled=%d blocked_captcha=%d blocked_consent=%d blocked_interstitial=%d blocked_challenge=%d sessions_open=%d sessions_in_use=%d\n",
			b.Launches, b.Disconnects, b.NavigationTimeouts, b.ConsentHandled,
			b.BlockedCaptcha, b.BlockedConsent, b.BlockedInterstitial, b.BlockedChallenge, b.SessionsOpen, b.SessionsInUse,
		)
	}
}
