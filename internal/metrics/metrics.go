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

const (
	TunnelReasonClientClosed   = "client_closed"
	TunnelReasonUpstreamClosed = "upstream_closed"
	TunnelReasonUpstreamError  = "upstream_error"
	TunnelReasonShutdownForced = "shutdown_forced"
	TunnelReasonUnknown        = "unknown"
)

var tunnelCloseReasons = []string{
	TunnelReasonClientClosed,
	TunnelReasonUpstreamClosed,
	TunnelReasonUpstreamError,
	TunnelReasonShutdownForced,
	TunnelReasonUnknown,
}

type tunnelClosedKey struct {
	backend *backend.Backend
	reason  string
}

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
	activeTunnels      int64
	backendTunnels     map[*backend.Backend]int64
	tunnelClosed       map[tunnelClosedKey]uint64
	tunnelBuckets      []float64
	tunnelBucketCount  []uint64
	tunnelDurationSum  float64
	tunnelDurationN    uint64
}

func New(backends []*backend.Backend) *Metrics {
	buckets := []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	return &Metrics{
		Backends:           append([]*backend.Backend(nil), backends...),
		latencyBuckets:     buckets,
		latencyBucketCount: make([]uint64, len(buckets)),
		backendTunnels:     make(map[*backend.Backend]int64),
		tunnelClosed:       make(map[tunnelClosedKey]uint64),
		tunnelBuckets:      []float64{1, 5, 15, 60, 300, 900, 3600},
		tunnelBucketCount:  make([]uint64, 7),
	}
}

func (m *Metrics) SetBackends(backends []*backend.Backend) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Backends = append([]*backend.Backend(nil), backends...)
	current := make(map[*backend.Backend]struct{}, len(backends))
	for _, b := range backends {
		if b != nil {
			current[b] = struct{}{}
		}
	}
	for b := range m.backendTunnels {
		if _, ok := current[b]; !ok {
			delete(m.backendTunnels, b)
		}
	}
	for key := range m.tunnelClosed {
		if _, ok := current[key.backend]; !ok {
			delete(m.tunnelClosed, key)
		}
	}
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

func (m *Metrics) RecordTunnelOpened(b *backend.Backend) {
	m.mu.Lock()
	m.activeTunnels++
	if b != nil && m.hasBackendLocked(b) {
		m.backendTunnels[b]++
	}
	m.mu.Unlock()
}

func (m *Metrics) RecordTunnelClosed(b *backend.Backend, reason string, duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeTunnels > 0 {
		m.activeTunnels--
	}
	if b != nil && m.hasBackendLocked(b) {
		if m.backendTunnels[b] > 0 {
			m.backendTunnels[b]--
		}
		if !isTunnelCloseReason(reason) {
			reason = TunnelReasonUnknown
		}
		m.tunnelClosed[tunnelClosedKey{backend: b, reason: reason}]++
	}
	if duration < 0 {
		duration = 0
	}
	seconds := duration.Seconds()
	for i, bucket := range m.tunnelBuckets {
		if seconds <= bucket {
			m.tunnelBucketCount[i]++
		}
	}
	m.tunnelDurationSum += seconds
	m.tunnelDurationN++
}

func (m *Metrics) hasBackendLocked(candidate *backend.Backend) bool {
	for _, current := range m.Backends {
		if current == candidate {
			return true
		}
	}
	return false
}

func isTunnelCloseReason(reason string) bool {
	for _, known := range tunnelCloseReasons {
		if reason == known {
			return true
		}
	}
	return false
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
	b.WriteString("# HELP loadbear_active_tunnels Total number of active upgraded tunnels.\n")
	b.WriteString("# TYPE loadbear_active_tunnels gauge\n")
	fmt.Fprintf(&b, "loadbear_active_tunnels %d\n", m.activeTunnels)
	b.WriteString("# HELP loadbear_backend_active_tunnels Active upgraded tunnels per backend.\n")
	b.WriteString("# TYPE loadbear_backend_active_tunnels gauge\n")
	for _, backendItem := range backends {
		if backendItem == nil {
			continue
		}
		fmt.Fprintf(&b, "loadbear_backend_active_tunnels{backend=\"%s\"} %d\n",
			escapeLabel(backendItem.URL.String()), m.backendTunnels[backendItem])
	}
	b.WriteString("# HELP loadbear_tunnel_duration_seconds Duration of completed upgraded tunnels in seconds.\n")
	b.WriteString("# TYPE loadbear_tunnel_duration_seconds histogram\n")
	for i, bucket := range m.tunnelBuckets {
		label := strconv.FormatFloat(bucket, 'f', -1, 64)
		fmt.Fprintf(&b, "loadbear_tunnel_duration_seconds_bucket{le=%q} %d\n", label, m.tunnelBucketCount[i])
	}
	fmt.Fprintf(&b, "loadbear_tunnel_duration_seconds_bucket{le=\"+Inf\"} %d\n", m.tunnelDurationN)
	fmt.Fprintf(&b, "loadbear_tunnel_duration_seconds_sum %.9g\n", m.tunnelDurationSum)
	fmt.Fprintf(&b, "loadbear_tunnel_duration_seconds_count %d\n", m.tunnelDurationN)
	b.WriteString("# HELP loadbear_tunnel_closed_total Total number of closed upgraded tunnels by backend and reason.\n")
	b.WriteString("# TYPE loadbear_tunnel_closed_total counter\n")
	for _, backendItem := range backends {
		if backendItem == nil {
			continue
		}
		label := backendItem.URL.String()
		for _, reason := range tunnelCloseReasons {
			fmt.Fprintf(&b, "loadbear_tunnel_closed_total{backend=\"%s\",reason=\"%s\"} %d\n",
				escapeLabel(label), reason, m.tunnelClosed[tunnelClosedKey{backend: backendItem, reason: reason}])
		}
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
