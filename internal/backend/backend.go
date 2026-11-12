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
		return
	}
	if b.CircuitState == CircuitClosed && int(b.consecutiveFailures) >= b.CircuitFailureThreshold {
		b.CircuitState = CircuitOpen
		b.CircuitOpenedAt = time.Now()
	}
}

func (b *Backend) RecordSuccess() {
	b.circuitMux.Lock()
	defer b.circuitMux.Unlock()
	b.consecutiveFailures = 0
	if b.CircuitState == CircuitOpen {
		if time.Since(b.CircuitOpenedAt) >= b.CircuitCooldown {
			b.CircuitState = CircuitClosed
		}
	}
	if b.CircuitState == CircuitHalfOpen {
		b.CircuitState = CircuitClosed
	}
}

func (b *Backend) AllowRequest() bool {
	b.circuitMux.Lock()
	defer b.circuitMux.Unlock()
	if b.CircuitState == CircuitOpen {
		if time.Since(b.CircuitOpenedAt) >= b.CircuitCooldown {
			b.CircuitState = CircuitHalfOpen
			return true
		}
		return false
	}
	if b.CircuitState == CircuitHalfOpen {
		return true
	}
	return true
}

func (b *Backend) Export() map[string]any {
	return map[string]any{
		"url":                b.URL.String(),
		"name":               b.Name,
		"healthy":            b.IsHealthy(),
		"active_connections": b.ActiveConnections.Load(),
		"total_requests":     b.TotalRequests.Load(),
		"total_failures":     b.TotalFailures.Load(),
		"circuit_state":      string(b.CircuitState),
		"weight":             b.Weight,
	}
}
