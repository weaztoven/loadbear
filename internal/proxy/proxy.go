package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

type Handler struct {
	mu          sync.RWMutex
	config      *config.Config
	logger      *slog.Logger
	backends    []*backend.Backend
	strategy    balancer.Strategy
	transport   *http.Transport
	metrics     *metrics.Metrics
	rateLimiter *ratelimit.RateLimiter
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
		config:      cfg,
		logger:      logger,
		backends:    backends,
		strategy:    strategy,
		transport:   transport,
		metrics:     metricsCollector,
		rateLimiter: ratelimit.New(cfg.RateLimit.Rate, cfg.RateLimit.Burst, cfg.RateLimit.Window.Duration()),
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

func (h *Handler) SetBackends(backends []*backend.Backend) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.backends = backends
	if h.metrics != nil {
		h.metrics.Backends = backends
	}
}

func (h *Handler) currentBackends() []*backend.Backend {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*backend.Backend, len(h.backends))
	copy(out, h.backends)
	return out
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	response := &statusWriter{ResponseWriter: w}
	w = response
	requestID := requestIDFromHeader(r)
	if requestID == "" {
		requestID = newRequestID()
	}
	w.Header().Set("X-Request-ID", requestID)
	clientIP := clientIP(r, h.config.RateLimit.TrustedProxy)
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
			w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limit exceeded\n"))
			return
		}
	}
	selectionRequest := r.Clone(r.Context())
	selectionRequest.RemoteAddr = clientIP
	selected = h.strategy.Select(h.currentBackends(), selectionRequest)
	if selected == nil {
		h.writePlainText(w, http.StatusServiceUnavailable, "no healthy backend available")
		return
	}
	if r.Header.Get("X-Request-ID") == "" {
		r.Header.Set("X-Request-ID", requestID)
	}
	clientIP = clientIPFromHeader(r, h.config.RateLimit.TrustedProxy)
	forwardedFor := clientIP
	if h.config.RateLimit.TrustedProxy && r.Header.Get("X-Forwarded-For") != "" {
		forwardedFor = strings.TrimSpace(r.Header.Get("X-Forwarded-For")) + ", " + clientIPFromHeader(r, false)
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
	var serveAttempt func(*backend.Backend, int)
	serveAttempt = func(current *backend.Backend, attempt int) {
		selected = current
		current.IncrementActive()
		defer current.DecrementActive()
		reverseProxy := &httputil.ReverseProxy{
			Transport: h.transport,
			Rewrite: func(req *httputil.ProxyRequest) {
				req.SetURL(current.URL)
				req.Out.Host = current.URL.Host
				stripHopByHopHeaders(req.Out.Header)
				req.Out.Header.Set("X-Forwarded-For", forwardedFor)
				req.Out.Header.Set("X-Forwarded-Proto", schemeFromRequest(r))
				req.Out.Header.Set("X-Forwarded-Host", r.Host)
				req.Out.Header.Set("X-Request-ID", requestID)
			},
		}
		reverseProxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
			current.SetHealthy(false)
			current.RecordFailure()
			if retryableMethod(r.Method) && attempt < h.config.Proxy.RetryMaxAttempts {
				if next := h.strategy.Select(h.currentBackends(), selectionRequest); next != nil && next != current {
					if waitErr := waitForRetry(r.Context(), h.config.Proxy.RetryBackoff.Duration()); waitErr == nil {
						serveAttempt(next, attempt+1)
						return
					} else {
						err = waitErr
					}
				}
			}
			requestError = err.Error()
			statusCode := http.StatusBadGateway
			if errors.Is(err, context.DeadlineExceeded) || isTimeoutError(err) {
				statusCode = http.StatusGatewayTimeout
			}
			if h.metrics != nil {
				h.metrics.RecordRequest(current, statusCode, time.Since(start))
			}
			h.writePlainText(w, statusCode, http.StatusText(statusCode))
		}
		reverseProxy.ModifyResponse = func(resp *http.Response) error {
			resp.Header.Set("X-Request-ID", requestID)
			current.RecordSuccess()
			current.SetHealthy(true)
			if h.metrics != nil {
				h.metrics.RecordRequest(current, resp.StatusCode, time.Since(start))
			}
			return nil
		}
		reverseProxy.ServeHTTP(w, r)
	}
	serveAttempt(selected, 1)
}

type statusWriter struct {
	http.ResponseWriter
	status int
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

func (w *statusWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
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
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
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
	items := make([]map[string]any, 0, len(h.currentBackends()))
	for _, b := range h.currentBackends() {
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

func clientIP(r *http.Request, trustedProxy bool) string {
	if trustedProxy {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			parts := strings.Split(forwarded, ",")
			if len(parts) > 0 {
				return strings.TrimSpace(parts[0])
			}
		}
	}
	addr := r.RemoteAddr
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

func clientIPFromHeader(r *http.Request, trustedProxy bool) string {
	if trustedProxy {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			parts := strings.Split(forwarded, ",")
			if len(parts) > 0 {
				return strings.TrimSpace(parts[0])
			}
		}
	}
	return clientIP(r, false)
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
	if h.strategy == nil {
		return ""
	}
	return h.strategy.Name()
}

func (h *Handler) SetStrategy(s balancer.Strategy) {
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
