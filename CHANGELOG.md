# Changelog

All notable changes to this project are documented here.

## [Unreleased]

### Security

- Pin outbound dials to resolved IPs to close DNS rebinding between resolve and connect.
- Reject decimal-encoded IPv4 host literals in the SSRF guard (e.g. `http://2130706433/`).
- `crawld` supports optional `CRAWLD_API_KEY` / `HTTP_API_KEY` bearer authentication.
- Default `HTTP_ADDR` is `127.0.0.1:8080` (loopback only unless explicitly overridden).

### Added

- Chaos integration test: concurrent Postgres frontier workers with random lease abandonment.
- Fuzz targets for `NormalizeURL` and `robotstxt.Parse`.
- Benchmarks for fetch throughput and URL normalization (`benchmark_test.go`).
- Runnable `Example*` tests and `examples/basic` for pkg.go.dev.
- CI: `govulncheck`, coverage artifact upload, parallel lint job.
- This changelog.

### Changed

- README: per-domain rate limiting is documented as **per process** (in-memory token bucket); horizontal scale requires shared coordination or fewer instances per target site.

### Fixed

- Register `/healthz`, `/readyz`, and `/version` on `crawld` (previously documented only).
