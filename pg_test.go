package crawler_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hmza-hb/crawler"
	"github.com/hmza-hb/crawler/internal/platform/db"
	"github.com/hmza-hb/crawler/internal/platform/observe"
	"github.com/hmza-hb/crawler/internal/platform/ratelimit"
	"github.com/hmza-hb/crawler/internal/platform/testutil"
)

// newPool returns a pool on an isolated schema with the crawler's migrations
// applied. It skips the test when no database is configured, so `go test ./...`
// works on a machine without one.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping Postgres test")
	}
	pool := testutil.Postgres(t)
	// The same runner production uses, so a migration that works here works
	// there. An already-applied file is skipped, which is what makes this
	// idempotent across tests sharing a schema.
	res, err := db.Migrate(context.Background(), pool, []db.Source{crawler.MigrationSource()}, nil)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Logf("migrations: %d applied, %d skipped", res.Applied, len(res.Skipped))
	return pool
}

func fastConfig() crawler.Config {
	c := crawler.DefaultConfig()
	c.UserAgent = "UpvistaTestBot/1.0"
	c.Concurrency = 4
	c.PerHostDelay = 0
	c.MaxRetries = 0
	c.AllowPrivateHosts = true
	c.MaxCrawlTime = 30 * time.Second
	c.MaxPages = 100
	return c
}

func TestPostgresStoreRoundTrip(t *testing.T) {
	pool := newPool(t)
	store := crawler.NewPostgresStore(pool)
	ctx := context.Background()

	doc := crawler.Document{
		URL:           "https://example.com/one",
		RequestedURL:  "https://example.com/one",
		Status:        200,
		ContentType:   "text/html",
		ContentLength: 11,
		ETag:          `"v1"`,
		Body:          []byte("hello world"),
		ContentHash:   "hash-1",
		RawHash:       "raw-1",
		Depth:         0,
		FetchedAt:     time.Now().UTC().Truncate(time.Millisecond),
		UserAgent:     "test",
		RunID:         "run-1",
		Links:         []crawler.Link{{Href: "https://example.com/two", Text: "Two", Internal: true}},
	}
	saved, err := store.Save(ctx, doc)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if saved.ID == "" {
		t.Fatal("Save must assign an ID")
	}

	got, err := store.Get(ctx, saved.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.URL != doc.URL || string(got.Body) != "hello world" {
		t.Errorf("round trip lost data: %+v", got)
	}
	if got.ETag != `"v1"` {
		t.Errorf("etag = %q, want %q", got.ETag, `"v1"`)
	}
	if len(got.Links) != 1 || got.Links[0].Href != "https://example.com/two" {
		t.Errorf("links = %+v, want the one saved link", got.Links)
	}

	if _, err := store.Get(ctx, "does-not-exist"); !errors.Is(err, crawler.ErrNotFound) {
		t.Errorf("Get(unknown) = %v, want ErrNotFound", err)
	}
}

func TestPostgresStoreUpdatesInPlaceOnRefetch(t *testing.T) {
	pool := newPool(t)
	store := crawler.NewPostgresStore(pool)
	ctx := context.Background()

	url := "https://example.com/stable"
	if _, err := store.Save(ctx, crawler.Document{URL: url, RequestedURL: url, ContentHash: "same", Status: 200, FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(ctx, crawler.Document{URL: url, RequestedURL: url, ContentHash: "same", Status: 200, FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM crawler_documents WHERE url = $1`, url).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("rows = %d, want 1: a refetch must update in place", n)
	}
}

func TestPostgresFrontierDeduplicatesAcrossCompletion(t *testing.T) {
	pool := newPool(t)
	frontier := crawler.NewPostgresFrontier(pool)
	defer frontier.Close()
	ctx := context.Background()

	if err := frontier.Reset(ctx, "run-x"); err != nil {
		t.Fatal(err)
	}
	items := []crawler.Item{
		{URL: "https://example.com/a", Key: "a", RunID: "run-x", Priority: 1},
		{URL: "https://example.com/b", Key: "b", RunID: "run-x", Priority: 2},
		{URL: "https://example.com/a", Key: "a", RunID: "run-x", Priority: 9},
	}
	added, err := frontier.Enqueue(ctx, items)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if added != 2 {
		t.Errorf("added = %d, want 2 (the duplicate key must be refused)", added)
	}

	claimed, err := frontier.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed %d, want 2", len(claimed))
	}
	// Highest priority first: the duplicate's priority 9 must not have won.
	if claimed[0].Key != "b" {
		t.Errorf("first claim = %q, want b (priority 2)", claimed[0].Key)
	}

	if err := frontier.Complete(ctx, claimed, nil); err != nil {
		t.Fatal(err)
	}
	// The key is still known to the run after completion, so re-enqueueing it
	// is refused. Without this, A -> B -> A crawls in circles forever.
	again, err := frontier.Enqueue(ctx, []crawler.Item{{URL: "https://example.com/a", Key: "a", RunID: "run-x"}})
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("re-enqueued %d items after completion, want 0", again)
	}
}

func TestPostgresFrontierLeaseExpires(t *testing.T) {
	pool := newPool(t)
	frontier := crawler.NewPostgresFrontier(pool)
	defer frontier.Close()
	ctx := context.Background()

	if err := frontier.Reset(ctx, "run-lease"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	frontier.SetClock(func() time.Time { return now })

	if _, err := frontier.Enqueue(ctx, []crawler.Item{{URL: "https://example.com/x", Key: "x", RunID: "run-lease"}}); err != nil {
		t.Fatal(err)
	}
	first, _ := frontier.Claim(ctx, 5, time.Second)
	if len(first) != 1 {
		t.Fatalf("first claim = %d, want 1", len(first))
	}
	held, _ := frontier.Claim(ctx, 5, time.Second)
	if len(held) != 0 {
		t.Errorf("second claim within the lease = %d, want 0", len(held))
	}
	// A worker that dies must not strand the item.
	now = now.Add(2 * time.Second)
	reclaimed, _ := frontier.Claim(ctx, 5, time.Second)
	if len(reclaimed) != 1 {
		t.Errorf("claim after lease expiry = %d, want 1", len(reclaimed))
	}
	if reclaimed[0].Attempts != 2 {
		t.Errorf("attempts = %d, want 2", reclaimed[0].Attempts)
	}
}

func TestPostgresFrontierPendingReportsBackoff(t *testing.T) {
	pool := newPool(t)
	frontier := crawler.NewPostgresFrontier(pool)
	defer frontier.Close()
	ctx := context.Background()

	if err := frontier.Reset(ctx, "run-pending"); err != nil {
		t.Fatal(err)
	}
	wait, pending, err := frontier.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Errorf("an empty frontier reported pending (wait %v)", wait)
	}

	now := time.Now()
	frontier.SetClock(func() time.Time { return now })
	if _, err := frontier.Enqueue(ctx, []crawler.Item{{URL: "https://example.com/y", Key: "y", RunID: "run-pending"}}); err != nil {
		t.Fatal(err)
	}
	claimed, _ := frontier.Claim(ctx, 5, time.Minute)
	if len(claimed) != 1 {
		t.Fatalf("claim = %d, want 1", len(claimed))
	}
	// Leased: pending, but with no knowable deadline, so a supervisor must
	// rely on its own wake-up rather than sleeping for a fixed interval.
	wait, pending, err = frontier.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !pending || wait != 0 {
		t.Errorf("leased item: pending=%v wait=%v, want pending with no delay", pending, wait)
	}
}

// TestCrawlOverPostgres exercises the whole stack against a real database: a
// crawl must store what it fetched and must not loop on a page that links back
// to itself.
func TestCrawlOverPostgres(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	var (
		mu   sync.Mutex
		hits = map[string]int{}
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "User-agent: *\nAllow: /\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, `<a href="/a">A</a><a href="/a">A again</a><a href="/b">B</a>`)
		case "/a":
			fmt.Fprint(w, `<a href="/">Home</a><p>alpha</p>`)
		case "/b":
			fmt.Fprint(w, `<a href="/">Home</a><p>beta</p>`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := fastConfig()
	store := crawler.NewPostgresStore(pool)
	frontier := crawler.NewPostgresFrontier(pool)
	defer frontier.Close()
	if err := frontier.Reset(ctx, ""); err != nil {
		t.Fatal(err)
	}

	metrics := crawler.NewMetrics(observe.New())
	fetcher, err := crawler.NewHTTPFetcher(cfg, crawler.Options{
		Store:   store,
		Metrics: metrics,
		Limiter: ratelimit.NewKeyed(1000, 1000, 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := crawler.New(fetcher, frontier, store, cfg, crawler.Options{Metrics: metrics})
	if err != nil {
		t.Fatal(err)
	}

	runID := fmt.Sprintf("run-%d", time.Now().UnixNano())
	res, err := c.Crawl(ctx, crawler.CrawlOptions{
		Seeds:        []string{srv.URL + "/"},
		RunID:        runID,
		Depth:        crawler.DepthStandard,
		FollowLinks:  true,
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if res.Fetched != 3 {
		t.Errorf("fetched = %d, want 3 (a page that links back must not be re-crawled) — errors %v", res.Fetched, res.Errors)
	}
	mu.Lock()
	got := hits["/"]
	mu.Unlock()
	if got != 1 {
		t.Errorf("/ was requested %d times, want exactly 1", got)
	}

	// The store must return what the crawl wrote.
	last, err := store.LastDocument(ctx, srv.URL+"/")
	if err != nil {
		t.Fatalf("LastDocument: %v", err)
	}
	if last == nil || last.ContentHash == "" {
		t.Error("the crawled document was not persisted with a content hash")
	}
	if err := store.RecordRun(ctx, res); err != nil {
		t.Errorf("RecordRun: %v", err)
	}
}
