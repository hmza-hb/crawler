package crawler_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hmza-hb/crawler"
	"github.com/hmza-hb/crawler/internal/platform/ratelimit"
)

// BenchmarkFetchPages measures end-to-end fetch throughput against a local server.
// Run: go test -bench=BenchmarkFetchPages -benchmem -count=5
func BenchmarkFetchPages(b *testing.B) {
	var pages atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "User-agent: *\nAllow: /\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		pages.Add(1)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><p>%s</p></body></html>`, r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := crawler.DefaultConfig()
	cfg.AllowPrivateHosts = true
	cfg.PerHostDelay = 0
	cfg.MaxRetries = 0
	cfg.Concurrency = 8
	cfg.FetchTimeout = 5 * time.Second

	fetcher, err := crawler.NewHTTPFetcher(cfg, crawler.Options{
		Limiter: ratelimit.NewKeyed(10_000, 10_000, 64),
	})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	target := srv.URL + "/page"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := fetcher.Fetch(ctx, crawler.Request{URL: target})
		if res.Outcome != crawler.OutcomeFetched && res.Outcome != crawler.OutcomeNotModified {
			b.Fatalf("fetch failed: %v", res.Err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(pages.Load()), "server_pages")
}

// BenchmarkNormalizeURL measures canonicalization cost (frontier dedupe hot path).
func BenchmarkNormalizeURL(b *testing.B) {
	u, _ := crawler.PublicURL("https://example.com/a/b?x=1&utm_source=t")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := crawler.NormalizeURL(u); err != nil {
			b.Fatal(err)
		}
	}
}
