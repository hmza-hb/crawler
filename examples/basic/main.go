//go:build ignore

// Basic embeds the crawler against an in-memory frontier for a single-process crawl.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/hmza-hb/crawler"
	"github.com/hmza-hb/crawler/internal/platform/ratelimit"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <seed-url>\n", os.Args[0])
		os.Exit(2)
	}
	seed := os.Args[1]

	cfg := crawler.DefaultConfig()
	cfg.MaxPages = 20
	cfg.Concurrency = 4
	cfg.FetchTimeout = 15 * time.Second

	mem := crawler.NewMemoryFrontier(10_000)
	store := crawler.NewMemoryStore()

	fetcher, err := crawler.NewHTTPFetcher(cfg, crawler.Options{
		Store:   store,
		Limiter: ratelimit.NewKeyed(2, 4, 4096),
	})
	if err != nil {
		log.Fatal(err)
	}

	c, err := crawler.New(fetcher, mem, store, cfg, crawler.Options{})
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	res, err := c.Crawl(ctx, crawler.CrawlOptions{
		Seeds:       []string{seed},
		FollowLinks: true,
		Depth:       crawler.DepthStandard,
		RunID:       fmt.Sprintf("example-%d", time.Now().Unix()),
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("fetched=%d skipped=%d failed=%d\n",
		res.Fetched, res.Skipped, res.Failed)
}
