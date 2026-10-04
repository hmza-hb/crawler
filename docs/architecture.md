# Architecture & System Design

`crawler` is an enterprise-grade web crawling microservice and library written in Go.

## High-Level Architecture

```
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

## Key Components

1. **`Fetcher` (`fetch.go`):** Implements polite fetching, RFC 9309 `robots.txt` enforcement, conditional GET (`If-None-Match`, `If-Modified-Since`), and rate limiting per domain.
2. **`Frontier` (`frontier.go`, `pgfrontier.go`):** Tracks pending and leased URLs. The PostgreSQL implementation utilizes `FOR UPDATE SKIP LOCKED` for concurrent workers.
3. **`Store` (`store.go`, `pgstore.go`):** Document storage abstraction. Manages document metadata, HTTP status codes, response headers, and content hashes.
4. **`Crawler` (`crawl.go`):** Manages concurrent worker goroutines, link extraction, atomic budget enforcement (`max_pages`, `max_bytes`, `max_duration`), and crawl run reporting.
