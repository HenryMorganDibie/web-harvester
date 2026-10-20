package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	values := []int64{10, 20, 30, 40}
	if got := percentile(values, .50); got != 25 {
		t.Fatalf("median = %d, want 25", got)
	}
	if got := percentile(values, .95); got != 40 {
		t.Fatalf("p95 = %d, want 40", got)
	}
	if got := percentile(nil, .95); got != 0 {
		t.Fatalf("empty p95 = %d, want 0", got)
	}
}

func TestWilson(t *testing.T) {
	lo, hi := wilson(0, 0)
	if lo != 0 || hi != 0 {
		t.Fatalf("wilson(0,0) = %v,%v, want 0,0", lo, hi)
	}
	// 27/30 = 90%: the Wilson interval is roughly 74.4%..96.5%.
	lo, hi = wilson(27, 30)
	if lo < .74 || lo > .75 || hi < .96 || hi > .97 {
		t.Fatalf("wilson(27,30) = %.4f,%.4f", lo, hi)
	}
	// A perfect small sample must not claim certainty.
	if lo, hi = wilson(5, 5); hi != 1 || lo > .6 {
		t.Fatalf("wilson(5,5) = %.4f,%.4f, want lower bound well below 100%%", lo, hi)
	}
}

func TestClassifyError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"proxy connect", errors.New("proxyconnect tcp: connection refused"), "proxy_connect"},
		{"dns", errors.New("lookup example.com: no such host"), "dns"},
		{"tls", errors.New("tls: bad certificate"), "tls"},
		{"network timeout", timeoutError{}, "timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyError(tt.err); got != tt.want {
				t.Fatalf("classifyError() = %q, want %q", got, tt.want)
			}
		})
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "timed out" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

var _ net.Error = timeoutError{}

func TestConfigureAnyIPProfile(t *testing.T) {
	tests := []struct {
		name, raw, profile, sessionID, state, proxyType, wantUser string
		wantErr                                                   bool
	}{
		{
			name:     "rotating removes session",
			raw:      "http://user_123,country_US,session_old:secret@portal.anyip.io:1080",
			profile:  "rotating",
			wantUser: "user_123,country_US",
		},
		{
			name:     "rotating drops session options from a dashboard sticky username",
			raw:      "http://user_123,type_residential,country_us,sesstime_10080,session_abc:secret@portal.anyip.io:1080",
			profile:  "rotating",
			wantUser: "user_123,type_residential,country_us",
		},
		{
			name:      "sticky keeps the dashboard session duration",
			raw:       "http://user_123,type_residential,country_us,sesstime_10080,session_abc:secret@portal.anyip.io:1080",
			profile:   "sticky",
			sessionID: "run1",
			wantUser:  "user_123,type_residential,country_us,sesstime_10080,session_run1",
		},
		{
			name:     "state targeting drops session options",
			raw:      "http://user_123,country_us,sesstime_10080,session_abc:secret@portal.anyip.io:1080",
			profile:  "state-targeted",
			state:    "Texas",
			wantUser: "user_123,country_US,region_texas",
		},
		{
			name:      "sticky session name is kept apart per type",
			raw:       "http://user_123,type_residential,country_us,session_abc:secret@portal.anyip.io:1080",
			profile:   "sticky",
			sessionID: "smoke1",
			proxyType: "mobile",
			wantUser:  "user_123,country_us,type_mobile,session_smoke1mobile",
		},
		{
			name:      "sticky keeps sessreplace_false and the dashboard duration",
			raw:       "http://user_123,type_residential,country_us,sesstime_10080,sessreplace_false,session_abc:secret@portal.anyip.io:1080",
			profile:   "sticky",
			sessionID: "smoke3",
			proxyType: "mobile",
			wantUser:  "user_123,country_us,sesstime_10080,sessreplace_false,type_mobile,session_smoke3mobile",
		},
		{
			name:      "sticky adds session",
			raw:       "http://user_123,country_US,session_old:secret@portal.anyip.io:1080",
			profile:   "sticky",
			sessionID: "whrun123",
			wantUser:  "user_123,country_US,session_whrun123",
		},
		{
			name:     "state targeting replaces region, city and country",
			raw:      "http://user_123,country_CA,region_ontario,city_toronto,session_old:secret@portal.anyip.io:1080",
			profile:  "state-targeted",
			state:    "New York",
			wantUser: "user_123,country_US,region_newyork",
		},
		{
			name:     "district of columbia matches the provider's region value",
			raw:      "http://user_123:secret@portal.anyip.io:1080",
			profile:  "state-targeted",
			state:    "District of Columbia",
			wantUser: "user_123,country_US,region_districtofcolumbia",
		},
		{
			name:      "type replaces an existing type",
			raw:       "http://user_123,type_residential:secret@portal.anyip.io:1080",
			profile:   "rotating",
			proxyType: "mobile",
			wantUser:  "user_123,type_mobile",
		},
		{
			name:      "type works without a session profile",
			raw:       "http://user_123,country_US:secret@portal.anyip.io:1080",
			profile:   "unspecified",
			proxyType: "residential",
			wantUser:  "user_123,country_US,type_residential",
		},
		{
			name:      "unknown type is rejected",
			raw:       "http://user_123:secret@portal.anyip.io:1080",
			profile:   "rotating",
			proxyType: "datacenter",
			wantErr:   true,
		},
		{
			name:      "underscore in session ID is rejected",
			raw:       "http://user_123:secret@portal.anyip.io:1080",
			profile:   "sticky",
			sessionID: "smoke2_mobile",
			wantErr:   true,
		},
		{
			name:      "session name over 32 characters with its type suffix is rejected",
			raw:       "http://user_123:secret@portal.anyip.io:1080",
			profile:   "sticky",
			sessionID: "abcdefghijklmnopqrstuvwxy",
			proxyType: "residential",
			wantErr:   true,
		},
		{
			name:      "pipe in session ID is rejected",
			raw:       "http://user_123:secret@portal.anyip.io:1080",
			profile:   "sticky",
			sessionID: "a|b",
			wantErr:   true,
		},
		{
			name:    "state targeting requires state",
			raw:     "http://user_123:secret@portal.anyip.io:1080",
			profile: "state-targeted",
			wantErr: true,
		},
		{
			name:    "sticky requires user authentication",
			raw:     "http://portal.anyip.io:1080",
			profile: "sticky",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxyURL, err := url.Parse(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			err = configureAnyIPProfile(proxyURL, tt.profile, tt.sessionID, tt.state, tt.proxyType)
			if (err != nil) != tt.wantErr {
				t.Fatalf("configureAnyIPProfile error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if got := proxyURL.User.Username(); got != tt.wantUser {
				t.Fatalf("username = %q, want %q", got, tt.wantUser)
			}
			if password, ok := proxyURL.User.Password(); !ok || password != "secret" {
				t.Fatalf("proxy password was not preserved")
			}
		})
	}
}

func TestProfileConfigDoesNotMutateBase(t *testing.T) {
	base, _ := url.Parse("http://user_123,country_US:secret@portal.anyip.io:1080")
	cfg := profileConfig{base: base, profile: "sticky"}
	if _, err := cfg.url("one"); err != nil {
		t.Fatal(err)
	}
	u, err := cfg.url("two")
	if err != nil {
		t.Fatal(err)
	}
	if got := u.User.Username(); got != "user_123,country_US,session_two" {
		t.Fatalf("second session username = %q", got)
	}
	if got := base.User.Username(); got != "user_123,country_US" {
		t.Fatalf("base username was mutated to %q", got)
	}
}

func TestRedactor(t *testing.T) {
	base, _ := url.Parse("http://user_abc123,country_US:p%40ss@portal.anyip.io:1080")
	redact := redactor(base)
	msg := redact("proxyconnect via user_abc123,country_US,session_x_r1:p@ss@portal and p%40ss")
	if strings.Contains(msg, "user_abc123") || strings.Contains(msg, "p@ss") || strings.Contains(msg, "p%40ss") {
		t.Fatalf("secret survived redaction: %q", msg)
	}
}

func TestRedactorRemovesRequestQueryTokens(t *testing.T) {
	base, _ := url.Parse("http://user_abc:pw@portal.anyip.io:1080")
	target, _ := url.Parse("https://ipinfo.io/json?token=tok123abc")
	redact := redactor(base, target, nil)
	msg := redact(`Get "https://ipinfo.io/json?token=tok123abc": context deadline exceeded`)
	if strings.Contains(msg, "tok123abc") {
		t.Fatalf("query token survived redaction: %q", msg)
	}
}

func TestAddIPInfoToken(t *testing.T) {
	u, _ := url.Parse("https://ipinfo.io/json")
	addIPInfoToken(u, "tok123")
	if u.Query().Get("token") != "tok123" || u.Path != "/json" {
		t.Fatalf("token not added: %s", u)
	}
	explicit, _ := url.Parse("https://ipinfo.io/json?token=mine")
	addIPInfoToken(explicit, "tok123")
	if explicit.Query().Get("token") != "mine" {
		t.Fatalf("explicit token was overridden: %s", explicit)
	}
	other, _ := url.Parse("https://example.com/page")
	addIPInfoToken(other, "tok123")
	if other.RawQuery != "" {
		t.Fatalf("token leaked to another host: %s", other)
	}
	addIPInfoToken(nil, "tok123")
}

// fakeProxy is a scripted forward proxy. Plain-HTTP requests are answered
// according to the session in the Proxy-Authorization username; CONNECT
// requests are refused the way anyIP refuses an unsatisfiable tunnel.
type fakeProxy struct {
	mu        sync.Mutex
	usernames []string
}

func (f *fakeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user := proxyUser(r)
	f.mu.Lock()
	f.usernames = append(f.usernames, user)
	f.mu.Unlock()
	switch {
	case r.Method == http.MethodConnect:
		w.Header().Set(anyIPErrorHeader, "no_peers_available")
		w.WriteHeader(http.StatusBadGateway)
	case strings.Contains(user, "session_bad,") || strings.HasSuffix(user, "session_bad"):
		w.Header().Set(anyIPErrorHeader, "peer_not_found")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, "peer_not_found")
	case strings.Contains(user, "session_auth"):
		w.Header().Set(anyIPErrorHeader, "wrong_customer_credentials")
		w.WriteHeader(http.StatusProxyAuthRequired)
	default:
		fmt.Fprint(w, `{"ip":"203.0.113.7","region":"Texas","country":"US","org":"AS64500 Example"}`)
	}
}

func proxyUser(r *http.Request) string {
	raw, ok := strings.CutPrefix(r.Header.Get("Proxy-Authorization"), "Basic ")
	if !ok {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return ""
	}
	user, _, _ := strings.Cut(string(decoded), ":")
	return user
}

func testRunner(t *testing.T, proxy *httptest.Server, profile, session string, retries int, newSession bool) *runner {
	t.Helper()
	base, err := url.Parse(strings.Replace(proxy.URL, "http://", "http://user_test:secret@", 1))
	if err != nil {
		t.Fatal(err)
	}
	return &runner{
		client:        newClient(2*time.Second, false),
		cfg:           profileConfig{base: base, profile: profile, expectedState: "Texas"},
		expectedState: "Texas",
		expectSubstr:  `"ip"`,
		timeout:       2 * time.Second,
		retries:       retries,
		newSession:    newSession,
		sessionID:     session,
		redact:        redactor(base),
		hashIP:        func(ip string) string { return ip },
	}
}

func TestRunSampleRecoversOnNewSession(t *testing.T) {
	fp := &fakeProxy{}
	proxy := httptest.NewServer(fp)
	defer proxy.Close()
	r := testRunner(t, proxy, "sticky", "bad", 2, true)

	// The target is never contacted: the fake proxy answers plain-HTTP
	// requests itself, as the provider's gateway does for its own errors.
	target, _ := url.Parse("http://geo.invalid/json")
	s := r.runSample(target, target, "same-request")

	if !s.TargetOK || !s.TargetUsable || !s.Recovered || !s.NewSession || s.FirstAttemptOK {
		t.Fatalf("sample = %+v, want a usable result recovered on a new session", s)
	}
	if s.Attempts != 2 || s.ProxyError != "peer_not_found" || s.ErrorClass != "" {
		t.Fatalf("attempts=%d proxyError=%q errorClass=%q", s.Attempts, s.ProxyError, s.ErrorClass)
	}
	if len(s.AttemptErrors) != 1 || s.AttemptErrors[0] != "proxy_peer_not_found" {
		t.Fatalf("attempt errors = %v", s.AttemptErrors)
	}
	if s.FailedBytes == 0 {
		t.Fatalf("bytes from the failed attempt were not counted")
	}
	// Wire bytes cover both attempts' request and response headers, so they
	// must exceed the bodies alone.
	if s.WireBytes <= s.TargetBytes+s.FailedBytes {
		t.Fatalf("wire bytes = %d, want more than body bytes %d", s.WireBytes, s.TargetBytes+s.FailedBytes)
	}
	if s.ObservedIP != "203.0.113.7" || s.StateMatch == nil || !*s.StateMatch || !s.GeoSameRequest {
		t.Fatalf("geo from the target request was not applied: %+v", s)
	}
	if r.sessionID != "badr1" {
		t.Fatalf("runner session = %q, want the recovered session to be kept", r.sessionID)
	}
	if got := fp.usernames; len(got) != 2 || got[0] != "user_test,session_bad" || got[1] != "user_test,session_badr1" {
		t.Fatalf("proxy usernames = %v", got)
	}
}

func TestRunSampleRecordsRefusedTunnel(t *testing.T) {
	proxy := httptest.NewServer(&fakeProxy{})
	defer proxy.Close()
	r := testRunner(t, proxy, "rotating", "", 1, false)

	target, _ := url.Parse("https://target.invalid/")
	s := r.runSample(target, nil, "none")

	if s.TargetOK || s.Attempts != 2 || s.ProxyError != "no_peers_available" {
		t.Fatalf("sample = %+v, want two refused tunnels with the provider's error code", s)
	}
	for _, class := range s.AttemptErrors {
		if class != "proxy_no_peers_available" {
			t.Fatalf("attempt errors = %v", s.AttemptErrors)
		}
	}
	if strings.Contains(s.Error, "user_test") || strings.Contains(s.Error, "secret") {
		t.Fatalf("error leaked credentials: %q", s.Error)
	}
}

func TestRunSampleDoesNotRetryAuthFailure(t *testing.T) {
	proxy := httptest.NewServer(&fakeProxy{})
	defer proxy.Close()
	r := testRunner(t, proxy, "sticky", "auth", 3, false)

	target, _ := url.Parse("http://target.invalid/")
	s := r.runSample(target, nil, "none")

	if s.TargetOK || s.Attempts != 1 || s.ProxyError != "wrong_customer_credentials" {
		t.Fatalf("sample = %+v, want one non-retried auth failure", s)
	}
}

func TestRenewedSession(t *testing.T) {
	cases := []struct {
		i, n int
		want string
	}{
		{0, 15, "wh1"}, {14, 15, "wh1"}, {15, 15, "wh1s1"}, {29, 15, "wh1s1"}, {30, 15, "wh1s2"},
	}
	for _, c := range cases {
		if got := renewedSession("wh1", c.i, c.n); got != c.want {
			t.Fatalf("renewedSession(wh1, %d, %d) = %q, want %q", c.i, c.n, got, c.want)
		}
		if err := validSessionName(renewedSession("wh1", c.i, c.n) + "residential"); err != nil {
			t.Fatalf("renewed name invalid: %v", err)
		}
	}
}

func TestRandomSessionID(t *testing.T) {
	a, b := randomSessionID(), randomSessionID()
	if a == b || !strings.HasPrefix(a, "wh") || strings.ContainsAny(a, ", |@") {
		t.Fatalf("session IDs %q %q must be unique per run and valid in a username", a, b)
	}
}

func TestIPHasher(t *testing.T) {
	hash := ipHasher()
	a, b := hash("198.51.100.1"), hash("198.51.100.2")
	if a == b || a != hash("198.51.100.1") || strings.Contains(a, "198.51") {
		t.Fatalf("hashes %q %q must be stable per IP, distinct across IPs, and not contain the IP", a, b)
	}
	if ipHasher()("198.51.100.1") == a {
		t.Fatalf("hash salt must differ between runs")
	}
}

func TestAggregator(t *testing.T) {
	a := newAggregator("rotating")
	a.add(sample{TargetOK: true, TargetUsable: true, FirstAttemptOK: true, Attempts: 1, TargetMS: 100, TotalMS: 100, ObservedIP: "198.51.100.1", TargetBytes: 10})
	a.add(sample{TargetOK: true, TargetUsable: false, FirstAttemptOK: true, Attempts: 1, TargetMS: 300, TotalMS: 300, ObservedIP: "198.51.100.1", TargetBytes: 10})
	a.add(sample{TargetOK: true, TargetUsable: true, Recovered: true, RecoveryMS: 700, Attempts: 2, TargetMS: 200, TotalMS: 900, ObservedIP: "198.51.100.2", AttemptErrors: []string{"timeout"}, FailedBytes: 5})
	a.add(sample{Attempts: 3, AttemptErrors: []string{"proxy_peer_timeout", "proxy_peer_timeout", "timeout"}, ProxyError: "peer_timeout"})
	sum := a.finish()

	if sum.Samples != 4 || sum.TargetSuccesses != 3 || sum.UsableResults != 2 || sum.FirstAttemptSuccesses != 2 || sum.Recovered != 1 {
		t.Fatalf("counts = %+v", sum)
	}
	if sum.RetryAttempts != 3 || sum.ErrorClasses["proxy_peer_timeout"] != 2 || sum.ErrorClasses["timeout"] != 2 || sum.ProxyErrorCodes["peer_timeout"] != 1 {
		t.Fatalf("retry accounting = %+v", sum)
	}
	if sum.DistinctIPs != 2 || sum.IPChanges != 1 || sum.IPChecks != 3 {
		t.Fatalf("ip accounting: distinct=%d changes=%d checks=%d", sum.DistinctIPs, sum.IPChanges, sum.IPChecks)
	}
	if sum.MedianTargetMS != 200 || sum.MedianTotalMS != 300 || sum.MedianRecoveryMS != 700 {
		t.Fatalf("latency: median target=%d total=%d recovery=%d", sum.MedianTargetMS, sum.MedianTotalMS, sum.MedianRecoveryMS)
	}
	if sum.TargetBytes != 20 || sum.FailedAttemptBytes != 5 || sum.UsablePct != 50 || sum.LatencyNote == "" {
		t.Fatalf("bytes/pct/note = %+v", sum)
	}
}
