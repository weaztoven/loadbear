package metrics

import (
	"fmt"
	"sort"
	"strconv"
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
	mu                 sync.Mutex
	Backends           []*backend.Backend
	requestCount       uint64
	failureCount       uint64
	rateLimitedCount   uint64
	latencyBuckets     []float64
	latencyBucketCount []uint64
	latencySum         float64
}

func New(backends []*backend.Backend) *Metrics {
	buckets := []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	return &Metrics{
		Backends:           append([]*backend.Backend(nil), backends...),
		latencyBuckets:     buckets,
		latencyBucketCount: make([]uint64, len(buckets)),
	}
}

func (m *Metrics) SetBackends(backends []*backend.Backend) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Backends = append([]*backend.Backend(nil), backends...)
}

func (m *Metrics) RecordRequest(b *backend.Backend, status int, latency time.Duration) {
	m.recordRequest(b, status, latency, status >= 500, true)
}

func (m *Metrics) RecordObservation(status int, latency time.Duration, failed bool) {
	m.recordRequest(nil, status, latency, failed, false)
}

func (m *Metrics) recordRequest(b *backend.Backend, status int, latency time.Duration, failed, recordBackendFailure bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requestCount++
	if failed || status >= 500 {
		m.failureCount++
	}
	seconds := latency.Seconds()
	m.latencySum += seconds
	for i, bucket := range m.latencyBuckets {
		if seconds <= bucket {
			m.latencyBucketCount[i]++
		}
	}
	if b != nil {
		b.RecordRequest()
		if recordBackendFailure && (failed || status >= 500) {
			b.RecordFailure()
		}
	}
}

func (m *Metrics) RecordRateLimited() {
	m.mu.Lock()
	m.rateLimitedCount++
	m.mu.Unlock()
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
			CircuitState:      string(b.GetCircuitState()),
		})
	}
	return status
}

func (m *Metrics) PrometheusText() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	backends := append([]*backend.Backend(nil), m.Backends...)
	sort.Slice(backends, func(i, j int) bool {
		if backends[i] == nil {
			return backends[j] != nil
		}
		if backends[j] == nil {
			return false
		}
		return backends[i].URL.String() < backends[j].URL.String()
	})

	var b strings.Builder
	b.WriteString("# HELP loadbear_requests_total Total number of proxied requests.\n")
	b.WriteString("# TYPE loadbear_requests_total counter\n")
	fmt.Fprintf(&b, "loadbear_requests_total %d\n", m.requestCount)
	b.WriteString("# HELP loadbear_failures_total Total number of failing requests.\n")
	b.WriteString("# TYPE loadbear_failures_total counter\n")
	fmt.Fprintf(&b, "loadbear_failures_total %d\n", m.failureCount)
	b.WriteString("# HELP loadbear_rate_limited_total Total number of rate-limited requests.\n")
	b.WriteString("# TYPE loadbear_rate_limited_total counter\n")
	fmt.Fprintf(&b, "loadbear_rate_limited_total %d\n", m.rateLimitedCount)
	b.WriteString("# HELP loadbear_request_latency_seconds Request latency in seconds.\n")
	b.WriteString("# TYPE loadbear_request_latency_seconds histogram\n")
	for i, bucket := range m.latencyBuckets {
		label := strconv.FormatFloat(bucket, 'f', -1, 64)
		fmt.Fprintf(&b, "loadbear_request_latency_seconds_bucket{le=%q} %d\n", label, m.latencyBucketCount[i])
	}
	fmt.Fprintf(&b, "loadbear_request_latency_seconds_bucket{le=\"+Inf\"} %d\n", m.requestCount)
	fmt.Fprintf(&b, "loadbear_request_latency_seconds_sum %.9g\n", m.latencySum)
	fmt.Fprintf(&b, "loadbear_request_latency_seconds_count %d\n", m.requestCount)

	b.WriteString("# HELP loadbear_backend_up Backend health gauge.\n")
	b.WriteString("# TYPE loadbear_backend_up gauge\n")
	for _, backendItem := range backends {
		if backendItem == nil {
			continue
		}
		value := 0
		if backendItem.IsHealthy() {
			value = 1
		}
		fmt.Fprintf(&b, "loadbear_backend_up{backend=\"%s\"} %d\n", escapeLabel(backendItem.URL.String()), value)
	}
	b.WriteString("# HELP loadbear_backend_requests_total Total requests sent to each backend.\n")
	b.WriteString("# TYPE loadbear_backend_requests_total counter\n")
	for _, backendItem := range backends {
		if backendItem == nil {
			continue
		}
		fmt.Fprintf(&b, "loadbear_backend_requests_total{backend=\"%s\"} %d\n",
			escapeLabel(backendItem.URL.String()), backendItem.TotalRequests.Load())
	}
	b.WriteString("# HELP loadbear_backend_failures_total Total failed requests for each backend.\n")
	b.WriteString("# TYPE loadbear_backend_failures_total counter\n")
	for _, backendItem := range backends {
		if backendItem == nil {
			continue
		}
		fmt.Fprintf(&b, "loadbear_backend_failures_total{backend=\"%s\"} %d\n",
			escapeLabel(backendItem.URL.String()), backendItem.TotalFailures.Load())
	}
	b.WriteString("# HELP loadbear_backend_active_connections Active backend connections.\n")
	b.WriteString("# TYPE loadbear_backend_active_connections gauge\n")
	for _, backendItem := range backends {
		if backendItem == nil {
			continue
		}
		fmt.Fprintf(&b, "loadbear_backend_active_connections{backend=\"%s\"} %d\n",
			escapeLabel(backendItem.URL.String()), backendItem.ActiveConnections.Load())
	}
	b.WriteString("# HELP loadbear_circuit_state Backend circuit state (0 closed, 1 open, 2 half-open).\n")
	b.WriteString("# TYPE loadbear_circuit_state gauge\n")
	for _, backendItem := range backends {
		if backendItem == nil {
			continue
		}
		fmt.Fprintf(&b, "loadbear_circuit_state{backend=\"%s\"} %d\n",
			escapeLabel(backendItem.URL.String()), circuitStateValue(backendItem.GetCircuitState()))
	}
	return b.String()
}

func escapeLabel(value string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(value)
}

func circuitStateValue(state backend.CircuitState) int {
	switch state {
	case backend.CircuitOpen:
		return 1
	case backend.CircuitHalfOpen:
		return 2
	default:
		return 0
	}
}
