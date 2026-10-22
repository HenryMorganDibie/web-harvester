package fetcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTavilyFetcher_Fetch(t *testing.T) {
	var got tavilySearchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost { t.Errorf("method = %s, want POST", r.Method) }
		if r.Header.Get("Content-Type") != "application/json" { t.Errorf("missing JSON content type") }
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil { t.Errorf("decode request: %v", err) }
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"query":"golang worker pool","results":[{"title":"Go docs","url":"https://go.dev","content":"Concurrency in Go","score":0.9}]}`))
	}))
	defer srv.Close()
	f := NewTavilyFetcher("test-key", 5*time.Second)
	f.BaseURL = srv.URL + "/search"
	resp, err := f.Fetch(context.Background(), Request{Query: "golang worker pool"})
	if err != nil { t.Fatalf("Fetch returned error: %v", err) }
	if resp.StatusCode != http.StatusOK || len(resp.Body) == 0 { t.Fatalf("unexpected response: %+v", resp) }
	if got.APIKey != "test-key" || got.Query != "golang worker pool" || got.Topic != "general" || got.SearchDepth != "basic" || got.MaxResults != 10 {
		t.Errorf("unexpected request payload: %+v", got)
	}
}

func TestTavilyFetcher_RejectsMissingResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"query":"q"}`)) }))
	defer srv.Close()
	f := NewTavilyFetcher("test-key", time.Second)
	f.BaseURL = srv.URL
	if _, err := f.Fetch(context.Background(), Request{Query: "q"}); err == nil { t.Fatal("expected missing-results error") }
}
