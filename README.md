<div align="center">

# Hermes Crawler

**A restart-safe, politeness-enforcing, SSRF-hardened web crawler engine for Go.**
Embed it as a library. Run it as a microservice. Scale it with nothing but PostgreSQL.

[![CI](https://github.com/hmza-hb/crawler/actions/workflows/ci.yml/badge.svg)](https://github.com/hmza-hb/crawler/actions)
[![Coverage](https://img.shields.io/badge/coverage-see%20CI%20artifact-lightgrey)](https://github.com/hmza-hb/crawler/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/hmza-hb/crawler.svg)](https://pkg.go.dev/github.com/hmza-hb/crawler)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

```bash
go get github.com/hmza-hb/crawler
```

</div>

---

## Why this exists

Writing a crawler that works on your laptop is a weekend project. Writing one that survives production is a different job:

| What happens in production | What a naive crawler does | What Hermes does |
|---|---|---|
| You run a second instance | Both fetch the same URLs | Workers lease rows with `FOR UPDATE SKIP LOCKED`; no double-claims, no lock waits |
| A worker is OOM-killed mid-crawl | URLs stuck "in progress" forever | Every claim carries a lease; expired leases are reclaimed automatically |
| 100 workers, one target site | You hammer the host and get banned | Per-process token bucket plus Postgres `host_limits` on `PgFrontier` (`CRAWLER_PER_HOST_DELAY`) so multiple `crawld` instances share one gap per registrable domain |
| A user submits `http://169.254.169.254/latest/meta-data` | You leak cloud credentials | SSRF guard: no HTTP(S) proxy env, every resolved IP checked, connect uses pinned literals; redirect hops validated |
| The same page is re-crawled daily | Full re-download, full re-write | Conditional GET (`ETag` / `Last-Modified`); a `304` skips the body and the write |
| `robots.txt` is ambiguous or unreachable | Ignored, or parsed by regex | Purpose-built RFC 9309 parser with defined failure semantics |

Hermes was built as the crawling infrastructure layer for larger systems: RAG ingestion pipelines, search indexers, change monitors, and site archivers. It is deliberately small in surface area and strict about invariants.

---

## Architecture

```mermaid
flowchart TB
    API["crawld REST API<br/>(optional)"] --> C
    LIB["Embedded Go library"] --> C

    subgraph Core
        C["Crawler<br/>worker pool, link canonicalization, budgets"]
        F["Frontier<br/>Memory or PostgreSQL"]
        X["Fetcher<br/>polite HTTP, conditional GET"]
        S["Store<br/>documents and metadata"]
        C --> F
        C --> X
        C --> S
    end

    X --> R["RFC 9309<br/>robots parser"]
    X --> G["SSRF guard<br/>DNS + redirect validation"]
    X --> L["Per-domain<br/>token bucket"]
    F -.-> PG[("PostgreSQL")]
    S -.-> PG
```

Four components, each behind an interface:

| Component | Files | Responsibility |
|---|---|---|
| **Crawler** | `crawl.go` | Worker pool, depth traversal, link canonicalization, HTML and sitemap parsing, atomic budget tracking |
| **Frontier** | `frontier.go`, `pgfrontier.go` | URL state machine and scheduling. `MemoryFrontier` for single-process use, `PgFrontier` for distributed use |
| **Fetcher** | `fetch.go`, `robotstxt/`, `internal/platform/ratelimit` | Connection pooling, robots enforcement, rate limiting, SSRF checks, conditional GET |
| **Store** | `store.go`, `pgstore.go` | Status, headers, latency, raw and normalized SHA-256 content hashes, extracted links |

Because each piece is an interface, you can swap the frontier or store without touching crawl logic.

---

## Engineering deep dive

### 1. Distributed work leasing without a message broker

The frontier is a PostgreSQL table. Workers claim work with row-level locks that skip rows other workers already hold:

```sql
-- Simplified illustration of the claim pattern
UPDATE frontier
SET    state = 'leased', leased_until = now() + $lease
WHERE  id IN (
    SELECT id FROM frontier
    WHERE  state = 'pending'
       OR (state = 'leased' AND leased_until < now())
    ORDER  BY priority, id
    LIMIT  $batch
    FOR UPDATE SKIP LOCKED
)
RETURNING *;
```

`SKIP LOCKED` means a worker never waits on a row another worker is claiming. It moves to the next available one. Throughput scales with workers instead of serializing on a hot row, and the only infrastructure you operate is the PostgreSQL you probably already have.

```mermaid
stateDiagram-v2
    [*] --> pending
    pending --> leased: worker claims
    leased --> completed: fetch + store OK
    leased --> failed: retries exhausted
    leased --> pending: lease expires (worker died)
    failed --> [*]
    completed --> [*]
```

**Delivery semantics:** leasing gives *at-least-once* processing. If a worker stalls past its lease, another worker may claim the same URL. Writes are keyed so a repeated fetch is safe to apply twice, and content hashes make duplicates detectable.

**Trade-off, stated plainly:** PostgreSQL as a queue is the right call up to the point where queue write throughput becomes your bottleneck. Beyond that, the `Frontier` interface is the seam for a different backend.

### 2. Crash recovery by construction

There is no recovery code path to get wrong. A claim is a lease with a deadline, and an expired lease is just a claimable row.

`TestChaosMultiWorkerFrontier` (with `TEST_DATABASE_URL`, including CI) enqueues many URLs, runs concurrent claimers that sometimes hold a lease until it expires without calling `Complete`, and asserts every URL is eventually completed and the active frontier drains. Lease duration is derived from `FetchTimeout`, retries, and a one-minute buffer; there is no mid-fetch lease extension. After repeated failures an item moves to terminal `failed` state (`last_error`, attempt count) and is exposed via `/v1/stats` as `frontier_failed`.

### 3. Politeness is enforced at the transport, not requested of the caller

- **RFC 9309 parser** (`robotstxt/`): wildcards (`*`, `$`), longest-match precedence, group selection, and `Crawl-delay`.
- **Defined failure semantics:** a `404` on `robots.txt` means no restrictions; an unreachable or erroring robots endpoint fails *closed*, so Hermes does not crawl a host whose rules it cannot read.
- **Per-domain token bucket** (in-process) plus **`host_limits` in Postgres** on `PgFrontier`: each claim advances `next_allowed_at` by `CRAWLER_PER_HOST_DELAY` for that URL's registrable domain, at most one queued row per domain per claim batch, so multiple `crawld` processes share one polite gap per site. `MemoryFrontier` keeps the in-process limiter only.

### 4. SSRF isolation

If users can submit URLs, your crawler is a request proxy into your own network. Hermes closes that off at the network layer:

1. **No proxy env:** `Transport.Proxy = nil` so `HTTP_PROXY` cannot steer requests elsewhere.
2. Resolve DNS once; **every** returned address must pass the blocklist; then **dial IP literals** (no second resolve).
3. Block loopback, RFC 1918, multicast, `255.255.255.255`, CGNAT, documentation/benchmark/reserved ranges (`192.0.0.0/24`, `198.18.0.0/15`, `240.0.0.0/4`, …), IPv6 ULA/link-local/multicast, zone IDs, decimal IPv4 literals, IPv4-mapped private addresses, and NAT64 (`64:ff9b::/96`) when the embedded IPv4 is blocked.
4. **Redirect hops** to private hosts are refused (same-origin redirects on loopback are allowed only with `CRAWLER_ALLOW_PRIVATE_HOSTS` and unchanged host:port).

`CRAWLER_ALLOW_PRIVATE_HOSTS=true` allows dialing private targets for local dev; redirect rules still block cross-authority private hops.

### 5. Conditional re-validation

Hermes stores `ETag` and `Last-Modified` per document and replays them as `If-None-Match` / `If-Modified-Since`. A `304 Not Modified` skips the body transfer and the database write, which removes both bandwidth cost and write amplification on recrawls of mostly-static sites. Savings depend on how often your targets change.

### 6. Hard resource budgets

A crawl stops when the first limit trips: total pages, retained bytes, wall-clock duration, depth, or pages per host. Budgets are tracked atomically across the worker pool, so concurrency cannot overshoot them. The result reports which limit ended the run (`Stats.StoppedBy`).

---

## Quick start

### As a library

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

### As a distributed service

```bash
# Docker Compose: Postgres + crawld
make docker-up

# Or locally
export DATABASE_URL='postgres://postgres:postgres@localhost:5432/crawler?sslmode=disable'
export HTTP_ADDR='127.0.0.1:8080'
export CRAWLD_API_KEY='change-me'   # required when listening beyond loopback
go run ./cmd/crawld
```

Run as many `crawld` instances as you like against the same database. They coordinate through the frontier table and need nothing else.

---

## REST API

| Method | Endpoint | Description |
|---|---|---|
| `POST` | `/v1/crawl` | Run a bounded multi-page crawl |
| `POST` | `/v1/fetch` | Politely fetch and persist a single URL |
| `GET` | `/v1/documents/{id}` | Stored document metadata by UUID |
| `GET` | `/v1/documents?url={url}` | Latest stored document for a URL |
| `GET` | `/v1/hosts/{host}/robots` | Cached robots.txt rules and crawl delay |
| `GET` | `/v1/stats?run_id={id}` | Queue depth and run statistics |
| `GET` | `/healthz`, `/readyz` | Liveness and database readiness |
| `GET` | `/metrics` | Prometheus metrics |

Full spec: [`docs/openapi.yaml`](docs/openapi.yaml)

```bash
curl -X POST http://127.0.0.1:8080/v1/fetch \
  -H "Authorization: Bearer $CRAWLD_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"url": "https://example.com"}'
```

```json
{
  "outcome": "fetched",
  "url": "https://example.com/",
  "status": 200,
  "content_type": "text/html; charset=UTF-8",
  "content_hash": "a67f3...e3b1",
  "etag": "\"314b5b701509a25b3992070e6508d519\"",
  "not_modified": false,
  "body_truncated": false,
  "bytes": 1256,
  "elapsed_ms": 142
}
```

---

## Configuration

| Variable | Description | Default |
|---|---|---|
| `DATABASE_URL` | PostgreSQL DSN | required for `crawld` |
| `HTTP_ADDR` | API listen address | `127.0.0.1:8080` |
| `CRAWLD_API_KEY` / `HTTP_API_KEY` | Bearer token for `/v1/*`; **required** if `HTTP_ADDR` is not loopback | unset (ok on `127.0.0.1` / `localhost` / `::1` only) |
| `CRAWLER_USER_AGENT` | Upstream `User-Agent` | `HermesCrawler/1.0 (+https://github.com/hmza-hb/crawler)` |
| `CRAWLER_CONCURRENCY` | Worker goroutines per run | `10` |
| `CRAWLER_PER_HOST_DELAY` | Minimum gap between requests to one domain | `250ms` |
| `CRAWLER_MAX_BYTES` | Max response body size | `10485760` (10 MB) |
| `CRAWLER_MAX_RETRIES` | Retries on transient failure | `2` |
| `CRAWLER_ALLOW_PRIVATE_HOSTS` | Disable the SSRF guard (dev only) | `false` |

---

## Observability

- `GET /healthz` returns 200 when the process is alive.
- `GET /readyz` pings PostgreSQL.
- `GET /metrics` serves the Prometheus text format (Go runtime and process metrics via `prometheus/client_golang`). **Does not require the API key**; bind to a trusted network or protect with your edge proxy. Application-specific `crawler_*` series in code are not yet registered on this endpoint.

---

## What you can build with it

- **RAG ingestion:** crawl, dedupe by content hash, and feed clean documents to your embedding pipeline. Conditional GET keeps re-ingestion cheap.
- **Search and indexing:** a polite, resumable discovery layer.
- **Change monitoring:** recrawl on a schedule; `304` vs `200` plus hash comparison tells you what changed.
- **Archiving:** budgeted, restart-safe site capture.

---

## Threat model and non-goals

**In scope:** SSRF via submitted URLs and redirects, runaway resource consumption (budgets), abusive request rates (per-domain limiting), worker crashes (leases).

**Not in scope:**

- **JavaScript rendering.** Hermes parses static HTML and XML sitemaps. SPAs that need client-side rendering will not be executed.
- **Multi-node memory mode.** `MemoryFrontier` is single-process. Distributed deployments require `PgFrontier`.
- **Exactly-once processing.** Delivery is at-least-once by design; store writes are built to tolerate repeats.

---

## Development

```bash
make test              # unit + integration (set TEST_DATABASE_URL for Postgres tests)
go test -race ./...    # race detector
make test-coverage     # coverage report
make lint
go test -bench=. -benchmem -count=5 ./...   # fetch throughput, URL normalization
```

Benchmarks (`go test -bench=. -benchmem -count=5 ./...`, **linux/amd64**, **go1.27.0**, CPU **Intel(R) Core(TM) i7-7820HQ @ 2.90GHz**):

| Benchmark | ns/op (approx.) | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkFetchPages` | 91–113k | ~9.3k | 107 |
| `BenchmarkNormalizeURL` | 378–396 | 160 | 5 |

`BenchmarkFetchPages` uses a local `httptest` server with politeness disabled. Conditional `304` responses skip body retention and store writes (`fetch_test.go`).

See [CHANGELOG.md](CHANGELOG.md) and [examples/](examples/).

### Project layout

```
.
├── examples/              # runnable integration sketches
├── cmd/crawld/            # REST microservice entrypoint
├── internal/platform/     # db pool, http server, observability, rate limiting
├── robotstxt/             # RFC 9309 parser
├── docs/                  # architecture.md, openapi.yaml
├── migrations/            # embedded PostgreSQL migrations
├── config.go              # configuration and validation
├── crawl.go               # worker pool and crawl loop
├── fetch.go               # polite fetcher and security guards
├── frontier.go            # in-memory frontier
├── pgfrontier.go          # PostgreSQL SKIP LOCKED frontier
├── store.go / pgstore.go  # storage interfaces and implementations
├── Dockerfile
└── docker-compose.yml
```

---

## Roadmap

- [ ] Adaptive per-host crawl delay based on observed server latency
- [ ] Pluggable object storage (S3 / GCS) for raw bodies
- [ ] JSON-LD and Schema.org extraction

## Contributing

1. Open an issue describing the bug or feature.
2. Include tests for new behavior.
3. Make sure `make test` and `make lint` pass.

Security issues: please report privately rather than opening a public issue.

## License

MIT © 2026 Hamza Hafeez
