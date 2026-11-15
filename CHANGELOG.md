# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]


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