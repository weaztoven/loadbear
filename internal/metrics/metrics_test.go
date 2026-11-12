package metrics_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
		"loadbear_request_latency_seconds_bucket{le=\"0.010\"} 1",
		"loadbear_request_latency_seconds_bucket{le=\"+Inf\"} 3",
		"loadbear_backend_up{name=\"127.0.0.1:8081\"} 1",
		"loadbear_backend_active_connections{name=\"127.0.0.1:8081\"} 1",
	} {
		if !strings.Contains(text, metric) {
			t.Errorf("Prometheus output missing %q:\n%s", metric, text)
		}
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
