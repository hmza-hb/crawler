# Changelog

All notable changes to this project are documented here.

## [1.1.0] — unreleased

### Security

These fixes apply to deployments on **v1.0.3** and earlier as well as this release.

- **SSRF / DNS rebinding:** resolve once, reject if any address is private or unroutable, dial IP literals; HTTP proxy env vars are ignored (`Transport.Proxy = nil`).
- **Default bind:** `HTTP_ADDR` default is `127.0.0.1:8080` (was `:8080` in docs/compose examples). **Breaking** for anyone relying on all-interfaces bind without setting `HTTP_ADDR`.
- **API auth:** optional `CRAWLD_API_KEY` / `HTTP_API_KEY` bearer on `/v1/*`; **required** when listening on a non-loopback address. `/healthz`, `/readyz`, `/version`, and `/metrics` stay unauthenticated (expose `/metrics` only on trusted networks).

### Added

- `host_limits` Postgres table and claim-time politeness gate shared across `crawld` instances (`CRAWLER_PER_HOST_DELAY`).
- Terminal frontier `failed` state with `last_error` and attempt count; `/v1/stats` includes `frontier_failed`.
- `GET /metrics` Prometheus handler (`prometheus/client_golang`).
- SSRF, auth, politeness, and failed-state integration tests; fuzz corpora under `testdata/fuzz/`.
- CI: `go vet`, `go build ./examples/...`, scheduled fuzz job.

### Changed

- Redirect policy: private-host redirects are refused unless `CRAWLER_ALLOW_PRIVATE_HOSTS` and the target stays on the same host authority (blocks cross-port SSRF to loopback).
- README benchmarks and chaos-test wording aligned with actual tests.

## [1.0.3]

Prior baseline referenced by the security notes above.
