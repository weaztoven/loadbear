package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/weaztoven/loadbear/internal/backend"
	"github.com/weaztoven/loadbear/internal/balancer"
	"github.com/weaztoven/loadbear/internal/config"
	"github.com/weaztoven/loadbear/internal/metrics"
	"github.com/weaztoven/loadbear/internal/proxy"
)

func BenchmarkProxyProfile(b *testing.B) {
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"backend":"profile","path":%q}`, r.URL.Path)
	}))
	defer backendServer.Close()
	cfg := config.DefaultConfig()
	cfg.RateLimit.Enabled = false
	backendItem, err := backend.New(backendServer.URL, 1)
	if err != nil {
		b.Fatal(err)
	}
	strategy, err := balancer.New(cfg.Strategy)
	if err != nil {
		b.Fatal(err)
	}
	handler := proxy.NewHandler(&cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), strategy,
		[]*backend.Backend{backendItem}, metrics.New([]*backend.Backend{backendItem}))
	defer handler.StopBackground()
	proxyServer := httptest.NewServer(recoverMiddleware(handler))
	defer proxyServer.Close()
	client := &http.Client{}

	b.ReportAllocs()
	b.Logf("profile environment: OS=%s Go=%s CPUs=%d", runtime.GOOS, runtime.Version(), runtime.NumCPU())
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			response, err := client.Get(proxyServer.URL + "/profile")
			if err != nil {
				b.Error(err)
				continue
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				b.Errorf("proxy status = %d, want 200", response.StatusCode)
			}
		}
	})
}
