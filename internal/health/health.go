package health

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/weaztoven/loadbear/internal/backend"
)

type Checker struct {
	Path               string
	Interval           time.Duration
	Timeout            time.Duration
	HealthyThreshold   int
	UnhealthyThreshold int
	Logger             *slog.Logger
	ctx                context.Context
	cancel             context.CancelFunc
	startOnce          sync.Once
	done               chan struct{}
	mu                 sync.RWMutex
	backends           []*backend.Backend
	stateMu            sync.Mutex
	states             map[*backend.Backend]checkState
}

type checkState struct {
	failures  int
	successes int
}

func New(backends []*backend.Backend, path string, interval, timeout time.Duration, healthyThreshold, unhealthyThreshold int, logger *slog.Logger) *Checker {
	ctx, cancel := context.WithCancel(context.Background())
	return &Checker{
		Path:               path,
		Interval:           interval,
		Timeout:            timeout,
		HealthyThreshold:   healthyThreshold,
		UnhealthyThreshold: unhealthyThreshold,
		Logger:             logger,
		ctx:                ctx,
		cancel:             cancel,
		done:               make(chan struct{}),
		backends:           append([]*backend.Backend(nil), backends...),
		states:             make(map[*backend.Backend]checkState),
	}
}

func (c *Checker) SetBackends(backends []*backend.Backend) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.backends = append([]*backend.Backend(nil), backends...)
	current := make(map[*backend.Backend]struct{}, len(c.backends))
	for _, b := range c.backends {
		current[b] = struct{}{}
	}
	for b := range c.states {
		if _, ok := current[b]; !ok {
			delete(c.states, b)
		}
	}
}

func (c *Checker) backendSnapshot() []*backend.Backend {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]*backend.Backend(nil), c.backends...)
}

func (c *Checker) Start() {
	c.startOnce.Do(func() {
		if c.Interval <= 0 {
			close(c.done)
			return
		}
		go func() {
			defer close(c.done)
			ticker := time.NewTicker(c.Interval)
			defer ticker.Stop()
			for {
				select {
				case <-c.ctx.Done():
					return
				case <-ticker.C:
					for _, b := range c.backendSnapshot() {
						c.check(b)
					}
				}
			}
		}()
	})
}

func (c *Checker) Stop() {
	c.cancel()
	c.startOnce.Do(func() {
		close(c.done)
	})
	<-c.done
}

func (c *Checker) check(b *backend.Backend) bool {
	if b == nil {
		return false
	}
	client := &http.Client{Timeout: c.Timeout}
	endpoint := b.URL.String()
	if c.Path != "" {
		endpoint = strings.TrimRight(b.URL.String(), "/") + c.Path
	}
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return c.recordResult(b, false, 0, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return c.recordResult(b, false, 0, err)
	}
	defer resp.Body.Close()
	return c.recordResult(b, resp.StatusCode >= 200 && resp.StatusCode < 400, resp.StatusCode, nil)
}

func (c *Checker) recordResult(b *backend.Backend, healthy bool, status int, err error) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	found := false
	for _, current := range c.backends {
		if current == b {
			found = true
			break
		}
	}
	if !found {
		return healthy
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	state := c.states[b]
	wasHealthy := b.IsHealthy()
	if healthy {
		state.successes++
		state.failures = 0
		if !wasHealthy && state.successes >= c.HealthyThreshold {
			b.SetHealthy(true)
			if c.Logger != nil {
				c.Logger.Info("backend marked healthy", "backend", b.Name)
			}
		}
	} else {
		state.failures++
		state.successes = 0
		if wasHealthy && state.failures >= c.UnhealthyThreshold {
			b.SetHealthy(false)
			if c.Logger != nil {
				args := []any{"backend", b.Name}
				if err != nil {
					args = append(args, "path", c.Path, "error", err)
				} else {
					args = append(args, "status", status)
				}
				c.Logger.Warn("backend marked unhealthy", args...)
			}
		}
	}
	c.states[b] = state
	return healthy
}
