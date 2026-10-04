# crawler (`hermes-crawler`)

A high-throughput, restart-safe, politeness-enforcing web crawler engine and microservice in Go.

[![CI Pipeline](https://github.com/hmza-hb/crawler/actions/workflows/ci.yml/badge.svg)](https://github.com/hmza-hb/crawler/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/hmza-hb/crawler.svg)](https://pkg.go.dev/github.com/hmza-hb/crawler)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

`crawler` (Hermes Crawler) is a production-grade web crawling engine and microservice implemented in Go (`github.com/hmza-hb/crawler`). It accepts seed URLs or XML sitemaps, traverses hypermedia links under atomic resource budgets, and persists documents into PostgreSQL with HTTP `ETag` and `Last-Modified` conditional re-validation.

It operates both as an embeddable Go package and as a standalone REST API microservice daemon (`cmd/crawld`).

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

## Why `crawler`?

Crawling web resources at scale while maintaining system safety and legal/politeness compliance presents non-trivial distributed engineering challenges:

1. **Strict Politeness Compliance:** Uncontrolled crawlers risk overloading target hosts or violating site access policies. `crawler` incorporates a dedicated RFC 9309 compliant `robots.txt` parser and domain-keyed token bucket rate limiting.
2. **Re-validation & Bandwidth Efficiency:** Downloading identical web pages repeatedly wastes network I/O and storage. `crawler` executes conditional HTTP GET requests (`If-None-Match` / `If-Modified-Since`), processing `304 Not Modified` responses to prevent write amplification.
3. **Restart Safety & Non-Blocking Leasing:** Node crashes during long-running crawls can corrupt queue state. `crawler` provides a PostgreSQL frontier (`PgFrontier`) utilizing `FOR UPDATE SKIP LOCKED` for concurrent work leasing and automatic lease recovery without external message brokers.
4. **Zero-Trust Network Isolation:** Malicious links or open redirects can trigger Server-Side Request Forgery (SSRF) against cloud infrastructure endpoints (`169.254.169.254`, internal services). `crawler` intercepts DNS resolutions and redirect chains, validating target IP addresses against private CIDR ranges prior to HTTP transport.

---

## Engineering Guarantees & Features

- **RFC 9309 Compliant Engine:** Parses wildcards (`*`, `$`), `Crawl-delay`, group precedence, treats `404 Not Found` as permissive, and fails closed on network errors.
- **Conditional GET Re-validation:** Tracks HTTP `ETag` and `Last-Modified` response metadata, avoiding body downloads when content remains unchanged.
- **SSRF Isolation & IP Range Guards:** Re-evaluates every DNS lookup and redirect hop against private ranges (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `169.254.169.254`, loopback).
- **Distributed Work Leasing (`SKIP LOCKED`):** Multi-node URL scheduling backed by PostgreSQL row-level locks without lock contention.
- **Atomic Budget Enforcement:** Hard bounds across total pages, retained bytes, wall-clock duration, crawl depth, and per-host page limits.
- **Dual Infrastructure Modes:** Embeddable Go package and standalone REST API daemon (`cmd/crawld`).
- **Prometheus Observability:** Native exporter tracking fetch latencies, queue depth, status codes, and HTTP server metrics.

---

## System Architecture & Domain Subsystems

The core architecture isolates concerns into four primary component abstractions:

```text
┌─────────────────────────────────────────────────────────────────────────────────┐
│                                   Crawler                                       │
│    Worker Pool Coordinator · Link Canonicalization · Budget Enforcement         │
└───────┬─────────────────────────────────────────────────────────────────┬───────┘
        │                                                                 │
        ▼                                                                 ▼
┌───────────────────────────────┐                 ┌───────────────────────────────┐
│           Frontier            │                 │            Fetcher            │
│ Memory / PostgreSQL Queueing  │                 │ Polite HTTP Client / SSRF     │
└───────────────┬───────────────┘                 └───────────────┬───────────────┘
                │                                                 │
                └────────────────────────┬────────────────────────┘
                                         ▼
                         ┌───────────────────────────────┐
                         │             Store             │
                         │ Document & Metadata Storage   │
                         └───────────────────────────────┘
```

### Subsystem Breakdown

1. **`Fetcher` ([`fetch.go`](file:///home/hamza/code/crawler/fetch.go)):** Manages HTTP connection pooling, domain-keyed token bucket rate limiting ([`internal/platform/ratelimit`](file:///home/hamza/code/crawler/internal/platform/ratelimit)), RFC 9309 parsing, and conditional GET re-validation.
2. **`Frontier` ([`frontier.go`](file:///home/hamza/code/crawler/frontier.go), [`pgfrontier.go`](file:///home/hamza/code/crawler/pgfrontier.go)):** Manages URL traversal state (`pending` -> `leased` -> `completed`/`failed`). `MemoryFrontier` provides an in-memory priority queue, while `PgFrontier` handles PostgreSQL persistence with row locking.
3. **`Store` ([`store.go`](file:///home/hamza/code/crawler/store.go), [`pgstore.go`](file:///home/hamza/code/crawler/pgstore.go)):** Persistence layer storing HTTP status codes, headers, execution latency, raw/normalized SHA-256 content hashes, and extracted hypermedia links.
4. **`Crawler` ([`crawl.go`](file:///home/hamza/code/crawler/crawl.go)):** Multi-threaded worker pool coordinator executing depth traversal, link canonicalization, HTML/Sitemap parsing, and budget tracking.

→ [Detailed Architecture Document](docs/architecture.md)

---

## Deep-Dive Engineering Decisions

### 1. PostgreSQL `SKIP LOCKED` vs External Message Queues
Rather than coupling the system to complex external message queues (e.g., Kafka, RabbitMQ, or Redis Streams), `PgFrontier` uses PostgreSQL `FOR UPDATE SKIP LOCKED`. Worker nodes query pending URLs concurrently without waiting for locked rows. This eliminates lock contention while reducing operational setup overhead to PostgreSQL.

### 2. Lease Expiry & Fault Recovery
When a worker claims a URL from `PgFrontier`, a lease timestamp (`leased_until`) is set. If a crawler worker process panics or dies mid-crawl, the lease automatically expires. Subsequent worker polls detect and reclaim expired leases seamlessly without manual intervention.

### 3. Domain-Keyed Token Bucket Rate Limiting
Rate limiting operates at the registrable domain level (via `publicsuffix`) rather than globally or per-IP. This ensures domain politeness (`PerHostDelay`) while allowing maximum parallel throughput across distinct web hosts.

### 4. Zero-Trust SSRF Interception
To prevent Server-Side Request Forgery (SSRF), DNS resolution is performed explicitly prior to HTTP transport execution. Target IP addresses are evaluated against reserved private CIDRs, loopback addresses, and cloud provider metadata IPs (`169.254.169.254`). Redirect chains are re-checked at every hop.

---

## Quick Start & Usage

### 1. Embeddable Go Library

Add the package to your `go.mod`:

```bash
go get github.com/hmza-hb/crawler
```

Initialize and run a bounded crawl:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/hmza-hb/crawler"
)

func main() {
	cfg := crawler.DefaultConfig()
	store := crawler.NewMemoryStore()
	frontier := crawler.NewMemoryFrontier(cfg)

	fetcher, err := crawler.NewHTTPFetcher(cfg, crawler.Options{Store: store})
	if err != nil {
		panic(err)
	}

	loop, err := crawler.New(fetcher, frontier, store, cfg, crawler.Options{})
	if err != nil {
		panic(err)
	}

	res, err := loop.Crawl(context.Background(), crawler.CrawlOptions{
		Seeds:       []string{"https://example.com"},
		FollowLinks: true,
		Budget: crawler.Budget{
			MaxPages:    50,
			MaxDuration: 2 * time.Minute,
		},
	})
	if err != nil && err != crawler.ErrCrawlStopped {
		panic(err)
	}

	fmt.Printf("Fetched: %d, Not Modified: %d, Stopped By: %s\n",
		res.Fetched, res.NotModified, res.Stats.StoppedBy)
}
```

### 2. Standalone Microservice (`crawld`)

#### Running with Docker Compose
```bash
make docker-up
```

#### Running locally via Go CLI
```bash
export DATABASE_URL='postgres://postgres:postgres@localhost:5432/crawler?sslmode=disable'
export HTTP_ADDR=':8080'

go run ./cmd/crawld
```

---

## HTTP REST API Endpoints

| Method | Endpoint | Description |
| :--- | :--- | :--- |
| `POST` | `/v1/crawl` | Trigger a bounded, multi-page crawl run |
| `POST` | `/v1/fetch` | Politely fetch a single URL with persistence |
| `GET` | `/v1/documents/{id}` | Retrieve stored document metadata by UUID |
| `GET` | `/v1/documents?url={url}` | Query the last stored document for a target URL |
| `GET` | `/v1/hosts/{host}/robots` | Inspect cached `robots.txt` rules and crawl delay |
| `GET` | `/v1/stats?run_id={id}` | Retrieve queue depth and run statistics |
| `GET` | `/healthz`, `/readyz` | Liveness and database connectivity readiness probes |
| `GET` | `/metrics` | Prometheus metrics endpoint |

### Example REST Request & Response

#### Single URL Fetch Request
```bash
curl -X POST http://localhost:8080/v1/fetch \
  -H 'Content-Type: application/json' \
  -d '{"url": "https://example.com"}'
```

#### Response Payload
```json
{
  "outcome": "fetched",
  "url": "https://example.com/",
  "status": 200,
  "status_text": "OK",
  "content_type": "text/html; charset=UTF-8",
  "content_hash": "a67f3...e3b1",
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

## Configuration Reference

The service and library are configured via `Config` or environment variables:

| Environment Variable | Description | Default |
| :--- | :--- | :--- |
| `DATABASE_URL` | PostgreSQL connection DSN | Required for `crawld` |
| `HTTP_ADDR` | Listen address for API microservice | `:8080` |
| `CRAWLER_USER_AGENT` | HTTP `User-Agent` header sent upstream | `HermesCrawler/1.0 (+https://github.com/hmza-hb/crawler)` |
| `CRAWLER_CONCURRENCY` | Concurrent worker goroutines per run | `10` |
| `CRAWLER_PER_HOST_DELAY` | Minimum delay between requests to same host | `250ms` |
| `CRAWLER_MAX_BYTES` | Maximum allowed HTTP response body size | `10485760` (10MB) |
| `CRAWLER_MAX_RETRIES` | Retry attempts for transient failures | `2` |
| `CRAWLER_ALLOW_PRIVATE_HOSTS` | Override SSRF guard to permit loopback/private IPs | `false` |

---

## Observability & Health Monitoring

### 1. Probes
- `GET /healthz` — Service liveness probe returning HTTP 200.
- `GET /readyz` — Database readiness probe executing a ping against PostgreSQL.

### 2. Prometheus Metrics
`GET /metrics` exposes standardized operational metrics:
- `crawler_fetch_duration_seconds`: Histogram measuring HTTP fetch latency.
- `crawler_fetched_documents_total`: Counter by HTTP status code and outcome.
- `crawler_frontier_depth`: Gauge of current pending URLs in queue.

---

## Development & Verification

### Running Unit & Integration Tests
```bash
make test
```

### Running Tests with Race Detection
```bash
go test -v -race ./...
```

### Coverage Reports
```bash
make test-coverage
```

### Performance Benchmarking
Execute Go micro-benchmarking suite for robots parsing, URL normalization, and frontier queueing:
```bash
go test -bench=. -benchmem ./...
```

---

## Project Structure

```text
.
├── cmd/
│   └── crawld/                 # REST API microservice entrypoint & CLI flags
├── internal/
│   └── platform/               # Platform infrastructure (db pool, httpx server, observe, ratelimit)
├── robotstxt/                  # Custom RFC 9309 robots.txt parser engine
├── docs/
│   ├── architecture.md         # Deep-dive system design document
│   └── openapi.yaml            # OpenAPI 3.0 REST API specification
├── migrations/                 # Embedded PostgreSQL migrations (`001_init.sql`, etc.)
├── docker-compose.yml          # Local development stack (Postgres + Service)
├── Dockerfile                  # Multi-stage production container build
├── Makefile                    # Build, test, and container targets
├── config.go                   # Core configuration structures & validation
├── crawl.go                    # Crawler worker loop & execution manager
├── fetch.go                    # Polite HTTP fetcher & security guards
├── frontier.go                 # In-memory frontier queue implementation
├── pgfrontier.go               # PostgreSQL SKIP LOCKED frontier implementation
├── store.go                    # Storage interfaces & memory storage
└── pgstore.go                  # PostgreSQL document & run persistence
```

---

## Limitations & Scope

- **No JavaScript Execution:** `crawler` parses static HTML and XML sitemaps. Single-page applications (SPAs) requiring client-side JS rendering are not executed.
- **Single-Process Memory Mode:** `MemoryFrontier` operates in-memory for standalone single-process usage. Multi-node distributed deployment requires `PgFrontier` backed by PostgreSQL.

---

## Roadmap

- [ ] Dynamic adaptive per-host crawl delay based on server response latency.
- [ ] Pluggable Object Storage (AWS S3 / GCS) backend for raw HTML content bodies.
- [ ] JSON-LD and Schema.org metadata extraction pipeline.

---

## Contributing

Contributions are welcome! Please follow these guidelines:

1. Open an issue describing the proposed bug fix or feature.
2. Ensure all changes include unit tests covering new behavior.
3. Verify that `make test` and `make lint` pass before submitting pull requests.

---

## License

[MIT](LICENSE) © 2026 Hamza & Contributors
