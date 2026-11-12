# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
