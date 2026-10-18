package crawl

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/HenryMorganDibie/web-harvester/internal/extract"
	"github.com/HenryMorganDibie/web-harvester/internal/fetcher"
	"github.com/HenryMorganDibie/web-harvester/internal/model"
	"github.com/HenryMorganDibie/web-harvester/internal/proxy"
	"github.com/HenryMorganDibie/web-harvester/internal/robots"
	"github.com/HenryMorganDibie/web-harvester/internal/store"
	"github.com/HenryMorganDibie/web-harvester/internal/worker"
)

// Crawler crawls one target.
type Crawler struct {
	// Pool does every fetch: its Fetcher matches the target's Render mode,
	// and its ProxyPool, Limiter, Metrics, MaxRetries and UserAgent apply.
	// Rate limiting is keyed by host, so RatePerHost is the total request
	// rate to one host across all proxies.
	Pool *worker.Pool
	// Robots supplies robots.txt rules. Required unless the target sets
	// respect_robots: false.
	Robots *robots.Cache
	Sink   store.PageSink
	RunID  string

	// gates enforces robots.txt Crawl-delay: host -> *gate.
	gates sync.Map
}

// gate spaces requests to one host at least delay apart.
type gate struct {
	mu   sync.Mutex
	next time.Time
}

// waitCrawlDelay blocks until host may be requested again under delay,
// and reserves the following slot. It reports false if ctx ended first.
func (c *Crawler) waitCrawlDelay(ctx context.Context, host string, delay time.Duration) bool {
	v, _ := c.gates.LoadOrStore(host, &gate{})
	g := v.(*gate)
	g.mu.Lock()
	now := time.Now()
	at := g.next
	if at.Before(now) {
		at = now
	}
	g.next = at.Add(delay)
	g.mu.Unlock()
	t := time.NewTimer(time.Until(at))
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Stats summarizes a finished crawl.
type Stats struct {
	Fetched         int64 // pages fetched and written
	Failed          int64 // pages dropped after retries (blocks, errors)
	RobotsDisallow  int64 // URLs skipped for robots.txt
	Flagged         int64 // pages whose required extraction data was missing
	Discovered      int64 // in-scope URLs queued, start URLs included
	BudgetExhausted bool  // MaxPages stopped the crawl from queueing more
}

type job struct {
	url   string
	depth int
}

// Run crawls t until every in-scope URL within MaxDepth and MaxPages has
// been visited or ctx ends, and returns what happened.
func (c *Crawler) Run(ctx context.Context, t Target) (Stats, error) {
	s, err := compile(t)
	if err != nil {
		return Stats{}, err
	}
	if *s.RespectRobots && c.Robots == nil {
		return Stats{}, fmt.Errorf("crawl: target %q respects robots.txt but no robots cache is configured", s.Name)
	}
	workers := s.Concurrency
	if workers <= 0 {
		workers = c.Pool.Concurrency
	}
	if workers <= 0 {
		workers = 1
	}

	var stats Stats
	seen := map[string]bool{}
	var pending []job
	enqueue := func(raw string, depth int) {
		if seen[raw] {
			return
		}
		if len(seen) >= s.MaxPages {
			stats.BudgetExhausted = true
			return
		}
		seen[raw] = true
		stats.Discovered++
		pending = append(pending, job{raw, depth})
	}
	for _, u := range s.starts {
		enqueue(u, 0)
	}

	jobs := make(chan job)
	found := make(chan []job)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				found <- c.visit(ctx, s, j, &stats)
			}
		}()
	}

	// The coordinator owns seen and pending: it hands out jobs, takes back
	// discovered links, and stops once nothing is queued or in flight. On
	// cancellation it stops handing out work and waits for in-flight
	// fetches to return.
	inFlight := 0
	canceled := ctx.Done()
	for len(pending) > 0 || inFlight > 0 {
		var send chan job
		var next job
		if len(pending) > 0 {
			send, next = jobs, pending[0]
		}
		select {
		case send <- next:
			pending = pending[1:]
			inFlight++
		case links := <-found:
			inFlight--
			if ctx.Err() == nil {
				for _, l := range links {
					enqueue(l.url, l.depth)
				}
			}
		case <-canceled:
			canceled, pending = nil, nil
		}
	}
	close(jobs)
	wg.Wait()
	return stats, ctx.Err()
}

// visit fetches one URL, writes its page, and returns the in-scope links to
// follow.
func (c *Crawler) visit(ctx context.Context, s *scope, j job, stats *Stats) []job {
	u, err := url.Parse(j.url)
	if err != nil {
		return nil
	}
	if *s.RespectRobots {
		rules := c.Robots.For(ctx, u)
		if !rules.Allowed(u.EscapedPath() + query(u)) {
			atomic.AddInt64(&stats.RobotsDisallow, 1)
			c.Pool.Metrics.IncRobotsDisallowed()
			return nil
		}
		if rules.CrawlDelay > 0 && !c.waitCrawlDelay(ctx, strings.ToLower(u.Host), rules.CrawlDelay) {
			return nil
		}
	}

	var page *model.Page
	var follow []string
	build := func(px *proxy.Proxy) fetcher.Request {
		return fetcher.Request{
			URL:          j.url,
			UserAgent:    c.Pool.UserAgent,
			ProxyURL:     px.URL,
			Language:     localePart(s.Locale, 1),
			Country:      localePart(s.Locale, 0),
			Device:       s.Device,
			WaitSelector: s.WaitSelector,
		}
	}
	use := func(resp *fetcher.Response, px *proxy.Proxy) error {
		final := resp.FinalURL
		if final == "" {
			final = j.url
		}
		p := &model.Page{}
		if isHTML(resp.ContentType) {
			var err error
			if p, err = extract.Page(final, resp.Body, s.Extract); err != nil {
				return err
			}
			if j.depth < s.MaxDepth {
				follow = followLinks(final, resp.Body, s.Follow, p.Links)
			}
			if p.Extraction != nil {
				p.RawBody = resp.Body
			}
		}
		p.Target, p.RunID, p.URL, p.Depth = s.Name, c.RunID, j.url, j.depth
		if final != j.url {
			p.FinalURL = final
		}
		p.StatusCode, p.ContentType = resp.StatusCode, resp.ContentType
		p.FetchedAt, p.LatencyMS, p.ProxyUsed = time.Now().UTC(), resp.Latency.Milliseconds(), px.DisplayID()
		page = p
		return nil
	}

	if err := c.Pool.Acquire(ctx, build, rateByHost, use); err != nil {
		var dropped *worker.DroppedError
		if errors.As(err, &dropped) && dropped.Canceled {
			return nil
		}
		atomic.AddInt64(&stats.Failed, 1)
		c.Pool.Metrics.IncDropped()
		log.Printf("crawl: %s: %s %v", s.Name, j.url, err)
		return nil
	}
	if err := c.Sink.WritePage(page); err != nil {
		log.Printf("crawl: %s: write %s: %v", s.Name, j.url, err)
	}
	atomic.AddInt64(&stats.Fetched, 1)
	c.Pool.Metrics.IncSuccess()
	if page.Extraction != nil {
		atomic.AddInt64(&stats.Flagged, 1)
		c.Pool.Metrics.IncCalibrationFlagged()
	}

	var next []job
	for _, l := range follow {
		lu, err := url.Parse(l)
		if err != nil {
			continue
		}
		if n := normalize(lu); s.follows(n) {
			next = append(next, job{n, j.depth + 1})
		}
	}
	return next
}

// rateByHost keys the rate limiter by the request's host, so the limit is
// per site across every proxy: politeness is owed to the site, not to each
// egress IP.
func rateByHost(_ *proxy.Proxy, req fetcher.Request) string {
	if u, err := url.Parse(req.URL); err == nil {
		return strings.ToLower(u.Host)
	}
	return req.URL
}

// followLinks returns the links selector picks from the page, reusing the
// extracted links when selector is the default.
func followLinks(pageURL string, body []byte, selector string, extracted []string) []string {
	if selector == "a[href]" && extracted != nil {
		return extracted
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil
	}
	return extract.Links(base, doc, selector)
}

func query(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	return "?" + u.RawQuery
}

// localePart returns part i (0 = country, 1 = language) of a
// "COUNTRY-language" locale, or "".
func localePart(locale string, i int) string {
	parts := strings.SplitN(locale, "-", 2)
	if len(parts) != 2 {
		return ""
	}
	return parts[i]
}

func isHTML(contentType string) bool {
	ct := strings.ToLower(contentType)
	return ct == "" || strings.Contains(ct, "html")
}

// RobotsGetter fetches robots.txt files through pool, so they use the same
// proxies, per-host rate limit and retries as page fetches. pool's Fetcher
// should be a plain HTTP fetcher: robots.txt is never rendered.
func RobotsGetter(pool *worker.Pool) robots.Getter {
	return func(ctx context.Context, robotsURL string) (int, []byte, error) {
		var status int
		var body []byte
		build := func(px *proxy.Proxy) fetcher.Request {
			return fetcher.Request{URL: robotsURL, UserAgent: pool.UserAgent, ProxyURL: px.URL}
		}
		use := func(resp *fetcher.Response, _ *proxy.Proxy) error {
			status, body = resp.StatusCode, resp.Body
			return nil
		}
		err := pool.Acquire(ctx, build, rateByHost, use)
		if err == nil {
			return status, body, nil
		}
		var se *fetcher.StatusError
		if errors.As(err, &se) {
			return se.StatusCode, nil, nil
		}
		var rl *fetcher.RateLimitError
		if errors.As(err, &rl) {
			return rl.StatusCode, nil, nil
		}
		return 0, nil, err
	}
}
