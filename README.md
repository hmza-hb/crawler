# crawler

A high-throughput, restart-safe, politeness-enforcing web crawler engine and microservice in Go.

[![CI Pipeline](https://github.com/hmza-hb/crawler/actions/workflows/ci.yml/badge.svg)](https://github.com/hmza-hb/crawler/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/hmza-hb/crawler.svg)](https://pkg.go.dev/github.com/hmza-hb/crawler)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

`crawler` (Hermes Crawler) is a production-grade web crawler designed to traverse web domains safely, respect site politeness rules, and persist document content with conditional HTTP GET re-validation (`ETag` / `Last-Modified`). It runs as an embeddable Go library or as a standalone REST API microservice daemon (`cmd/crawld`).

```text
                       ┌─────────────────────────────────────────┐
                       │           crawld REST API               │
                       └──────────────────┬──────────────────────┘
                                          │
                                          ▼
                       ┌───────────────────────────────────────────┐
                       │    Crawler Worker Pool (crawl.go)         │
                       └─────┬───────────────────────────────┬─────┘
                             │                               │
                             ▼                               ▼
        ┌───────────────────────────────┐     ┌───────────────────────────────┐
        │     MemoryFrontier Queue      │     │      PgFrontier (SKIP LOCKED) │
        └──────────────┬────────────────┘     └──────────────┬────────────────┘
                       │                                     │
                       └──────────────────┬──────────────────┘
                                          │
                                          ▼
                      ┌───────────────────────────────────────┐
                      │    Polite HTTP Fetcher (fetch.go)     │
                      └──────────────────┬────────────────────┘
                                         │
              ┌──────────────────────────┴──────────────────────────┐
              ▼                                                     ▼
 ┌─────────────────────────┐                               ┌─────────────────┐
 │ RFC 9309 Robots Parser  │                               │  SSRF IP Guard  │
 └─────────────────────────┘                               └─────────────────┘
```

---

## Why?

Crawling web resources reliably at scale introduces several critical engineering challenges:

1. **Web Politeness & Compliance:** Unthrottled crawlers risk overloading target hosts or violating site policies. `crawler` includes a custom RFC 9309 compliant `robots.txt` parser and domain-keyed token bucket rate limiting.
2. **Bandwidth Efficiency:** Downloading identical web pages repeatedly wastes network I/O and storage. `crawler` re-validates previously fetched documents using HTTP `304 Not Modified` (`If-None-Match` / `If-Modified-Since`).
3. **Restart Safety & Locking:** Node crashes during large crawls can lose queue state. `crawler` provides a PostgreSQL frontier (`PgFrontier`) that leverages `FOR UPDATE SKIP LOCKED` for concurrent work leasing and automatic lease recovery.
4. **Security & SSRF Isolation:** Malicious or misconfigured links can trigger SSRF attacks against internal network metadata endpoints. `crawler` inspects DNS resolutions and redirect hops against private/reserved IP ranges before dispatching requests.

---

## Features

- **Distributed Work Leasing:** Shared URL queueing powered by PostgreSQL `FOR UPDATE SKIP LOCKED`.
- **RFC 9309 `robots.txt` Engine:** Handles wildcards, `Crawl-delay`, line length caps, and group precedence.
- **Per-Domain Rate Limiting:** Keyed token-bucket rate limiter ensuring host isolation.
- **Conditional GET Re-validation:** Tracks `ETag` and `Last-Modified` headers to avoid body re-downloads.
- **SSRF Guard:** Rejects loopback, private CIDRs (`10.0.0.0/8`, `192.168.0.0/16`), and metadata IPs (`169.254.169.254`).
- **Atomic Budgeting:** Limits total pages, byte retention, wall-clock duration, depth, and per-host caps.
- **Dual Execution Mode:** Works both as an embeddable Go package and a REST API daemon (`cmd/crawld`).
- **Prometheus Observability:** Exposes request latency, fetch outcomes, queue depth, and HTTP metrics.

---

## Quick Start

### 1. Run via Docker Compose

```bash
git clone https://github.com/hmza-hb/crawler.git
cd crawler

# Spin up Postgres and crawld microservice
make docker-up
```

### 2. Trigger a Crawl Job

```bash
curl -X POST http://localhost:8080/v1/crawl \
  -H 'Content-Type: application/json' \
  -d '{
    "seeds": ["https://example.com"],
    "follow_links": true,
    "max_pages": 10,
    "max_duration": "2m"
  }'
```

---

## Example

### API Request
```bash
curl -X POST http://localhost:8080/v1/fetch \
  -H 'Content-Type: application/json' \
  -d '{"url": "https://example.com"}'
```

### API Response
```json
{
  "outcome": "fetched",
  "url": "https://example.com/",
  "status": 200,
  "status_text": "OK",
  "content_type": "text/html; charset=UTF-8",
  "content_hash": "a67f...e3b1",
  "etag": "\"314b5b701509a25b3992070e6508d519\"",
  "last_modified": "2024-01-30T10:00:00Z",
  "not_modified": false,
  "filtered": false,
  "body_truncated": false,
  "bytes": 1256,
  "elapsed_ms": 142
}
```

---

## Architecture

The system is composed of four primary abstractions:

* **`Crawler` ([`crawl.go`](file:///home/hamza/code/crawler/crawl.go)):** Worker pool coordinator managing link extraction, depth propagation, and budget bounds.
* **`Frontier` ([`frontier.go`](file:///home/hamza/code/crawler/frontier.go), [`pgfrontier.go`](file:///home/hamza/code/crawler/pgfrontier.go)):** Queue interface backing URL discovery, priority ranking, and work leasing.
* **`Fetcher` ([`fetch.go`](file:///home/hamza/code/crawler/fetch.go)):** HTTP transport client handling network execution, rate limiting, and SSRF security guards.
* **`Store` ([`store.go`](file:///home/hamza/code/crawler/store.go), [`pgstore.go`](file:///home/hamza/code/crawler/pgstore.go)):** Persistence manager recording metadata, status codes, headers, and document bodies.

→ [Detailed Architecture & Specifications](docs/architecture.md)

---

## Design Decisions

### 1. PostgreSQL `SKIP LOCKED` vs Dedicated Message Queue
Rather than requiring an external broker (e.g. RabbitMQ or Redis Streams) for queue management, `PgFrontier` uses PostgreSQL `FOR UPDATE SKIP LOCKED`. This allows multiple crawler instances to lease unvisited URLs concurrently from the database without lock contention, keeping the operational footprint minimal.

### 2. Lease Expiry & Crash Recovery
When a node claims a URL from `PgFrontier`, it receives a time-bounded lease (`leased_until`). If a worker worker node crashes or encounters a fatal fault mid-crawl, the lease automatically expires. Subsequent worker polls reclaim expired leases without manual intervention.

### 3. Domain-Keyed Token Bucket Rate Limiting
Rate limiting operates at the registered domain level (using publicsuffix logic) rather than per-IP or globally. This guarantees domain politeness (`PerHostDelay`) while permitting high concurrent throughput across distinct target hosts.

### 4. Zero-Trust SSRF Interception
To prevent server-side request forgery (SSRF), DNS resolution occurs explicitly prior to request execution. Target IPs are evaluated against CIDRs for private networks, loopback addresses, and cloud provider metadata IPs (`169.254.169.254`). Redirect chains are re-validated at every hop.

---

## Configuration

Service and engine behavior can be customized via environment variables:

| Environment Variable | Description | Default |
| :--- | :--- | :--- |
| `DATABASE_URL` | PostgreSQL connection DSN | Required for `crawld` |
| `HTTP_ADDR` | HTTP API listen address | `:8080` |
| `CRAWLER_USER_AGENT` | Upstream HTTP `User-Agent` string | `HermesCrawler/1.0 (+https://github.com/hmza-hb/crawler)` |
| `CRAWLER_CONCURRENCY` | Concurrent worker goroutines per run | `10` |
| `CRAWLER_PER_HOST_DELAY` | Minimum time delay between requests to same domain | `250ms` |
| `CRAWLER_MAX_BYTES` | Maximum allowed HTTP response body size | `10485760` (10MB) |
| `CRAWLER_MAX_RETRIES` | Max retries for transient HTTP errors | `2` |
| `CRAWLER_ALLOW_PRIVATE_HOSTS` | Permit private/loopback hosts (Development only) | `false` |

---

## Performance & Benchmarking

`crawler` includes built-in Go benchmarks for critical performance paths:

To execute performance benchmarks locally:

```bash
# Run benchmarks for robots parser, URL normalization, and frontier queueing
go test -bench=. -benchmem ./...
```

---

## Observability

### 1. Health & Readiness Probes
- `GET /healthz` — Basic liveness probe returning HTTP 200 OK.
- `GET /readyz` — Database connectivity probe verifying PostgreSQL ping.

### 2. Prometheus Metrics
`GET /metrics` exposes standardized operational metrics:
- `crawler_fetch_duration_seconds`: Histogram of HTTP fetch latency.
- `crawler_fetched_documents_total`: Counter by HTTP status code and outcome.
- `crawler_frontier_depth`: Gauge of active pending items in the frontier queue.

---

## Development

```bash
# Run complete test suite (unit + integration)
make test

# Run tests with race detector enabled
go test -v -race ./...

# Run code linter
make lint
```

---

## Project Structure

```text
.
├── cmd/
│   └── crawld/                 # HTTP REST API daemon entrypoint
├── internal/
│   └── platform/               # Core platform modules (db, httpx, observe, ratelimit)
├── robotstxt/                  # Custom RFC 9309 robots parser package
├── docs/
│   ├── architecture.md         # System design documentation
│   └── openapi.yaml            # OpenAPI 3.0 specification
├── migrations/                 # PostgreSQL schema migrations
├── docker-compose.yml          # Dev stack definition
├── Dockerfile                  # Multi-stage production container build
├── Makefile                    # Developer targets
├── config.go                   # System configuration & defaults
├── crawl.go                    # Crawler worker pool engine
├── fetch.go                    # HTTP client, SSRF guard & robots handling
├── frontier.go                 # In-memory frontier queue
├── pgfrontier.go               # PostgreSQL SKIP LOCKED frontier
├── store.go                    # Memory document storage
└── pgstore.go                  # PostgreSQL document storage
```

---

## Limitations

- **No JavaScript Execution:** `crawler` parses static HTML and XML sitemaps. Single-page applications (SPAs) requiring client-side JS rendering are not executed.
- **Single-Process Frontier Mode:** `MemoryFrontier` operates in-memory for standalone single-process usage. Distributed multi-node scaling requires `PgFrontier` with PostgreSQL.

---

## Roadmap

- [ ] Support for HTTP `robots.txt` Crawl-Delay dynamically per host worker pool.
- [ ] Pluggable storage backends for S3 / Object Storage for raw HTML document bodies.
- [ ] Structured JSON-LD / Microdata metadata extraction pipeline.

---

## Contributing

Contributions are welcome! Please follow these guidelines:

1. Open an issue describing the proposed bug fix or feature.
2. Ensure all changes include unit tests covering new behavior.
3. Verify that `make test` and `make lint` pass before submitting pull requests.

---

## License

[MIT](LICENSE) © 2026 Hamza & Contributors
