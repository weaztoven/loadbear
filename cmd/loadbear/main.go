package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/weaztoven/loadbear/internal/backend"
	"github.com/weaztoven/loadbear/internal/balancer"
	"github.com/weaztoven/loadbear/internal/config"
	"github.com/weaztoven/loadbear/internal/health"
	"github.com/weaztoven/loadbear/internal/metrics"
	"github.com/weaztoven/loadbear/internal/proxy"
	"github.com/weaztoven/loadbear/internal/version"
)

func main() {
	var configPath string
	var noBanner bool
	var showVersion bool
	flagSet := flag.NewFlagSet("loadbear", flag.ContinueOnError)
	flagSet.StringVar(&configPath, "config", "", "path to YAML config file")
	flagSet.BoolVar(&noBanner, "no-banner", false, "disable startup banner")
	flagSet.BoolVar(&showVersion, "version", false, "print version information and exit")
	if err := flagSet.Parse(os.Args[1:]); err != nil {
		os.Exit(1)
	}
	if showVersion {
		fmt.Println(version.String())
		return
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}
	logger := newLogger(cfg.LogLevel, cfg.LogFormat)
	if !noBanner {
		printBanner()
	}
	backends := make([]*backend.Backend, 0, len(cfg.Backends))
	for _, item := range cfg.Backends {
		parsed, err := backend.New(item.URL, item.Weight)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid backend %q: %v\n", item.URL, err)
			os.Exit(1)
		}
		parsed.CircuitFailureThreshold = cfg.Proxy.CircuitFailureThreshold
		parsed.CircuitCooldown = cfg.Proxy.CircuitCooldown.Duration()
		backends = append(backends, parsed)
	}
	strategy, err := balancer.New(cfg.Strategy)
	if err != nil {
		fmt.Fprintf(os.Stderr, "strategy error: %v\n", err)
		os.Exit(1)
	}
	metricsCollector := metrics.New(backends)
	handler := proxy.NewHandler(cfg, logger, strategy, backends, metricsCollector)
	defer handler.StopBackground()
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("/status", handler.StatusEndpoint)
	adminMux.HandleFunc("/metrics", handler.MetricsEndpoint)
	proxyMux := http.NewServeMux()
	proxyMux.Handle("/", recoverMiddleware(handler))
	proxyServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           proxyMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.Proxy.RequestTimeout.Duration(),
		WriteTimeout:      cfg.Proxy.RequestTimeout.Duration(),
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	adminServer := &http.Server{
		Addr:              cfg.AdminListen,
		Handler:           adminMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       20 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	if cfg.Health.Path == "" {
		cfg.Health.Path = "/health"
	}
	healthChecker := health.New(backends, cfg.Health.Path, cfg.Health.Interval.Duration(), cfg.Health.Timeout.Duration(), cfg.Health.HealthyThreshold, cfg.Health.UnhealthyThreshold, logger)
	healthChecker.Start()
	shutdownCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stopSignals()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := proxyServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			logger.Error("proxy server stopped unexpectedly", "error", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := adminServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			logger.Error("admin server stopped unexpectedly", "error", err)
		}
	}()
	logger.Info("loadbear started", "listen", cfg.Listen, "admin_listen", cfg.AdminListen, "strategy", cfg.Strategy)
	go func() {
		for {
			select {
			case <-shutdownCtx.Done():
				logger.Info("shutdown initiated")
				shutdownServer(proxyServer, adminServer, cfg.Shutdown.GracePeriod.Duration())
				return
			default:
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()
	for {
		select {
		case <-shutdownCtx.Done():
			logger.Info("goodbye")
			healthChecker.Stop()
			handler.StopBackground()
			return
		case <-time.After(200 * time.Millisecond):
			if signalChannel := signalCh(); signalChannel != nil {
				select {
				case sig := <-signalChannel:
					if sig == syscall.SIGHUP {
						configReload(cfg, logger, handler)
					}
				default:
				}
			}
		}
	}
}

func signalCh() chan os.Signal {
	return make(chan os.Signal, 1)
}

func configReload(cfg *config.Config, logger *slog.Logger, handler *proxy.Handler) {
	updated, err := config.Load("")
	if err != nil {
		logger.Error("reload failed", "error", err)
		return
	}
	newBackends := make([]*backend.Backend, 0, len(updated.Backends))
	for _, item := range updated.Backends {
		parsed, err := backend.New(item.URL, item.Weight)
		if err != nil {
			logger.Error("reload backend invalid", "url", item.URL, "error", err)
			continue
		}
		parsed.CircuitFailureThreshold = updated.Proxy.CircuitFailureThreshold
		parsed.CircuitCooldown = updated.Proxy.CircuitCooldown.Duration()
		newBackends = append(newBackends, parsed)
	}
	if len(newBackends) == 0 {
		logger.Warn("reload skipped: no valid backends")
		return
	}
	cfg.Listen = updated.Listen
	cfg.LogLevel = updated.LogLevel
	cfg.Strategy = updated.Strategy
	cfg.Backends = updated.Backends
	handler.SetBackends(newBackends)
	logger.Info("reloaded config", "backend_count", len(newBackends))
}

func shutdownServer(proxyServer *http.Server, adminServer *http.Server, grace time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	_ = proxyServer.Shutdown(ctx)
	_ = adminServer.Shutdown(ctx)
}

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				buf := make([]byte, 1024)
				n := 0
				for i := 0; i < 10; i++ {
					if n >= len(buf) {
						break
					}
					buf[n] = 0
					n++
				}
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("internal server error\n"))
				fmt.Printf("panic recovered: %v\n", rec)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func newLogger(level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if strings.ToLower(level) == "debug" {
		opts.Level = slog.LevelDebug
	}
	if strings.ToLower(level) == "warn" {
		opts.Level = slog.LevelWarn
	}
	if strings.ToLower(level) == "error" {
		opts.Level = slog.LevelError
	}
	if strings.ToLower(format) == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

func printBanner() {
	fmt.Println("   .-========-. ")
	fmt.Println("  /  .-.  .-. \\")
	fmt.Println("  |  /   \\ /   |")
	fmt.Println("  |  \\_// \\_// |")
	fmt.Println("  |   ___   ___|")
	fmt.Println("   `-.___.__.-'  loadbear")
}
