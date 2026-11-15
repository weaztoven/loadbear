# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- **Behavior change:** With the default empty `proxy.circuit_failure_statuses`, consecutive upstream 5xx responses now accumulate toward `proxy.circuit_failure_threshold` and open the circuit. They still do not mark the backend unhealthy or trigger status-based retries.
- The reverse proxy reuses response-copy buffers to reduce per-request allocation observed in the profile benchmark; request and response behavior is unchanged.

### Fixed

- Upstream 5xx responses no longer reset the consecutive circuit-failure count before being recorded. Unlisted 429/404 responses remain non-failures.
- A health check canceled while reloading settings no longer marks its backend unhealthy.

### Added

- SIGHUP reload of health-check path, interval, timeout, and healthy/unhealthy thresholds. Changed settings replace the checker loop without overlapping loops and preserve health flags for unchanged backends.
- Opt-in `admin.pprof_enabled` profiling handlers on the admin listener and a reproducible proxy profile benchmark.
- A Linux CI benchmark smoke test that runs `loadgen` through the proxy and checks request/error counts without performance thresholds.

## [0.2.1] - 2026-11-15

### Added

- Standard-library `cmd/loadgen` with bounded latency sampling and JSON/Markdown summaries.
- Reproducible Linux/macOS and Windows benchmark scripts for direct-backend and proxy strategy scenarios.
- `proxy.circuit_failure_statuses` to configure additional breaker failure statuses, with retries only for eligible requests before response commitment.
- Graceful tracking and force-close of upgraded tunnels when shutdown grace expires.
- Cumulative Prometheus latency buckets with `_sum`/`_count`, per-backend request/failure and circuit metrics, and a rate-limited counter.

### Changed

- Prometheus backend labels now use escaped `backend` values and stable backend ordering.
- Backend request counters include each upstream attempt; terminal transport errors no longer double-count backend failures.

## [0.2.0] - 2026-11-15

### Added

- Linux/macOS `SIGHUP` reload of backend URLs, weights and selection strategy. Unchanged backends keep their state (health, counters, circuit breaker) and in-flight requests are not interrupted. An invalid config is rejected and the running config is kept. Other settings (listen addresses, timeouts, health-check settings, rate limiting, logging) still require a restart.
- Integration tests for HTTP/1.1 upgrade handshakes and bidirectional byte echo through the proxy.

### Fixed

- `SIGHUP` terminated the process instead of reloading. It is now handled separately from the shutdown signals.
- `Upgrade` and `Connection` headers were stripped from every request, which broke WebSocket upgrades. They are now preserved for upgrade requests only; upgrade requests are never retried.
- The access-log response wrapper did not implement `http.Hijacker` and `http.Flusher`, which prevented upgrade tunnels.
- Backend lists held by the metrics and health-check components were read and written without synchronization.

## [0.1.1] - 2026-11-13

### Fixed

- The circuit breaker admitted unlimited concurrent probes in the half-open state; exactly one probe is now admitted and its slot is released on every exit path.
- Retries could start after the client's deadline, and requests with non-replayable bodies could be retried. Retries now require a replayable body and an uncommitted response, and respect the original request context.

### Changed

- **Breaking:** `rate_limit.trusted_proxy` (boolean) is replaced by `rate_limit.trusted_proxies`, a list of trusted IPs/CIDRs. `X-Forwarded-For` is only honored when the direct peer matches. Configurations using the old key must migrate.
- The admin listener defaults to `127.0.0.1:9090`; the Docker config binds `:9090` inside its network namespace.

### Added

- Fake backend modes for hanging, closing before headers, dropping mid-response, and slow responses.
- Regression tests for the issues above.

## [0.1.0] - 2026-11-12

### Added

- HTTP reverse proxy with request ID and forwarded-header handling.
- Round-robin, smooth weighted round-robin, least-connections, and IP-hash strategies.
- Periodic active health checks.
- Retries for GET, HEAD, and OPTIONS requests after upstream errors.
- Per-backend circuit breakers.
- Per-client-IP token-bucket rate limiting.
- YAML configuration with listen-address and log-level environment overrides.
- Admin-port `/status` JSON and `/metrics` Prometheus text endpoints.
- Graceful shutdown on termination signals.
- Docker images and a Docker Compose demo.

[Unreleased]: https://github.com/weaztoven/loadbear/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/weaztoven/loadbear/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/weaztoven/loadbear/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/weaztoven/loadbear/releases/tag/v0.1.0