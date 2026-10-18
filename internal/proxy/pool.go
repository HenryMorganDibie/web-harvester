// Package proxy provides a rotating pool of egress identities with health
// tracking: a proxy that fails repeatedly is put in cooldown instead of
// being hammered, and comes back into rotation once the cooldown expires.
// Outcomes are weighted (ReportOutcome): a block page cools a proxy at
// once, a rate limit cools it for exactly the server's Retry-After, and
// repeat offenders get exponentially longer cooldowns up to MaxCooldown.
// Success/failure counts are tracked per proxy so rotation can be weighted
// toward healthier proxies, and the pool can be reconfigured at runtime
// (Reload) without dropping accumulated health stats for proxies that stay
// in the list.
//
// With zero proxies configured, the pool hands out a single "direct"
// identity so the same worker code path runs unchanged in local/dev mode.
//
// Authenticated proxies work with no special handling: a URL of the form
// http://user:pass@host:port carries Basic auth that Go's net/http applies
// automatically for both plain HTTP proxying and HTTPS CONNECT tunneling —
// see TestPool_AuthenticatedProxyURL.
package proxy

import (
	"errors"
	"net/url"
	"math/rand"
	"sync"
	"time"
)

// ErrAllBanned is returned when every proxy in the pool is currently cooling
// down. Callers should back off and retry the whole pool later.
var ErrAllBanned = errors.New("proxy: all proxies are in cooldown")

// Strategy selects how Next() picks among currently-healthy proxies.
type Strategy int

const (
	// RoundRobin cycles through proxies in order. The default: predictable,
	// fair, no assumptions about which proxies are "better."
	RoundRobin Strategy = iota
	// Random picks uniformly at random among healthy proxies. Useful to
	// avoid synchronized round-robin patterns across many worker processes
	// that each start their own Pool.
	Random
	// WeightedSuccessRate biases selection toward proxies with a higher
	// observed success rate, falling back to uniform weighting for proxies
	// with no history yet. Use once a pool has run long enough to have
	// meaningful per-proxy stats; on a fresh pool it behaves like Random.
	WeightedSuccessRate
)

// Outcome is what a request through a proxy tells the pool about that
// proxy's health.
type Outcome int

const (
	// Success resets the failure streak and the cooldown escalation.
	Success Outcome = iota
	// Failure (timeout, connect error, 5xx) counts toward the ban
	// threshold.
	Failure
	// Blocked (CAPTCHA, "unusual traffic", JS-check page) puts the proxy in
	// cooldown immediately, escalating on repeats.
	Blocked
	// RateLimited (HTTP 429) cools the proxy for the server's Retry-After
	// if given, otherwise like Blocked.
	RateLimited
	// Neutral outcomes (client errors, consent walls, cancellation) say
	// nothing about the proxy and change nothing.
	Neutral
)

// healthAlpha weights the most recent outcome in a proxy's health score
// (an exponentially weighted moving average of success).
const healthAlpha = 0.2

// Proxy is one egress identity (e.g. a proxy URL). Key() is what the rate
// limiter and metrics key off of.
type Proxy struct {
	URL string

	mu               sync.Mutex
	consecutiveFails int
	bannedUntil      time.Time
	totalSuccess     uint64
	totalFailure     uint64
	lastUsedAt       time.Time

	// banStreak counts cooldowns since the last success; each one doubles
	// the next cooldown. health is the success EWMA (valid when hasHealth).
	banStreak int
	health    float64
	hasHealth bool
}

// Key returns a stable identifier for this proxy, or "direct" when no proxy
// URL is configured (i.e. outbound requests go straight out).
func (p *Proxy) Key() string {
	if p.URL == "" {
		return "direct"
	}
	return p.URL
}

// DisplayID returns a credential-free identifier suitable for result records.
// Keep Key() for internal rate-limit and health bookkeeping; never serialize it.
func (p *Proxy) DisplayID() string {
	if p.URL == "" {
		return "direct"
	}
	u, err := url.Parse(p.URL)
	if err != nil || u.Host == "" {
		return "configured-proxy"
	}
	if u.Scheme == "" {
		return u.Host
	}
	return u.Scheme + "://" + u.Host
}

func (p *Proxy) isBanned() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Now().Before(p.bannedUntil)
}

// Stats is a point-in-time snapshot of one proxy's health.
type Stats struct {
	URL              string
	TotalSuccess     uint64
	TotalFailure     uint64
	ConsecutiveFails int
	Banned           bool
	BannedUntil      time.Time
	LastUsedAt       time.Time
	// BanStreak is the number of cooldowns since the last success.
	BanStreak int
	// Health is the recent-success score in [0,1] (EWMA), or -1 with no
	// history yet.
	Health float64
}

// SuccessRate returns TotalSuccess / (TotalSuccess + TotalFailure), or 0 if
// there's no history yet.
func (s Stats) SuccessRate() float64 {
	total := s.TotalSuccess + s.TotalFailure
	if total == 0 {
		return 0
	}
	return float64(s.TotalSuccess) / float64(total)
}

// Stats returns a snapshot of this proxy's current health.
func (p *Proxy) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{
		URL:              p.URL,
		TotalSuccess:     p.totalSuccess,
		TotalFailure:     p.totalFailure,
		ConsecutiveFails: p.consecutiveFails,
		Banned:           time.Now().Before(p.bannedUntil),
		BannedUntil:      p.bannedUntil,
		LastUsedAt:       p.lastUsedAt,
		BanStreak:        p.banStreak,
		Health:           p.healthLocked(),
	}
}

func (p *Proxy) healthLocked() float64 {
	if !p.hasHealth {
		return -1
	}
	return p.health
}

func (p *Proxy) recordHealthLocked(ok bool) {
	v := 0.0
	if ok {
		v = 1
	}
	if !p.hasHealth {
		p.health, p.hasHealth = v, true
		return
	}
	p.health = (1-healthAlpha)*p.health + healthAlpha*v
}

// Pool hands out proxies according to Strategy, skipping any currently
// banned, and tracks failures to decide when to ban.
type Pool struct {
	mu           sync.Mutex
	proxies      []*Proxy
	byURL        map[string]*Proxy
	idx          int
	banThreshold int
	banCooldown  time.Duration

	// Strategy selects the rotation policy; zero-value is RoundRobin, so
	// existing callers that don't set it get unchanged behavior.
	Strategy Strategy

	// MaxCooldown caps escalating cooldowns (banCooldown doubles per
	// consecutive cooldown without a success). Zero means 16x banCooldown.
	MaxCooldown time.Duration
}

// NewPool builds a pool from a list of proxy URLs. An empty list is valid
// and results in a single direct (no-proxy) identity.
func NewPool(urls []string, banThreshold int, banCooldown time.Duration) *Pool {
	pl := &Pool{
		byURL:        make(map[string]*Proxy),
		banThreshold: banThreshold,
		banCooldown:  banCooldown,
	}
	pl.setProxies(urls)
	return pl
}

func (pl *Pool) setProxies(urls []string) {
	if len(urls) == 0 {
		urls = []string{""}
	}
	pl.proxies = make([]*Proxy, 0, len(urls))
	for _, u := range urls {
		p := &Proxy{URL: u}
		pl.proxies = append(pl.proxies, p)
		pl.byURL[u] = p
	}
}

// Reload replaces the pool's proxy list at runtime — the "dynamic
// configuration" seam: point it at a file watcher, an API poll, or a
// provider's proxy-list endpoint. Proxies whose URL appears in both the old
// and new list keep their accumulated health stats (success/failure counts,
// ban state); proxies removed from the list are dropped; new URLs start
// with a clean history. Safe to call concurrently with Next/ReportResult.
func (pl *Pool) Reload(urls []string) {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	if len(urls) == 0 {
		urls = []string{""} // preserve "direct" semantics on an empty reload
	}

	newByURL := make(map[string]*Proxy, len(urls))
	newProxies := make([]*Proxy, 0, len(urls))
	for _, u := range urls {
		if existing, ok := pl.byURL[u]; ok {
			newProxies = append(newProxies, existing)
			newByURL[u] = existing
			continue
		}
		p := &Proxy{URL: u}
		newProxies = append(newProxies, p)
		newByURL[u] = p
	}

	pl.proxies = newProxies
	pl.byURL = newByURL
	pl.idx = 0
}

// Next returns a healthy proxy chosen according to Strategy.
func (pl *Pool) Next() (*Proxy, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	switch pl.Strategy {
	case Random:
		return pl.nextRandomLocked()
	case WeightedSuccessRate:
		return pl.nextWeightedLocked()
	default:
		return pl.nextRoundRobinLocked()
	}
}

func (pl *Pool) nextRoundRobinLocked() (*Proxy, error) {
	n := len(pl.proxies)
	for i := 0; i < n; i++ {
		p := pl.proxies[pl.idx]
		pl.idx = (pl.idx + 1) % n
		if !p.isBanned() {
			pl.markUsed(p)
			return p, nil
		}
	}
	return nil, ErrAllBanned
}

func (pl *Pool) healthyLocked() []*Proxy {
	healthy := make([]*Proxy, 0, len(pl.proxies))
	for _, p := range pl.proxies {
		if !p.isBanned() {
			healthy = append(healthy, p)
		}
	}
	return healthy
}

func (pl *Pool) nextRandomLocked() (*Proxy, error) {
	healthy := pl.healthyLocked()
	if len(healthy) == 0 {
		return nil, ErrAllBanned
	}
	p := healthy[rand.Intn(len(healthy))]
	pl.markUsed(p)
	return p, nil
}

func (pl *Pool) nextWeightedLocked() (*Proxy, error) {
	healthy := pl.healthyLocked()
	if len(healthy) == 0 {
		return nil, ErrAllBanned
	}

	// Weight = recent health (success EWMA, so a proxy that has started
	// failing loses weight quickly even with a long good history), with a
	// floor so a proxy with zero history (or zero health) still gets
	// picked sometimes rather than being starved.
	const floor = 0.1
	weights := make([]float64, len(healthy))
	total := 0.0
	for i, p := range healthy {
		w := p.Stats().Health
		if w < 0 {
			w = 0.5 // no history: treat as average until proven otherwise
		}
		if w < floor {
			w = floor
		}
		weights[i] = w
		total += w
	}

	r := rand.Float64() * total
	for i, w := range weights {
		r -= w
		if r <= 0 {
			pl.markUsed(healthy[i])
			return healthy[i], nil
		}
	}
	// Floating point fallback: last one.
	pl.markUsed(healthy[len(healthy)-1])
	return healthy[len(healthy)-1], nil
}

func (pl *Pool) markUsed(p *Proxy) {
	p.mu.Lock()
	p.lastUsedAt = time.Now()
	p.mu.Unlock()
}

// ReportResult records whether the last request through p succeeded: a nil
// err is Success, anything else a Failure. See ReportOutcome.
func (pl *Pool) ReportResult(p *Proxy, err error) bool {
	if err == nil {
		return pl.ReportOutcome(p, Success, 0)
	}
	return pl.ReportOutcome(p, Failure, 0)
}

// ReportOutcome records the outcome of the last request through p and
// returns true exactly when this call put p into a new cooldown, so callers
// can count cooldown events rather than every failure. retryAfter is the
// server's Retry-After for RateLimited (zero if none).
//
// Failures ban after banThreshold in a row; Blocked, and RateLimited
// without a Retry-After, cool down immediately. Those cooldowns escalate:
// banCooldown doubles for each cooldown since the last success, up to
// MaxCooldown. RateLimited with a Retry-After cools for exactly that long
// and does not escalate.
func (pl *Pool) ReportOutcome(p *Proxy, outcome Outcome, retryAfter time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch outcome {
	case Success:
		p.consecutiveFails = 0
		p.banStreak = 0
		p.totalSuccess++
		p.recordHealthLocked(true)
		return false
	case Neutral:
		return false
	}

	p.totalFailure++
	p.recordHealthLocked(false)
	switch outcome {
	case RateLimited:
		if retryAfter > 0 {
			return pl.coolLocked(p, retryAfter)
		}
		return pl.coolLocked(p, pl.escalatedCooldownLocked(p))
	case Blocked:
		return pl.coolLocked(p, pl.escalatedCooldownLocked(p))
	default: // Failure
		p.consecutiveFails++
		if pl.banThreshold > 0 && p.consecutiveFails >= pl.banThreshold {
			return pl.coolLocked(p, pl.escalatedCooldownLocked(p))
		}
		return false
	}
}

// escalatedCooldownLocked returns banCooldown doubled per cooldown since
// the last success, capped at MaxCooldown, and advances the streak.
func (pl *Pool) escalatedCooldownLocked(p *Proxy) time.Duration {
	max := pl.MaxCooldown
	if max <= 0 {
		max = 16 * pl.banCooldown
	}
	d := pl.banCooldown
	for i := 0; i < p.banStreak && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	p.banStreak++
	return d
}

// coolLocked puts p in cooldown for d (never shortening an existing one)
// and resets the failure count, so a proxy that keeps failing after its
// cooldown is banned again rather than never. Returns true if p was not
// already cooling.
func (pl *Pool) coolLocked(p *Proxy, d time.Duration) bool {
	now := time.Now()
	wasCooling := now.Before(p.bannedUntil)
	if until := now.Add(d); until.After(p.bannedUntil) {
		p.bannedUntil = until
	}
	p.consecutiveFails = 0
	return !wasCooling
}

// NextAvailableAt returns when the soonest cooling proxy becomes usable
// again, or the zero time if a proxy is available now.
func (pl *Pool) NextAvailableAt() time.Time {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	var earliest time.Time
	now := time.Now()
	for _, p := range pl.proxies {
		p.mu.Lock()
		until := p.bannedUntil
		p.mu.Unlock()
		if !now.Before(until) {
			return time.Time{}
		}
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
		}
	}
	return earliest
}

// Counts returns how many proxies are available and how many are cooling.
func (pl *Pool) Counts() (available, cooling int) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	for _, p := range pl.proxies {
		if p.isBanned() {
			cooling++
		} else {
			available++
		}
	}
	return available, cooling
}

// Size returns how many proxies are configured (at least 1).
func (pl *Pool) Size() int {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	return len(pl.proxies)
}

// AllStats returns a snapshot of every proxy's health, in pool order — the
// data a /metrics-style endpoint or an operator dashboard would want.
func (pl *Pool) AllStats() []Stats {
	pl.mu.Lock()
	proxies := append([]*Proxy(nil), pl.proxies...)
	pl.mu.Unlock()

	out := make([]Stats, len(proxies))
	for i, p := range proxies {
		out[i] = p.Stats()
	}
	return out
}
