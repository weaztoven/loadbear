package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/weaztoven/loadbear/internal/backend"
	"github.com/weaztoven/loadbear/internal/balancer"
	"github.com/weaztoven/loadbear/internal/config"
	"github.com/weaztoven/loadbear/internal/metrics"
	"github.com/weaztoven/loadbear/internal/proxy"
)

func newPprofTestHandler(t *testing.T, cfg *config.Config) (*proxy.Handler, *httptest.Server) {
	t.Helper()
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "backend response")
	}))
	b, err := backend.New(backendServer.URL, 1)
	if err != nil {
		t.Fatal(err)
	}
	strategy, err := balancer.New(cfg.Strategy)
	if err != nil {
		t.Fatal(err)
	}
	handler := proxy.NewHandler(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), strategy,
		[]*backend.Backend{b}, metrics.New([]*backend.Backend{b}))
	t.Cleanup(func() {
		handler.StopBackground()
		backendServer.Close()
	})
	return handler, backendServer
}

func TestPprofDisabledByDefault(t *testing.T) {
	cfg := config.DefaultConfig()
	handler, _ := newPprofTestHandler(t, &cfg)
	response := httptest.NewRecorder()
	newAdminMux(&cfg, handler).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("disabled pprof status = %d, want 404", response.Code)
	}
}

func TestPprofEnabledOnlyOnAdminMux(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Admin.PprofEnabled = true
	cfg.RateLimit.Enabled = false
	handler, backendServer := newPprofTestHandler(t, &cfg)
	adminServer := httptest.NewServer(newAdminMux(&cfg, handler))
	defer adminServer.Close()
	proxyMux := http.NewServeMux()
	proxyMux.Handle("/", recoverMiddleware(handler))
	proxyServer := httptest.NewServer(proxyMux)
	defer proxyServer.Close()

	adminResponse, err := http.Get(adminServer.URL + "/debug/pprof/")
	if err != nil {
		t.Fatal(err)
	}
	adminBody, readErr := io.ReadAll(adminResponse.Body)
	_ = adminResponse.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if adminResponse.StatusCode != http.StatusOK || len(adminBody) == 0 {
		t.Fatalf("admin pprof response = %d %q", adminResponse.StatusCode, adminBody)
	}

	proxyResponse, err := http.Get(proxyServer.URL + "/debug/pprof/")
	if err != nil {
		t.Fatal(err)
	}
	proxyBody, readErr := io.ReadAll(proxyResponse.Body)
	_ = proxyResponse.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if proxyResponse.StatusCode != http.StatusOK || string(proxyBody) != "backend response" {
		t.Fatalf("proxy listener served pprof instead of forwarding: status=%d body=%q backend=%s",
			proxyResponse.StatusCode, proxyBody, backendServer.URL)
	}
}
