// Command proxybench runs a small, auditable proxy quality benchmark.
// It deliberately fails closed when HARVESTER_PROXY_URL is not set.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// anyIPErrorHeader carries the provider's machine-readable error code
// (for example no_peers_available or peer_timeout) on proxy-generated
// responses, including failed CONNECT tunnels.
const anyIPErrorHeader = "X-Anyip-Error"

type geoResponse struct {
	IP      string `json:"ip"`
	Region  string `json:"region"`
	Country string `json:"country"`
	Org     string `json:"org"`
}

type sample struct {
	Timestamp       time.Time `json:"timestamp"`
	Profile         string    `json:"profile"`
	ExpectedState   string    `json:"expected_state,omitempty"`
	TargetHost      string    `json:"target_host"`
	TargetStatus    int       `json:"target_status,omitempty"`
	TargetMS        int64     `json:"target_ms,omitempty"`
	TotalMS         int64     `json:"total_ms"`
	TargetOK        bool      `json:"target_ok"`
	TargetUsable    bool      `json:"target_usable"`
	TargetBytes     int64     `json:"target_bytes"`
	WireBytes       int64     `json:"wire_bytes"`
	FailedBytes     int64     `json:"failed_attempt_bytes,omitempty"`
	Attempts        int       `json:"attempts"`
	FirstAttemptOK  bool      `json:"first_attempt_ok"`
	Recovered       bool      `json:"recovered,omitempty"`
	RecoveryMS      int64     `json:"recovery_ms,omitempty"`
	NewSession      bool      `json:"new_session,omitempty"`
	ConnReused      bool      `json:"conn_reused,omitempty"`
	AttemptErrors   []string  `json:"attempt_errors,omitempty"`
	ProxyError      string    `json:"proxy_error,omitempty"`
	GeoStatus       int       `json:"geo_status,omitempty"`
	GeoMS           int64     `json:"geo_ms,omitempty"`
	GeoBytes        int64     `json:"geo_bytes,omitempty"`
	GeoWireBytes    int64     `json:"geo_wire_bytes,omitempty"`
	GeoSameRequest  bool      `json:"geo_same_request,omitempty"`
	ObservedIP      string    `json:"observed_ip,omitempty"`
	ObservedState   string    `json:"observed_state,omitempty"`
	ObservedCountry string    `json:"observed_country,omitempty"`
	ObservedOrg     string    `json:"observed_org,omitempty"`
	StateMatch      *bool     `json:"state_match,omitempty"`
	GeoErrorClass   string    `json:"geo_error_class,omitempty"`
	ErrorClass      string    `json:"error_class,omitempty"`
	Error           string    `json:"error,omitempty"`
}

type settings struct {
	TargetHost      string `json:"target_host"`
	GeoHost         string `json:"geo_host,omitempty"`
	GeoMode         string `json:"geo_mode"`
	ProxyType       string `json:"proxy_type,omitempty"`
	ExpectedState   string `json:"expected_state,omitempty"`
	Requests        int    `json:"requests"`
	TimeoutMS       int64  `json:"timeout_ms"`
	DelayMS         int64  `json:"delay_ms"`
	KeepAlive       bool   `json:"keepalive"`
	Retries         int    `json:"retries"`
	RetryBackoffMS  int64  `json:"retry_backoff_ms"`
	RetryNewSession bool   `json:"retry_new_session"`
	SessionID       string `json:"session_id,omitempty"`
	// GeoAuthenticated records whether ipinfo requests carried a token,
	// without recording the token.
	GeoAuthenticated bool `json:"geo_authenticated"`
	UsableCheck      bool `json:"usable_check"`
	HashIPs          bool `json:"hash_ips"`
}

type summary struct {
	Profile               string         `json:"profile"`
	Commit                string         `json:"commit,omitempty"`
	StartedAt             time.Time      `json:"started_at"`
	FinishedAt            time.Time      `json:"finished_at"`
	Settings              settings       `json:"settings"`
	Samples               int            `json:"samples"`
	TargetSuccesses       int            `json:"target_successes"`
	TargetSuccessPct      float64        `json:"target_success_pct"`
	UsableResults         int            `json:"usable_results"`
	UsablePct             float64        `json:"usable_pct"`
	UsableCI95            [2]float64     `json:"usable_ci95_pct"`
	FirstAttemptSuccesses int            `json:"first_attempt_successes"`
	RetryAttempts         int            `json:"retry_attempts"`
	Recovered             int            `json:"recovered"`
	MedianRecoveryMS      int64          `json:"median_recovery_ms,omitempty"`
	NewSessionRecoveries  int            `json:"new_session_recoveries,omitempty"`
	MedianTargetMS        int64          `json:"median_target_ms,omitempty"`
	P95TargetMS           int64          `json:"p95_target_ms,omitempty"`
	MedianTotalMS         int64          `json:"median_total_ms,omitempty"`
	P95TotalMS            int64          `json:"p95_total_ms,omitempty"`
	LatencyNote           string         `json:"latency_note,omitempty"`
	TargetBytes           int64          `json:"target_body_bytes"`
	GeoBytes              int64          `json:"geo_body_bytes"`
	FailedAttemptBytes    int64          `json:"failed_attempt_body_bytes"`
	WireBytes             int64          `json:"wire_bytes"`
	WireBytesPerSample    int64          `json:"wire_bytes_per_sample"`
	BytesNote             string         `json:"bytes_note"`
	IPChecks              int            `json:"ip_checks"`
	DistinctIPs           int            `json:"distinct_ips"`
	IPChanges             int            `json:"ip_changes"`
	ReusedConnections     int            `json:"reused_connections"`
	StateMatches          int            `json:"state_matches,omitempty"`
	StateChecks           int            `json:"state_checks,omitempty"`
	ObservedStates        map[string]int `json:"observed_states,omitempty"`
	ErrorClasses          map[string]int `json:"error_classes,omitempty"`
	ProxyErrorCodes       map[string]int `json:"proxy_error_codes,omitempty"`
}

// profileConfig is everything needed to derive the proxy URL for one attempt.
type profileConfig struct {
	base          *url.URL
	profile       string
	proxyType     string
	expectedState string
}

func (p profileConfig) url(sessionID string) (*url.URL, error) {
	u := *p.base
	if p.base.User != nil {
		user := *p.base.User
		u.User = &user
	}
	if err := configureAnyIPProfile(&u, p.profile, sessionID, p.expectedState, p.proxyType); err != nil {
		return nil, err
	}
	return &u, nil
}

// attemptState is attached to each request's context so the transport hooks
// can select the proxy URL and report what happened on the wire.
type attemptState struct {
	proxyURL   *url.URL
	proxyError string
	reused     bool
	wireBytes  atomic.Int64
}

// countingConn attributes every byte exchanged with the proxy to the attempt
// that dialed it. With keep-alives off each attempt has its own connection,
// so this includes the CONNECT exchange, TLS handshake and headers.
type countingConn struct {
	net.Conn
	n *atomic.Int64
}

func (c countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.n.Add(int64(n))
	return n, err
}

func (c countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.n.Add(int64(n))
	return n, err
}

type attemptKey struct{}

func attemptFrom(ctx context.Context) *attemptState {
	a, _ := ctx.Value(attemptKey{}).(*attemptState)
	return a
}

type runner struct {
	client        *http.Client
	cfg           profileConfig
	expectedState string
	expectSubstr  string
	timeout       time.Duration
	retries       int
	backoff       time.Duration
	newSession    bool
	sessionID     string
	sessionSeq    int
	redact        func(string) string
	hashIP        func(string) string
}

func main() {
	profile := flag.String("profile", "unspecified", "proxy behavior: rotating, sticky, or state-targeted")
	sessionID := flag.String("session-id", "", "AnyIP sticky session ID; random per run when omitted. A reused ID resumes that session's exit for up to its sesstime, even across runs")
	expectedState := flag.String("expected-state", "", "expected US state/region name; used for AnyIP state targeting and compared with geo probe")
	proxyType := flag.String("type", "", "AnyIP network type: residential or mobile; empty keeps the proxy URL's setting")
	target := flag.String("target-url", "https://httpbin.org/status/200", "URL you are authorized to request")
	geoURL := flag.String("geo-url", "https://ipinfo.io/json", "IP geolocation endpoint used to observe proxy egress; 'none' disables it; equal to -target-url measures geo on the target request itself")
	expectSubstr := flag.String("expect-substring", "", "a 2xx response is usable only if its body contains this text; empty treats every 2xx as usable")
	requests := flag.Int("requests", 5, "number of sequential samples (each makes at most one geo probe and one target request plus retries)")
	timeout := flag.Duration("timeout", 15*time.Second, "timeout per HTTP request")
	delay := flag.Duration("delay", time.Second, "delay between samples")
	keepAlive := flag.Bool("keepalive", false, "reuse proxy tunnels between requests; off by default so rotating sessions get a new exit per request")
	retries := flag.Int("retries", 0, "retries per sample after a transport error, proxy error, 429 or 5xx")
	retryBackoff := flag.Duration("retry-backoff", 500*time.Millisecond, "base delay before a retry, doubled per retry; a Retry-After of at most 30s takes precedence")
	retryNewSession := flag.Bool("retry-new-session", false, "sticky profile: retry on a new session ID, and keep it if the retry succeeds")
	hashIPs := flag.Bool("hash-ips", false, "record exit IPs as salted hashes; the per-run salt is never written, so change and distinct-IP counts still work")
	outPath := flag.String("out", "-", "JSONL output path, or - for stdout")
	flag.Parse()

	if *requests < 1 || *requests > 500 {
		fatal("-requests must be between 1 and 500")
	}
	if *timeout <= 0 || *timeout > 2*time.Minute {
		fatal("-timeout must be greater than zero and at most 2m")
	}
	if *delay < 0 || *delay > time.Minute {
		fatal("-delay must be between 0 and 1m")
	}
	if *retries < 0 || *retries > 5 {
		fatal("-retries must be between 0 and 5")
	}
	if *retryBackoff < 0 || *retryBackoff > 30*time.Second {
		fatal("-retry-backoff must be between 0 and 30s")
	}
	targetURL, err := parseHTTPURL(*target)
	if err != nil {
		fatal("-target-url must be a valid HTTP(S) URL")
	}
	var geoTarget *url.URL
	geoMode := "none"
	if *geoURL != "none" && *geoURL != "" {
		if geoTarget, err = parseHTTPURL(*geoURL); err != nil {
			fatal("-geo-url must be a valid HTTP(S) URL or 'none'")
		}
		geoMode = "separate"
		if geoTarget.String() == targetURL.String() {
			geoMode = "same-request"
		}
	}
	if token := strings.TrimSpace(os.Getenv("IPINFO_TOKEN")); token != "" {
		addIPInfoToken(targetURL, token)
		addIPInfoToken(geoTarget, token)
	}
	if *expectedState != "" && geoMode == "none" {
		fatal("-expected-state needs a geo probe; do not combine it with -geo-url none")
	}

	proxyRaw := strings.TrimSpace(os.Getenv("HARVESTER_PROXY_URL"))
	if proxyRaw == "" {
		fatal("HARVESTER_PROXY_URL is required; refusing to send benchmark traffic directly")
	}
	proxyURL, err := url.Parse(proxyRaw)
	if err != nil || (proxyURL.Scheme != "http" && proxyURL.Scheme != "https") || proxyURL.Hostname() == "" {
		fatal("HARVESTER_PROXY_URL must be a valid HTTP(S) proxy URL")
	}
	if *sessionID == "" {
		*sessionID = randomSessionID()
	}
	cfg := profileConfig{base: proxyURL, profile: *profile, proxyType: *proxyType, expectedState: *expectedState}
	if _, err := cfg.url(*sessionID); err != nil {
		fatal(err.Error())
	}
	if *retryNewSession && *profile != "sticky" {
		fatal("-retry-new-session only applies to the sticky profile")
	}

	r := &runner{
		client:        newClient(*timeout, *keepAlive),
		cfg:           cfg,
		expectedState: *expectedState,
		expectSubstr:  *expectSubstr,
		timeout:       *timeout,
		retries:       *retries,
		backoff:       *retryBackoff,
		newSession:    *retryNewSession,
		sessionID:     *sessionID,
		redact:        redactor(proxyURL, targetURL, geoTarget),
		hashIP:        func(ip string) string { return ip },
	}
	if *hashIPs {
		r.hashIP = ipHasher()
	}

	var writer io.Writer = os.Stdout
	if *outPath != "-" {
		file, err := os.OpenFile(*outPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			fatal("cannot open output file")
		}
		defer file.Close()
		writer = file
	}
	encoder := json.NewEncoder(writer)
	agg := newAggregator(*profile)
	agg.sum.Commit = commit()
	agg.sum.Settings = settings{
		TargetHost: targetURL.Hostname(), GeoMode: geoMode, ProxyType: *proxyType,
		ExpectedState: *expectedState, Requests: *requests,
		TimeoutMS: timeout.Milliseconds(), DelayMS: delay.Milliseconds(), KeepAlive: *keepAlive,
		Retries: *retries, RetryBackoffMS: retryBackoff.Milliseconds(), RetryNewSession: *retryNewSession,
		UsableCheck: *expectSubstr != "", HashIPs: *hashIPs,
		GeoAuthenticated: strings.Contains(targetURL.RawQuery, "token=") || (geoTarget != nil && strings.Contains(geoTarget.RawQuery, "token=")),
	}
	if *profile == "sticky" {
		agg.sum.Settings.SessionID = *sessionID
	}
	if geoTarget != nil {
		agg.sum.Settings.GeoHost = geoTarget.Hostname()
	}
	for i := 0; i < *requests; i++ {
		if i > 0 && *delay > 0 {
			time.Sleep(*delay)
		}
		s := r.runSample(targetURL, geoTarget, geoMode)
		if err := encoder.Encode(s); err != nil {
			fatal("failed to write JSONL output")
		}
		agg.add(s)
	}
	enc := json.NewEncoder(os.Stderr)
	enc.SetIndent("", "  ")
	if err := enc.Encode(agg.finish()); err != nil {
		fatal("failed to write summary")
	}
}

// newClient builds an HTTP client whose proxy is chosen per request from the
// attempt state in its context, and which records the provider's error code
// when the proxy refuses a CONNECT tunnel.
func newClient(timeout time.Duration, keepAlive bool) *http.Client {
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			if a := attemptFrom(req.Context()); a != nil {
				return a.proxyURL, nil
			}
			return nil, errors.New("proxybench: request has no proxy selection")
		},
		OnProxyConnectResponse: func(ctx context.Context, _ *url.URL, _ *http.Request, res *http.Response) error {
			if a := attemptFrom(ctx); a != nil {
				a.proxyError = res.Header.Get(anyIPErrorHeader)
			}
			return nil
		},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if a := attemptFrom(ctx); a != nil {
				return countingConn{Conn: conn, n: &a.wireBytes}, nil
			}
			return conn, nil
		},
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     !keepAlive,
	}
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	}}
}

// addIPInfoToken authenticates requests to ipinfo.io, whose unauthenticated
// rate limit otherwise produces 429s that would be counted against the
// proxy. The token comes from the environment so it never appears in
// command lines or workflow inputs; an explicit token in the URL wins.
func addIPInfoToken(u *url.URL, token string) {
	if u == nil || !strings.EqualFold(u.Hostname(), "ipinfo.io") {
		return
	}
	q := u.Query()
	if q.Get("token") == "" {
		q.Set("token", token)
		u.RawQuery = q.Encode()
	}
}

func parseHTTPURL(raw string) (*url.URL, error) {
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return nil, err
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return nil, errors.New("not an HTTP(S) URL")
	}
	return u, nil
}

func configureAnyIPProfile(proxyURL *url.URL, profile, sessionID, expectedState, proxyType string) error {
	known := profile == "rotating" || profile == "sticky" || profile == "state-targeted"
	if !known && proxyType == "" {
		return nil
	}
	if proxyType != "" && proxyType != "residential" && proxyType != "mobile" {
		return fmt.Errorf("-type must be residential or mobile")
	}
	if proxyURL.User == nil || proxyURL.User.Username() == "" {
		return fmt.Errorf("AnyIP profile %q requires username/password proxy authentication; IP-whitelist URLs cannot add session, region or type parameters", profile)
	}

	username := proxyURL.User.Username()
	parts := strings.Split(username, ",")
	base := parts[0]
	params := make([]string, 0, len(parts)+3)
	for _, p := range parts[1:] {
		lower := strings.ToLower(strings.TrimSpace(p))
		switch {
		case known && strings.HasPrefix(lower, "session_"):
			// Session mode is controlled by the selected benchmark profile.
			continue
		case (profile == "rotating" || profile == "state-targeted") && strings.HasPrefix(lower, "sess"):
			// Session options (sesstime_, sessreplace_, sessasn_) are
			// meaningless without a session, and dashboard-generated
			// usernames for sticky profiles include them.
			continue
		case profile == "state-targeted" && (strings.HasPrefix(lower, "region_") || strings.HasPrefix(lower, "country_") || strings.HasPrefix(lower, "city_")):
			// Replace prior location settings with the requested US state.
			continue
		case proxyType != "" && strings.HasPrefix(lower, "type_"):
			continue
		default:
			params = append(params, p)
		}
	}
	if proxyType != "" {
		params = append(params, "type_"+proxyType)
	}

	switch profile {
	case "sticky":
		if sessionID == "" {
			sessionID = "webharvesterbenchmark"
		}
		// anyIP keys a session by its name alone: a second request with the
		// same name but a different type_ flag resumes the first session's
		// exit. Suffixing the type keeps residential and mobile arms apart.
		if proxyType != "" {
			sessionID += proxyType
		}
		if err := validSessionName(sessionID); err != nil {
			return err
		}
		params = append(params, "session_"+sessionID)
	case "state-targeted":
		region, err := regionSlug(expectedState)
		if err != nil {
			return err
		}
		params = append(params, "country_US", "region_"+region)
	}

	configuredUsername := base
	if len(params) > 0 {
		configuredUsername += "," + strings.Join(params, ",")
	}
	password, hasPassword := proxyURL.User.Password()
	if hasPassword {
		proxyURL.User = url.UserPassword(configuredUsername, password)
	} else {
		proxyURL.User = url.User(configuredUsername)
	}
	return nil
}

// regionSlug converts a state name to AnyIP's region value. The provider's
// published list (dashboard.anyip.io/api/data/region/US) uses the lowercase
// name with spaces removed, e.g. "District of Columbia" -> districtofcolumbia.
func regionSlug(state string) (string, error) {
	if strings.TrimSpace(state) == "" {
		return "", fmt.Errorf("-expected-state is required for state-targeted profile")
	}
	region := strings.ToLower(strings.TrimSpace(state))
	region = strings.NewReplacer(" ", "", "-", "", "_", "", ".", "", "'", "").Replace(region)
	for _, r := range region {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return "", fmt.Errorf("expected state %q cannot be converted to an AnyIP region slug", state)
		}
	}
	return region, nil
}

type response struct {
	status     int
	body       []byte
	retryAfter time.Duration
	ms         int64
	proxyError string
	reused     bool
	wireBytes  int64
	err        error
}

func (r *runner) do(target *url.URL, sessionID string) response {
	proxyURL, err := r.cfg.url(sessionID)
	if err != nil {
		return response{err: err}
	}
	a := &attemptState{proxyURL: proxyURL}
	ctx := context.WithValue(context.Background(), attemptKey{}, a)
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { a.reused = info.Reused }})
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	start := time.Now()
	var res response
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err == nil {
		var resp *http.Response
		resp, err = r.client.Do(req)
		if err == nil {
			res.status = resp.StatusCode
			if code := resp.Header.Get(anyIPErrorHeader); code != "" {
				a.proxyError = code
			}
			if secs, perr := strconv.Atoi(resp.Header.Get("Retry-After")); perr == nil && secs >= 0 {
				res.retryAfter = time.Duration(secs) * time.Second
			}
			res.body, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
		}
	}
	res.ms = time.Since(start).Milliseconds()
	res.wireBytes = a.wireBytes.Load()
	res.err, res.proxyError, res.reused = err, a.proxyError, a.reused
	return res
}

func (r *runner) runSample(targetURL, geoURL *url.URL, geoMode string) sample {
	s := sample{
		Timestamp: time.Now().UTC(), Profile: r.cfg.profile, ExpectedState: r.expectedState,
		TargetHost: targetURL.Hostname(), GeoSameRequest: geoMode == "same-request",
	}
	if geoMode == "separate" {
		g := r.do(geoURL, r.sessionID)
		s.GeoStatus, s.GeoMS, s.GeoBytes, s.GeoWireBytes = g.status, g.ms, int64(len(g.body)), g.wireBytes
		if g.err != nil {
			s.GeoErrorClass = classifyError(g.err)
		} else if g.status >= 200 && g.status < 300 {
			s.applyGeo(g.body, r.expectedState, r.hashIP)
		}
	}

	start := time.Now()
	var firstFailure time.Time // when the first attempt was seen to fail
	session := r.sessionID
	for attempt := 0; attempt <= r.retries; attempt++ {
		if attempt > 0 {
			wait := r.backoff << (attempt - 1)
			time.Sleep(wait)
			if r.newSession {
				r.sessionSeq++
				session = fmt.Sprintf("%sr%d", r.sessionID, r.sessionSeq)
			}
		}
		res := r.do(targetURL, session)
		s.Attempts++
		s.WireBytes += res.wireBytes
		if attempt == 0 {
			s.ConnReused = res.reused
		}
		s.TargetStatus, s.TargetMS = res.status, res.ms
		ok := res.err == nil && res.status >= 200 && res.status < 300
		if ok {
			s.TargetOK = true
			s.TargetBytes = int64(len(res.body))
			s.TargetUsable = r.expectSubstr == "" || strings.Contains(string(res.body), r.expectSubstr)
			s.FirstAttemptOK = attempt == 0
			if attempt > 0 {
				s.Recovered = true
				s.RecoveryMS = time.Since(firstFailure).Milliseconds()
				if session != r.sessionID {
					s.NewSession = true
					r.sessionID = session
				}
			}
			if geoMode == "same-request" {
				s.applyGeo(res.body, r.expectedState, r.hashIP)
			}
			s.ErrorClass, s.Error = "", ""
			break
		}

		if attempt == 0 {
			firstFailure = time.Now()
		}
		s.FailedBytes += int64(len(res.body))
		class := statusClass(res.status)
		if res.err != nil {
			class = classifyError(res.err)
			s.Error = r.redact(res.err.Error())
		}
		if res.proxyError != "" {
			s.ProxyError = res.proxyError
			class = "proxy_" + res.proxyError
		}
		s.ErrorClass = class
		s.AttemptErrors = append(s.AttemptErrors, class)
		if !retryable(res) || attempt == r.retries {
			break
		}
		if res.retryAfter > 0 && res.retryAfter <= 30*time.Second && res.retryAfter > r.backoff<<attempt {
			time.Sleep(res.retryAfter - r.backoff<<attempt)
		}
	}
	s.TotalMS = time.Since(start).Milliseconds()
	return s
}

func (s *sample) applyGeo(body []byte, expectedState string, hashIP func(string) string) {
	var geo geoResponse
	if json.Unmarshal(body, &geo) != nil {
		return
	}
	if geo.IP != "" {
		geo.IP = hashIP(geo.IP)
	}
	s.ObservedIP, s.ObservedState, s.ObservedCountry, s.ObservedOrg = geo.IP, geo.Region, geo.Country, geo.Org
	if expectedState != "" && s.ObservedState != "" {
		match := strings.EqualFold(strings.TrimSpace(expectedState), strings.TrimSpace(s.ObservedState))
		s.StateMatch = &match
	}
}

// validSessionName enforces anyIP's documented rule for session names:
// 1-32 characters, letters and digits only. Phase 0 showed the gateway
// accepting names with underscores without an error, yet rotating the
// exit on every request as if no session had been requested.
func validSessionName(name string) error {
	if len(name) < 1 || len(name) > 32 {
		return fmt.Errorf("session name %q must be 1-32 characters including the type and retry suffixes; shorten -session-id", name)
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return fmt.Errorf("session name %q may contain only letters and digits (anyIP rule); remove underscores, hyphens and other characters from -session-id", name)
		}
	}
	return nil
}

// randomSessionID names a sticky session uniquely per run, so a run never
// resumes an exit left behind by an earlier run with the same name.
func randomSessionID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		fatal("cannot generate session ID")
	}
	return "wh" + hex.EncodeToString(b)
}

// ipHasher returns a function mapping an IP to a short salted SHA-256 digest.
// The salt lives only in memory, so the digests cannot be reversed by
// enumerating the IPv4 space, yet equal IPs within one run stay equal.
func ipHasher() func(string) string {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		fatal("cannot generate IP hash salt")
	}
	return func(ip string) string {
		h := sha256.New()
		h.Write(salt)
		h.Write([]byte(ip))
		return "h:" + hex.EncodeToString(h.Sum(nil))[:16]
	}
}

// retryable reports whether another attempt could plausibly succeed. Auth
// and request-shape failures (407, 4xx other than 429) cannot.
func retryable(res response) bool {
	if res.err != nil {
		return true
	}
	return res.status == http.StatusTooManyRequests || res.status >= 500
}

func statusClass(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "http_429"
	case status >= 500:
		return "http_5xx"
	case status >= 400:
		return "http_4xx"
	case status >= 300:
		return "http_3xx"
	default:
		return "unknown"
	}
}

func classifyError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "proxyconnect"), strings.Contains(msg, "connect: connection refused"):
		return "proxy_connect"
	case strings.Contains(msg, "tls"), strings.Contains(msg, "certificate"):
		return "tls"
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "lookup"):
		return "dns"
	default:
		return "network"
	}
}

// redactor removes the proxy password and account ID from error text. It
// matches the account ID rather than the full username because each
// attempt's username carries different session, region and type flags.
// Query values of the target and geo URLs are removed too, because Go puts
// the request URL in its errors and those queries can carry API tokens.
func redactor(proxyURL *url.URL, requestURLs ...*url.URL) func(string) string {
	var secrets []string
	for _, u := range requestURLs {
		if u == nil || u.RawQuery == "" {
			continue
		}
		secrets = append(secrets, u.RawQuery)
		for _, values := range u.Query() {
			for _, v := range values {
				if v != "" {
					secrets = append(secrets, v, url.QueryEscape(v))
				}
			}
		}
	}
	if proxyURL.User != nil {
		if account := strings.Split(proxyURL.User.Username(), ",")[0]; account != "" {
			secrets = append(secrets, account, url.QueryEscape(account))
		}
		if password, ok := proxyURL.User.Password(); ok && password != "" {
			secrets = append(secrets, password, url.QueryEscape(password), url.PathEscape(password))
		}
	}
	return func(msg string) string {
		for _, s := range secrets {
			msg = strings.ReplaceAll(msg, s, "***")
		}
		return msg
	}
}

type aggregator struct {
	sum        summary
	latencies  []int64
	totals     []int64
	recoveries []int64
	ips        map[string]bool
	lastIP     string
}

func newAggregator(profile string) *aggregator {
	return &aggregator{
		sum: summary{
			Profile: profile, StartedAt: time.Now().UTC(),
			ObservedStates: map[string]int{}, ErrorClasses: map[string]int{}, ProxyErrorCodes: map[string]int{},
			BytesNote: "body bytes exclude headers, TLS and tunnel overhead; wire_bytes counts everything exchanged with the proxy (exact per attempt only with keepalive off) and should be reconciled against the provider's usage figures",
		},
		ips: map[string]bool{},
	}
}

func (a *aggregator) add(s sample) {
	sum := &a.sum
	sum.Samples++
	sum.RetryAttempts += s.Attempts - 1
	sum.TargetBytes += s.TargetBytes
	sum.GeoBytes += s.GeoBytes
	sum.FailedAttemptBytes += s.FailedBytes
	sum.WireBytes += s.WireBytes + s.GeoWireBytes
	if s.ConnReused {
		sum.ReusedConnections++
	}
	if s.TargetOK {
		sum.TargetSuccesses++
		a.latencies = append(a.latencies, s.TargetMS)
		a.totals = append(a.totals, s.TotalMS)
	}
	if s.TargetUsable {
		sum.UsableResults++
	}
	if s.FirstAttemptOK {
		sum.FirstAttemptSuccesses++
	}
	if s.Recovered {
		sum.Recovered++
		a.recoveries = append(a.recoveries, s.RecoveryMS)
		if s.NewSession {
			sum.NewSessionRecoveries++
		}
	}
	if s.ObservedIP != "" {
		sum.IPChecks++
		a.ips[s.ObservedIP] = true
		if a.lastIP != "" && a.lastIP != s.ObservedIP {
			sum.IPChanges++
		}
		a.lastIP = s.ObservedIP
	}
	if s.ObservedState != "" {
		sum.ObservedStates[s.ObservedState]++
	}
	if s.StateMatch != nil {
		sum.StateChecks++
		if *s.StateMatch {
			sum.StateMatches++
		}
	}
	for _, class := range s.AttemptErrors {
		sum.ErrorClasses[class]++
	}
	if s.GeoErrorClass != "" {
		sum.ErrorClasses["geo_"+s.GeoErrorClass]++
	}
	if s.ProxyError != "" {
		sum.ProxyErrorCodes[s.ProxyError]++
	}
}

func (a *aggregator) finish() summary {
	sum := a.sum
	sum.FinishedAt = time.Now().UTC()
	sum.DistinctIPs = len(a.ips)
	if sum.Samples > 0 {
		sum.TargetSuccessPct = pct(sum.TargetSuccesses, sum.Samples)
		sum.UsablePct = pct(sum.UsableResults, sum.Samples)
		sum.WireBytesPerSample = sum.WireBytes / int64(sum.Samples)
		lo, hi := wilson(sum.UsableResults, sum.Samples)
		sum.UsableCI95 = [2]float64{round1(lo * 100), round1(hi * 100)}
	}
	sum.MedianTargetMS, sum.P95TargetMS = medianP95(a.latencies)
	sum.MedianTotalMS, sum.P95TotalMS = medianP95(a.totals)
	sum.MedianRecoveryMS, _ = medianP95(a.recoveries)
	if len(a.latencies) < 20 {
		sum.LatencyNote = fmt.Sprintf("only %d successful samples; p95 is the slowest value or close to it and is not a stable estimate", len(a.latencies))
	}
	return sum
}

func medianP95(values []int64) (int64, int64) {
	if len(values) == 0 {
		return 0, 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return percentile(sorted, .50), percentile(sorted, .95)
}

func percentile(values []int64, p float64) int64 {
	if len(values) == 0 {
		return 0
	}
	if p == .50 && len(values)%2 == 0 {
		return (values[len(values)/2-1] + values[len(values)/2]) / 2
	}
	i := int(math.Ceil(p*float64(len(values)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(values) {
		i = len(values) - 1
	}
	return values[i]
}

// wilson returns the 95% Wilson score interval for k successes in n trials,
// which stays meaningful at small n and at rates near 0% or 100%.
func wilson(k, n int) (float64, float64) {
	if n == 0 {
		return 0, 0
	}
	const z = 1.959964
	p := float64(k) / float64(n)
	nf := float64(n)
	denom := 1 + z*z/nf
	center := (p + z*z/(2*nf)) / denom
	half := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf)) / denom
	return math.Max(0, center-half), math.Min(1, center+half)
}

func pct(k, n int) float64 { return round1(float64(k) * 100 / float64(n)) }

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// commit identifies the code that produced a result: the CI commit when run
// in GitHub Actions, otherwise the VCS revision stamped into the binary.
func commit() string {
	if sha := os.Getenv("GITHUB_SHA"); sha != "" {
		return sha
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var revision string
	var modified bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision != "" && modified {
		revision += "+dirty"
	}
	return revision
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, "proxybench:", message)
	os.Exit(2)
}
