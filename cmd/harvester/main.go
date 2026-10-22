// Command harvester runs the SERP harvesting pipeline end to end: load
// config, build the queue/proxy pool/rate limiter/fetcher/parser/sink, run
// the worker pool, and report throughput.
//
// Mock mode (default) is fully offline and deterministic — safe to run
// anywhere, including CI. Live mode issues real HTTP requests directly to
// the target and should only be run deliberately, at low volume, by someone
// who has reviewed the target site's Terms of Service. Playwright mode sends
// the same real requests through headless Chromium and carries the same
// requirement. Provider mode routes through a third-party SERP data API
// instead of the target directly. See README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/HenryMorganDibie/web-harvester/internal/config"
	"github.com/HenryMorganDibie/web-harvester/internal/fetcher"
	"github.com/HenryMorganDibie/web-harvester/internal/metrics"
	"github.com/HenryMorganDibie/web-harvester/internal/parser"
	"github.com/HenryMorganDibie/web-harvester/internal/proxy"
	"github.com/HenryMorganDibie/web-harvester/internal/queue"
	"github.com/HenryMorganDibie/web-harvester/internal/ratelimit"
	"github.com/HenryMorganDibie/web-harvester/internal/store"
	"github.com/HenryMorganDibie/web-harvester/internal/worker"
	"github.com/redis/go-redis/v9"
)

func main() {
	configPath := flag.String("config", "configs/config.example.yaml", "path to YAML config")
	mode := flag.String("mode", "", "override config mode: mock|live|provider|tavily|playwright|hybrid")
	queriesFlag := flag.String("queries", "", "comma-separated queries, overrides config")
	iAcceptLiveRisk := flag.Bool("i-have-reviewed-tos", false, "required to run --mode live, playwright or hybrid")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if *mode != "" {
		cfg.Mode = *mode
	}
	if *queriesFlag != "" {
		cfg.Queries = strings.Split(*queriesFlag, ",")
	}
	if len(cfg.Queries) == 0 && cfg.QueueBackend != "redis" {
		log.Fatal("no queries configured (set queries: in config or pass -queries)")
	}

	counters := &metrics.Counters{}
	if cfg.Mode == "playwright" || cfg.Mode == "hybrid" {
		counters.Browser = &metrics.BrowserCounters{}
	}
	latencies := metrics.NewLatencyRecorder(10000)

	f, p := buildFetcherAndParser(cfg, iAcceptLiveRisk, counters)
	// The Playwright fetcher owns a browser process; close it once the
	// pool has drained, including after SIGINT/SIGTERM.
	if c, ok := f.(io.Closer); ok {
		defer func() {
			if err := c.Close(); err != nil {
				log.Printf("close fetcher: %v", err)
			}
		}()
	}
	sink := buildSink(cfg)

	proxyPool := proxy.NewPool(cfg.Proxies, cfg.ProxyBanFails, cfg.ProxyBanCooldown)
	proxyPool.Strategy = parseProxyStrategy(cfg.ProxyStrategy)
	proxyPool.MaxCooldown = cfg.ProxyBanCooldownMax
	limiter := ratelimit.New(cfg.RatePerProxyRPS, cfg.RateBurst)
	if cfg.AdaptiveRate {
		limiter = ratelimit.NewAdaptive(cfg.RatePerProxyRPS, cfg.RateBurst, cfg.RateMinRPS)
	}
	counters.ProxyGauges = func() metrics.ProxyGauges {
		available, cooling := proxyPool.Counts()
		return metrics.ProxyGauges{Available: available, Cooling: cooling, Throttled: limiter.Throttled()}
	}

	pool := &worker.Pool{
		Concurrency: cfg.Concurrency,
		MaxRetries:  cfg.MaxRetries,
		Fetcher:     f,
		ProxyPool:   proxyPool,
		Limiter:     limiter,
		Parser:      p,
		Sink:        sink,
		Metrics:     counters,
		Latencies:   latencies,

		MaxProxyWait: cfg.MaxProxyWait,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.MetricsAddr != "" {
		go func() {
			log.Printf("serving Prometheus metrics on %s/metrics (health at /healthz)", cfg.MetricsAddr)
			if err := metrics.StartServer(ctx, cfg.MetricsAddr, counters, latencies); err != nil {
				log.Printf("metrics server stopped: %v", err)
			}
		}()
	}

	reportCtx, cancelReport := context.WithCancel(ctx)
	reportDone := counters.StartReporter(reportCtx, cfg.ReportInterval)

	jobs := buildJobSource(ctx, cfg)

	log.Printf(
		"starting harvest: mode=%s queue_backend=%s concurrency=%d proxies=%d rate/proxy=%.2f req/s",
		cfg.Mode, cfg.QueueBackend, cfg.Concurrency, proxyPool.Size(), cfg.RatePerProxyRPS,
	)

	pool.Run(ctx, jobs)

	cancelReport()
	<-reportDone

	log.Println("harvest complete")
}

// buildFetcherAndParser picks the fetcher implementation and its matching
// parser together, since a fetcher's output format (HTML vs. JSON)
// determines which parser can read it.
func buildFetcherAndParser(cfg config.Config, iAcceptLiveRisk *bool, counters *metrics.Counters) (fetcher.Fetcher, worker.Parser) {
	switch cfg.Mode {
	case "mock":
		f, err := fetcher.NewMockFromDir(cfg.MockFixtureDir)
		if err != nil {
			log.Fatalf("build mock fetcher: %v", err)
		}
		return f, parser.New()

	case "live":
		if !*iAcceptLiveRisk {
			fmt.Fprintln(os.Stderr, liveModeWarning)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, liveModeBanner(cfg))
		return newHTTPFetcher(cfg, counters), parser.New()

	case "provider":
		if cfg.ProviderBaseURL == "" {
			log.Fatal("provider mode requires provider_base_url in config")
		}
		apiKey := os.Getenv(cfg.ProviderAPIKeyEnv)
		if apiKey == "" {
			log.Fatalf("provider mode requires the %s environment variable to be set", cfg.ProviderAPIKeyEnv)
		}
		f := fetcher.NewProviderFetcher(cfg.ProviderBaseURL, apiKey, cfg.ProviderEngine, cfg.RequestTimeout)
		return f, parser.NewJSON()

	case "tavily":
		apiKey := os.Getenv(cfg.TavilyAPIKeyEnv)
		if apiKey == "" {
			log.Fatalf("tavily mode requires the %s environment variable to be set", cfg.TavilyAPIKeyEnv)
		}
		return fetcher.NewTavilyFetcher(apiKey, cfg.RequestTimeout), parser.NewTavily()

	case "playwright", "hybrid":
		if !*iAcceptLiveRisk {
			fmt.Fprintln(os.Stderr, liveModeWarning)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, liveModeBanner(cfg))
		browser, err := fetcher.NewPlaywrightFetcher(fetcher.PlaywrightConfig{
			Endpoint:       cfg.LiveEndpoint,
			Timeout:        cfg.RequestTimeout,
			PoolSize:       cfg.BrowserPoolSize,
			MaxSessionUses: cfg.BrowserMaxSessionUses,
			Headless:       !cfg.BrowserHeadful,
			ExecutablePath: cfg.BrowserExecutablePath,
			WaitSelector:   cfg.BrowserWaitSelector,
			Metrics:        counters.Browser,
		})
		if err != nil {
			log.Fatalf("build playwright fetcher: %v (install the driver and Chromium with `make playwright-install` / scripts/install-playwright.sh)", err)
		}
		if cfg.Mode == "playwright" {
			return browser, parser.New()
		}
		return &fetcher.FailoverFetcher{
			Primary:    newHTTPFetcher(cfg, counters),
			Secondary:  browser,
			OnFailover: func(fetcher.Outcome) { counters.IncFailover() },
		}, parser.New()

	default:
		log.Fatalf("unknown mode %q (want mock|live|provider|tavily|playwright|hybrid)", cfg.Mode)
		return nil, nil // unreachable
	}
}

// newHTTPFetcher builds the plain HTTP fetcher used by live and hybrid mode.
func newHTTPFetcher(cfg config.Config, counters *metrics.Counters) *fetcher.HTTPFetcher {
	f, err := fetcher.NewHTTPFetcher(cfg.LiveEndpoint, cfg.RequestTimeout)
	if err != nil {
		log.Fatalf("build http fetcher: %v", err)
	}
	f.OnConsentHandled = counters.IncHTTPConsentHandled
	return f
}

// buildSink picks the result sink: JSON-Lines (to OutputPath, default
// stdout) or PostgreSQL. The DSN is read from the environment, never from
// config, the same pattern as the provider API key.
func buildSink(cfg config.Config) store.Sink {
	switch cfg.SinkBackend {
	case "", "jsonl":
		if cfg.OutputPath == "-" || cfg.OutputPath == "" {
			return store.NewJSONLSink(os.Stdout)
		}
		out, err := os.Create(cfg.OutputPath)
		if err != nil {
			log.Fatalf("create output file: %v", err)
		}
		return store.NewJSONLSink(out)

	case "postgres":
		dsn := os.Getenv(cfg.PostgresDSNEnv)
		if dsn == "" {
			log.Fatalf("sink_backend: postgres requires the %s environment variable to be set", cfg.PostgresDSNEnv)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sink, err := store.NewPostgresSink(ctx, dsn)
		if err != nil {
			log.Fatalf("build postgres sink: %v", err)
		}
		return sink

	default:
		log.Fatalf("unknown sink_backend %q (want jsonl|postgres)", cfg.SinkBackend)
		return nil // unreachable
	}
}

// buildJobSource picks the queue backend. Redis mode optionally seeds the
// stream from cfg.Queries first, purely so `queue_backend: redis` is
// runnable as a local demo without a separate producer process.
func buildJobSource(ctx context.Context, cfg config.Config) <-chan queue.Job {
	switch cfg.QueueBackend {
	case "", "memory":
		return (&queue.MemorySource{Queries: cfg.Queries, Buffer: cfg.Concurrency * 2}).Jobs(ctx)

	case "redis":
		client := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
		src := &queue.RedisStreamSource{
			Client:   client,
			Stream:   cfg.RedisStream,
			Group:    cfg.RedisGroup,
			Consumer: cfg.RedisConsumer,
		}
		if err := src.EnsureGroup(ctx); err != nil {
			log.Fatalf("redis queue: %v", err)
		}
		if cfg.RedisSeedQueue {
			for _, q := range cfg.Queries {
				if err := src.PushQuery(ctx, q); err != nil {
					log.Fatalf("redis queue: seed query %q: %v", q, err)
				}
			}
			log.Printf("seeded %d queries onto redis stream %q", len(cfg.Queries), cfg.RedisStream)
		}
		return src.Jobs(ctx)

	default:
		log.Fatalf("unknown queue_backend %q (want memory|redis)", cfg.QueueBackend)
		return nil // unreachable
	}
}

func parseProxyStrategy(s string) proxy.Strategy {
	switch s {
	case "", "round_robin":
		return proxy.RoundRobin
	case "random":
		return proxy.Random
	case "weighted_success_rate":
		return proxy.WeightedSuccessRate
	default:
		log.Fatalf("unknown proxy_strategy %q (want round_robin|random|weighted_success_rate)", s)
		return proxy.RoundRobin // unreachable
	}
}

const liveModeWarning = `refusing to run --mode live, playwright or hybrid without -i-have-reviewed-tos

These modes send real requests to the configured endpoint (default: Google
Search), playwright and hybrid through a headless browser. Before running
any of them you should have reviewed:
  - The target site's Terms of Service and robots.txt
  - Applicable law in your and the target's jurisdiction
  - Your own risk tolerance for IP blocks / CAPTCHAs at the configured rate

Re-run with -i-have-reviewed-tos once you have done so.`

func liveModeBanner(cfg config.Config) string {
	return fmt.Sprintf(
		"[%s mode] endpoint=%s concurrency=%d rate/proxy=%.2f req/s — this WILL make real network requests",
		cfg.Mode, cfg.LiveEndpoint, cfg.Concurrency, cfg.RatePerProxyRPS,
	)
}
