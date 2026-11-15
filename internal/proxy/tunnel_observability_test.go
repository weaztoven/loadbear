package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/weaztoven/loadbear/internal/backend"
	"github.com/weaztoven/loadbear/internal/config"
)

type tunnelTestFixture struct {
	handler       *Handler
	backend       *backend.Backend
	proxyServer   *httptest.Server
	clientConn    net.Conn
	upstreamConns chan net.Conn
}

func newTunnelTestFixture(t *testing.T, cfg *config.Config) *tunnelTestFixture {
	t.Helper()
	upstreamConns := make(chan net.Conn, 4)
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
		_, _ = fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		if err := rw.Flush(); err != nil {
			_ = conn.Close()
			return
		}
		upstreamConns <- conn
		_, _ = io.Copy(conn, rw)
		_ = conn.Close()
	}))
	t.Cleanup(backendServer.Close)
	h, backends := testHandler(t, cfg, backendServer)
	proxyServer := httptest.NewServer(h)
	t.Cleanup(proxyServer.Close)
	fixture := &tunnelTestFixture{
		handler:       h,
		backend:       backends[0],
		proxyServer:   proxyServer,
		upstreamConns: upstreamConns,
	}
	fixture.clientConn = fixture.open(t)
	return fixture
}

func (f *tunnelTestFixture) open(t *testing.T) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(f.proxyServer.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := fmt.Fprint(conn, "GET /socket HTTP/1.1\r\nHost: proxy.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	req, _ := http.NewRequest(http.MethodGet, "http://proxy.test/socket", nil)
	response, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade response status = %d, want 101", response.StatusCode)
	}
	return conn
}

func waitForTunnelCleanup(t *testing.T, h *Handler, b *backend.Backend) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		h.tunnelMu.Lock()
		done := len(h.tunnels) == 0 && h.pendingUpgrades == 0
		tunnelCount := len(h.tunnels)
		changed := h.tunnelChanged
		h.tunnelMu.Unlock()
		if done && b.ActiveConnections.Load() == 0 {
			return
		}
		select {
		case <-changed:
		case <-timer.C:
			t.Fatalf("tunnel cleanup timed out: active=%d tunnels=%d", b.ActiveConnections.Load(), tunnelCount)
		}
	}
}

func closeUpstreamCleanly(t *testing.T, f *tunnelTestFixture) {
	t.Helper()
	upstream := <-f.upstreamConns
	if err := upstream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.clientConn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := f.clientConn.Read(make([]byte, 1))
	if err != io.EOF {
		t.Fatalf("client read after upstream close = %v, want EOF", err)
	}
	if err := f.clientConn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	_ = f.clientConn.Close()
}

func waitForClientEnd(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := conn.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("client connection remained readable after upstream reset")
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func resetUpstream(t *testing.T, f *tunnelTestFixture) {
	t.Helper()
	upstream := <-f.upstreamConns
	tcpConn, ok := upstream.(*net.TCPConn)
	if !ok {
		t.Fatalf("upstream connection type = %T, want *net.TCPConn", upstream)
	}
	if err := tcpConn.SetLinger(0); err != nil {
		t.Fatal(err)
	}
	_ = tcpConn.Close()
	waitForClientEnd(t, f.clientConn)
	_ = f.clientConn.Close()
}

func TestTunnelCloseReasonsAndActiveGauges(t *testing.T) {
	for _, test := range []struct {
		name   string
		reason string
		close  func(*testing.T, *tunnelTestFixture)
	}{
		{
			name:   "client closed cleanly",
			reason: "client_closed",
			close: func(t *testing.T, f *tunnelTestFixture) {
				t.Helper()
				if err := f.clientConn.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "upstream closed cleanly",
			reason: "upstream_closed",
			close: func(t *testing.T, f *tunnelTestFixture) {
				closeUpstreamCleanly(t, f)
			},
		},
		{
			name:   "upstream reset",
			reason: "upstream_error",
			close: func(t *testing.T, f *tunnelTestFixture) {
				t.Helper()
				upstream := <-f.upstreamConns
				tcpConn, ok := upstream.(*net.TCPConn)
				if !ok {
					t.Fatalf("upstream connection type = %T, want *net.TCPConn", upstream)
				}
				if err := tcpConn.SetLinger(0); err != nil {
					t.Fatal(err)
				}
				_ = tcpConn.Close()
			},
		},
		{
			name:   "client reset is unknown",
			reason: "unknown",
			close: func(t *testing.T, f *tunnelTestFixture) {
				t.Helper()
				tcpConn, ok := f.clientConn.(*net.TCPConn)
				if !ok {
					t.Fatalf("client connection type = %T, want *net.TCPConn", f.clientConn)
				}
				if err := tcpConn.SetLinger(0); err != nil {
					t.Fatal(err)
				}
				_ = tcpConn.Close()
			},
		},
		{
			name:   "shutdown force close",
			reason: "shutdown_forced",
			close: func(t *testing.T, f *tunnelTestFixture) {
				t.Helper()
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if got := f.handler.Shutdown(ctx); got != 1 {
					t.Fatalf("forced tunnel count = %d, want 1", got)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.RateLimit.Enabled = false
			fixture := newTunnelTestFixture(t, cfg)
			before := fixture.handler.MetricsText()
			if !strings.Contains(before, "loadbear_active_tunnels 1") ||
				!strings.Contains(before, fmt.Sprintf(`loadbear_backend_active_tunnels{backend="%s"} 1`, fixture.backend.URL)) {
				t.Fatalf("active tunnel gauges not incremented:\n%s", before)
			}
			test.close(t, fixture)
			waitForTunnelCleanup(t, fixture.handler, fixture.backend)
			after := fixture.handler.MetricsText()
			for _, line := range []string{
				"loadbear_active_tunnels 0",
				fmt.Sprintf(`loadbear_backend_active_tunnels{backend="%s"} 0`, fixture.backend.URL),
				fmt.Sprintf(`loadbear_tunnel_closed_total{backend="%s",reason="%s"} 1`, fixture.backend.URL, test.reason),
			} {
				if !strings.Contains(after, line) {
					t.Errorf("metrics missing %q:\n%s", line, after)
				}
			}
		})
	}
}

func TestTunnelErrorCircuitBreakerIsOptInAndCountsRepeatedUpstreamErrors(t *testing.T) {
	t.Run("disabled default leaves breaker unchanged", func(t *testing.T) {
		cfg := testConfig()
		cfg.RateLimit.Enabled = false
		fixture := newTunnelTestFixture(t, cfg)
		closeUpstreamCleanly(t, fixture)
		waitForTunnelCleanup(t, fixture.handler, fixture.backend)
		if fixture.backend.TotalFailures.Load() != 0 || fixture.backend.GetCircuitState() != backend.CircuitClosed {
			t.Fatalf("disabled tunnel-drop feature changed breaker: failures=%d state=%s",
				fixture.backend.TotalFailures.Load(), fixture.backend.GetCircuitState())
		}
	})

	t.Run("early drops accumulate to threshold", func(t *testing.T) {
		cfg := testConfig()
		cfg.RateLimit.Enabled = false
		cfg.Proxy.CircuitCountTunnelErrors = true
		fixture := newTunnelTestFixture(t, cfg)
		b := fixture.backend
		b.CircuitFailureThreshold = 2
		for i := 0; i < 2; i++ {
			if i > 0 {
				fixture.clientConn = fixture.open(t)
			}
			resetUpstream(t, fixture)
			waitForTunnelCleanup(t, fixture.handler, b)
			if i == 0 && b.GetCircuitState() != backend.CircuitClosed {
				t.Fatalf("circuit opened after one drop: %s", b.GetCircuitState())
			}
		}
		if got := b.GetCircuitState(); got != backend.CircuitOpen || b.TotalFailures.Load() != 2 {
			t.Fatalf("circuit after threshold drops: state=%s failures=%d, want open/2",
				got, b.TotalFailures.Load())
		}
		if !b.IsHealthy() {
			t.Fatal("tunnel drops alone should not mark the backend unhealthy")
		}
	})
}

func TestTunnelErrorCircuitBreakerIgnoresClientShutdownAndCleanUpstreamClose(t *testing.T) {
	for _, test := range []struct {
		name  string
		close func(*testing.T, *tunnelTestFixture)
	}{
		{
			name: "client close",
			close: func(t *testing.T, f *tunnelTestFixture) {
				t.Helper()
				_ = f.clientConn.Close()
			},
		},
		{
			name: "shutdown force close",
			close: func(t *testing.T, f *tunnelTestFixture) {
				t.Helper()
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if got := f.handler.Shutdown(ctx); got != 1 {
					t.Fatalf("forced tunnel count = %d, want 1", got)
				}
			},
		},
		{
			name: "early clean upstream close",
			close: func(t *testing.T, f *tunnelTestFixture) {
				closeUpstreamCleanly(t, f)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.RateLimit.Enabled = false
			cfg.Proxy.CircuitCountTunnelErrors = true
			fixture := newTunnelTestFixture(t, cfg)
			fixture.backend.CircuitFailureThreshold = 1
			test.close(t, fixture)
			waitForTunnelCleanup(t, fixture.handler, fixture.backend)
			if got := fixture.backend.TotalFailures.Load(); got != 0 ||
				fixture.backend.GetCircuitState() != backend.CircuitClosed {
				t.Fatalf("non-counted tunnel close changed breaker: failures=%d state=%s\n%s",
					got, fixture.backend.GetCircuitState(), fixture.handler.MetricsText())
			}
		})
	}
}

func TestClientInitiatedWebSocketCloseDoesNotCountAsTunnelError(t *testing.T) {
	const threshold = 2
	closeFrames := make(chan struct{}, threshold+1)
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		if err := rw.Flush(); err != nil {
			_ = conn.Close()
			return
		}
		frame := make([]byte, 8)
		if _, err := io.ReadFull(rw, frame); err != nil ||
			frame[0] != 0x88 || frame[1] != 0x82 || frame[6] != 0x02 || frame[7] != 0xea {
			_ = conn.Close()
			return
		}
		if _, err := conn.Write([]byte{0x88, 0x02, 0x03, 0xe8}); err != nil {
			_ = conn.Close()
			return
		}
		_ = conn.Close()
		closeFrames <- struct{}{}
	}))
	t.Cleanup(backendServer.Close)

	cfg := testConfig()
	cfg.RateLimit.Enabled = false
	cfg.Proxy.CircuitCountTunnelErrors = true
	handler, backends := testHandler(t, cfg, backendServer)
	b := backends[0]
	b.CircuitFailureThreshold = threshold
	proxyServer := httptest.NewServer(handler)
	t.Cleanup(proxyServer.Close)
	fixture := &tunnelTestFixture{handler: handler, backend: b, proxyServer: proxyServer}

	for i := 0; i < threshold+1; i++ {
		client := fixture.open(t)
		fixture.clientConn = client
		if _, err := client.Write([]byte{0x88, 0x82, 0x01, 0x02, 0x03, 0x04, 0x02, 0xea}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-closeFrames:
		case <-time.After(time.Second):
			t.Fatal("backend did not complete the WebSocket close handshake within one second")
		}
		if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		responseFrame := make([]byte, 4)
		if _, err := io.ReadFull(client, responseFrame); err != nil {
			t.Fatalf("read backend close frame: %v", err)
		}
		if string(responseFrame) != string([]byte{0x88, 0x02, 0x03, 0xe8}) {
			t.Fatalf("backend close frame = %v", responseFrame)
		}
		_, err := client.Read(make([]byte, 1))
		if err != io.EOF {
			t.Fatalf("read after backend close frame = %v, want EOF", err)
		}
		_ = client.Close()
		waitForTunnelCleanup(t, handler, b)
	}

	if got := b.TotalFailures.Load(); got != 0 || b.GetCircuitState() != backend.CircuitClosed {
		t.Fatalf("client-initiated clean close changed breaker: failures=%d state=%s",
			got, b.GetCircuitState())
	}
	closeMetric := fmt.Sprintf(
		`loadbear_tunnel_closed_total{backend="%s",reason="upstream_closed"} %d`,
		b.URL, threshold+1,
	)
	if !strings.Contains(handler.MetricsText(), closeMetric) {
		t.Fatalf("clean close reason was not retained in metrics:\n%s", handler.MetricsText())
	}
	if !fixture.backend.IsHealthy() {
		t.Fatal("client-initiated clean close marked the backend unhealthy")
	}
}
