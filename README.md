# crawler (`hermes-crawler`)

[![CI Pipeline](https://github.com/hmza-hb/crawler/actions/workflows/ci.yml/badge.svg)](https://github.com/hmza-hb/crawler/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/hmza-hb/crawler.svg)](https://pkg.go.dev/github.com/hmza-hb/crawler)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A high-throughput, polite, restart-safe web crawler engine and microservice written in Go. It accepts seed URLs or XML sitemaps, traverses links under configurable budgets, and persists documents in PostgreSQL with `ETag` and `Last-Modified` conditional re-validation.

It operates both as an embeddable Go library and as a standalone HTTP microservice (`cmd/crawld`).

---

## Core Guarantees & Architectural Highlights

- **RFC 9309 `robots.txt` Compliance:** Parses complex site rules, obeys `Crawl-delay`, treats `404` as unrestricted access, and fails closed on network errors.
- **Conditional HTTP GET (304 Not Modified):** Replays `ETag` (`If-None-Match`) and `Last-Modified` (`If-Modified-Since`) headers to prevent redundant downloads.
- **SSRF Isolation & IP Guards:** Re-evaluates every DNS resolution and redirect hop against private ranges (`10.0.0.0/8`, `192.168.0.0/16`, `169.254.169.254`, loopback).
- **Postgres `FOR UPDATE SKIP LOCKED` Frontier:** Distributed queueing allowing concurrent crawler nodes to lease items safely without lock contention.
- **Atomic Budget Enforcement:** Hard bounds for total pages, retained bytes, wall-clock duration, and per-host caps.

---

## Project Structure

```text
.
├── cmd/
│   └── crawld/                 # HTTP REST API Microservice entrypoint
├── internal/
│   └── platform/               # Standalone platform infrastructure (db, httpx, retry, observe)
├── robotstxt/                  # RFC 9309 compliant robots.txt evaluation engine
├── docs/
│   ├── architecture.md          # Architecture & system design document
│   └── openapi.yaml             # OpenAPI 3.0 API specification
├── migrations/                 # Embedded PostgreSQL schema migrations
├── docker-compose.yml           # Local dev stack (Postgres + Service)
├── Dockerfile                   # Multi-stage production container build
└── Makefile                     # Build & developer automation commands
```

---

## Quick Start & Usage

### 1. Embedded Library Usage

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

    fetcher, _ := crawler.NewHTTPFetcher(cfg, crawler.Options{Store: store})
    loop, _ := crawler.New(fetcher, frontier, store, cfg, crawler.Options{})

    res, _ := loop.Crawl(context.Background(), crawler.CrawlOptions{
        Seeds:       []string{"https://example.com"},
        FollowLinks: true,
        Budget:      crawler.Budget{MaxPages: 100, MaxDuration: 5 * time.Minute},
    })

    fmt.Printf("Fetched: %d, Not Modified: %d, Stopped By: %s\n",
        res.Fetched, res.NotModified, res.Stats.StoppedBy)
}
```

### 2. Running as a Microservice (`crawld`)

Via Docker Compose:
```bash
make docker-up
```

Via Go CLI:
```bash
export DATABASE_URL='postgres://postgres:postgres@localhost:5432/crawler?sslmode=disable'
export HTTP_ADDR=':8080'

go run ./cmd/crawld
```

Execute a crawl job via REST API:
```bash
curl -X POST http://localhost:8080/v1/crawl \
  -H 'Content-Type: application/json' \
  -d '{
    "seeds": ["https://example.com"],
    "follow_links": true,
    "max_pages": 50,
    "max_duration": "5m"
  }'
```

---

## HTTP REST Endpoints

| Method | Endpoint | Description |
| --- | --- | --- |
| `POST` | `/v1/crawl` | Trigger a bounded crawl job |
| `POST` | `/v1/fetch` | Politely fetch a single URL |
| `GET` | `/v1/documents/{id}` | Retrieve stored document metadata by ID |
| `GET` | `/v1/documents?url=` | Query last stored copy of a specific URL |
| `GET` | `/v1/hosts/{host}/robots` | Inspect cached `robots.txt` rules for a host |
| `GET` | `/v1/stats?run_id=` | Fetch queue depth and runtime statistics |

---

## Development & Testing

```bash
# Run unit & integration test suite
make test

# Generate test coverage report
make test-coverage

# Build binary
make build
```

---

## License

[MIT](LICENSE) © 2026 Hamza & Contributors
