package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hmza-hb/crawler"
	"github.com/hmza-hb/crawler/internal/platform/db"
	"github.com/hmza-hb/crawler/internal/platform/httpx"
	"github.com/hmza-hb/crawler/internal/platform/logging"
	"github.com/hmza-hb/crawler/internal/platform/ratelimit"
	"github.com/hmza-hb/crawler/internal/platform/testutil"
)

// testServer builds the real api on an isolated schema and returns an httptest
// server in front of it, so the handlers are exercised exactly as the binary
// wires them.
func testServer(t *testing.T) (*httptest.Server, *pgxpool.Pool) {
	t.Helper()
	if os.Getenv("TEST_DATABASE_URL") == "" && os.Getenv("DATABASE_URL") == "" {
		t.Skip("no test database configured")
	}
	pool := testutil.Postgres(t)
	if _, err := db.Migrate(context.Background(), pool, []db.Source{crawler.MigrationSource()}, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := crawler.DefaultConfig()
	cfg.AllowPrivateHosts = true
	cfg.PerHostDelay = 0
	cfg.MaxRetries = 0
	cfg.Concurrency = 2

	store := crawler.NewPostgresStore(pool)
	frontier := crawler.NewPostgresFrontier(pool)
	t.Cleanup(frontier.Close)
	if err := frontier.Reset(context.Background(), ""); err != nil {
		t.Fatal(err)
	}

	fetcher, err := crawler.NewHTTPFetcher(cfg, crawler.Options{
		Store:   store,
		Limiter: ratelimit.NewKeyed(1000, 1000, 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := crawler.New(fetcher, frontier, store, cfg, crawler.Options{})
	if err != nil {
		t.Fatal(err)
	}

	a := &api{
		cfg:      cfg,
		store:    store,
		frontier: frontier,
		fetcher:  fetcher,
		crawler:  loop,
		log:      logging.New("error", "json", io.Discard),
	}

	mux := http.NewServeMux()
	mux.Handle("POST /v1/crawl", httpx.Handler(a.crawl))
	mux.Handle("POST /v1/fetch", httpx.Handler(a.fetchOne))
	mux.Handle("GET /v1/documents/{id}", httpx.Handler(a.getDocument))
	mux.Handle("GET /v1/documents", httpx.Handler(a.getDocumentByURL))
	mux.Handle("GET /v1/hosts/{host}/robots", httpx.Handler(a.hostRobots))
	mux.Handle("/v1/stats", httpx.Handler(a.stats))

	srv := httptest.NewServer(httpx.Chain(mux,
		httpx.Recover(slog.New(slog.NewTextHandler(io.Discard, nil))),
		httpx.RequestID("X-Request-ID"),
	))
	t.Cleanup(srv.Close)
	return srv, pool
}

// site serves a tiny two-page site with permissive robots.txt.
func site(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "User-agent: *\nDisallow: /private\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, `<a href="/about">About</a><a href="/private/x">Secret</a>`)
		case "/about":
			fmt.Fprint(w, `<p>We build things.</p>`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func postJSON(t *testing.T, base, path string, body any) (*http.Response, []byte) {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(base+path, "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func TestCrawldCrawlEndpoint(t *testing.T) {
	srv, _ := testServer(t)
	target := site(t)

	resp, body := postJSON(t, srv.URL, "/v1/crawl", map[string]any{
		"seeds":        []string{target.URL + "/"},
		"depth":        "standard",
		"follow_links": true,
		"max_pages":    5,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var got crawlResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v (body %s)", err, body)
	}
	if got.RunID == "" {
		t.Error("a run id must be returned")
	}
	if got.Fetched < 2 {
		t.Errorf("fetched = %d, want at least the seed and /about", got.Fetched)
	}
	if got.StoppedBy == "" {
		t.Error("the run must say why it stopped")
	}
}

func TestCrawldCrawlRequiresSeeds(t *testing.T) {
	srv, _ := testServer(t)
	resp, body := postJSON(t, srv.URL, "/v1/crawl", map[string]any{"depth": "standard"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", resp.StatusCode, body)
	}
}

func TestCrawldCrawlRejectsNonHTTPSchemes(t *testing.T) {
	srv, _ := testServer(t)
	for _, bad := range []string{"file:///etc/passwd", "gopher://example.com", "not-a-url"} {
		resp, body := postJSON(t, srv.URL, "/v1/crawl", map[string]any{"seeds": []string{bad}})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("seed %q: status = %d, want 400; body = %s", bad, resp.StatusCode, body)
		}
	}
}

func TestCrawldFetchEndpointStoresDocument(t *testing.T) {
	srv, _ := testServer(t)
	target := site(t)

	resp, body := postJSON(t, srv.URL, "/v1/fetch", map[string]any{"url": target.URL + "/about"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var got fetchResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Outcome != crawler.OutcomeFetched {
		t.Fatalf("outcome = %q, want fetched (reason %q)", got.Outcome, got.Reason)
	}
	if got.ContentType != "text/html" {
		t.Errorf("content type = %q, want text/html", got.ContentType)
	}
	if got.ContentHash == "" {
		t.Error("a content hash must be returned so callers can dedupe")
	}
	if got.Bytes == 0 {
		t.Error("the byte count must be reported")
	}
}

func TestCrawldFetchReportsFailureTruthfully(t *testing.T) {
	srv, _ := testServer(t)
	target := site(t)

	resp, body := postJSON(t, srv.URL, "/v1/fetch", map[string]any{"url": target.URL + "/nope"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: a 404 upstream is a valid answer", resp.StatusCode)
	}
	var got fetchResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Outcome != crawler.OutcomeFailed {
		t.Errorf("outcome = %q, want failed", got.Outcome)
	}
	if got.Status != 404 {
		t.Errorf("status = %d, want 404", got.Status)
	}
	if got.Reason == "" {
		t.Error("a failure must carry a reason")
	}
}

func TestCrawldFetchRejectsBadURL(t *testing.T) {
	srv, _ := testServer(t)
	resp, body := postJSON(t, srv.URL, "/v1/fetch", map[string]any{"url": "file:///etc/passwd"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", resp.StatusCode, body)
	}
}

func TestCrawldDocumentLookupByURL(t *testing.T) {
	srv, _ := testServer(t)
	target := site(t)

	postJSON(t, srv.URL, "/v1/fetch", map[string]any{"url": target.URL + "/about"})

	u, _ := url.Parse(target.URL + "/about")
	norm, err := crawler.NormalizeURL(u)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(srv.URL + "/v1/documents?url=" + url.QueryEscape(norm))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got documentResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ID == "" {
		t.Error("the stored document must have an id")
	}
	if got.ContentHash == "" {
		t.Error("the stored document must carry a content hash")
	}

	// And by id.
	resp2, err := http.Get(srv.URL + "/v1/documents/" + got.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("Get by id: status = %d, want 200", resp2.StatusCode)
	}
}

func TestCrawldDocumentNotFound(t *testing.T) {
	srv, _ := testServer(t)

	resp, err := http.Get(srv.URL + "/v1/documents/missing-id")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}

	resp2, err := http.Get(srv.URL + "/v1/documents?url=https://example.com/never-fetched")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("status by url = %d, want 404", resp2.StatusCode)
	}
}

func TestCrawldRobotsEndpointReflectsPolicy(t *testing.T) {
	srv, _ := testServer(t)
	target := site(t)

	host := mustHost(t, target.URL)
	// Nothing cached yet.
	resp, err := http.Get(srv.URL + "/v1/hosts/" + host + "/robots")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("before any fetch: status = %d, want 404", resp.StatusCode)
	}

	// A fetch populates the cache. Ask for a disallowed path so robots.txt is
	// actually consulted and cached.
	postJSON(t, srv.URL, "/v1/fetch", map[string]any{"url": target.URL + "/private/x"})
	resp2, err := http.Get(srv.URL + "/v1/hosts/" + host + "/robots")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; robots should now be cached", resp2.StatusCode)
	}
	var got struct {
		Origin string   `json:"origin"`
		Host   string   `json:"host"`
		Rules  []string `json:"rules"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Host == "" {
		t.Error("the host must be reported")
	}
	found := false
	for _, r := range got.Rules {
		// Rules() emits a canonical lowercase form.
		if strings.EqualFold(r, "disallow: /private") {
			found = true
		}
	}
	if !found {
		t.Errorf("rules = %v, want one for \"disallow: /private\"", got.Rules)
	}
}

func TestCrawldStats(t *testing.T) {
	srv, _ := testServer(t)
	target := site(t)
	postJSON(t, srv.URL, "/v1/crawl", map[string]any{
		"seeds": []string{target.URL + "/"}, "follow_links": true, "max_pages": 5,
	})

	resp, err := http.Get(srv.URL + "/v1/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		FrontierDepth int `json:"frontier_depth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.FrontierDepth != 0 {
		t.Errorf("frontier depth = %d, want 0 after the run drained", got.FrontierDepth)
	}
}

func TestCrawldCrawlBudgetStopsItself(t *testing.T) {
	srv, _ := testServer(t)
	target := site(t)

	resp, body := postJSON(t, srv.URL, "/v1/crawl", map[string]any{
		"seeds": []string{target.URL + "/"}, "follow_links": true, "max_pages": 1,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var got crawlResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Fetched > 1 {
		t.Errorf("fetched = %d, want at most the 1-page budget", got.Fetched)
	}
	if got.StoppedBy != "max_pages" && got.StoppedBy != "frontier_drained" {
		t.Errorf("stopped by %q, want max_pages or frontier_drained", got.StoppedBy)
	}
}

func TestCrawldRejectsMalformedJSON(t *testing.T) {
	srv, _ := testServer(t)
	resp, err := http.Post(srv.URL+"/v1/crawl", "application/json", bytes.NewReader([]byte("{not json")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}
