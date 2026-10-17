// Command proxybench runs a small, auditable proxy quality benchmark.
// It deliberately fails closed when HARVESTER_PROXY_URL is not set.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

type geoResponse struct {
	IP      string `json:"ip"`
	Region  string `json:"region"`
	Country string `json:"country"`
}

type sample struct {
	Timestamp     time.Time `json:"timestamp"`
	Profile       string    `json:"profile"`
	ExpectedState string    `json:"expected_state,omitempty"`
	TargetHost    string    `json:"target_host"`
	TargetStatus  int       `json:"target_status,omitempty"`
	TargetMS      int64     `json:"target_ms,omitempty"`
	TargetOK      bool      `json:"target_ok"`
	GeoStatus     int       `json:"geo_status,omitempty"`
	GeoMS         int64     `json:"geo_ms,omitempty"`
	ObservedIP    string    `json:"observed_ip,omitempty"`
	ObservedState string    `json:"observed_state,omitempty"`
	ObservedCountry string  `json:"observed_country,omitempty"`
	StateMatch    *bool     `json:"state_match,omitempty"`
	ErrorClass    string    `json:"error_class,omitempty"`
	Error         string    `json:"error,omitempty"`
}

type summary struct {
	Profile          string         `json:"profile"`
	Samples          int            `json:"samples"`
	TargetSuccesses  int            `json:"target_successes"`
	TargetSuccessPct float64        `json:"target_success_pct"`
	MedianTargetMS   int64          `json:"median_target_ms,omitempty"`
	P95TargetMS      int64          `json:"p95_target_ms,omitempty"`
	StateMatches     int            `json:"state_matches,omitempty"`
	StateChecks      int            `json:"state_checks,omitempty"`
	ObservedStates   map[string]int `json:"observed_states,omitempty"`
	ErrorClasses     map[string]int `json:"error_classes,omitempty"`
}

func main() {
	profile := flag.String("profile", "unspecified", "operator-defined profile label, e.g. rotating, sticky, or state-targeted-TX")
	expectedState := flag.String("expected-state", "", "expected US state/region name; compared case-insensitively with geo probe")
	target := flag.String("target-url", "https://httpbin.org/status/200", "URL you are authorized to request")
	geoURL := flag.String("geo-url", "https://ipinfo.io/json", "IP geolocation endpoint used to observe proxy egress")
	requests := flag.Int("requests", 5, "number of sequential samples (each makes one geo probe and one target request)")
	timeout := flag.Duration("timeout", 15*time.Second, "timeout per HTTP request")
	delay := flag.Duration("delay", time.Second, "delay between samples")
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
	targetURL, err := url.ParseRequestURI(*target)
	if err != nil || (targetURL.Scheme != "https" && targetURL.Scheme != "http") || targetURL.Hostname() == "" {
		fatal("-target-url must be a valid HTTP(S) URL")
	}
	geoTarget, err := url.ParseRequestURI(*geoURL)
	if err != nil || (geoTarget.Scheme != "https" && geoTarget.Scheme != "http") || geoTarget.Hostname() == "" {
		fatal("-geo-url must be a valid HTTP(S) URL")
	}

	proxyRaw := strings.TrimSpace(os.Getenv("HARVESTER_PROXY_URL"))
	if proxyRaw == "" {
		fatal("HARVESTER_PROXY_URL is required; refusing to send benchmark traffic directly")
	}
	proxyURL, err := url.Parse(proxyRaw)
	if err != nil || (proxyURL.Scheme != "http" && proxyURL.Scheme != "https") || proxyURL.Hostname() == "" {
		fatal("HARVESTER_PROXY_URL must be a valid HTTP(S) proxy URL")
	}

	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		DialContext: (&net.Dialer{Timeout: *timeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: *timeout,
		ResponseHeaderTimeout: *timeout,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: *timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 { return errors.New("too many redirects") }
		return nil
	}}

	var writer io.Writer = os.Stdout
	var file *os.File
	if *outPath != "-" {
		file, err = os.OpenFile(*outPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil { fatal("cannot open output file") }
		defer file.Close()
		writer = file
	}
	encoder := json.NewEncoder(writer)
	latencies := make([]int64, 0, *requests)
	counts := summary{Profile: *profile, ObservedStates: map[string]int{}, ErrorClasses: map[string]int{}}
	for i := 0; i < *requests; i++ {
		if i > 0 && *delay > 0 { time.Sleep(*delay) }
		s := runSample(client, *profile, *expectedState, targetURL, geoTarget, *timeout)
		if err := encoder.Encode(s); err != nil { fatal("failed to write JSONL output") }
		counts.Samples++
		if s.TargetOK {
			counts.TargetSuccesses++
			latencies = append(latencies, s.TargetMS)
		}
		if s.ObservedState != "" { counts.ObservedStates[s.ObservedState]++ }
		if s.StateMatch != nil {
			counts.StateChecks++
			if *s.StateMatch { counts.StateMatches++ }
		}
		if s.ErrorClass != "" { counts.ErrorClasses[s.ErrorClass]++ }
	}
	if counts.Samples > 0 { counts.TargetSuccessPct = float64(counts.TargetSuccesses) * 100 / float64(counts.Samples) }
	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		counts.MedianTargetMS = percentile(latencies, .50)
		counts.P95TargetMS = percentile(latencies, .95)
	}
	enc := json.NewEncoder(os.Stderr)
	enc.SetIndent("", "  ")
	if err := enc.Encode(counts); err != nil { fatal("failed to write summary") }
}

func runSample(client *http.Client, profile, expectedState string, targetURL, geoURL *url.URL, timeout time.Duration) sample {
	s := sample{
		Timestamp: time.Now().UTC(), Profile: profile, ExpectedState: expectedState,
		TargetHost: targetURL.Hostname(),
	}
	geoStart := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, geoURL.String(), nil)
	if err == nil {
		var resp *http.Response
		resp, err = client.Do(req)
		if err == nil {
			s.GeoStatus = resp.StatusCode
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if readErr != nil { err = readErr } else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				var geo geoResponse
				if json.Unmarshal(body, &geo) == nil {
					s.ObservedIP, s.ObservedState, s.ObservedCountry = geo.IP, geo.Region, geo.Country
				}
			}
		}
	}
	cancel()
	s.GeoMS = time.Since(geoStart).Milliseconds()
	if expectedState != "" && s.ObservedState != "" {
		match := strings.EqualFold(strings.TrimSpace(expectedState), strings.TrimSpace(s.ObservedState))
		s.StateMatch = &match
	}
	if err != nil {
		s.ErrorClass = classifyError(err)
		s.Error = safeError(err)
	}

	targetStart := time.Now()
	ctx, cancel = context.WithTimeout(context.Background(), timeout)
	req, targetErr := http.NewRequestWithContext(ctx, http.MethodGet, targetURL.String(), nil)
	if targetErr == nil {
		var resp *http.Response
		resp, targetErr = client.Do(req)
		if targetErr == nil {
			s.TargetStatus = resp.StatusCode
			_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if readErr != nil { targetErr = readErr }
		}
	}
	cancel()
	s.TargetMS = time.Since(targetStart).Milliseconds()
	s.TargetOK = targetErr == nil && s.TargetStatus >= 200 && s.TargetStatus < 300
	if targetErr != nil {
		s.ErrorClass = classifyError(targetErr)
		if s.Error == "" { s.Error = safeError(targetErr) }
	} else if s.TargetStatus == http.StatusTooManyRequests {
		s.ErrorClass = "http_429"
	} else if s.TargetStatus >= 500 {
		s.ErrorClass = "http_5xx"
	} else if s.TargetStatus >= 400 {
		s.ErrorClass = "http_4xx"
	}
	return s
}

func classifyError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) { return "timeout" }
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() { return "timeout" }
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

func safeError(err error) string {
	// Do not expose proxy credentials if the HTTP library includes the URL in an error.
	msg := err.Error()
	if proxy := os.Getenv("HARVESTER_PROXY_URL"); proxy != "" {
		if parsed, e := url.Parse(proxy); e == nil && parsed.User != nil {
			msg = strings.ReplaceAll(msg, parsed.User.String()+"@", "***@")
			msg = strings.ReplaceAll(msg, parsed.User.String(), "***")
		}
	}
	return msg
}

func percentile(values []int64, p float64) int64 {
	if len(values) == 0 { return 0 }
	i := int(math.Ceil(p*float64(len(values)))) - 1
	if i < 0 { i = 0 }; if i >= len(values) { i = len(values)-1 }
	return values[i]
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, "proxybench:", message)
	os.Exit(2)
}

