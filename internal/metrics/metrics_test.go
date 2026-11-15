package metrics_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weaztoven/loadbear/internal/backend"
	"github.com/weaztoven/loadbear/internal/balancer"
	"github.com/weaztoven/loadbear/internal/config"
	"github.com/weaztoven/loadbear/internal/metrics"
	"github.com/weaztoven/loadbear/internal/proxy"
)

func TestMetricsSnapshotAndPrometheusOutput(t *testing.T) {
	b, err := backend.New("http://127.0.0.1:8081", 1)
	if err != nil {
		t.Fatal(err)
	}
	b.IncrementActive()
	collector := metrics.New([]*backend.Backend{b, nil})
	collector.RecordRequest(b, http.StatusOK, 8*time.Millisecond)
	collector.RecordRequest(b, http.StatusInternalServerError, 20*time.Second)
	collector.RecordRequest(nil, http.StatusServiceUnavailable, 5*time.Millisecond)

	snapshot := collector.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("snapshot length = %d; want 1", len(snapshot))
	}
	item := snapshot[0]
	if item.URL != b.URL.String() || item.Name != b.Name || !item.Healthy ||
		item.ActiveConnections != 1 || item.TotalRequests != 2 ||
		item.TotalFailures != 1 || item.CircuitState != string(backend.CircuitClosed) {
		t.Fatalf("unexpected snapshot: %+v", item)
	}
	text := collector.PrometheusText()
	for _, metric := range []string{
		"loadbear_requests_total 3",
		"loadbear_failures_total 2",
		"loadbear_request_latency_seconds_bucket{le=\"0.01\"} 2",
		"loadbear_request_latency_seconds_bucket{le=\"+Inf\"} 3",
		"loadbear_request_latency_seconds_sum 20.013",
		"loadbear_request_latency_seconds_count 3",
		"loadbear_backend_up{backend=\"http://127.0.0.1:8081\"} 1",
		"loadbear_backend_active_connections{backend=\"http://127.0.0.1:8081\"} 1",
	} {
		if !strings.Contains(text, metric) {
			t.Errorf("Prometheus output missing %q:\n%s", metric, text)
		}
	}
}

func TestPrometheusHistogramIsCumulativeAndIncludesSumAndCount(t *testing.T) {
	b, err := backend.New("http://127.0.0.1:8081", 1)
	if err != nil {
		t.Fatal(err)
	}
	b.IncrementActive()
	collector := metrics.New([]*backend.Backend{b})
	collector.RecordRequest(b, http.StatusOK, 8*time.Millisecond)
	collector.RecordRequest(b, http.StatusInternalServerError, 20*time.Second)
	collector.RecordRequest(nil, http.StatusServiceUnavailable, 5*time.Millisecond)
	collector.RecordTunnelOpened(b)
	collector.RecordTunnelClosed(b, "upstream_error", 75*time.Second)

	want := `# HELP loadbear_requests_total Total number of proxied requests.
# TYPE loadbear_requests_total counter
loadbear_requests_total 3
# HELP loadbear_failures_total Total number of failing requests.
# TYPE loadbear_failures_total counter
loadbear_failures_total 2
# HELP loadbear_rate_limited_total Total number of rate-limited requests.
# TYPE loadbear_rate_limited_total counter
loadbear_rate_limited_total 0
# HELP loadbear_request_latency_seconds Request latency in seconds.
# TYPE loadbear_request_latency_seconds histogram
loadbear_request_latency_seconds_bucket{le="0.001"} 0
loadbear_request_latency_seconds_bucket{le="0.0025"} 0
loadbear_request_latency_seconds_bucket{le="0.005"} 1
loadbear_request_latency_seconds_bucket{le="0.01"} 2
loadbear_request_latency_seconds_bucket{le="0.025"} 2
loadbear_request_latency_seconds_bucket{le="0.05"} 2
loadbear_request_latency_seconds_bucket{le="0.1"} 2
loadbear_request_latency_seconds_bucket{le="0.25"} 2
loadbear_request_latency_seconds_bucket{le="0.5"} 2
loadbear_request_latency_seconds_bucket{le="1"} 2
loadbear_request_latency_seconds_bucket{le="2.5"} 2
loadbear_request_latency_seconds_bucket{le="5"} 2
loadbear_request_latency_seconds_bucket{le="10"} 2
loadbear_request_latency_seconds_bucket{le="+Inf"} 3
loadbear_request_latency_seconds_sum 20.013
loadbear_request_latency_seconds_count 3
# HELP loadbear_backend_up Backend health gauge.
# TYPE loadbear_backend_up gauge
loadbear_backend_up{backend="http://127.0.0.1:8081"} 1
# HELP loadbear_backend_requests_total Total requests sent to each backend.
# TYPE loadbear_backend_requests_total counter
loadbear_backend_requests_total{backend="http://127.0.0.1:8081"} 2
# HELP loadbear_backend_failures_total Total failed requests for each backend.
# TYPE loadbear_backend_failures_total counter
loadbear_backend_failures_total{backend="http://127.0.0.1:8081"} 1
# HELP loadbear_backend_active_connections Active backend connections.
# TYPE loadbear_backend_active_connections gauge
loadbear_backend_active_connections{backend="http://127.0.0.1:8081"} 1
# HELP loadbear_circuit_state Backend circuit state (0 closed, 1 open, 2 half-open).
# TYPE loadbear_circuit_state gauge
loadbear_circuit_state{backend="http://127.0.0.1:8081"} 0
# HELP loadbear_active_tunnels Total number of active upgraded tunnels.
# TYPE loadbear_active_tunnels gauge
loadbear_active_tunnels 0
# HELP loadbear_backend_active_tunnels Active upgraded tunnels per backend.
# TYPE loadbear_backend_active_tunnels gauge
loadbear_backend_active_tunnels{backend="http://127.0.0.1:8081"} 0
# HELP loadbear_tunnel_duration_seconds Duration of completed upgraded tunnels in seconds.
# TYPE loadbear_tunnel_duration_seconds histogram
loadbear_tunnel_duration_seconds_bucket{le="1"} 0
loadbear_tunnel_duration_seconds_bucket{le="5"} 0
loadbear_tunnel_duration_seconds_bucket{le="15"} 0
loadbear_tunnel_duration_seconds_bucket{le="60"} 0
loadbear_tunnel_duration_seconds_bucket{le="300"} 1
loadbear_tunnel_duration_seconds_bucket{le="900"} 1
loadbear_tunnel_duration_seconds_bucket{le="3600"} 1
loadbear_tunnel_duration_seconds_bucket{le="+Inf"} 1
loadbear_tunnel_duration_seconds_sum 75
loadbear_tunnel_duration_seconds_count 1
# HELP loadbear_tunnel_closed_total Total number of closed upgraded tunnels by backend and reason.
# TYPE loadbear_tunnel_closed_total counter
loadbear_tunnel_closed_total{backend="http://127.0.0.1:8081",reason="client_closed"} 0
loadbear_tunnel_closed_total{backend="http://127.0.0.1:8081",reason="upstream_closed"} 0
loadbear_tunnel_closed_total{backend="http://127.0.0.1:8081",reason="upstream_error"} 1
loadbear_tunnel_closed_total{backend="http://127.0.0.1:8081",reason="shutdown_forced"} 0
loadbear_tunnel_closed_total{backend="http://127.0.0.1:8081",reason="unknown"} 0
`
	if got := collector.PrometheusText(); got != want {
		t.Fatalf("Prometheus output mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	t.Log(want)
}

func TestPrometheusLabelsUseFullBackendURL(t *testing.T) {
	b, err := backend.New("http://127.0.0.1:8081", 1)
	if err != nil {
		t.Fatal(err)
	}
	b.URL.Path = "/api"
	text := metrics.New([]*backend.Backend{b}).PrometheusText()
	if !strings.Contains(text, `loadbear_backend_up{backend="http://127.0.0.1:8081/api"} 1`) {
		t.Fatalf("backend label does not identify the full URL:\n%s", text)
	}
}

func TestPrometheusOutputTracksBackendReloads(t *testing.T) {
	removed, err := backend.New("http://127.0.0.1:8081", 1)
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := backend.New("http://127.0.0.1:8082", 1)
	if err != nil {
		t.Fatal(err)
	}
	collector := metrics.New([]*backend.Backend{remaining, removed})
	collector.RecordTunnelOpened(removed)
	before := collector.PrometheusText()
	if !strings.Contains(before, `loadbear_backend_requests_total{backend="http://127.0.0.1:8081"}`) {
		t.Fatalf("initial output missing backend request series:\n%s", before)
	}
	if strings.Index(before, "backend=\"http://127.0.0.1:8081\"") >
		strings.Index(before, "backend=\"http://127.0.0.1:8082\"") {
		t.Fatalf("backend metrics are not sorted:\n%s", before)
	}
	collector.SetBackends([]*backend.Backend{remaining})
	after := collector.PrometheusText()
	if strings.Contains(after, "127.0.0.1:8081") ||
		!strings.Contains(after, `loadbear_backend_requests_total{backend="http://127.0.0.1:8082"}`) ||
		strings.Contains(after, "loadbear_tunnel_closed_total{backend=\"http://127.0.0.1:8081\"") {
		t.Fatalf("output did not reflect backend reload:\n%s", after)
	}
	collector.RecordTunnelClosed(removed, "upstream_closed", time.Second)
	afterClose := collector.PrometheusText()
	if strings.Contains(afterClose, "127.0.0.1:8081") ||
		!strings.Contains(afterClose, "loadbear_active_tunnels 0") {
		t.Fatalf("removed tunnel reappeared after cleanup:\n%s", afterClose)
	}
}

func TestPrometheusCircuitStateValues(t *testing.T) {
	closed, err := backend.New("http://127.0.0.1:8081", 1)
	if err != nil {
		t.Fatal(err)
	}
	open, err := backend.New("http://127.0.0.1:8082", 1)
	if err != nil {
		t.Fatal(err)
	}
	open.CircuitFailureThreshold = 1
	open.RecordFailure()
	halfOpen, err := backend.New("http://127.0.0.1:8083", 1)
	if err != nil {
		t.Fatal(err)
	}
	halfOpen.CircuitState = backend.CircuitOpen
	halfOpen.CircuitOpenedAt = time.Now().Add(-time.Hour)
	halfOpen.CircuitCooldown = time.Minute
	if allowed, probe := halfOpen.AllowRequestWithProbe(); !allowed || !probe {
		t.Fatal("expected expired open circuit to admit its half-open probe")
	}

	text := metrics.New([]*backend.Backend{closed, open, halfOpen}).PrometheusText()
	for _, line := range []string{
		`loadbear_circuit_state{backend="http://127.0.0.1:8081"} 0`,
		`loadbear_circuit_state{backend="http://127.0.0.1:8082"} 1`,
		`loadbear_circuit_state{backend="http://127.0.0.1:8083"} 2`,
	} {
		if !strings.Contains(text, line) {
			t.Errorf("Prometheus output missing %q:\n%s", line, text)
		}
	}
}

func TestPrometheusTunnelMetricsAndReasons(t *testing.T) {
	b, err := backend.New("http://127.0.0.1:8081", 1)
	if err != nil {
		t.Fatal(err)
	}
	collector := metrics.New([]*backend.Backend{b})
	reasons := []string{"client_closed", "upstream_closed", "upstream_error", "shutdown_forced", "unknown"}
	durations := []time.Duration{time.Second, 5 * time.Second, 15 * time.Second, time.Minute, time.Hour}
	for i, reason := range reasons {
		collector.RecordTunnelOpened(b)
		collector.RecordTunnelClosed(b, reason, durations[i])
	}
	text := collector.PrometheusText()
	for _, line := range []string{
		"loadbear_active_tunnels 0",
		`loadbear_backend_active_tunnels{backend="http://127.0.0.1:8081"} 0`,
		`loadbear_tunnel_duration_seconds_bucket{le="1"} 1`,
		`loadbear_tunnel_duration_seconds_bucket{le="5"} 2`,
		`loadbear_tunnel_duration_seconds_bucket{le="15"} 3`,
		`loadbear_tunnel_duration_seconds_bucket{le="60"} 4`,
		`loadbear_tunnel_duration_seconds_bucket{le="3600"} 5`,
		"loadbear_tunnel_duration_seconds_bucket{le=\"+Inf\"} 5",
		"loadbear_tunnel_duration_seconds_sum 3681",
		"loadbear_tunnel_duration_seconds_count 5",
		`loadbear_tunnel_closed_total{backend="http://127.0.0.1:8081",reason="client_closed"} 1`,
		`loadbear_tunnel_closed_total{backend="http://127.0.0.1:8081",reason="upstream_closed"} 1`,
		`loadbear_tunnel_closed_total{backend="http://127.0.0.1:8081",reason="upstream_error"} 1`,
		`loadbear_tunnel_closed_total{backend="http://127.0.0.1:8081",reason="shutdown_forced"} 1`,
		`loadbear_tunnel_closed_total{backend="http://127.0.0.1:8081",reason="unknown"} 1`,
	} {
		if !strings.Contains(text, line) {
			t.Errorf("Prometheus output missing %q:\n%s", line, text)
		}
	}
	t.Log(text)
}

func TestConcurrentPrometheusUpdatesAndBackendSwaps(t *testing.T) {
	first, err := backend.New("http://127.0.0.1:8081", 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := backend.New("http://127.0.0.1:8082", 1)
	if err != nil {
		t.Fatal(err)
	}
	collector := metrics.New([]*backend.Backend{first, second})
	const workers = 8
	const updates = 100
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for i := 0; i < updates; i++ {
				target := first
				if (worker+i)%2 == 0 {
					target = second
				}
				collector.RecordRequest(target, http.StatusOK, time.Millisecond)
				collector.RecordTunnelOpened(target)
				collector.RecordTunnelClosed(target, "client_closed", time.Millisecond)
				if i%10 == 0 {
					collector.RecordRateLimited()
				}
				if i%5 == 0 {
					collector.SetBackends([]*backend.Backend{first, second})
				}
			}
		}(worker)
	}
	group.Wait()
	text := collector.PrometheusText()
	if !strings.Contains(text, fmt.Sprintf("loadbear_requests_total %d", workers*updates)) ||
		!strings.Contains(text, fmt.Sprintf("loadbear_rate_limited_total %d", workers*((updates+9)/10))) ||
		!strings.Contains(text, "loadbear_active_tunnels 0") {
		t.Fatalf("concurrent metrics totals incorrect:\n%s", text)
	}
}

func TestStatusJSONAndMetricsHTTPEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer server.Close()
	b, err := backend.New(server.URL, 2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.RateLimit.Enabled = false
	strategy, err := balancer.New(cfg.Strategy)
	if err != nil {
		t.Fatal(err)
	}
	collector := metrics.New([]*backend.Backend{b})
	handler := proxy.NewHandler(&cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), strategy, []*backend.Backend{b}, collector)
	defer handler.StopBackground()

	status := httptest.NewRecorder()
	handler.StatusEndpoint(status, httptest.NewRequest(http.MethodGet, "/status", nil))
	if status.Code != http.StatusOK || status.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status endpoint headers/code = %d %#v", status.Code, status.Header())
	}
	var document struct {
		Backends []map[string]any `json:"backends"`
	}
	if err := json.Unmarshal(status.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Backends) != 1 {
		t.Fatalf("status backend count = %d", len(document.Backends))
	}
	for _, key := range []string{"url", "healthy", "active_connections", "total_requests", "total_failures", "circuit_state"} {
		if _, ok := document.Backends[0][key]; !ok {
			t.Errorf("status JSON missing %q: %#v", key, document.Backends[0])
		}
	}

	metricsResponse := httptest.NewRecorder()
	handler.MetricsEndpoint(metricsResponse, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metricsResponse.Code != http.StatusOK ||
		!strings.HasPrefix(metricsResponse.Header().Get("Content-Type"), "text/plain") ||
		!strings.Contains(metricsResponse.Body.String(), "loadbear_requests_total") {
		t.Fatalf("metrics endpoint = %d %#v %q", metricsResponse.Code, metricsResponse.Header(), metricsResponse.Body.String())
	}
}
