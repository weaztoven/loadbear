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

type RoundRobin struct {
	next atomic.Uint64
}

func (s *RoundRobin) Name() string { return "round_robin" }

func (s *RoundRobin) Select(backends []*backend.Backend, _ *http.Request) *backend.Backend {
	var candidates []*backend.Backend
	for _, b := range backends {
		if b.IsHealthy() && b.AllowRequest() {
			candidates = append(candidates, b)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	idx := int(s.next.Add(1) % uint64(len(candidates)))
	return candidates[idx]
}

type SmoothWeightedRoundRobin struct{}

func (s *SmoothWeightedRoundRobin) Name() string { return "smooth_weighted_round_robin" }

func (s *SmoothWeightedRoundRobin) Select(backends []*backend.Backend, _ *http.Request) *backend.Backend {
	var candidates []*backend.Backend
	for _, b := range backends {
		if b.IsHealthy() && b.AllowRequest() {
			candidates = append(candidates, b)
		}
	}
	if len(candidates) == 0 {
		return nil
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
	if best != nil {
		best.CurrentWeight.Add(-total)
	}
	return best
}

type LeastConnections struct{}

func (s *LeastConnections) Name() string { return "least_connections" }

func (s *LeastConnections) Select(backends []*backend.Backend, _ *http.Request) *backend.Backend {
	var best *backend.Backend
	var bestScore int64 = 1 << 62
	for _, b := range backends {
		if !b.IsHealthy() || !b.AllowRequest() {
			continue
		}
		value := b.ActiveConnections.Load()
		if best == nil || value < bestScore {
			best = b
			bestScore = value
		}
	}
	return best
}

type IPHash struct{}

func (s *IPHash) Name() string { return "ip_hash" }

func (s *IPHash) Select(backends []*backend.Backend, req *http.Request) *backend.Backend {
	var candidates []*backend.Backend
	for _, b := range backends {
		if b.IsHealthy() && b.AllowRequest() {
			candidates = append(candidates, b)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
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
	h := fnv.New32a()
	_, _ = h.Write([]byte(clientIP))
	idx := int(h.Sum32()) % len(candidates)
	return candidates[idx]
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
