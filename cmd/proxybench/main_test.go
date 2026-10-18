package main

import (
	"context"
	"errors"
	"net"
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
