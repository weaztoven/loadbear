package proxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/weaztoven/loadbear/internal/backend"
	"github.com/weaztoven/loadbear/internal/balancer"
	"github.com/weaztoven/loadbear/internal/config"
	"github.com/weaztoven/loadbear/internal/metrics"
	"github.com/weaztoven/loadbear/internal/ratelimit"
)

var errResponseRetryHandled = errors.New("configured failure response retry handled")

type Handler struct {
	mu          sync.RWMutex
	config      *config.Config
	logger      *slog.Logger
	backends    []*backend.Backend
	strategy    balancer.Strategy
	transport   *http.Transport
	bufferPool  *reverseProxyBufferPool
	metrics     *metrics.Metrics
	rateLimiter *ratelimit.RateLimiter

	tunnelMu        sync.Mutex
	tunnels         map[net.Conn]*tunnelLifecycle
	tunnelClosing   bool
	tunnelChanged   chan struct{}
	pendingUpgrades int
}

type tunnelLifecycle struct {
	backend *backend.Backend
	started time.Time
	mu      sync.Mutex
	reason  string
	tracked bool
}

func (t *tunnelLifecycle) markUpstreamError() {
	t.markReason(metrics.TunnelReasonUpstreamError)
}

func (t *tunnelLifecycle) markReason(reason string) {
	t.mu.Lock()
	if t.reason == "" {
		t.reason = reason
	}
	t.mu.Unlock()
}

func (t *tunnelLifecycle) markShutdownForced() {
	t.mu.Lock()
	t.reason = metrics.TunnelReasonShutdownForced
	t.mu.Unlock()
}

func (t *tunnelLifecycle) closeReason() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reason == "" {
		return metrics.TunnelReasonUnknown
	}
	return t.reason
}

type observedTunnelBody struct {
	io.ReadWriteCloser
	tunnel *tunnelLifecycle
}

type observedTunnelCloseWriter struct {
	*observedTunnelBody
	closeWriter interface{ CloseWrite() error }
}

func (b *observedTunnelCloseWriter) CloseWrite() error {
	return b.closeWriter.CloseWrite()
}

func observeTunnelBody(body io.ReadWriteCloser, tunnel *tunnelLifecycle) io.ReadWriteCloser {
	observed := &observedTunnelBody{ReadWriteCloser: body, tunnel: tunnel}
	if closeWriter, ok := body.(interface{ CloseWrite() error }); ok {
		return &observedTunnelCloseWriter{observedTunnelBody: observed, closeWriter: closeWriter}
	}
	return observed
}

func (b *observedTunnelBody) Read(p []byte) (int, error) {
	n, err := b.ReadWriteCloser.Read(p)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		b.tunnel.markReason(metrics.TunnelReasonUpstreamClosed)
	} else if err != nil {
		b.tunnel.markUpstreamError()
	}
	return n, err
}

func (b *observedTunnelBody) Write(p []byte) (int, error) {
	n, err := b.ReadWriteCloser.Write(p)
	if err != nil {
		b.tunnel.markUpstreamError()
	}
	return n, err
}

type observedClientConn struct {
	net.Conn
	tunnel *tunnelLifecycle
}

func (c *observedClientConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if errors.Is(err, io.EOF) {
		c.tunnel.markReason(metrics.TunnelReasonClientClosed)
	} else if err != nil {
		c.tunnel.markReason(metrics.TunnelReasonUnknown)
	}
	return n, err
}

func (c *observedClientConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err != nil {
		c.tunnel.markReason(metrics.TunnelReasonUnknown)
	}
	return n, err
}

type observedClientCloseWriter struct {
	*observedClientConn
	closeWriter interface{ CloseWrite() error }
}

func (c *observedClientCloseWriter) CloseWrite() error {
	return c.closeWriter.CloseWrite()
}

func observeClientConn(conn net.Conn, tunnel *tunnelLifecycle) net.Conn {
	observed := &observedClientConn{Conn: conn, tunnel: tunnel}
	if closeWriter, ok := conn.(interface{ CloseWrite() error }); ok {
		return &observedClientCloseWriter{observedClientConn: observed, closeWriter: closeWriter}
	}
	return observed
}

type reverseProxyBufferPool struct {
	pool sync.Pool
}

func (p *reverseProxyBufferPool) Get() []byte {
	if v := p.pool.Get(); v != nil {
		return *(v.(*[]byte))
	}
	return make([]byte, 32*1024)
}

func (p *reverseProxyBufferPool) Put(buffer []byte) {
	p.pool.Put(&buffer)
}

func NewHandler(cfg *config.Config, logger *slog.Logger, strategy balancer.Strategy, backends []*backend.Backend, metricsCollector *metrics.Metrics) *Handler {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   cfg.Proxy.DialTimeout.Duration(),
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          cfg.Proxy.MaxIdleConns,
		MaxIdleConnsPerHost:   cfg.Proxy.MaxIdleConnsPerHost,
		IdleConnTimeout:       cfg.Proxy.IdleConnTimeout.Duration(),
		ResponseHeaderTimeout: cfg.Proxy.ResponseHeaderTimeout.Duration(),
		TLSHandshakeTimeout:   cfg.Proxy.TLSHandshakeTimeout.Duration(),
		ForceAttemptHTTP2:     true,
	}
	handler := &Handler{
		config:        cfg,
		logger:        logger,
		backends:      backends,
		strategy:      strategy,
		transport:     transport,
		bufferPool:    &reverseProxyBufferPool{},
		metrics:       metricsCollector,
		rateLimiter:   ratelimit.New(cfg.RateLimit.Rate, cfg.RateLimit.Burst, cfg.RateLimit.Window.Duration()),
		tunnels:       make(map[net.Conn]*tunnelLifecycle),
		tunnelChanged: make(chan struct{}, 1),
	}
	if cfg.RateLimit.Enabled {
		evictionInterval := cfg.RateLimit.Window.Duration() / 2
		if evictionInterval <= 0 {
			evictionInterval = cfg.RateLimit.Window.Duration()
		}
		if err := handler.rateLimiter.Start(evictionInterval); err != nil {
			logger.Error("failed to start rate limiter eviction", "error", err)
		}
	}
	return handler
}

func (h *Handler) StopBackground() {
	h.rateLimiter.Stop()
}

func (h *Handler) BeginShutdown() {
	h.tunnelMu.Lock()
	h.tunnelClosing = true
	h.signalTunnelChangeLocked()
	h.tunnelMu.Unlock()
}

func (h *Handler) Shutdown(ctx context.Context) int {
	h.BeginShutdown()
	for {
		h.tunnelMu.Lock()
		if len(h.tunnels) == 0 && h.pendingUpgrades == 0 {
			h.tunnelMu.Unlock()
			return 0
		}
		changed := h.tunnelChanged
		h.tunnelMu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return h.forceCloseTunnels()
		}
	}
}

func (h *Handler) trackTunnel(conn net.Conn, tunnel *tunnelLifecycle) bool {
	h.tunnelMu.Lock()
	if h.tunnelClosing {
		h.tunnelMu.Unlock()
		_ = conn.Close()
		return false
	}
	tunnel.tracked = true
	h.tunnels[conn] = tunnel
	if h.metrics != nil {
		h.metrics.RecordTunnelOpened(tunnel.backend)
	}
	h.signalTunnelChangeLocked()
	h.tunnelMu.Unlock()
	return true
}

func (h *Handler) untrackTunnel(conn net.Conn, tunnel *tunnelLifecycle) {
	h.tunnelMu.Lock()
	if tunnel.tracked {
		reason := tunnel.closeReason()
		duration := time.Since(tunnel.started)
		if h.metrics != nil {
			h.metrics.RecordTunnelClosed(tunnel.backend, reason, duration)
		}
		if h.config.Proxy.CircuitCountTunnelErrors {
			if reason == metrics.TunnelReasonUpstreamError {
				tunnel.backend.RecordTunnelFailure()
			} else {
				tunnel.backend.RecordTunnelSuccess()
			}
		}
	}
	delete(h.tunnels, conn)
	h.signalTunnelChangeLocked()
	h.tunnelMu.Unlock()
}

func (h *Handler) beginUpgradeRequest() {
	h.tunnelMu.Lock()
	h.pendingUpgrades++
	h.signalTunnelChangeLocked()
	h.tunnelMu.Unlock()
}

func (h *Handler) finishUpgradeRequest() {
	h.tunnelMu.Lock()
	h.pendingUpgrades--
	h.signalTunnelChangeLocked()
	h.tunnelMu.Unlock()
}

func (h *Handler) signalTunnelChangeLocked() {
	select {
	case h.tunnelChanged <- struct{}{}:
	default:
	}
}

func (h *Handler) forceCloseTunnels() int {
	h.tunnelMu.Lock()
	connections := make([]net.Conn, 0, len(h.tunnels))
	for conn, tunnel := range h.tunnels {
		tunnel.markShutdownForced()
		connections = append(connections, conn)
	}
	h.tunnelMu.Unlock()

	for _, conn := range connections {
		_ = conn.Close()
	}
	if len(connections) > 0 && h.logger != nil {
		h.logger.Info("forced closed upgraded tunnels during shutdown", "count", len(connections))
	}
	for {
		h.tunnelMu.Lock()
		if len(h.tunnels) == 0 && h.pendingUpgrades == 0 {
			h.tunnelMu.Unlock()
			return len(connections)
		}
		changed := h.tunnelChanged
		h.tunnelMu.Unlock()
		<-changed
	}
}

func (h *Handler) SetBackends(backends []*backend.Backend) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.backends = append([]*backend.Backend(nil), backends...)
	if h.metrics != nil {
		h.metrics.SetBackends(backends)
	}
}

func (h *Handler) ReplaceBackendSet(backends []*backend.Backend, strategy balancer.Strategy, weights map[*backend.Backend]int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for b, weight := range weights {
		b.SetWeight(weight)
	}
	h.backends = append([]*backend.Backend(nil), backends...)
	h.strategy = strategy
	for _, b := range h.backends {
		if b != nil {
			b.CurrentWeight.Store(0)
		}
	}
	if h.metrics != nil {
		h.metrics.SetBackends(backends)
	}
}

func (h *Handler) currentBackends() []*backend.Backend {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return append([]*backend.Backend(nil), h.backends...)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	upgradeRequest := isUpgradeRequest(r)
	if upgradeRequest {
		h.beginUpgradeRequest()
		defer h.finishUpgradeRequest()
	}
	response := &statusWriter{ResponseWriter: w, trackTunnel: h.trackTunnel}
	w = response
	defer func() {
		if conn := response.hijackedConn; conn != nil {
			h.untrackTunnel(conn, response.tunnel)
		}
	}()
	requestID := requestIDFromHeader(r)
	if requestID == "" {
		requestID = newRequestID()
	}
	w.Header().Set("X-Request-ID", requestID)
	clientIP := clientIP(r, h.config.RateLimit.TrustedProxies)
	var selected *backend.Backend
	var requestError string
	defer func() {
		if h.logger == nil {
			return
		}
		level := slog.LevelInfo
		if isHealthOrAdminPath(r.URL.Path) {
			level = slog.LevelDebug
		}
		h.logger.Log(r.Context(), level, "proxy request",
			"request_id", requestID,
			"method", r.Method,
			"path", r.URL.RequestURI(),
			"client_ip", clientIP,
			"backend", backendName(selected),
			"status", response.statusCode(),
			"latency", time.Since(start),
			"error", requestError,
		)
	}()
	if h.config.RateLimit.Enabled {
		allowed, retryAfter := h.rateLimiter.Allow(clientIP)
		if !allowed {
			if h.metrics != nil {
				h.metrics.RecordRateLimited()
			}
			w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limit exceeded\n"))
			return
		}
	}
	selectionRequest := r.Clone(r.Context())
	selectionRequest.RemoteAddr = clientIP
	selected, probe := h.selectBackend(selectionRequest)
	if selected == nil {
		h.writePlainText(w, http.StatusServiceUnavailable, "no healthy backend available")
		return
	}
	if r.Header.Get("X-Request-ID") == "" {
		r.Header.Set("X-Request-ID", requestID)
	}
	clientIP = clientIPFromHeader(r, h.config.RateLimit.TrustedProxies)
	forwardedFor := clientIP
	if isTrustedProxy(r.RemoteAddr, h.config.RateLimit.TrustedProxies) && r.Header.Get("X-Forwarded-For") != "" {
		forwardedFor = strings.TrimSpace(r.Header.Get("X-Forwarded-For")) + ", " + remotePeerIP(r.RemoteAddr)
	}
	if r.Header.Get("X-Forwarded-Proto") == "" {
		r.Header.Set("X-Forwarded-Proto", schemeFromRequest(r))
	}
	if r.Header.Get("X-Forwarded-Host") == "" {
		r.Header.Set("X-Forwarded-Host", r.Host)
	}
	if r.Header.Get("X-Request-ID") == "" {
		r.Header.Set("X-Request-ID", requestID)
	}
	var serveAttempt func(*backend.Backend, int, *http.Request, bool)
	serveAttempt = func(current *backend.Backend, attempt int, attemptRequest *http.Request, isProbe bool) {
		selected = current
		current.IncrementActive()
		current.RecordRequest()
		probeResolved := false
		defer func() {
			if isProbe && !probeResolved {
				current.AbortHalfOpenProbe()
			}
			current.DecrementActive()
		}()
		reverseProxy := &httputil.ReverseProxy{
			Transport:  h.transport,
			BufferPool: h.bufferPool,
			Rewrite: func(req *httputil.ProxyRequest) {
				req.SetURL(current.URL)
				req.Out.Host = current.URL.Host
				stripHopByHopHeaders(req.Out.Header)
				if isUpgradeRequest(r) {
					req.Out.Header.Set("Connection", "Upgrade")
					req.Out.Header.Set("Upgrade", r.Header.Get("Upgrade"))
				}
				req.Out.Header.Set("X-Forwarded-For", forwardedFor)
				req.Out.Header.Set("X-Forwarded-Proto", schemeFromRequest(r))
				req.Out.Header.Set("X-Forwarded-Host", r.Host)
				req.Out.Header.Set("X-Request-ID", requestID)
			},
		}
		reverseProxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
			if errors.Is(err, errResponseRetryHandled) {
				return
			}
			current.SetHealthy(false)
			current.RecordFailure()
			probeResolved = true
			if !isUpgradeRequest(r) && retryableMethod(r.Method) && requestBodyReplayable(r) &&
				!response.wroteHeader() && attempt < h.config.Proxy.RetryMaxAttempts {
				next, nextIsProbe := h.selectBackend(selectionRequest)
				if nextIsProbe && next == current {
					next.AbortHalfOpenProbe()
				}
				if next != nil && next != current {
					retryProbeTransferred := false
					if nextIsProbe {
						defer func() {
							if !retryProbeTransferred {
								next.AbortHalfOpenProbe()
							}
						}()
					}
					retryRequest, replayErr := cloneReplayableRequest(r)
					if replayErr != nil {
						err = replayErr
					} else if waitErr := waitForRetry(r.Context(), h.config.Proxy.RetryBackoff.Duration()); waitErr != nil {
						err = waitErr
					} else if contextErr := r.Context().Err(); contextErr != nil {
						err = contextErr
					} else {
						retryProbeTransferred = true
						serveAttempt(next, attempt+1, retryRequest, nextIsProbe)
						return
					}
				}
			}
			requestError = err.Error()
			statusCode := http.StatusBadGateway
			if errors.Is(err, context.DeadlineExceeded) || isTimeoutError(err) {
				statusCode = http.StatusGatewayTimeout
			}
			if h.metrics != nil {
				h.metrics.RecordObservation(statusCode, time.Since(start), true)
			}
			h.writePlainText(w, statusCode, http.StatusText(statusCode))
		}
		reverseProxy.ModifyResponse = func(resp *http.Response) error {
			resp.Header.Set("X-Request-ID", requestID)
			probeResolved = true
			current.SetHealthy(true)
			if resp.StatusCode == http.StatusSwitchingProtocols && isUpgradeRequest(r) {
				tunnelBody, ok := resp.Body.(io.ReadWriteCloser)
				if !ok {
					return errors.New("upgraded backend response body is not read/write/closable")
				}
				tunnel := &tunnelLifecycle{backend: current, started: time.Now()}
				resp.Body = observeTunnelBody(tunnelBody, tunnel)
				response.tunnel = tunnel
			}
			configuredFailure := h.isCircuitFailureStatus(resp.StatusCode)
			failureStatus := resp.StatusCode >= http.StatusInternalServerError || configuredFailure
			if failureStatus {
				if configuredFailure && !isUpgradeRequest(r) && retryableMethod(r.Method) &&
					requestBodyReplayable(r) && !response.wroteHeader() &&
					attempt < h.config.Proxy.RetryMaxAttempts {
					next, nextIsProbe := h.selectBackendExcluding(selectionRequest, current)
					if next != nil {
						retryProbeTransferred := false
						if nextIsProbe {
							defer func() {
								if !retryProbeTransferred {
									next.AbortHalfOpenProbe()
								}
							}()
						}
						retryRequest, replayErr := cloneReplayableRequest(r)
						if replayErr == nil {
							replayErr = waitForRetry(r.Context(), h.config.Proxy.RetryBackoff.Duration())
						}
						if replayErr == nil {
							replayErr = r.Context().Err()
						}
						if replayErr == nil {
							current.RecordFailure()
							retryProbeTransferred = true
							serveAttempt(next, attempt+1, retryRequest, nextIsProbe)
							return errResponseRetryHandled
						}
					}
				}
				current.RecordFailure()
				if h.metrics != nil {
					h.metrics.RecordObservation(resp.StatusCode, time.Since(start), true)
				}
			} else {
				if resp.StatusCode == http.StatusSwitchingProtocols && isUpgradeRequest(r) &&
					h.config.Proxy.CircuitCountTunnelErrors {
					current.RecordHandshakeSuccess()
				} else {
					current.RecordSuccess()
				}
				if h.metrics != nil {
					h.metrics.RecordObservation(resp.StatusCode, time.Since(start), false)
				}
			}
			return nil
		}
		reverseProxy.ServeHTTP(w, attemptRequest)
	}
	serveAttempt(selected, 1, r, probe)
}

func (h *Handler) selectBackend(req *http.Request) (*backend.Backend, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	backends := h.backends
	strategy := h.strategy
	if strategy, ok := strategy.(balancer.ProbeStrategy); ok {
		return strategy.SelectWithProbe(backends, req)
	}
	return strategy.Select(backends, req), false
}

func (h *Handler) selectBackendExcluding(req *http.Request, excluded *backend.Backend) (*backend.Backend, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	backends := make([]*backend.Backend, 0, len(h.backends))
	for _, candidate := range h.backends {
		if candidate != nil && candidate != excluded {
			backends = append(backends, candidate)
		}
	}
	if strategy, ok := h.strategy.(balancer.ProbeStrategy); ok {
		return strategy.SelectWithProbe(backends, req)
	}
	return h.strategy.Select(backends, req), false
}

func (h *Handler) isCircuitFailureStatus(status int) bool {
	for _, configured := range h.config.Proxy.CircuitFailureStatuses {
		if configured == status {
			return true
		}
	}
	return false
}

type statusWriter struct {
	http.ResponseWriter
	status       int
	trackTunnel  func(net.Conn, *tunnelLifecycle) bool
	hijackedConn net.Conn
	tunnel       *tunnelLifecycle
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying response writer does not support hijacking")
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	if w.trackTunnel == nil || w.trackTunnel(conn, w.tunnel) {
		w.hijackedConn = conn
		if w.tunnel != nil {
			conn = observeClientConn(conn, w.tunnel)
		}
	}
	return conn, rw, nil
}

func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *statusWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *statusWriter) wroteHeader() bool {
	return w.status != 0
}

func requestBodyReplayable(r *http.Request) bool {
	return r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 || r.GetBody != nil
}

func cloneReplayableRequest(r *http.Request) (*http.Request, error) {
	clone := r.Clone(r.Context())
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		clone.Body = http.NoBody
		return clone, nil
	}
	if r.GetBody == nil {
		return nil, errors.New("request body is not replayable")
	}
	body, err := r.GetBody()
	if err != nil {
		return nil, fmt.Errorf("recreate request body for retry: %w", err)
	}
	clone.Body = body
	clone.GetBody = r.GetBody
	return clone, nil
}

func backendName(b *backend.Backend) string {
	if b == nil {
		return ""
	}
	return b.URL.Host
}

func isHealthOrAdminPath(path string) bool {
	return path == "/health" || path == "/status" || path == "/metrics"
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func retryableMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

func (h *Handler) Status() []map[string]any {
	backends := h.currentBackends()
	items := make([]map[string]any, 0, len(backends))
	for _, b := range backends {
		if b == nil {
			continue
		}
		items = append(items, b.Export())
	}
	return items
}

func (h *Handler) JSONStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"backends": h.Status()})
}

func (h *Handler) writePlainText(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(message + "\n"))
}

func requestIDFromHeader(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Request-ID"))
}

func newRequestID() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func clientIP(r *http.Request, trustedProxies []string) string {
	peerIP := remotePeerIP(r.RemoteAddr)
	if isTrustedProxy(r.RemoteAddr, trustedProxies) {
		if forwarded := firstForwardedIP(r.Header.Get("X-Forwarded-For")); forwarded != "" {
			return forwarded
		}
	}
	return peerIP
}

func clientIPFromHeader(r *http.Request, trustedProxies []string) string {
	return clientIP(r, trustedProxies)
}

func remotePeerIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return strings.Split(host, "%")[0]
	}
	if ip := net.ParseIP(strings.Trim(remoteAddr, "[]")); ip != nil {
		return ip.String()
	}
	return remoteAddr
}

func isTrustedProxy(remoteAddr string, trustedProxies []string) bool {
	peerIP := net.ParseIP(remotePeerIP(remoteAddr))
	if peerIP == nil {
		return false
	}
	for _, trustedProxy := range trustedProxies {
		trustedProxy = strings.TrimSpace(trustedProxy)
		if ip := net.ParseIP(trustedProxy); ip != nil && ip.Equal(peerIP) {
			return true
		}
		if _, network, err := net.ParseCIDR(trustedProxy); err == nil && network.Contains(peerIP) {
			return true
		}
	}
	return false
}

func firstForwardedIP(header string) string {
	if header == "" {
		return ""
	}
	first := strings.TrimSpace(strings.Split(header, ",")[0])
	ip := net.ParseIP(strings.Trim(first, "[]"))
	if ip == nil {
		return ""
	}
	return ip.String()
}

func schemeFromRequest(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func stripHopByHopHeaders(h http.Header) {
	for _, key := range []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(key)
	}
}

func isUpgradeRequest(r *http.Request) bool {
	if r == nil || r.Header.Get("Upgrade") == "" {
		return false
	}
	for _, token := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			return true
		}
	}
	return false
}

func joinURLPath(base, target string) string {
	if target == "" {
		return base
	}
	if base == "" {
		return target
	}
	if strings.HasSuffix(base, "/") && strings.HasPrefix(target, "/") {
		return base + strings.TrimPrefix(target, "/")
	}
	if !strings.HasSuffix(base, "/") && !strings.HasPrefix(target, "/") {
		return base + "/" + target
	}
	return base + target
}

func isTimeoutError(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func (h *Handler) MetricsText() string {
	if h.metrics != nil {
		return h.metrics.PrometheusText()
	}
	return ""
}

func (h *Handler) SetMetrics(m *metrics.Metrics) {
	h.metrics = m
}

func (h *Handler) MetricsEndpoint(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(h.MetricsText()))
}

func (h *Handler) StatusEndpoint(w http.ResponseWriter, r *http.Request) {
	h.JSONStatus(w, r)
}

func (h *Handler) RoundTrip(req *http.Request) (*http.Response, error) {
	return h.transport.RoundTrip(req)
}

func (h *Handler) Transport() *http.Transport {
	return h.transport
}

func (h *Handler) Config() *config.Config {
	return h.config
}

func (h *Handler) StrategyName() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.strategy == nil {
		return ""
	}
	return h.strategy.Name()
}

func (h *Handler) SetStrategy(s balancer.Strategy) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.strategy = s
}

func (h *Handler) CurrentBackends() []*backend.Backend {
	return h.currentBackends()
}

func (h *Handler) BuilderURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.String()
}
