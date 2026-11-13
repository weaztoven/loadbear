package balancer

import (
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/weaztoven/loadbear/internal/backend"
)

type Strategy interface {
	Name() string
	Select([]*backend.Backend, *http.Request) *backend.Backend
}

type ProbeStrategy interface {
	SelectWithProbe([]*backend.Backend, *http.Request) (*backend.Backend, bool)
}

type RoundRobin struct {
	next atomic.Uint64
}

func (s *RoundRobin) Name() string { return "round_robin" }

func (s *RoundRobin) Select(backends []*backend.Backend, req *http.Request) *backend.Backend {
	selected, _ := s.SelectWithProbe(backends, req)
	return selected
}

func (s *RoundRobin) SelectWithProbe(backends []*backend.Backend, _ *http.Request) (*backend.Backend, bool) {
	excluded := make(map[*backend.Backend]bool)
	for {
		candidates := availableBackends(backends, excluded)
		if len(candidates) == 0 {
			return nil, false
		}
		selected := candidates[int(s.next.Add(1)%uint64(len(candidates)))]
		if allowed, probe := selected.AllowRequestWithProbe(); allowed {
			return selected, probe
		}
		excluded[selected] = true
	}
}

type SmoothWeightedRoundRobin struct{}

func (s *SmoothWeightedRoundRobin) Name() string { return "smooth_weighted_round_robin" }

func (s *SmoothWeightedRoundRobin) Select(backends []*backend.Backend, req *http.Request) *backend.Backend {
	selected, _ := s.SelectWithProbe(backends, req)
	return selected
}

func (s *SmoothWeightedRoundRobin) SelectWithProbe(backends []*backend.Backend, _ *http.Request) (*backend.Backend, bool) {
	excluded := make(map[*backend.Backend]bool)
	for {
		candidates := availableBackends(backends, excluded)
		if len(candidates) == 0 {
			return nil, false
		}
		var total int64
		best := candidates[0]
		bestScore := int64(-1 << 62)
		for _, b := range candidates {
			current := b.CurrentWeight.Add(int64(b.Weight))
			total += int64(b.Weight)
			if current > bestScore {
				bestScore = current
				best = b
			}
		}
		best.CurrentWeight.Add(-total)
		if allowed, probe := best.AllowRequestWithProbe(); allowed {
			return best, probe
		}
		excluded[best] = true
	}
}

type LeastConnections struct{}

func (s *LeastConnections) Name() string { return "least_connections" }

func (s *LeastConnections) Select(backends []*backend.Backend, req *http.Request) *backend.Backend {
	selected, _ := s.SelectWithProbe(backends, req)
	return selected
}

func (s *LeastConnections) SelectWithProbe(backends []*backend.Backend, _ *http.Request) (*backend.Backend, bool) {
	excluded := make(map[*backend.Backend]bool)
	for {
		candidates := availableBackends(backends, excluded)
		var best *backend.Backend
		var bestScore int64 = 1 << 62
		for _, b := range candidates {
			value := b.ActiveConnections.Load()
			if best == nil || value < bestScore {
				best = b
				bestScore = value
			}
		}
		if best == nil {
			return nil, false
		}
		if allowed, probe := best.AllowRequestWithProbe(); allowed {
			return best, probe
		}
		excluded[best] = true
	}
}

type IPHash struct{}

func (s *IPHash) Name() string { return "ip_hash" }

func (s *IPHash) Select(backends []*backend.Backend, req *http.Request) *backend.Backend {
	selected, _ := s.SelectWithProbe(backends, req)
	return selected
}

func (s *IPHash) SelectWithProbe(backends []*backend.Backend, req *http.Request) (*backend.Backend, bool) {
	clientIP := ""
	if req != nil {
		clientIP = req.RemoteAddr
		if host, _, err := net.SplitHostPort(clientIP); err == nil {
			clientIP = host
		}
	}
	if clientIP == "" {
		clientIP = "default"
	}
	excluded := make(map[*backend.Backend]bool)
	for {
		candidates := availableBackends(backends, excluded)
		if len(candidates) == 0 {
			return nil, false
		}
		h := fnv.New32a()
		_, _ = h.Write([]byte(clientIP))
		selected := candidates[int(h.Sum32())%len(candidates)]
		if allowed, probe := selected.AllowRequestWithProbe(); allowed {
			return selected, probe
		}
		excluded[selected] = true
	}
}

func availableBackends(backends []*backend.Backend, excluded map[*backend.Backend]bool) []*backend.Backend {
	var candidates []*backend.Backend
	for _, b := range backends {
		if b != nil && !excluded[b] && b.IsHealthy() && b.CircuitAvailable() {
			candidates = append(candidates, b)
		}
	}
	return candidates
}

func New(name string) (Strategy, error) {
	switch name {
	case "", "round_robin":
		return &RoundRobin{}, nil
	case "smooth_weighted_round_robin":
		return &SmoothWeightedRoundRobin{}, nil
	case "least_connections":
		return &LeastConnections{}, nil
	case "ip_hash":
		return &IPHash{}, nil
	default:
		return nil, fmt.Errorf("unknown strategy %q", name)
	}
}
