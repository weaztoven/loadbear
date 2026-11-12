package balancer

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/weaztoven/loadbear/internal/backend"
)

func mustBackend(tb testing.TB, rawURL string, weight int) *backend.Backend {
	tb.Helper()
	b, err := backend.New(rawURL, weight)
	if err != nil {
		tb.Fatalf("new backend: %v", err)
	}
	return b
}

func TestRoundRobinSelectsHealthy(t *testing.T) {
	b1 := mustBackend(t, "http://127.0.0.1:8081", 1)
	b2 := mustBackend(t, "http://127.0.0.1:8082", 1)
	b2.SetHealthy(false)
	s := &RoundRobin{}
	if got := s.Select([]*backend.Backend{b1, b2}, &http.Request{}); got != b1 {
		t.Fatalf("expected first backend, got %v", got)
	}
}

func TestRoundRobinDistributionAndEmptyCandidates(t *testing.T) {
	backends := []*backend.Backend{
		mustBackend(t, "http://127.0.0.1:8081", 1),
		mustBackend(t, "http://127.0.0.1:8082", 1),
		mustBackend(t, "http://127.0.0.1:8083", 1),
	}
	strategy := &RoundRobin{}
	if strategy.Name() != "round_robin" {
		t.Fatalf("strategy name = %q", strategy.Name())
	}
	counts := make(map[*backend.Backend]int)
	for range 300 {
		counts[strategy.Select(backends, nil)]++
	}
	for _, b := range backends {
		if counts[b] != 100 {
			t.Errorf("%s selected %d times; want 100", b.Name, counts[b])
		}
		b.SetHealthy(false)
	}
	if got := strategy.Select(backends, nil); got != nil {
		t.Fatalf("selection with no healthy backend = %v", got)
	}
}

func TestWeightedDistribution(t *testing.T) {
	b1 := mustBackend(t, "http://127.0.0.1:8081", 2)
	b2 := mustBackend(t, "http://127.0.0.1:8082", 1)
	s := &SmoothWeightedRoundRobin{}
	counts := map[string]int{}
	for i := 0; i < 30; i++ {
		b := s.Select([]*backend.Backend{b1, b2}, &http.Request{})
		counts[b.Name]++
	}
	if counts[b1.Name] == 0 || counts[b2.Name] == 0 {
		t.Fatalf("expected weighted distribution, got %#v", counts)
	}
	if counts[b1.Name] < counts[b2.Name] {
		t.Fatalf("expected heavier backend to win often, got %#v", counts)
	}
}

func TestSmoothWeightedDistributionAccuracyAndSkipping(t *testing.T) {
	b1 := mustBackend(t, "http://127.0.0.1:8081", 5)
	b2 := mustBackend(t, "http://127.0.0.1:8082", 2)
	b3 := mustBackend(t, "http://127.0.0.1:8083", 3)
	strategy := &SmoothWeightedRoundRobin{}
	if strategy.Name() != "smooth_weighted_round_robin" {
		t.Fatalf("strategy name = %q", strategy.Name())
	}
	counts := make(map[*backend.Backend]int)
	for range 1000 {
		counts[strategy.Select([]*backend.Backend{b1, b2, b3}, nil)]++
	}
	for b, want := range map[*backend.Backend]int{b1: 500, b2: 200, b3: 300} {
		if counts[b] != want {
			t.Errorf("%s selected %d times; want %d", b.Name, counts[b], want)
		}
	}
	b1.SetHealthy(false)
	counts = make(map[*backend.Backend]int)
	for range 50 {
		counts[strategy.Select([]*backend.Backend{b1, b2, b3}, nil)]++
	}
	if counts[b1] != 0 || counts[b2] != 20 || counts[b3] != 30 {
		t.Fatalf("unhealthy weighted backend was not skipped: %#v", counts)
	}
	b2.SetHealthy(false)
	b3.SetHealthy(false)
	if got := strategy.Select([]*backend.Backend{b1, b2, b3}, nil); got != nil {
		t.Fatalf("weighted selection with no candidates = %v", got)
	}
}

func TestLeastConnections(t *testing.T) {
	b1 := mustBackend(t, "http://127.0.0.1:8081", 1)
	b2 := mustBackend(t, "http://127.0.0.1:8082", 1)
	b1.IncrementActive()
	s := &LeastConnections{}
	if got := s.Select([]*backend.Backend{b1, b2}, &http.Request{}); got != b2 {
		t.Fatalf("expected backend 2, got %v", got)
	}
}

func TestLeastConnectionsTieUnhealthyAndEmpty(t *testing.T) {
	b1 := mustBackend(t, "http://127.0.0.1:8081", 1)
	b2 := mustBackend(t, "http://127.0.0.1:8082", 1)
	strategy := &LeastConnections{}
	if strategy.Name() != "least_connections" {
		t.Fatalf("strategy name = %q", strategy.Name())
	}
	if got := strategy.Select([]*backend.Backend{b1, b2}, nil); got != b1 {
		t.Fatalf("tie should select first backend, got %v", got)
	}
	b1.SetHealthy(false)
	if got := strategy.Select([]*backend.Backend{b1, b2}, nil); got != b2 {
		t.Fatalf("unhealthy backend was not skipped, got %v", got)
	}
	b2.SetHealthy(false)
	if got := strategy.Select([]*backend.Backend{b1, b2}, nil); got != nil {
		t.Fatalf("selection with no candidates = %v", got)
	}
}

func TestIPHashIsStable(t *testing.T) {
	b1 := mustBackend(t, "http://127.0.0.1:8081", 1)
	b2 := mustBackend(t, "http://127.0.0.1:8082", 1)
	b3 := mustBackend(t, "http://127.0.0.1:8083", 1)
	s := &IPHash{}
	req1 := &http.Request{Header: http.Header{}, RemoteAddr: "10.0.0.5:1234"}
	req2 := &http.Request{Header: http.Header{}, RemoteAddr: "10.0.0.5:1234"}
	if got1 := s.Select([]*backend.Backend{b1, b2, b3}, req1); got1 != s.Select([]*backend.Backend{b1, b2, b3}, req2) {
		t.Fatalf("expected stable hash for same IP")
	}
}

func TestIPHashConsistencyAndUnhealthySkipping(t *testing.T) {
	b1 := mustBackend(t, "http://127.0.0.1:8081", 1)
	b2 := mustBackend(t, "http://127.0.0.1:8082", 1)
	b3 := mustBackend(t, "http://127.0.0.1:8083", 1)
	strategy := &IPHash{}
	if strategy.Name() != "ip_hash" {
		t.Fatalf("strategy name = %q", strategy.Name())
	}
	req1 := &http.Request{Header: make(http.Header), RemoteAddr: "198.51.100.10:1234"}
	req2 := &http.Request{Header: make(http.Header), RemoteAddr: "198.51.100.10:5678"}
	req1.Header.Set("X-Forwarded-For", "203.0.113.1")
	req2.Header.Set("X-Forwarded-For", "203.0.113.2")
	if got1, got2 := strategy.Select([]*backend.Backend{b1, b2, b3}, req1), strategy.Select([]*backend.Backend{b1, b2, b3}, req2); got1 != got2 {
		t.Fatalf("same client IP mapped to different backends: %v and %v", got1, got2)
	}
	expected := strategy.Select([]*backend.Backend{b1, b2, b3}, req1)
	for port := 1; port <= 100; port++ {
		req := &http.Request{Header: make(http.Header), RemoteAddr: fmt.Sprintf("198.51.100.10:%d", port)}
		if got := strategy.Select([]*backend.Backend{b1, b2, b3}, req); got != expected {
			t.Fatalf("same client IP on port %d mapped to %v; want %v", port, got, expected)
		}
	}
	b1.SetHealthy(false)
	b2.SetHealthy(false)
	if got := strategy.Select([]*backend.Backend{b1, b2, b3}, req1); got != b3 {
		t.Fatalf("IP hash did not skip unhealthy backends: %v", got)
	}
	b3.SetHealthy(false)
	if got := strategy.Select([]*backend.Backend{b1, b2, b3}, req1); got != nil {
		t.Fatalf("IP hash with no candidates = %v", got)
	}
	if got := strategy.Select([]*backend.Backend{b1, b2, b3}, nil); got != nil {
		t.Fatalf("nil request/no candidates = %v", got)
	}
}

func TestStrategyFactory(t *testing.T) {
	for _, name := range []string{"", "round_robin", "smooth_weighted_round_robin", "least_connections", "ip_hash"} {
		if _, err := New(name); err != nil {
			t.Errorf("New(%q): %v", name, err)
		}
	}
	if strategy, err := New("unknown"); err == nil || strategy != nil {
		t.Fatalf("New unknown strategy = %v, %v", strategy, err)
	}
}

func BenchmarkRoundRobinSelect(b *testing.B) {
	backends := []*backend.Backend{
		mustBackend(b, "http://127.0.0.1:8081", 1),
		mustBackend(b, "http://127.0.0.1:8082", 1),
		mustBackend(b, "http://127.0.0.1:8083", 1),
	}
	s := &RoundRobin{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Select(backends, &http.Request{})
	}
}
