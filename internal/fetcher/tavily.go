package fetcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TavilyFetcher calls Tavily's native POST /search API. It is intentionally
// separate from ProviderFetcher so the existing SerpApi-compatible contract
// and AI Overview follow-up behavior remain unchanged.
type TavilyFetcher struct {
	BaseURL string
	APIKey  string
	Timeout time.Duration
}

type tavilySearchRequest struct {
	APIKey      string `json:"api_key"`
	Query       string `json:"query"`
	SearchDepth string `json:"search_depth"`
	MaxResults  int    `json:"max_results"`
	Topic       string `json:"topic"`
}

func NewTavilyFetcher(apiKey string, timeout time.Duration) *TavilyFetcher {
	return &TavilyFetcher{BaseURL: "https://api.tavily.com/search", APIKey: apiKey, Timeout: timeout}
}

// Fetch returns Tavily's native JSON response for parser.TavilyParser.
func (f *TavilyFetcher) Fetch(ctx context.Context, req Request) (*Response, error) {
	start := time.Now()
	if strings.TrimSpace(f.APIKey) == "" {
		return nil, fmt.Errorf("tavily fetcher: API key is empty")
	}
	endpoint := f.BaseURL
	if endpoint == "" {
		endpoint = "https://api.tavily.com/search"
	}
	payload := tavilySearchRequest{APIKey: f.APIKey, Query: req.Query, SearchDepth: "basic", MaxResults: 10, Topic: "general"}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("tavily fetcher: encode request: %w", err)
	}
	client := &http.Client{Timeout: f.Timeout}
	if req.ProxyURL != "" {
		pu, err := url.Parse(req.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("tavily fetcher: invalid proxy url: %w", err)
		}
		client.Transport = &http.Transport{Proxy: http.ProxyURL(pu)}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("tavily fetcher: build request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("tavily fetcher: request failed: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, fmt.Errorf("tavily fetcher: read response: %w", err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter, _ := parseRetryAfter(resp.Header.Get("Retry-After"))
		return nil, &RateLimitError{StatusCode: resp.StatusCode, RetryAfter: retryAfter, Body: string(responseBody)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Do not include the request payload or API key in errors.
		return nil, fmt.Errorf("tavily fetcher: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	var check struct { Results json.RawMessage `json:"results"` }
	if err := json.Unmarshal(responseBody, &check); err != nil {
		return nil, fmt.Errorf("tavily fetcher: invalid JSON response: %w", err)
	}
	if len(check.Results) == 0 || string(check.Results) == "null" {
		return nil, fmt.Errorf("tavily fetcher: response is missing results")
	}
	return &Response{StatusCode: resp.StatusCode, Body: responseBody, Latency: time.Since(start), ContentType: resp.Header.Get("Content-Type")}, nil
}
