package metrics

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/weaztoven/loadbear/internal/backend"
)

type BackendStatus struct {
	URL               string `json:"url"`
	Name              string `json:"name"`
	Healthy           bool   `json:"healthy"`
	ActiveConnections int64  `json:"active_connections"`
	TotalRequests     uint64 `json:"total_requests"`
	TotalFailures     uint64 `json:"total_failures"`
	CircuitState      string `json:"circuit_state"`
}

type Metrics struct {
	mu             sync.Mutex
	Backends       []*backend.Backend
	requestCount   uint64
	failureCount   uint64
	histogram      map[string]uint64
	latencyBuckets []float64
}

func New(backends []*backend.Backend) *Metrics {
	return &Metrics{
		Backends:       backends,
		latencyBuckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		histogram:      make(map[string]uint64),
	}
}

func (m *Metrics) RecordRequest(b *backend.Backend, status int, latency time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requestCount++
	if status >= 500 {
		m.failureCount++
	}
	for _, bucket := range m.latencyBuckets {
		if latency.Seconds() <= bucket {
			key := fmt.Sprintf("%.3f", bucket)
			m.histogram[key]++
			break
		}
	}
	if b != nil {
		b.RecordRequest()
		if status >= 500 {
			b.RecordFailure()
		}
	}
}

func (m *Metrics) Snapshot() []BackendStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := make([]BackendStatus, 0, len(m.Backends))
	for _, b := range m.Backends {
		if b == nil {
			continue
		}
		status = append(status, BackendStatus{
			URL:               b.URL.String(),
			Name:              b.Name,
			Healthy:           b.IsHealthy(),
			ActiveConnections: b.ActiveConnections.Load(),
			TotalRequests:     b.TotalRequests.Load(),
			TotalFailures:     b.TotalFailures.Load(),
			CircuitState:      string(b.CircuitState),
		})
	}
	return status
}

func (m *Metrics) PrometheusText() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	b.WriteString("# HELP loadbear_requests_total Total number of proxied requests.\n")
	b.WriteString("# TYPE loadbear_requests_total counter\n")
	fmt.Fprintf(&b, "loadbear_requests_total %d\n", m.requestCount)
	b.WriteString("# HELP loadbear_failures_total Total number of failing requests.\n")
	b.WriteString("# TYPE loadbear_failures_total counter\n")
	fmt.Fprintf(&b, "loadbear_failures_total %d\n", m.failureCount)
	b.WriteString("# HELP loadbear_request_latency_seconds Request latency in seconds.\n")
	b.WriteString("# TYPE loadbear_request_latency_seconds histogram\n")
	for _, bucket := range m.latencyBuckets {
		label := fmt.Sprintf("%.3f", bucket)
		fmt.Fprintf(&b, "loadbear_request_latency_seconds_bucket{le=\"%s\"} %d\n", label, m.histogram[label])
	}
	b.WriteString("loadbear_request_latency_seconds_bucket{le=\"+Inf\"} ")
	fmt.Fprintf(&b, "%d\n", m.requestCount)
	b.WriteString("# HELP loadbear_backend_up Backend health gauge.\n")
	b.WriteString("# TYPE loadbear_backend_up gauge\n")
	for _, backendItem := range m.Backends {
		if backendItem == nil {
			continue
		}
		value := 0
		if backendItem.IsHealthy() {
			value = 1
		}
		fmt.Fprintf(&b, "loadbear_backend_up{name=\"%s\"} %d\n", backendItem.Name, value)
	}
	b.WriteString("# HELP loadbear_backend_active_connections Active backend connections.\n")
	b.WriteString("# TYPE loadbear_backend_active_connections gauge\n")
	for _, backendItem := range m.Backends {
		if backendItem == nil {
			continue
		}
		fmt.Fprintf(&b, "loadbear_backend_active_connections{name=\"%s\"} %d\n", backendItem.Name, backendItem.ActiveConnections.Load())
	}
	return b.String()
}
