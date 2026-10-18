package proxy

import (
	"strings"
	"testing"
)

func TestProxyDisplayIDRedactsCredentials(t *testing.T) {
	p := &Proxy{URL: "http://user_abc,type_residential:super-secret@portal.anyip.io:1080"}
	got := p.DisplayID()
	if got != "http://portal.anyip.io:1080" {
		t.Fatalf("DisplayID() = %q, want credential-free host identifier", got)
	}
	if strings.Contains(got, "super-secret") || strings.Contains(got, "user_abc") || strings.Contains(got, "@") {
		t.Fatalf("DisplayID() leaked proxy credentials: %q", got)
	}
}

func TestProxyDisplayIDDirectAndMalformed(t *testing.T) {
	if got := (&Proxy{}).DisplayID(); got != "direct" {
		t.Fatalf("direct DisplayID() = %q, want direct", got)
	}
	if got := (&Proxy{URL: "not a url"}).DisplayID(); got != "configured-proxy" {
		t.Fatalf("malformed DisplayID() = %q, want configured-proxy", got)
	}
}
