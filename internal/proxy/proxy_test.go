package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/weaztoven/loadbear/internal/backend"
	"github.com/weaztoven/loadbear/internal/balancer"
	"github.com/weaztoven/loadbear/internal/config"
	"github.com/weaztoven/loadbear/internal/metrics"
)

func testBackend(t *testing.T, rawURL string) *backend.Backend {
	t.Helper()
	b, err := backend.New(rawURL, 1)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testHandler(t *testing.T, cfg *config.Config, servers ...*httptest.Server) (*Handler, []*backend.Backend) {
	t.Helper()
	strategy, err := balancer.New(cfg.Strategy)
	if err != nil {
		t.Fatal(err)
	}
	backends := make([]*backend.Backend, 0, len(servers))
	for _, server := range servers {
		backends = append(backends, testBackend(t, server.URL))
	}
	handler := NewHandler(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), strategy, backends, metrics.New(backends))
	t.Cleanup(handler.StopBackground)
	return handler, backends
}

func testConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.RateLimit.Enabled = false
	cfg.Proxy.RetryMaxAttempts = 3
	return &cfg
}

func configWithCircuitFailureStatuses(t *testing.T, statusCodes []int, urls ...string) *config.Config {
	t.Helper()
	var data strings.Builder
	data.WriteString("strategy: round_robin\nbackends:\n")
	for _, backendURL := range urls {
		fmt.Fprintf(&data, "  - url: %q\n    weight: 1\n", backendURL)
	}
	data.WriteString("proxy:\n  circuit_failure_threshold: 2\n")
	if len(statusCodes) > 0 {
		data.WriteString("  circuit_failure_statuses: [")
		for i, status := range statusCodes {
			if i > 0 {
				data.WriteString(", ")
			}
			data.WriteString(strconv.Itoa(status))
		}
		data.WriteString("]\n")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(data.String()), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RateLimit.Enabled = false
	return cfg
}

func perform(h http.Handler, method string, headers http.Header) *httptest.ResponseRecorder {
	return performPath(h, method, "/api?q=1", headers)
}

func performPath(h http.Handler, method, path string, headers http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://proxy.test"+path, nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header = headers.Clone()
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res
}

func TestRoundRobinDistributionAcrossThreeBackends(t *testing.T) {
	var counts [3]atomic.Int32
	var responses [3]string
	servers := make([]*httptest.Server, 3)
	for i := range servers {
		index := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			counts[index].Add(1)
			responses[index] = fmt.Sprintf("backend-%d", index+1)
			fmt.Fprint(w, responses[index])
		}))
		defer servers[i].Close()
	}
	h, _ := testHandler(t, testConfig(), servers...)
	seen := make(map[string]bool)
	for range 3 {
		res := performPath(h, http.MethodGet, "/", make(http.Header))
		if res.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %q", res.Code, res.Body.String())
		}
		seen[res.Body.String()] = true
	}
	if len(seen) != 3 {
		t.Fatalf("three consecutive requests reached only %d distinct backends: %#v", len(seen), seen)
	}
	for i := range counts {
		if got := counts[i].Load(); got != 1 {
			t.Fatalf("backend %d served %d requests; want 1", i, got)
		}
	}
}

func TestNoHealthyBackendsReturns503(t *testing.T) {
	servers := make([]*httptest.Server, 3)
	for i := range servers {
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer servers[i].Close()
	}
	h, backends := testHandler(t, testConfig(), servers...)
	for i, b := range backends {
		servers[i].Close()
		b.SetHealthy(false)
	}
	res := perform(h, http.MethodGet, make(http.Header))
	assertStatusBody(t, res, http.StatusServiceUnavailable, "no healthy backend available\n")
}

func TestWebSocketUpgradePassthrough(t *testing.T) {
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Connection"), "Upgrade") ||
			!strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "upgrade headers missing", http.StatusBadRequest)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		if err := rw.Flush(); err != nil {
			return
		}
		_, _ = io.Copy(conn, rw)
	}))
	defer backendServer.Close()

	h, backends := testHandler(t, testConfig(), backendServer)
	proxyServer := httptest.NewServer(h)
	defer proxyServer.Close()
	proxyAddress := strings.TrimPrefix(proxyServer.URL, "http://")
	conn, err := net.DialTimeout("tcp", proxyAddress, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "GET /socket HTTP/1.1\r\nHost: proxy.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	request, _ := http.NewRequest(http.MethodGet, "http://proxy.test/socket", nil)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d, want 101", response.StatusCode)
	}
	if _, err := io.WriteString(conn, "hello"); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, len("hello"))
	if _, err := io.ReadFull(reader, echo); err != nil {
		t.Fatalf("read echoed bytes: %v", err)
	}
	if string(echo) != "hello" {
		t.Fatalf("echo = %q, want %q", echo, "hello")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for backends[0].ActiveConnections.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := backends[0].ActiveConnections.Load(); got != 0 {
		t.Fatalf("active connections after upgrade close = %d, want 0", got)
	}
}

func TestNormalRequestsStripHopByHopHeaders(t *testing.T) {
	received := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	h, _ := testHandler(t, testConfig(), server)
	headers := make(http.Header)
	headers.Set("Connection", "keep-alive")
	headers.Set("Keep-Alive", "timeout=5")
	headers.Set("Upgrade", "websocket")
	headers.Set("Proxy-Authorization", "secret")
	res := perform(h, http.MethodGet, headers)
	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", res.Code)
	}
	got := <-received
	for _, name := range []string{"Connection", "Keep-Alive", "Upgrade", "Proxy-Authorization"} {
		if value := got.Get(name); value != "" {
			t.Errorf("hop-by-hop header %s was forwarded as %q", name, value)
		}
	}
}

func TestUpgradeRequestsAreNotRetried(t *testing.T) {
	var fallbackHits atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackHits.Add(1)
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))
	defer fallback.Close()
	closeBeforeHeaders := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer closeBeforeHeaders.Close()
	h, _ := testHandler(t, testConfig(), fallback, closeBeforeHeaders)
	proxyServer := httptest.NewServer(h)
	defer proxyServer.Close()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(proxyServer.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprint(conn, "GET /socket HTTP/1.1\r\nHost: proxy.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("upgrade failure status = %d, want 502", response.StatusCode)
	}
	if got := fallbackHits.Load(); got != 0 {
		t.Fatalf("upgrade request retried to fallback %d times", got)
	}
}

func TestFailoverAndRecovery(t *testing.T) {
	var firstHits atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstHits.Add(1)
		fmt.Fprint(w, "first")
	}))
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "second")
	}))
	secondURL := second.URL
	second.Close()
	h, backends := testHandler(t, testConfig(), first, &httptest.Server{URL: secondURL})
	defer first.Close()
	res := perform(h, http.MethodGet, make(http.Header))
	if res.Code != http.StatusOK || res.Body.String() != "first" {
		t.Fatalf("failover response = %d %q", res.Code, res.Body.String())
	}

	addr := strings.TrimPrefix(secondURL, "http://")
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("restart backend listener: %v", err)
	}
	recovered := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "second") })}
	go func() { _ = recovered.Serve(listener) }()
	defer recovered.Close()
	backends[1].SetHealthy(true)
	res = perform(h, http.MethodGet, make(http.Header))
	if res.Code != http.StatusOK {
		t.Fatalf("recovery response status = %d, body = %q", res.Code, res.Body.String())
	}
	if res.Body.String() != "second" || firstHits.Load() == 0 {
		t.Fatalf("expected fallback then recovered backend, body=%q firstHits=%d", res.Body.String(), firstHits.Load())
	}
}

func TestRetryOnlyForIdempotentMethods(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			var fallbackHits atomic.Int32
			failing := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			failingURL := failing.URL
			failing.Close()
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fallbackHits.Add(1)
				w.WriteHeader(http.StatusAccepted)
			}))
			defer fallback.Close()
			cfg := testConfig()
			h, _ := testHandler(t, cfg, fallback, mustServerURL(t, failingURL))
			res := perform(h, method, make(http.Header))
			if method == http.MethodPost {
				if res.Code != http.StatusBadGateway {
					t.Fatalf("POST status = %d; want 502", res.Code)
				}
				if got := fallbackHits.Load(); got != 0 {
					t.Fatalf("POST retried to fallback %d times", got)
				}
				return
			}
			if res.Code != http.StatusAccepted {
				t.Fatalf("%s status = %d body %q; want retry to fallback", method, res.Code, res.Body.String())
			}
			if got := fallbackHits.Load(); got != 1 {
				t.Fatalf("%s fallback hits = %d; want 1", method, got)
			}
		})
	}
}

func TestDefaultCircuitFailureStatusesDoNotTreat429AsBreakerFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	h, backends := testHandler(t, testConfig(), server)
	for range 2 {
		if got := perform(h, http.MethodGet, make(http.Header)).Code; got != http.StatusTooManyRequests {
			t.Fatalf("response status = %d, want 429", got)
		}
	}
	if backends[0].GetCircuitState() != backend.CircuitClosed || backends[0].TotalFailures.Load() != 0 {
		t.Fatalf("default 429 behavior changed: state=%s failures=%d",
			backends[0].GetCircuitState(), backends[0].TotalFailures.Load())
	}
}

func TestConfiguredCircuitFailureStatusesOpenAndRecover(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusTooManyRequests)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer server.Close()
	cfg := configWithCircuitFailureStatuses(t, []int{http.StatusTooManyRequests}, server.URL)
	cfg.Proxy.CircuitCooldown = config.YAMLDuration(time.Hour)
	h, backends := testHandler(t, cfg, server)
	b := backends[0]
	b.CircuitFailureThreshold = cfg.Proxy.CircuitFailureThreshold
	b.CircuitCooldown = cfg.Proxy.CircuitCooldown.Duration()
	for i := 0; i < 2; i++ {
		if got := perform(h, http.MethodGet, make(http.Header)).Code; got != http.StatusTooManyRequests {
			t.Fatalf("configured failure status = %d, want 429", got)
		}
	}
	if got := b.GetCircuitState(); got != backend.CircuitOpen {
		t.Fatalf("circuit state after configured failures = %q, want open", got)
	}
	if got := b.TotalFailures.Load(); got != 2 {
		t.Fatalf("failure count = %d, want 2", got)
	}
	status.Store(http.StatusOK)
	b.CircuitOpenedAt = time.Now().Add(-2 * time.Hour)
	if got := perform(h, http.MethodGet, make(http.Header)).Code; got != http.StatusOK {
		t.Fatalf("recovery response status = %d, want 200", got)
	}
	if got := b.GetCircuitState(); got != backend.CircuitClosed {
		t.Fatalf("circuit state after recovery = %q, want closed", got)
	}
}

func TestConfiguredCircuitFailureStatusRetriesOnlyEligibleRequests(t *testing.T) {
	for _, test := range []struct {
		name       string
		method     string
		wantStatus int
		wantRetry  bool
	}{
		{name: "GET retries", method: http.MethodGet, wantStatus: http.StatusAccepted, wantRetry: true},
		{name: "POST does not retry", method: http.MethodPost, wantStatus: http.StatusTooManyRequests},
	} {
		t.Run(test.name, func(t *testing.T) {
			var fallbackHits atomic.Int32
			failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, "busy")
			}))
			defer failing.Close()
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fallbackHits.Add(1)
				w.WriteHeader(http.StatusAccepted)
			}))
			defer fallback.Close()
			cfg := configWithCircuitFailureStatuses(t, []int{http.StatusTooManyRequests}, fallback.URL, failing.URL)
			cfg.Proxy.RetryMaxAttempts = 2
			h, backends := testHandler(t, cfg, fallback, failing)
			res := perform(h, test.method, make(http.Header))
			if res.Code != test.wantStatus {
				t.Fatalf("response = %d %q, want %d", res.Code, res.Body.String(), test.wantStatus)
			}
			wantHits := int32(0)
			if test.wantRetry {
				wantHits = 1
			}
			if got := fallbackHits.Load(); got != wantHits {
				t.Fatalf("fallback hits = %d, want %d", got, wantHits)
			}
			if backends[1].TotalRequests.Load() != 1 || backends[1].TotalFailures.Load() != 1 {
				t.Fatalf("failed backend counters = requests:%d failures:%d, want 1 each",
					backends[1].TotalRequests.Load(), backends[1].TotalFailures.Load())
			}
			if test.wantRetry && backends[0].TotalRequests.Load() != 1 {
				t.Fatalf("fallback request count = %d, want 1", backends[0].TotalRequests.Load())
			}
		})
	}
}

func TestDefaultFiveHundredResponseIsCountedButNotRetried(t *testing.T) {
	var fallbackHits atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackHits.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer fallback.Close()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()
	h, backends := testHandler(t, testConfig(), fallback, failing)
	res := perform(h, http.MethodGet, make(http.Header))
	if res.Code != http.StatusInternalServerError || backends[1].TotalFailures.Load() != 1 || fallbackHits.Load() != 0 {
		t.Fatalf("default 500 behavior: status=%d failures=%d retries=%d",
			res.Code, backends[1].TotalFailures.Load(), fallbackHits.Load())
	}
}

func TestTerminalUpstreamErrorCountsOneBackendFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	serverURL := server.URL
	server.Close()
	h, backends := testHandler(t, testConfig(), &httptest.Server{URL: serverURL})
	res := perform(h, http.MethodPost, make(http.Header))
	if res.Code != http.StatusBadGateway {
		t.Fatalf("response status = %d, want 502", res.Code)
	}
	if got := backends[0].TotalFailures.Load(); got != 1 {
		t.Fatalf("backend failure count = %d, want 1", got)
	}
}

func TestRetryAfterAcceptedConnectionClosesBeforeHeaders(t *testing.T) {
	closeBeforeHeaders := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer closeBeforeHeaders.Close()
	var fallbackHits atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackHits.Add(1)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "retried")
	}))
	defer fallback.Close()
	h, _ := testHandler(t, testConfig(), fallback, closeBeforeHeaders)
	res := perform(h, http.MethodGet, make(http.Header))
	if res.Code != http.StatusAccepted || res.Body.String() != "retried" {
		t.Fatalf("retry response = %d %q", res.Code, res.Body.String())
	}
	if got := fallbackHits.Load(); got != 1 {
		t.Fatalf("fallback hits = %d; want 1", got)
	}
}

func TestRetryBackoffDelaysNextAttempt(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	failingURL := failing.URL
	failing.Close()
	fallbackReached := make(chan time.Time, 1)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackReached <- time.Now()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer fallback.Close()

	cfg := testConfig()
	cfg.Proxy.RetryBackoff = config.YAMLDuration(60 * time.Millisecond)
	h, _ := testHandler(t, cfg, fallback, mustServerURL(t, failingURL))
	start := time.Now()
	res := perform(h, http.MethodGet, make(http.Header))
	reachedAt := <-fallbackReached
	if res.Code != http.StatusAccepted {
		t.Fatalf("retry status = %d, body = %q", res.Code, res.Body.String())
	}
	if elapsed := reachedAt.Sub(start); elapsed < 40*time.Millisecond {
		t.Fatalf("retry occurred after %s; expected configured backoff", elapsed)
	}
}

func TestRequestAccessLogIncludesStatusAndRequestDetails(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	cfg := testConfig()
	strategy, err := balancer.New(cfg.Strategy)
	if err != nil {
		t.Fatal(err)
	}
	b := testBackend(t, server.URL)
	h := NewHandler(cfg, logger, strategy, []*backend.Backend{b}, metrics.New([]*backend.Backend{b}))
	t.Cleanup(h.StopBackground)

	req := httptest.NewRequest(http.MethodPost, "http://proxy.test/logged?item=1", nil)
	req.RemoteAddr = "192.0.2.44:9000"
	req.Header.Set("X-Request-ID", "request-123")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("response status = %d", recorder.Code)
	}
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
		t.Fatalf("parse access log: %v; output=%q", err, logs.String())
	}
	for key, want := range map[string]string{
		"msg":        "proxy request",
		"request_id": "request-123",
		"method":     http.MethodPost,
		"path":       "/logged?item=1",
		"client_ip":  "192.0.2.44",
		"backend":    b.URL.Host,
	} {
		if got := entry[key]; got != want {
			t.Errorf("log field %s = %#v; want %q", key, got, want)
		}
	}
	if got := entry["status"]; got != float64(http.StatusCreated) {
		t.Errorf("logged status = %#v; want %d", got, http.StatusCreated)
	}
	if got, ok := entry["latency"].(float64); !ok || got < 0 {
		t.Errorf("logged latency = %#v; want non-negative duration", entry["latency"])
	}
	if logs.String() == "" || strings.Count(strings.TrimSpace(logs.String()), "\n") != 0 {
		t.Fatalf("expected exactly one structured access log line, got %q", logs.String())
	}
	logs.Reset()
	req = httptest.NewRequest(http.MethodGet, "http://proxy.test/health", nil)
	req.RemoteAddr = "192.0.2.44:9000"
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
		t.Fatalf("parse health-path log: %v; output=%q", err, logs.String())
	}
	if got := entry["level"]; got != "DEBUG" {
		t.Fatalf("health-path log level = %#v; want DEBUG", got)
	}
}

func TestRetryBackoffStopsWhenRequestContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := waitForRetry(ctx, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForRetry error = %v; want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("cancelled retry waited %s", elapsed)
	}
}

func TestRetryDoesNotReplayNonReplayableRequestBody(t *testing.T) {
	var fallbackHits atomic.Int32
	failing := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	failingURL := failing.URL
	failing.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackHits.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer fallback.Close()
	h, _ := testHandler(t, testConfig(), fallback, mustServerURL(t, failingURL))
	req := httptest.NewRequest(http.MethodGet, "http://proxy.test/body", nil)
	req.Body = io.NopCloser(strings.NewReader("not replayable"))
	req.ContentLength = int64(len("not replayable"))
	req.GetBody = nil
	req.RemoteAddr = "192.0.2.10:1234"
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d; want 502 because request body cannot be replayed", recorder.Code)
	}
	if got := fallbackHits.Load(); got != 0 {
		t.Fatalf("non-replayable request body was retried %d times", got)
	}
}

func TestRetryReplaysBodyWhenGetBodyIsAvailable(t *testing.T) {
	var received string
	failing := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	failingURL := failing.URL
	failing.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received = string(body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer fallback.Close()
	h, _ := testHandler(t, testConfig(), fallback, mustServerURL(t, failingURL))
	req := httptest.NewRequest(http.MethodGet, "http://proxy.test/replay", nil)
	body := "replayable payload"
	req.Body = io.NopCloser(strings.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}
	req.RemoteAddr = "192.0.2.10:1234"
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusAccepted || received != body {
		t.Fatalf("retry status/body = %d/%q; want 202/%q", recorder.Code, received, body)
	}
}

func TestRetryBudgetHonorsOriginalRequestDeadline(t *testing.T) {
	var backendHits atomic.Int32
	hang := func(_ http.ResponseWriter, r *http.Request) {
		backendHits.Add(1)
		<-r.Context().Done()
	}
	first := httptest.NewServer(http.HandlerFunc(hang))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hang(w, r)
	}))
	defer second.Close()
	cfg := testConfig()
	cfg.Proxy.RetryBackoff = config.YAMLDuration(time.Second)
	h, _ := testHandler(t, cfg, first, second)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "http://proxy.test/deadline", nil).WithContext(ctx)
	req.RemoteAddr = "192.0.2.10:1234"
	start := time.Now()
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("request exceeded original deadline by too much: %s", elapsed)
	}
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d; want 504", recorder.Code)
	}
	if got := backendHits.Load(); got != 1 {
		t.Fatalf("attempts = %d; want one attempt before original deadline", got)
	}
}

func TestRetryDoesNotRetryAfterResponseHeaders(t *testing.T) {
	var fallbackHits atomic.Int32
	drop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer drop.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackHits.Add(1)
		_, _ = io.WriteString(w, "fallback")
	}))
	defer fallback.Close()
	cfg := testConfig()
	h, _ := testHandler(t, cfg, fallback, drop)
	proxyServer := httptest.NewServer(h)
	defer proxyServer.Close()

	resp, err := http.Get(proxyServer.URL + "/drop-mid")
	if err != nil {
		t.Fatalf("client did not receive committed upstream response headers: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want already-committed 200", resp.StatusCode)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr == nil {
		t.Fatalf("expected truncated response body, got %q", body)
	}
	if got := fallbackHits.Load(); got != 0 {
		t.Fatalf("proxy retried after response headers and reached fallback %d times", got)
	}
}

func TestLeastConnectionsActiveCountReleasedOnTimeoutAndCancel(t *testing.T) {
	for _, test := range []struct {
		name   string
		cancel bool
	}{
		{name: "upstream timeout"},
		{name: "client cancellation", cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			started := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				started <- struct{}{}
				<-r.Context().Done()
			}))
			defer server.Close()
			cfg := testConfig()
			cfg.Strategy = "least_connections"
			cfg.Proxy.ResponseHeaderTimeout = config.YAMLDuration(60 * time.Millisecond)
			h, backends := testHandler(t, cfg, server)
			proxyServer := httptest.NewServer(h)
			defer proxyServer.Close()
			requestContext := context.Background()
			var cancel context.CancelFunc
			if test.cancel {
				requestContext, cancel = context.WithCancel(requestContext)
				defer cancel()
			}
			req, err := http.NewRequestWithContext(requestContext, http.MethodGet, proxyServer.URL+"/wait", nil)
			if err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() {
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					_, err = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
				result <- err
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("upstream request did not start")
			}
			if test.cancel {
				cancel()
			}
			select {
			case <-result:
			case <-time.After(time.Second):
				t.Fatal("proxy request did not terminate")
			}
			deadline := time.Now().Add(time.Second)
			for backends[0].ActiveConnections.Load() != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if got := backends[0].ActiveConnections.Load(); got != 0 {
				t.Fatalf("active connection count = %d after request ended", got)
			}
		})
	}
}

func TestCancelledHalfOpenProbeCanBeAdmittedAgain(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	cfg := testConfig()
	h, backends := testHandler(t, cfg, server)
	b := backends[0]
	b.CircuitFailureThreshold = 1
	b.CircuitCooldown = time.Hour
	b.RecordFailure()
	b.CircuitOpenedAt = time.Now().Add(-2 * time.Hour)

	proxyDone := make(chan struct{})
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		close(proxyDone)
	}))
	defer proxyServer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxyServer.URL+"/cancel-probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("half-open probe did not reach backend")
	}
	cancel()
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("cancelled probe did not return")
	}
	select {
	case <-proxyDone:
	case <-time.After(time.Second):
		t.Fatal("proxy handler did not finish after cancellation")
	}
	if state := b.GetCircuitState(); state != backend.CircuitOpen {
		t.Fatalf("circuit state after cancelled probe = %q; want open", state)
	}
	b.CircuitOpenedAt = time.Now().Add(-2 * time.Hour)
	b.SetHealthy(true)
	selected := h.strategy.Select(backends, httptest.NewRequest(http.MethodGet, "/", nil))
	if selected != b || b.GetCircuitState() != backend.CircuitHalfOpen {
		t.Fatalf("probe slot was not released after cancellation: selected=%v state=%q", selected, b.GetCircuitState())
	}
	b.AbortHalfOpenProbe()
}

func TestPanickingHalfOpenProbeReleasesSlot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	h, backends := testHandler(t, testConfig(), server)
	b := backends[0]
	b.CircuitFailureThreshold = 1
	b.CircuitCooldown = time.Hour
	b.RecordFailure()
	b.CircuitOpenedAt = time.Now().Add(-2 * time.Hour)
	b.URL = nil
	h.logger = nil

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected reverse proxy to panic with a nil backend URL")
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	if state := b.GetCircuitState(); state != backend.CircuitOpen {
		t.Fatalf("circuit state after panicking probe = %q; want open", state)
	}
	b.URL, _ = url.Parse(server.URL)
	b.CircuitOpenedAt = time.Now().Add(-2 * time.Hour)
	if !b.AllowRequest() {
		t.Fatal("probe slot remained occupied after panic")
	}
	b.AbortHalfOpenProbe()
}

func TestLeastConnectionsActiveCountReleasedOnSuccessAndUpstreamError(t *testing.T) {
	for _, test := range []struct {
		name        string
		closeServer bool
		wantStatus  int
	}{
		{name: "success", wantStatus: http.StatusOK},
		{name: "upstream error", closeServer: true, wantStatus: http.StatusBadGateway},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			serverURL := server.URL
			if test.closeServer {
				server.Close()
			} else {
				defer server.Close()
			}
			cfg := testConfig()
			cfg.Strategy = "least_connections"
			h, backends := testHandler(t, cfg, mustServerURL(t, serverURL))
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "http://proxy.test/accounting", nil)
			h.ServeHTTP(recorder, req)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d; want %d", recorder.Code, test.wantStatus)
			}
			if got := backends[0].ActiveConnections.Load(); got != 0 {
				t.Fatalf("active connection count = %d after request", got)
			}
		})
	}
}

func TestLeastConnectionsActiveCountReleasedOnPanic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "body")
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.Strategy = "least_connections"
	h, backends := testHandler(t, cfg, server)
	defer func() {
		if recover() == nil {
			t.Fatal("expected writer panic")
		}
		if got := backends[0].ActiveConnections.Load(); got != 0 {
			t.Fatalf("active connection count = %d after panic", got)
		}
	}()
	h.ServeHTTP(panicResponseWriter{header: make(http.Header)}, httptest.NewRequest(http.MethodGet, "http://proxy.test/panic", nil))
}

type panicResponseWriter struct {
	header http.Header
}

func (w panicResponseWriter) Header() http.Header { return w.header }
func (panicResponseWriter) WriteHeader(int)       {}
func (panicResponseWriter) Write([]byte) (int, error) {
	panic("writer failure")
}

func mustServerURL(t *testing.T, raw string) *httptest.Server {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &httptest.Server{URL: parsed.String()}
}

func TestCircuitBreakerTransitions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer server.Close()
	h, backends := testHandler(t, testConfig(), server)
	b := backends[0]
	b.CircuitFailureThreshold = 1
	if got := string(b.CircuitState); got != "closed" {
		t.Fatalf("initial circuit state = %q", got)
	}
	b.RecordFailure()
	if got := string(b.CircuitState); got != "open" {
		t.Fatalf("after failure state = %q", got)
	}
	b.CircuitCooldown = time.Nanosecond
	b.CircuitOpenedAt = time.Now().Add(-time.Second)
	if selected := h.strategy.Select(backends, httptest.NewRequest(http.MethodGet, "/", nil)); selected != b ||
		b.GetCircuitState() != backend.CircuitHalfOpen {
		t.Fatalf("strategy did not admit half-open probe: selected=%v state=%q", selected, b.GetCircuitState())
	}
	b.RecordSuccess()
	if b.GetCircuitState() != backend.CircuitClosed {
		t.Fatalf("successful probe did not close circuit: %q", b.GetCircuitState())
	}
}

func TestProxyErrorStatusesAndPlainTextBodies(t *testing.T) {
	t.Run("bad gateway", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		server.Close()
		h, _ := testHandler(t, testConfig(), server)
		assertStatusBody(t, perform(h, http.MethodGet, make(http.Header)), http.StatusBadGateway, "Bad Gateway\n")
	})
	t.Run("service unavailable", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		defer server.Close()
		h, backends := testHandler(t, testConfig(), server)
		backends[0].SetHealthy(false)
		assertStatusBody(t, perform(h, http.MethodGet, make(http.Header)), http.StatusServiceUnavailable, "no healthy backend available\n")
	})
	t.Run("gateway timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(100 * time.Millisecond)
			fmt.Fprint(w, "late")
		}))
		defer server.Close()
		cfg := testConfig()
		cfg.Proxy.ResponseHeaderTimeout = config.YAMLDuration(10 * time.Millisecond)
		h, _ := testHandler(t, cfg, server)
		assertStatusBody(t, perform(h, http.MethodGet, make(http.Header)), http.StatusGatewayTimeout, "Gateway Timeout\n")
	})
}

func assertStatusBody(t *testing.T, res *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if res.Code != status || res.Body.String() != body {
		t.Fatalf("got status/body %d/%q; want %d/%q", res.Code, res.Body.String(), status, body)
	}
	if got := res.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("Content-Type = %q; want plain text", got)
	}
}

func TestRequestIDAndForwardedHeaders(t *testing.T) {
	var received http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Clone()
		fmt.Fprint(w, "ok")
	}))
	defer server.Close()
	h, _ := testHandler(t, testConfig(), server)

	res := perform(h, http.MethodGet, make(http.Header))
	if got := res.Header().Get("X-Request-ID"); got == "" {
		t.Fatal("request ID was not generated")
	}
	if got := received.Get("X-Request-ID"); got != res.Header().Get("X-Request-ID") {
		t.Fatalf("backend request ID = %q; response ID = %q", got, res.Header().Get("X-Request-ID"))
	}
	if received.Get("X-Forwarded-For") != "192.0.2.10" ||
		received.Get("X-Forwarded-Proto") != "http" ||
		received.Get("X-Forwarded-Host") != "proxy.test" {
		t.Fatalf("unexpected forwarded headers: %#v", received)
	}

	headers := make(http.Header)
	headers.Set("X-Request-ID", "caller-id")
	res = perform(h, http.MethodGet, headers)
	if got := res.Header().Get("X-Request-ID"); got != "caller-id" {
		t.Fatalf("preserved request ID = %q", got)
	}
	if got := received.Get("X-Request-ID"); got != "caller-id" {
		t.Fatalf("backend request ID = %q; want preserved caller ID", got)
	}
}

func TestRateLimitResponseAndTrustedForwarding(t *testing.T) {
	var received string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Get("X-Forwarded-For")
		fmt.Fprint(w, "ok")
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.RateLimit.Enabled = true
	cfg.RateLimit.Rate = 1
	cfg.RateLimit.Burst = 1
	cfg.RateLimit.Window = config.YAMLDuration(time.Minute)
	h, _ := testHandler(t, cfg, server)
	headers := make(http.Header)
	headers.Set("X-Forwarded-For", "198.51.100.10")

	res := perform(h, http.MethodGet, headers)
	if res.Code != http.StatusOK || received != "192.0.2.10" {
		t.Fatalf("untrusted proxy request = %d, XFF %q", res.Code, received)
	}
	res = perform(h, http.MethodGet, headers)
	if res.Code != http.StatusTooManyRequests || res.Header().Get("Retry-After") != "60" {
		t.Fatalf("rate-limited response = %d, Retry-After %q", res.Code, res.Header().Get("Retry-After"))
	}
	if !strings.Contains(h.metrics.PrometheusText(), "loadbear_rate_limited_total 1") {
		t.Fatal("rate-limited request was not counted in metrics")
	}

	cfg = testConfig()
	cfg.RateLimit.TrustedProxies = []string{"192.0.2.0/24"}
	h, _ = testHandler(t, cfg, server)
	res = perform(h, http.MethodGet, headers)
	if res.Code != http.StatusOK || received != "198.51.100.10, 192.0.2.10" {
		t.Fatalf("trusted proxy request = %d, XFF %q", res.Code, received)
	}
	cfg.RateLimit.TrustedProxies = []string{"203.0.113.0/24"}
	h, _ = testHandler(t, cfg, server)
	res = perform(h, http.MethodGet, headers)
	if res.Code != http.StatusOK || received != "192.0.2.10" {
		t.Fatalf("untrusted direct peer spoofed XFF: status=%d XFF=%q", res.Code, received)
	}
}

func TestHeaderAndURLHelpers(t *testing.T) {
	headers := http.Header{"Connection": {"X-Remove"}, "X-Remove": {"value"}, "Keep-Alive": {"timeout=5"}, "Upgrade": {"websocket"}, "X-Keep": {"value"}}
	stripHopByHopHeaders(headers)
	for _, key := range []string{"Connection", "Keep-Alive", "Upgrade", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding"} {
		if headers.Get(key) != "" {
			t.Errorf("%s was not removed", key)
		}
	}
	if headers.Get("X-Remove") != "value" || headers.Get("X-Keep") != "value" {
		t.Fatal("unrelated headers were removed")
	}
	for _, test := range []struct{ base, target, want string }{
		{"/base/", "/target", "/base/target"},
		{"/base", "target", "/base/target"},
		{"/base", "/target", "/base/target"},
		{"", "/target", "/target"},
		{"/base", "", "/base"},
	} {
		if got := joinURLPath(test.base, test.target); got != test.want {
			t.Errorf("joinURLPath(%q,%q) = %q; want %q", test.base, test.target, got, test.want)
		}
	}
}

func TestClientIPAndRequestIDHelpers(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil)
	req.RemoteAddr = "192.0.2.3:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.8, 192.0.2.1")
	if got := clientIP(req, nil); got != "192.0.2.3" {
		t.Fatalf("untrusted client IP = %q", got)
	}
	if got := clientIP(req, []string{"192.0.2.0/24"}); got != "198.51.100.8" {
		t.Fatalf("trusted client IP = %q", got)
	}
	if got := clientIP(req, []string{"192.0.2.4"}); got != "192.0.2.3" {
		t.Fatalf("spoofed client IP = %q", got)
	}
	if got := clientIPFromHeader(req, nil); got != "192.0.2.3" {
		t.Fatalf("untrusted forwarded IP = %q", got)
	}
	if got := requestIDFromHeader(req); got != "" {
		t.Fatalf("unexpected request ID %q", got)
	}
	if got := newRequestID(); len(got) != 32 {
		t.Fatalf("generated request ID length = %d", len(got))
	}
	if got := schemeFromRequest(req); got != "http" {
		t.Fatalf("scheme = %q", got)
	}
	req.TLS = &tls.ConnectionState{}
	if got := schemeFromRequest(req); got != "https" {
		t.Fatalf("TLS scheme = %q", got)
	}
}

func TestStatusAndEndpointMethods(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }))
	defer server.Close()
	h, backends := testHandler(t, testConfig(), server)
	if got := len(h.Status()); got != 1 {
		t.Fatalf("status entry count = %d", got)
	}
	statusRecorder := httptest.NewRecorder()
	h.StatusEndpoint(statusRecorder, httptest.NewRequest(http.MethodGet, "/status", nil))
	if statusRecorder.Code != http.StatusOK || !strings.Contains(statusRecorder.Body.String(), server.URL) {
		t.Fatalf("status endpoint = %d %q", statusRecorder.Code, statusRecorder.Body.String())
	}
	metricsRecorder := httptest.NewRecorder()
	h.MetricsEndpoint(metricsRecorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metricsRecorder.Code != http.StatusOK || !strings.Contains(metricsRecorder.Body.String(), "loadbear_") {
		t.Fatalf("metrics endpoint = %d %q", metricsRecorder.Code, metricsRecorder.Body.String())
	}
	h.SetMetrics(nil)
	h.SetMetrics(metrics.New(backends))
	if h.MetricsText() == "" || h.Transport() == nil || h.Config() == nil || h.StrategyName() != "round_robin" {
		t.Fatal("handler accessors returned unexpected values")
	}
	if got := h.BuilderURL(backends[0].URL); got != server.URL {
		t.Fatalf("BuilderURL = %q", got)
	}
	if got := h.BuilderURL(nil); got != "" {
		t.Fatalf("BuilderURL(nil) = %q", got)
	}
	h.SetStrategy(nil)
	if h.StrategyName() != "" {
		t.Fatal("nil strategy name should be empty")
	}
	h.SetBackends(nil)
	if len(h.CurrentBackends()) != 0 {
		t.Fatal("SetBackends did not update current backends")
	}
	if _, err := h.RoundTrip(httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1", nil)); err == nil {
		t.Fatal("RoundTrip to closed port unexpectedly succeeded")
	}
}
