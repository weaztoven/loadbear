package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func validConfig() Config {
	cfg := DefaultConfig()
	cfg.Backends = []BackendConfig{{URL: "http://127.0.0.1:8081", Weight: 1}}
	return cfg
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name      string
		change    func(*Config)
		wantField string
	}{
		{name: "listen", change: func(c *Config) { c.Listen = " " }, wantField: "listen"},
		{name: "log level", change: func(c *Config) { c.LogLevel = "" }, wantField: "log_level"},
		{name: "empty strategy", change: func(c *Config) { c.Strategy = "" }, wantField: "strategy"},
		{name: "unknown strategy", change: func(c *Config) { c.Strategy = "least_used" }, wantField: "strategy"},
		{name: "no backends", change: func(c *Config) { c.Backends = nil }, wantField: "backends"},
		{name: "empty backend URL", change: func(c *Config) { c.Backends[0].URL = " " }, wantField: "backends[0].url"},
		{name: "malformed backend URL", change: func(c *Config) { c.Backends[0].URL = "http://" }, wantField: "backends[0].url"},
		{name: "duplicate backend", change: func(c *Config) { c.Backends = append(c.Backends, c.Backends[0]) }, wantField: "backends[1].url"},
		{name: "zero weight", change: func(c *Config) { c.Backends[0].Weight = 0 }, wantField: "backends[0].weight"},
		{name: "negative weight", change: func(c *Config) { c.Backends[0].Weight = -1 }, wantField: "backends[0].weight"},
		{name: "health interval", change: func(c *Config) { c.Health.Interval = 0 }, wantField: "health.interval"},
		{name: "health timeout", change: func(c *Config) { c.Health.Timeout = 0 }, wantField: "health.timeout"},
		{name: "healthy threshold", change: func(c *Config) { c.Health.HealthyThreshold = 0 }, wantField: "health.healthy_threshold"},
		{name: "unhealthy threshold", change: func(c *Config) { c.Health.UnhealthyThreshold = 0 }, wantField: "health.unhealthy_threshold"},
		{name: "idle connection timeout", change: func(c *Config) { c.Proxy.IdleConnTimeout = 0 }, wantField: "proxy.idle_conn_timeout"},
		{name: "response header timeout", change: func(c *Config) { c.Proxy.ResponseHeaderTimeout = 0 }, wantField: "proxy.response_header_timeout"},
		{name: "TLS handshake timeout", change: func(c *Config) { c.Proxy.TLSHandshakeTimeout = 0 }, wantField: "proxy.tls_handshake_timeout"},
		{name: "dial timeout", change: func(c *Config) { c.Proxy.DialTimeout = 0 }, wantField: "proxy.dial_timeout"},
		{name: "request timeout", change: func(c *Config) { c.Proxy.RequestTimeout = 0 }, wantField: "proxy.request_timeout"},
		{name: "retry attempts", change: func(c *Config) { c.Proxy.RetryMaxAttempts = 0 }, wantField: "proxy.retry_max_attempts"},
		{name: "retry backoff", change: func(c *Config) { c.Proxy.RetryBackoff = 0 }, wantField: "proxy.retry_backoff"},
		{name: "circuit cooldown", change: func(c *Config) { c.Proxy.CircuitCooldown = 0 }, wantField: "proxy.circuit_cooldown"},
		{name: "rate", change: func(c *Config) { c.RateLimit.Rate = 0 }, wantField: "rate_limit.rate"},
		{name: "burst", change: func(c *Config) { c.RateLimit.Burst = 0 }, wantField: "rate_limit.burst"},
		{name: "rate window", change: func(c *Config) { c.RateLimit.Window = 0 }, wantField: "rate_limit.window"},
		{name: "invalid trusted proxy", change: func(c *Config) { c.RateLimit.TrustedProxies = []string{"not-an-ip"} }, wantField: "rate_limit.trusted_proxies[0]"},
		{name: "shutdown grace", change: func(c *Config) { c.Shutdown.GracePeriod = 0 }, wantField: "shutdown.grace_period"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfig()
			test.change(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), test.wantField) {
				t.Fatalf("Validate() error = %v; want field %q", err, test.wantField)
			}
		})
	}
	valid := validConfig()
	valid.RateLimit.TrustedProxies = []string{"127.0.0.1", "192.0.2.0/24", "2001:db8::/32"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestCircuitFailureStatusesValidation(t *testing.T) {
	for _, test := range []struct {
		name     string
		statuses string
	}{
		{name: "below minimum", statuses: "99"},
		{name: "above maximum", statuses: "600"},
		{name: "duplicate", statuses: "429, 429"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			data := []byte("backends:\n  - url: http://127.0.0.1:8081\n    weight: 1\nproxy:\n  circuit_failure_statuses: [" + test.statuses + "]\n")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), "proxy.circuit_failure_statuses") {
				t.Fatalf("Load error = %v; want proxy.circuit_failure_statuses validation error", err)
			}
		})
	}
}

func TestAdminPprofEnabledMustBeBoolean(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("backends:\n  - url: http://127.0.0.1:8081\n    weight: 1\nadmin:\n  pprof_enabled: enabled\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("non-boolean admin.pprof_enabled value was accepted")
	}
}

func TestLoadDefaultsAndEnvironmentOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("backends:\n  - url: http://127.0.0.1:8081\n    weight: 1\nrate_limit:\n  trusted_proxies: [127.0.0.1, 192.0.2.0/24]\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOADBEAR_LISTEN", ":8181")
	t.Setenv("LOADBEAR_LOG_LEVEL", "debug")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8181" || cfg.LogLevel != "debug" {
		t.Fatalf("environment overrides not applied: listen=%q log=%q", cfg.Listen, cfg.LogLevel)
	}
	if len(cfg.RateLimit.TrustedProxies) != 2 || cfg.RateLimit.TrustedProxies[1] != "192.0.2.0/24" {
		t.Fatalf("trusted proxy list not parsed: %#v", cfg.RateLimit.TrustedProxies)
	}
	defaults := DefaultConfig()
	if cfg.AdminListen != defaults.AdminListen || cfg.LogFormat != defaults.LogFormat ||
		cfg.Strategy != defaults.Strategy || cfg.Health.Path != defaults.Health.Path ||
		cfg.Proxy.MaxIdleConns != defaults.Proxy.MaxIdleConns ||
		cfg.Proxy.RetryMaxAttempts != defaults.Proxy.RetryMaxAttempts ||
		cfg.RateLimit.Burst != defaults.RateLimit.Burst ||
		cfg.Shutdown.GracePeriod.Duration() != defaults.Shutdown.GracePeriod.Duration() {
		t.Fatalf("optional YAML fields did not keep defaults: %+v", cfg)
	}
	if defaults.AdminListen != "127.0.0.1:9090" {
		t.Fatalf("admin listen default = %q", defaults.AdminListen)
	}
	if defaults.Admin.PprofEnabled {
		t.Fatal("admin pprof should be disabled by default")
	}
}

func TestLoadErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("missing config file succeeded")
	}
	path := filepath.Join(t.TempDir(), "bad-duration.yaml")
	if err := os.WriteFile(path, []byte("backends:\n  - url: http://127.0.0.1:8081\n    weight: 1\nhealth:\n  interval: soon\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "duration") {
		t.Fatalf("bad duration error = %v", err)
	}
}

func TestYAMLDurationParsing(t *testing.T) {
	for _, test := range []struct {
		input string
		want  time.Duration
		err   bool
	}{
		{input: "15s", want: 15 * time.Second},
		{input: "2", want: 2 * time.Second},
		{input: `""`, want: 0},
		{input: "soon", err: true},
	} {
		var decoded struct {
			Value YAMLDuration `yaml:"value"`
		}
		err := yaml.Unmarshal([]byte("value: "+test.input+"\n"), &decoded)
		if (err != nil) != test.err {
			t.Fatalf("yaml.Unmarshal(%q) error = %v; wantErr %v", test.input, err, test.err)
		}
		if err == nil && decoded.Value.Duration() != test.want {
			t.Fatalf("yaml.Unmarshal(%q) = %s; want %s", test.input, decoded.Value.Duration(), test.want)
		}
	}
	if got := YAMLDuration(3 * time.Second).Duration(); got != 3*time.Second {
		t.Fatalf("Duration() = %s", got)
	}
}

func TestLoadWithoutPathRequiresBackends(t *testing.T) {
	t.Setenv("LOADBEAR_LISTEN", ":8080")
	t.Setenv("LOADBEAR_LOG_LEVEL", "info")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "backends") {
		t.Fatalf("Load without path error = %v", err)
	}
}
