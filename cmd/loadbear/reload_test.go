package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/weaztoven/loadbear/internal/backend"
	"github.com/weaztoven/loadbear/internal/balancer"
	"github.com/weaztoven/loadbear/internal/config"
	"github.com/weaztoven/loadbear/internal/health"
	"github.com/weaztoven/loadbear/internal/metrics"
	"github.com/weaztoven/loadbear/internal/proxy"
)

func reloadTestConfig(strategy string, servers ...*httptest.Server) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Strategy = strategy
	cfg.RateLimit.Enabled = false
	cfg.Backends = make([]config.BackendConfig, 0, len(servers))
	for _, server := range servers {
		cfg.Backends = append(cfg.Backends, config.BackendConfig{URL: server.URL, Weight: 1})
	}
	return &cfg
}

func newReloadTestManager(t *testing.T, cfg *config.Config, logger *slog.Logger) (*reloadManager, *proxy.Handler, *health.Checker) {
	t.Helper()
	backends := make([]*backend.Backend, 0, len(cfg.Backends))
	for _, item := range cfg.Backends {
		b, err := backend.New(item.URL, item.Weight)
		if err != nil {
			t.Fatal(err)
		}
		b.CircuitFailureThreshold = cfg.Proxy.CircuitFailureThreshold
		b.CircuitCooldown = cfg.Proxy.CircuitCooldown.Duration()
		backends = append(backends, b)
	}
	strategy, err := balancer.New(cfg.Strategy)
	if err != nil {
		t.Fatal(err)
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	collector := metrics.New(backends)
	handler := proxy.NewHandler(cfg, logger, strategy, backends, collector)
	checker := health.New(backends, cfg.Health.Path, cfg.Health.Interval.Duration(), cfg.Health.Timeout.Duration(),
		cfg.Health.HealthyThreshold, cfg.Health.UnhealthyThreshold, logger)
	t.Cleanup(func() {
		checker.Stop()
		handler.StopBackground()
	})
	return newReloadManager("", cfg, handler, checker, logger), handler, checker
}

func TestReloadAddsRemovesAndPreservesUnchangedBackendState(t *testing.T) {
	servers := make([]*httptest.Server, 4)
	for i := range servers {
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, "ok")
		}))
		defer servers[i].Close()
	}
	initial := reloadTestConfig("round_robin", servers[0], servers[1])
	manager, handler, _ := newReloadTestManager(t, initial, nil)
	original := handler.CurrentBackends()[0]
	original.SetHealthy(false)
	original.TotalRequests.Add(7)
	original.CircuitFailureThreshold = 1
	original.RecordFailure()
	original.CurrentWeight.Store(12)

	updated := reloadTestConfig("smooth_weighted_round_robin", servers[0], servers[2])
	updated.Backends[0].Weight = 3
	if err := manager.Reload(updated); err != nil {
		t.Fatal(err)
	}
	current := handler.CurrentBackends()
	if len(current) != 2 || current[0] != original {
		t.Fatalf("reload did not preserve the unchanged backend: %#v", current)
	}
	if original.IsHealthy() || original.TotalRequests.Load() != 7 ||
		original.GetCircuitState() != backend.CircuitOpen || original.GetWeight() != 3 {
		t.Fatalf("unchanged backend state was not preserved or weight updated: healthy=%v requests=%d circuit=%s weight=%d",
			original.IsHealthy(), original.TotalRequests.Load(), original.GetCircuitState(), original.GetWeight())
	}
	if original.CurrentWeight.Load() != 0 {
		t.Fatalf("weighted balancer state = %d, want reset to 0", original.CurrentWeight.Load())
	}
	if handler.StrategyName() != "smooth_weighted_round_robin" {
		t.Fatalf("strategy after reload = %q", handler.StrategyName())
	}
	metricText := handler.MetricsText()
	if len(handler.Status()) != 2 ||
		!strings.Contains(metricText, `name="`+strings.TrimPrefix(servers[2].URL, "http://")+`"`) ||
		strings.Contains(metricText, `name="`+strings.TrimPrefix(servers[1].URL, "http://")+`"`) {
		t.Fatal("status and metrics did not reflect the reloaded backend set")
	}

	replacement := reloadTestConfig("least_connections", servers[2], servers[3])
	if err := manager.Reload(replacement); err != nil {
		t.Fatal(err)
	}
	current = handler.CurrentBackends()
	if len(current) != 2 || current[0] == original || current[0].URL.String() != servers[2].URL ||
		current[1].URL.String() != servers[3].URL {
		t.Fatalf("whole-list replacement produced unexpected backend set: %#v", current)
	}
}

func TestInvalidReloadKeepsOldBackendListServing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "old backend")
	}))
	defer server.Close()
	initial := reloadTestConfig("round_robin", server)
	manager, handler, _ := newReloadTestManager(t, initial, nil)
	original := handler.CurrentBackends()[0]
	invalidConfigs := map[string]*config.Config{
		"duplicate URLs": reloadTestConfig("round_robin", server, server),
		"zero backends":  reloadTestConfig("round_robin"),
		"bad weight": func() *config.Config {
			cfg := reloadTestConfig("round_robin", server)
			cfg.Backends[0].Weight = 0
			return cfg
		}(),
	}
	for name, invalid := range invalidConfigs {
		if err := manager.Reload(invalid); err == nil {
			t.Errorf("%s reload unexpectedly succeeded", name)
		}
		if current := handler.CurrentBackends(); len(current) != 1 || current[0] != original {
			t.Fatalf("%s reload changed the active backend list: %#v", name, current)
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "old backend" {
		t.Fatalf("old backend did not continue serving: status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestRemovedBackendFinishesInflightRequest(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	removed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		_, _ = fmt.Fprint(w, "finished")
	}))
	defer removed.Close()
	stable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "stable")
	}))
	defer stable.Close()
	initial := reloadTestConfig("round_robin", removed)
	manager, handler, _ := newReloadTestManager(t, initial, nil)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil))
		result <- recorder
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("in-flight request did not reach removed backend")
	}
	if err := manager.Reload(reloadTestConfig("round_robin", stable)); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case recorder := <-result:
		if recorder.Code != http.StatusOK || recorder.Body.String() != "finished" {
			t.Fatalf("in-flight request result = %d %q", recorder.Code, recorder.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight request did not finish")
	}
}

func TestConcurrentRequestsDuringRepeatedReloads(t *testing.T) {
	servers := make([]*httptest.Server, 3)
	var hits [3]atomic.Int32
	for i := range servers {
		index := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits[index].Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer servers[i].Close()
	}
	initial := reloadTestConfig("round_robin", servers[0], servers[1])
	manager, handler, _ := newReloadTestManager(t, initial, nil)
	var wg sync.WaitGroup
	errs := make(chan int, 100)
	for i := 0; i < cap(errs); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil))
			if recorder.Code != http.StatusOK {
				errs <- recorder.Code
			}
		}()
	}
	for i := 0; i < 30; i++ {
		strategy := []string{"round_robin", "smooth_weighted_round_robin", "least_connections", "ip_hash"}[i%4]
		serversInSet := []*httptest.Server{servers[0], servers[1+i%2]}
		if err := manager.Reload(reloadTestConfig(strategy, serversInSet...)); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	close(errs)
	for code := range errs {
		t.Errorf("concurrent request returned status %d", code)
	}
	if hits[0].Load() == 0 {
		t.Fatal("stable backend was never selected")
	}
}

func TestHealthCheckerTracksReloadedBackendsAndStops(t *testing.T) {
	var hits [3]atomic.Int32
	servers := make([]*httptest.Server, 3)
	for i := range servers {
		index := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits[index].Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer servers[i].Close()
	}
	initial := reloadTestConfig("round_robin", servers[0], servers[1])
	initial.Health.Interval = config.YAMLDuration(2 * time.Millisecond)
	manager, _, checker := newReloadTestManager(t, initial, nil)
	checker.Start()
	waitReloadCondition(t, time.Second, func() bool {
		return hits[0].Load() > 0 && hits[1].Load() > 0
	})
	if err := manager.Reload(reloadTestConfig("round_robin", servers[0], servers[2])); err != nil {
		t.Fatal(err)
	}
	waitReloadCondition(t, time.Second, func() bool { return hits[2].Load() > 0 })
	time.Sleep(20 * time.Millisecond)
	removedHits := hits[1].Load()
	time.Sleep(20 * time.Millisecond)
	if got := hits[1].Load(); got != removedHits {
		t.Fatalf("health checker continued probing removed backend: %d -> %d", removedHits, got)
	}
	checker.Stop()
}

func waitReloadCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	if !condition() {
		t.Fatal("condition was not met before timeout")
	}
}
