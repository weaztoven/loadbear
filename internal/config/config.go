package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultListen      = ":8080"
	DefaultAdminListen = "127.0.0.1:9090"
	DefaultLogLevel    = "info"
	DefaultLogFormat   = "json"
	DefaultStrategy    = "round_robin"
	DefaultHealthPath  = "/health"
	DefaultSocketPath  = ""
)

type YAMLDuration time.Duration

func (d *YAMLDuration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == 0 || node.Value == "" {
		return nil
	}
	if val, err := time.ParseDuration(node.Value); err == nil {
		*d = YAMLDuration(val)
		return nil
	}
	if i, err := strconv.Atoi(node.Value); err == nil {
		*d = YAMLDuration(time.Duration(i) * time.Second)
		return nil
	}
	return fmt.Errorf("invalid duration %q", node.Value)
}

func (d YAMLDuration) Duration() time.Duration {
	return time.Duration(d)
}

type BackendConfig struct {
	URL    string `yaml:"url"`
	Weight int    `yaml:"weight"`
}

type HealthConfig struct {
	Path               string       `yaml:"path"`
	Interval           YAMLDuration `yaml:"interval"`
	Timeout            YAMLDuration `yaml:"timeout"`
	HealthyThreshold   int          `yaml:"healthy_threshold"`
	UnhealthyThreshold int          `yaml:"unhealthy_threshold"`
}

type ProxyConfig struct {
	MaxIdleConns            int          `yaml:"max_idle_conns"`
	MaxIdleConnsPerHost     int          `yaml:"max_idle_conns_per_host"`
	IdleConnTimeout         YAMLDuration `yaml:"idle_conn_timeout"`
	ResponseHeaderTimeout   YAMLDuration `yaml:"response_header_timeout"`
	TLSHandshakeTimeout     YAMLDuration `yaml:"tls_handshake_timeout"`
	DialTimeout             YAMLDuration `yaml:"dial_timeout"`
	RequestTimeout          YAMLDuration `yaml:"request_timeout"`
	RetryMaxAttempts        int          `yaml:"retry_max_attempts"`
	RetryBackoff            YAMLDuration `yaml:"retry_backoff"`
	CircuitFailureThreshold int          `yaml:"circuit_failure_threshold"`
	CircuitCooldown         YAMLDuration `yaml:"circuit_cooldown"`
	CircuitFailureStatuses  []int        `yaml:"circuit_failure_statuses"`
}

type RateLimitConfig struct {
	Enabled        bool         `yaml:"enabled"`
	Rate           float64      `yaml:"rate"`
	Burst          int          `yaml:"burst"`
	Window         YAMLDuration `yaml:"window"`
	TrustedProxies []string     `yaml:"trusted_proxies"`
}

type ShutdownConfig struct {
	GracePeriod YAMLDuration `yaml:"grace_period"`
}

type Config struct {
	Listen      string          `yaml:"listen"`
	AdminListen string          `yaml:"admin_listen"`
	LogLevel    string          `yaml:"log_level"`
	LogFormat   string          `yaml:"log_format"`
	Strategy    string          `yaml:"strategy"`
	Backends    []BackendConfig `yaml:"backends"`
	Health      HealthConfig    `yaml:"health"`
	Proxy       ProxyConfig     `yaml:"proxy"`
	RateLimit   RateLimitConfig `yaml:"rate_limit"`
	Shutdown    ShutdownConfig  `yaml:"shutdown"`
}

func DefaultConfig() Config {
	return Config{
		Listen:      DefaultListen,
		AdminListen: DefaultAdminListen,
		LogLevel:    DefaultLogLevel,
		LogFormat:   DefaultLogFormat,
		Strategy:    DefaultStrategy,
		Backends:    nil,
		Health: HealthConfig{
			Path:               DefaultHealthPath,
			Interval:           YAMLDuration(10 * time.Second),
			Timeout:            YAMLDuration(2 * time.Second),
			HealthyThreshold:   2,
			UnhealthyThreshold: 2,
		},
		Proxy: ProxyConfig{
			MaxIdleConns:            100,
			MaxIdleConnsPerHost:     50,
			IdleConnTimeout:         YAMLDuration(90 * time.Second),
			ResponseHeaderTimeout:   YAMLDuration(10 * time.Second),
			TLSHandshakeTimeout:     YAMLDuration(10 * time.Second),
			DialTimeout:             YAMLDuration(5 * time.Second),
			RequestTimeout:          YAMLDuration(30 * time.Second),
			RetryMaxAttempts:        2,
			RetryBackoff:            YAMLDuration(100 * time.Millisecond),
			CircuitFailureThreshold: 5,
			CircuitCooldown:         YAMLDuration(30 * time.Second),
		},
		RateLimit: RateLimitConfig{
			Enabled: true,
			Rate:    100,
			Burst:   200,
			Window:  YAMLDuration(1 * time.Minute),
		},
		Shutdown: ShutdownConfig{GracePeriod: YAMLDuration(15 * time.Second)},
	}
}

func Load(path string) (*Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		cfg.applyEnvOverrides()
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
		return &cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	cfg.applyEnvOverrides()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyEnvOverrides() {
	if listen := os.Getenv("LOADBEAR_LISTEN"); listen != "" {
		c.Listen = listen
	}
	if level := os.Getenv("LOADBEAR_LOG_LEVEL"); level != "" {
		c.LogLevel = level
	}
}

func (c *Config) Validate() error {
	if strings.TrimSpace(c.Listen) == "" {
		return errors.New("listen is required")
	}
	if strings.TrimSpace(c.LogLevel) == "" {
		return errors.New("log_level is required")
	}
	if c.Strategy == "" {
		return errors.New("strategy is required")
	}
	validStrategies := map[string]struct{}{
		"round_robin":                 {},
		"smooth_weighted_round_robin": {},
		"least_connections":           {},
		"ip_hash":                     {},
	}
	if _, ok := validStrategies[strings.ToLower(c.Strategy)]; !ok {
		return fmt.Errorf("strategy %q is unknown; valid values are round_robin, smooth_weighted_round_robin, least_connections, ip_hash", c.Strategy)
	}
	if len(c.Backends) == 0 {
		return errors.New("backends must not be empty")
	}
	seen := map[string]struct{}{}
	for i, backend := range c.Backends {
		field := fmt.Sprintf("backends[%d].url", i)
		if strings.TrimSpace(backend.URL) == "" {
			return fmt.Errorf("%s is required", field)
		}
		if _, ok := seen[backend.URL]; ok {
			return fmt.Errorf("%s is duplicated: %q", field, backend.URL)
		}
		seen[backend.URL] = struct{}{}
		parsedURL, err := url.Parse(backend.URL)
		if err != nil || parsedURL.Host == "" ||
			(parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
			return fmt.Errorf("%s must be a valid HTTP(S) URL", field)
		}
		if backend.Weight <= 0 {
			return fmt.Errorf("backends[%d].weight must be positive", i)
		}
	}
	seenFailureStatuses := make(map[int]struct{}, len(c.Proxy.CircuitFailureStatuses))
	for i, status := range c.Proxy.CircuitFailureStatuses {
		field := fmt.Sprintf("proxy.circuit_failure_statuses[%d]", i)
		if status < 100 || status > 599 {
			return fmt.Errorf("%s must be between 100 and 599", field)
		}
		if _, ok := seenFailureStatuses[status]; ok {
			return fmt.Errorf("%s is duplicated: %d", field, status)
		}
		seenFailureStatuses[status] = struct{}{}
	}
	if c.Health.Interval.Duration() <= 0 {
		return errors.New("health.interval must be positive")
	}
	if c.Health.Timeout.Duration() <= 0 {
		return errors.New("health.timeout must be positive")
	}
	if c.Health.HealthyThreshold <= 0 {
		return errors.New("health.healthy_threshold must be positive")
	}
	if c.Health.UnhealthyThreshold <= 0 {
		return errors.New("health.unhealthy_threshold must be positive")
	}
	if c.Proxy.IdleConnTimeout.Duration() <= 0 {
		return errors.New("proxy.idle_conn_timeout must be positive")
	}
	if c.Proxy.ResponseHeaderTimeout.Duration() <= 0 {
		return errors.New("proxy.response_header_timeout must be positive")
	}
	if c.Proxy.TLSHandshakeTimeout.Duration() <= 0 {
		return errors.New("proxy.tls_handshake_timeout must be positive")
	}
	if c.Proxy.DialTimeout.Duration() <= 0 {
		return errors.New("proxy.dial_timeout must be positive")
	}
	if c.Proxy.RequestTimeout.Duration() <= 0 {
		return errors.New("proxy.request_timeout must be positive")
	}
	if c.Proxy.RetryMaxAttempts < 1 {
		return errors.New("proxy.retry_max_attempts must be at least 1")
	}
	if c.Proxy.RetryBackoff.Duration() <= 0 {
		return errors.New("proxy.retry_backoff must be positive")
	}
	if c.Proxy.CircuitCooldown.Duration() <= 0 {
		return errors.New("proxy.circuit_cooldown must be positive")
	}
	if c.RateLimit.Rate <= 0 {
		return errors.New("rate_limit.rate must be positive")
	}
	if c.RateLimit.Burst <= 0 {
		return errors.New("rate_limit.burst must be positive")
	}
	if c.RateLimit.Window.Duration() <= 0 {
		return errors.New("rate_limit.window must be positive")
	}
	for i, proxy := range c.RateLimit.TrustedProxies {
		proxy = strings.TrimSpace(proxy)
		if net.ParseIP(proxy) != nil {
			continue
		}
		if _, _, err := net.ParseCIDR(proxy); err != nil {
			return fmt.Errorf("rate_limit.trusted_proxies[%d] must be an IP address or CIDR", i)
		}
	}
	if c.Shutdown.GracePeriod.Duration() <= 0 {
		return errors.New("shutdown.grace_period must be positive")
	}
	return nil
}
