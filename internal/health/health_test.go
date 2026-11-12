package health

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/weaztoven/loadbear/internal/backend"
)

func TestHealthThresholdTransitions(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
		fmt.Fprintln(w, "health")
	}))
	defer server.Close()
	b, err := backend.New(server.URL, 1)
	if err != nil {
		t.Fatal(err)
	}
	checker := New([]*backend.Backend{b}, "/health", time.Hour, time.Second, 2, 2,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer checker.Stop()

	if !checker.check(b) || !b.IsHealthy() {
		t.Fatal("healthy backend was not kept healthy")
	}
	status.Store(http.StatusServiceUnavailable)
	checker.check(b)
	if !b.IsHealthy() {
		t.Fatal("backend went unhealthy before reaching unhealthy threshold")
	}
	checker.check(b)
	if b.IsHealthy() {
		t.Fatal("backend remained healthy after unhealthy threshold")
	}
	status.Store(http.StatusOK)
	checker.check(b)
	if b.IsHealthy() {
		t.Fatal("backend recovered before reaching healthy threshold")
	}
	checker.check(b)
	if !b.IsHealthy() {
		t.Fatal("backend did not recover after healthy threshold")
	}
	if checker.check(nil) {
		t.Fatal("nil backend check returned healthy")
	}
}

func TestHealthCheckErrorAndPathHandling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ready" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	b, err := backend.New(server.URL+"/", 1)
	if err != nil {
		t.Fatal(err)
	}
	checker := New([]*backend.Backend{b}, "/ready", time.Hour, time.Second, 1, 1,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !checker.check(b) {
		t.Fatal("expected health check to normalize slash and use configured path")
	}
	server.Close()
	if checker.check(b) {
		t.Fatal("closed backend returned healthy")
	}
	if b.IsHealthy() {
		t.Fatal("closed backend was not marked unhealthy")
	}
	checker.Stop()
}

func TestCheckerStopsCleanly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	b, err := backend.New(server.URL, 1)
	if err != nil {
		t.Fatal(err)
	}
	checker := New([]*backend.Backend{b}, "/health", time.Millisecond, time.Second, 1, 1,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	checker.Start()
	checker.Stop()
	select {
	case <-checker.done:
	default:
		t.Fatal("checker goroutine did not stop")
	}
	checker.Stop()
}

func TestCheckerStopBeforeStart(t *testing.T) {
	checker := New(nil, "/health", 0, time.Second, 1, 1, slog.Default())
	checker.Stop()
	select {
	case <-checker.done:
	default:
		t.Fatal("stop before start did not finish")
	}
	checker.Start()
}
