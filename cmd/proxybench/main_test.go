package main

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"
)

func TestPercentile(t *testing.T) {
	values := []int64{10, 20, 30, 40}
	if got := percentile(values, .50); got != 25 {
		t.Fatalf("median = %d, want 30", got)
	}
	if got := percentile(values, .95); got != 40 {
		t.Fatalf("p95 = %d, want 40", got)
	}
	if got := percentile(nil, .95); got != 0 {
		t.Fatalf("empty p95 = %d, want 0", got)
	}
}

func TestClassifyError(t *testing.T) {
	tests := []struct {
		name string
		err error
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
		name, raw, profile, sessionID, state, wantUser string
		wantErr bool
	}{
		{
			name: "rotating removes session",
			raw: "http://user_123,country_US,session_old:secret@portal.anyip.io:1080",
			profile: "rotating",
			wantUser: "user_123,country_US",
		},
		{
			name: "sticky adds session",
			raw: "http://user_123,country_US,session_old:secret@portal.anyip.io:1080",
			profile: "sticky",
			sessionID: "wh_run_123",
			wantUser: "user_123,country_US,session_wh_run_123",
		},
		{
			name: "state targeting replaces region and country",
			raw: "http://user_123,country_CA,region_ontario,session_old:secret@portal.anyip.io:1080",
			profile: "state-targeted",
			state: "New York",
			wantUser: "user_123,country_US,region_newyork",
		},
		{
			name: "state targeting requires state",
			raw: "http://user_123:secret@portal.anyip.io:1080",
			profile: "state-targeted",
			wantErr: true,
		},
		{
			name: "sticky requires user authentication",
			raw: "http://portal.anyip.io:1080",
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
			err = configureAnyIPProfile(proxyURL, tt.profile, tt.sessionID, tt.state)
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
			if !strings.Contains(proxyURL.String(), "secret") {
				t.Fatalf("test proxy URL unexpectedly lost its password")
			}
		})
	}
}
