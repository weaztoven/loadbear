package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/weaztoven/loadbear/internal/backend"
	"github.com/weaztoven/loadbear/internal/balancer"
	"github.com/weaztoven/loadbear/internal/config"
	"github.com/weaztoven/loadbear/internal/metrics"
	"github.com/weaztoven/loadbear/internal/proxy"
)

func startUpgradeProxy(t *testing.T, logOutput io.Writer) (*http.Server, *proxy.Handler, *backend.Backend, net.Conn) {
	t.Helper()
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/normal" {
			_, _ = io.WriteString(w, "normal response")
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		if rw.Flush() != nil {
			return
		}
		_, _ = io.Copy(conn, rw)
	}))
	t.Cleanup(backendServer.Close)
	cfg := config.DefaultConfig()
	cfg.RateLimit.Enabled = false
	b, err := backend.New(backendServer.URL, 1)
	if err != nil {
		t.Fatal(err)
	}
	strategy, err := balancer.New(cfg.Strategy)
	if err != nil {
		t.Fatal(err)
	}
	if logOutput == nil {
		logOutput = io.Discard
	}
	handler := proxy.NewHandler(&cfg, slog.New(slog.NewTextHandler(logOutput, nil)), strategy,
		[]*backend.Backend{b}, metrics.New([]*backend.Backend{b}))
	t.Cleanup(handler.StopBackground)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, Addr: listener.Addr().String()}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()

	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := fmt.Fprint(conn, "GET /socket HTTP/1.1\r\nHost: proxy.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://proxy.test/socket", nil)
	response, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade response status = %d, want 101", response.StatusCode)
	}
	return server, handler, b, conn
}

func TestShutdownWaitsForUpgradedConnectionToClose(t *testing.T) {
	server, handler, b, conn := startUpgradeProxy(t, nil)
	admin := &http.Server{}
	done := make(chan struct{})
	go func() {
		shutdownServer(server, admin, handler, 500*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("shutdown returned before the upgraded connection closed")
	case <-time.After(30 * time.Millisecond):
	}
	_ = conn.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after the upgraded connection closed")
	}
	if got := b.ActiveConnections.Load(); got != 0 {
		t.Fatalf("active backend connections after shutdown = %d, want 0", got)
	}
}

func TestShutdownForceClosesUpgradedConnectionAfterGrace(t *testing.T) {
	var logOutput bytes.Buffer
	server, handler, b, conn := startUpgradeProxy(t, &logOutput)
	admin := &http.Server{}
	grace := 75 * time.Millisecond
	start := time.Now()
	shutdownServer(server, admin, handler, grace)
	if elapsed := time.Since(start); elapsed < grace {
		t.Fatalf("shutdown returned after %s, before grace period %s", elapsed, grace)
	}
	if got := b.ActiveConnections.Load(); got != 0 {
		t.Fatalf("active backend connections after forced close = %d, want 0", got)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var data [1]byte
	if _, err := conn.Read(data[:]); err == nil {
		t.Fatal("upgraded client connection remained open after shutdown")
	}
	if !strings.Contains(logOutput.String(), "proxy request") {
		t.Fatal("access log was not emitted before shutdown returned")
	}
	if !strings.Contains(logOutput.String(), "forced closed upgraded tunnels") {
		t.Fatal("forced-close count was not logged")
	}
}

func TestNormalRequestStillWorksWithTunnelTracking(t *testing.T) {
	server, handler, _, conn := startUpgradeProxy(t, nil)
	defer conn.Close()
	response, err := http.Get("http://" + server.Addr + "/normal")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "normal response" {
		t.Fatalf("normal response = %d %q", response.StatusCode, body)
	}
	handler.StopBackground()
}
