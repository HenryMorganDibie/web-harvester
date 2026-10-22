// Package config loads the harvester's run configuration from YAML.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the full run configuration.
type Config struct {
	// Mode selects the fetch path:
	//   "mock"     - offline, deterministic, fixture-backed. Default.
	//   "live"     - real HTTP requests directly to LiveEndpoint.
	//   "provider" - real HTTP requests to a third-party SERP data API
	//                (ProviderBaseURL), parsed as JSON instead of HTML.
	//   "playwright" - headless Chromium renders LiveEndpoint's results
	//                page; the rendered HTML goes through the same HTML
	//                parser as "live".
	//   "hybrid"   - "live" first; a fetch that hits a JS-check
	//                interstitial or an undismissable consent wall is
	//                retried in the browser ("playwright"). CAPTCHAs are
	//                not retried in the browser.
	Mode string `yaml:"mode"`

	Queries []string `yaml:"queries"`

	// QueueBackend selects the job source: "memory" (default) or "redis"
	// (a Redis Stream, shared across any number of harvester processes).
	QueueBackend   string `yaml:"queue_backend"`
	RedisAddr      string `yaml:"redis_addr"`
	RedisStream    string `yaml:"redis_stream"`
	RedisGroup     string `yaml:"redis_group"`
	RedisConsumer  string `yaml:"redis_consumer"`
	RedisSeedQueue bool   `yaml:"redis_seed_queue"` // push Queries onto the stream before consuming, for local demos

	Concurrency int `yaml:"concurrency"`
	MaxRetries  int `yaml:"max_retries"`

	// RatePerProxyRPS is the max sustained requests/second per proxy
	// (or per direct egress IP, if Proxies is empty).
	RatePerProxyRPS float64 `yaml:"rate_per_proxy_rps"`
	RateBurst       int     `yaml:"rate_burst"`
	// AdaptiveRate halves a proxy's rate when the target rate-limits or
	// blocks it and recovers it gradually on success (AIMD), between
	// RateMinRPS (default RatePerProxyRPS/10) and RatePerProxyRPS.
	AdaptiveRate bool    `yaml:"adaptive_rate"`
	RateMinRPS   float64 `yaml:"rate_min_rps"`

	Proxies          []string      `yaml:"proxies"`
	ProxyBanFails    int           `yaml:"proxy_ban_fails"`
	ProxyBanCooldown time.Duration `yaml:"proxy_ban_cooldown"`
	// ProxyBanCooldownMax caps escalating cooldowns: each cooldown since a
	// proxy's last success doubles ProxyBanCooldown, up to this.
	ProxyBanCooldownMax time.Duration `yaml:"proxy_ban_cooldown_max"`
	// ProxyStrategy selects proxy.Strategy: "round_robin" (default),
	// "random", or "weighted_success_rate" (weighted by recent health).
	ProxyStrategy string `yaml:"proxy_strategy"`
	// MaxProxyWait bounds how long a job waits for a proxy to leave
	// cooldown when all proxies are cooling, before that attempt fails.
	MaxProxyWait time.Duration `yaml:"max_proxy_wait"`

	LiveEndpoint   string        `yaml:"live_endpoint"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
	MockFixtureDir string        `yaml:"mock_fixture_dir"`

	// Third-party SERP provider config, used when Mode is "provider". The
	// API key is read from the environment variable named by
	// ProviderAPIKeyEnv, never from this file, so it's never committed.
	ProviderBaseURL   string `yaml:"provider_base_url"`
	ProviderAPIKeyEnv string `yaml:"provider_api_key_env"`
	ProviderEngine    string `yaml:"provider_engine"`

	// Tavily is configured independently from the SerpApi-compatible provider.
	// The key is read from the named environment variable, never YAML.
	TavilyAPIKeyEnv string `yaml:"tavily_api_key_env"`

	// Headless-browser settings, used when Mode is "playwright". That mode
	// also uses LiveEndpoint, RequestTimeout (the per-fetch budget for
	// navigation, the consent step and BrowserWaitSelector together), and
	// the same proxy pool and rate limits as every other mode.
	//
	// BrowserPoolSize caps concurrently open browser sessions (one
	// context + page each); workers beyond it wait. BrowserHeadful shows
	// a browser window, for local debugging only. BrowserExecutablePath
	// selects a specific Chromium binary instead of the Playwright-
	// installed one; note that a full Chrome/Chromium build (also what
	// BrowserHeadful runs) makes background requests to Google services
	// outside the proxy pool, which the default headless shell does not.
	// BrowserWaitSelector, if set, is awaited after page
	// load so late-rendered results are captured. BrowserMaxSessionUses
	// recycles a session (and its cookies) after that many fetches.
	BrowserPoolSize       int    `yaml:"browser_pool_size"`
	BrowserHeadful        bool   `yaml:"browser_headful"`
	BrowserExecutablePath string `yaml:"browser_executable_path"`
	BrowserWaitSelector   string `yaml:"browser_wait_selector"`
	BrowserMaxSessionUses int    `yaml:"browser_max_session_uses"`

	// SinkBackend selects the result sink: "jsonl" (default, writes to
	// OutputPath) or "postgres" (structured storage, see PostgresDSNEnv).
	SinkBackend string `yaml:"sink_backend"`
	// PostgresDSNEnv names the environment variable holding the Postgres
	// connection string, read the same way ProviderAPIKeyEnv is — never
	// written to this file.
	PostgresDSNEnv string `yaml:"postgres_dsn_env"`

	OutputPath     string        `yaml:"output_path"`
	ReportInterval time.Duration `yaml:"report_interval"`

	// MetricsAddr, if set (e.g. ":9090"), serves Prometheus metrics at
	// /metrics for the duration of the run.
	MetricsAddr string `yaml:"metrics_addr"`
}

// Default returns sane defaults; Load overlays whatever the YAML file sets
// on top of these.
func Default() Config {
	return Config{
		Mode:             "mock",
		QueueBackend:     "memory",
		RedisAddr:        "localhost:6379",
		RedisStream:      "serp-harvester:queries",
		RedisGroup:       "harvesters",
		RedisConsumer:    "worker-1",
		Concurrency:      8,
		MaxRetries:       2,
		RatePerProxyRPS:  1,
		RateBurst:        1,
		ProxyBanFails:    3,
		ProxyBanCooldown: 30 * time.Second,
		ProxyStrategy:    "round_robin",
		LiveEndpoint:     "https://www.google.com/search",
		RequestTimeout:   10 * time.Second,
		MockFixtureDir:   "internal/parser/testdata",
		ProviderEngine:   "google",
		TavilyAPIKeyEnv:  "TAVILY_API_KEY",
		SinkBackend:      "jsonl",
		PostgresDSNEnv:   "POSTGRES_DSN",
		OutputPath:       "-", // stdout
		ReportInterval:   2 * time.Second,

		BrowserPoolSize:       4,
		BrowserMaxSessionUses: 50,

		AdaptiveRate:        true,
		ProxyBanCooldownMax: 10 * time.Minute,
		MaxProxyWait:        2 * time.Minute,
	}
}

// Load reads and parses the YAML config at path, applied on top of Default().
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("config: parse %s: %w", path, err)
	}
	// HARVESTER_PROXY_URL is a deployment-time override for a single proxy.
	// It keeps authenticated proxy credentials out of committed YAML files.
	if proxyURL := strings.TrimSpace(os.Getenv("HARVESTER_PROXY_URL")); proxyURL != "" {
		cfg.Proxies = []string{proxyURL}
	}
	return cfg, nil
}
