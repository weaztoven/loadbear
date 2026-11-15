package main

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/weaztoven/loadbear/internal/backend"
	"github.com/weaztoven/loadbear/internal/balancer"
	"github.com/weaztoven/loadbear/internal/config"
	"github.com/weaztoven/loadbear/internal/health"
	"github.com/weaztoven/loadbear/internal/proxy"
)

type reloadManager struct {
	mu                      sync.Mutex
	configPath              string
	circuitFailureThreshold int
	circuitCooldown         time.Duration
	handler                 *proxy.Handler
	healthChecker           *health.Checker
	logger                  *slog.Logger
}

func newReloadManager(configPath string, cfg *config.Config, handler *proxy.Handler, checker *health.Checker, logger *slog.Logger) *reloadManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &reloadManager{
		configPath:              configPath,
		circuitFailureThreshold: cfg.Proxy.CircuitFailureThreshold,
		circuitCooldown:         cfg.Proxy.CircuitCooldown.Duration(),
		handler:                 handler,
		healthChecker:           checker,
		logger:                  logger,
	}
}

func (m *reloadManager) ReloadFromFile() error {
	updated, err := config.Load(m.configPath)
	if err != nil {
		m.logger.Error("configuration reload rejected", "path", m.configPath, "error", err)
		return err
	}
	return m.Reload(updated)
}

func (m *reloadManager) Reload(updated *config.Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if updated == nil {
		err := fmt.Errorf("reload config is nil")
		m.logger.Error("configuration reload rejected", "error", err)
		return err
	}
	if err := updated.Validate(); err != nil {
		m.logger.Error("configuration reload rejected", "error", err)
		return err
	}
	strategy, err := balancer.New(updated.Strategy)
	if err != nil {
		m.logger.Error("configuration reload rejected", "error", err)
		return err
	}

	existing := make(map[string]*backend.Backend)
	for _, current := range m.handler.CurrentBackends() {
		if current != nil {
			existing[current.URL.String()] = current
		}
	}
	newBackends := make([]*backend.Backend, 0, len(updated.Backends))
	weightUpdates := make(map[*backend.Backend]int)
	unchanged := 0
	for _, item := range updated.Backends {
		candidate, err := backend.New(item.URL, item.Weight)
		if err != nil {
			m.logger.Error("configuration reload rejected", "url", item.URL, "error", err)
			return err
		}
		if current, ok := existing[candidate.URL.String()]; ok {
			newBackends = append(newBackends, current)
			weightUpdates[current] = item.Weight
			delete(existing, candidate.URL.String())
			unchanged++
			continue
		}
		candidate.CircuitFailureThreshold = m.circuitFailureThreshold
		candidate.CircuitCooldown = m.circuitCooldown
		newBackends = append(newBackends, candidate)
	}
	added := len(newBackends) - unchanged
	removed := len(existing)

	m.handler.ReplaceBackendSet(newBackends, strategy, weightUpdates)
	if m.healthChecker != nil {
		m.healthChecker.SetBackends(newBackends)
	}
	m.logger.Info("backend list reloaded", "added", added, "removed", removed, "unchanged", unchanged, "strategy", updated.Strategy)
	return nil
}
