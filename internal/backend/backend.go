package backend

import (
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

type CircuitState string

const (
	CircuitClosed   CircuitState = "closed"
	CircuitOpen     CircuitState = "open"
	CircuitHalfOpen CircuitState = "half-open"
)

type Backend struct {
	URL    *url.URL
	Name   string
	Weight int

	Healthy bool
	mu      sync.RWMutex

	ActiveConnections atomic.Int64
	TotalRequests     atomic.Uint64
	TotalFailures     atomic.Uint64
	CurrentWeight     atomic.Int64

	CircuitState            CircuitState
	CircuitFailureThreshold int
	CircuitCooldown         time.Duration
	CircuitOpenedAt         time.Time
	circuitMux              sync.Mutex
	consecutiveFailures     int32
	halfOpenProbeInFlight   bool
}

func New(rawURL string, weight int) (*Backend, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid URL %q: missing scheme or host", rawURL)
	}
	if weight <= 0 {
		return nil, fmt.Errorf("weight must be greater than zero for %q", rawURL)
	}
	return &Backend{
		URL:                     parsed,
		Name:                    parsed.Host,
		Weight:                  weight,
		Healthy:                 true,
		CircuitState:            CircuitClosed,
		CircuitFailureThreshold: 5,
		CircuitCooldown:         30 * time.Second,
	}, nil
}

func (b *Backend) IsHealthy() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.Healthy
}

func (b *Backend) SetHealthy(healthy bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Healthy = healthy
}

func (b *Backend) IncrementActive() {
	b.ActiveConnections.Add(1)
}

func (b *Backend) DecrementActive() {
	b.ActiveConnections.Add(-1)
}

func (b *Backend) RecordRequest() {
	b.TotalRequests.Add(1)
}

func (b *Backend) RecordFailure() {
	b.TotalFailures.Add(1)
	b.circuitMux.Lock()
	defer b.circuitMux.Unlock()
	b.consecutiveFailures++
	if b.CircuitState == CircuitHalfOpen {
		b.CircuitState = CircuitOpen
		b.CircuitOpenedAt = time.Now()
		b.halfOpenProbeInFlight = false
		return
	}
	if b.CircuitState == CircuitClosed && int(b.consecutiveFailures) >= b.CircuitFailureThreshold {
		b.CircuitState = CircuitOpen
		b.CircuitOpenedAt = time.Now()
		b.halfOpenProbeInFlight = false
	}
}

func (b *Backend) RecordSuccess() {
	b.circuitMux.Lock()
	defer b.circuitMux.Unlock()
	b.consecutiveFailures = 0
	if b.CircuitState == CircuitOpen {
		if time.Since(b.CircuitOpenedAt) >= b.CircuitCooldown {
			b.CircuitState = CircuitClosed
			b.halfOpenProbeInFlight = false
		}
	}
	if b.CircuitState == CircuitHalfOpen {
		b.CircuitState = CircuitClosed
		b.halfOpenProbeInFlight = false
	}
}

func (b *Backend) AllowRequest() bool {
	allowed, _ := b.AllowRequestWithProbe()
	return allowed
}

func (b *Backend) AllowRequestWithProbe() (bool, bool) {
	b.circuitMux.Lock()
	defer b.circuitMux.Unlock()
	if b.CircuitState == CircuitOpen {
		if time.Since(b.CircuitOpenedAt) >= b.CircuitCooldown {
			b.CircuitState = CircuitHalfOpen
			b.halfOpenProbeInFlight = true
			return true, true
		}
		return false, false
	}
	if b.CircuitState == CircuitHalfOpen {
		if b.halfOpenProbeInFlight {
			return false, false
		}
		b.halfOpenProbeInFlight = true
		return true, true
	}
	return true, false
}

func (b *Backend) AbortHalfOpenProbe() {
	b.circuitMux.Lock()
	defer b.circuitMux.Unlock()
	if b.CircuitState != CircuitHalfOpen || !b.halfOpenProbeInFlight {
		return
	}
	b.TotalFailures.Add(1)
	b.consecutiveFailures++
	b.CircuitState = CircuitOpen
	b.CircuitOpenedAt = time.Now()
	b.halfOpenProbeInFlight = false
}

func (b *Backend) CircuitAvailable() bool {
	b.circuitMux.Lock()
	defer b.circuitMux.Unlock()
	switch b.CircuitState {
	case CircuitClosed:
		return true
	case CircuitOpen:
		return time.Since(b.CircuitOpenedAt) >= b.CircuitCooldown
	case CircuitHalfOpen:
		return !b.halfOpenProbeInFlight
	default:
		return false
	}
}

func (b *Backend) GetCircuitState() CircuitState {
	b.circuitMux.Lock()
	defer b.circuitMux.Unlock()
	return b.CircuitState
}

func (b *Backend) Export() map[string]any {
	return map[string]any{
		"url":                b.URL.String(),
		"name":               b.Name,
		"healthy":            b.IsHealthy(),
		"active_connections": b.ActiveConnections.Load(),
		"total_requests":     b.TotalRequests.Load(),
		"total_failures":     b.TotalFailures.Load(),
		"circuit_state":      string(b.GetCircuitState()),
		"weight":             b.Weight,
	}
}
