package crawler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// crawlSite serves a small site: an index linking to two pages, each linking
// back to the index. robots.txt allows everything so the test exercises the
// crawl rather than the policy.
type crawlSite struct {
	*httptest.Server
	mu   sync.Mutex
	hits map[string]int
}

func newCrawlSite(t *testing.T, extra map[string]string) *crawlSite {
	t.Helper()
	s := &crawlSite{hits: map[string]int{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		s.count("/robots.txt")
		fmt.Fprint(w, "User-agent: *\nAllow: /\nCrawl-delay: 0\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.count(r.URL.Path)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, `<html><body>
				<a href="/a">Page A</a>
				<a href="/b">Page B</a>
				<a href="/a?utm_source=test">Page A again, tracked</a>
				<a href="mailto:hello@example.com">Email us</a>
				<a href="/a" rel="nofollow">Unendorsed</a>
			</body></html>`)
		case "/a":
			fmt.Fprint(w, `<html><body><a href="/">Home</a><p>Alpha content</p></body></html>`)
		case "/b":
			fmt.Fprint(w, `<html><body><a href="/c">Deeper</a><p>Beta content</p></body></html>`)
		case "/c":
			fmt.Fprint(w, `<html><body><a href="/d">Deepest</a><p>Gamma content</p></body></html>`)
		case "/d":
			fmt.Fprint(w, `<html><body><p>Delta content</p></body></html>`)
		case "/gone":
			s.count("/gone")
			w.WriteHeader(http.StatusNotFound)
		default:
			body, ok := extra[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprint(w, body)
		}
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *crawlSite) count(path string) {
	s.mu.Lock()
	s.hits[path]++
	s.mu.Unlock()
}

func (s *crawlSite) hitCount(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

// runCrawl wires the real components together the way cmd/crawld does.
func runCrawl(t *testing.T, cfg Config, opts CrawlOptions) (Result, *MemoryStore) {
	t.Helper()
	store := NewMemoryStore()
	frontier := NewMemoryFrontier(1000)
	t.Cleanup(frontier.Close)

	fetcher, err := NewHTTPFetcher(cfg, Options{Store: store, Limiter: testLimiter()})
	if err != nil {
		t.Fatalf("NewHTTPFetcher: %v", err)
	}
	c, err := New(fetcher, frontier, store, cfg, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := c.Crawl(context.Background(), opts)
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	return res, store
}

func TestCrawlFollowsLinksAndStopsAtMaxDepth(t *testing.T) {
	site := newCrawlSite(t, nil)
	cfg := testConfig()
	res, store := runCrawl(t, cfg, CrawlOptions{
		Seeds: []string{site.URL + "/"},
		// Standard depth reaches two hops: /, then /a and /b, then /c.
		Depth:       DepthStandard,
		FollowLinks: true,
	})

	// /d is three hops from the seed, so a two-hop crawl must not reach it.
	if res.Fetched != 4 {
		t.Errorf("fetched = %d, want 4 (/, /a, /b, /c) — errors %v", res.Fetched, res.Errors)
	}
	if site.hitCount("/d") != 0 {
		t.Error("a page three hops out was fetched past the depth limit")
	}
	paths := storedPaths(store)
	for _, want := range []string{"/", "/a", "/b", "/c"} {
		if !paths[want] {
			t.Errorf("path %q was not stored; stored = %v", want, paths)
		}
	}
	// The tracked duplicate and the mailto: link are both correctly excluded.
	if paths["/?utm_source=test"] {
		t.Error("a tracking-parameter duplicate was crawled")
	}
	if res.Stats.StoppedBy != "frontier_drained" {
		t.Errorf("stopped by %q, want frontier_drained", res.Stats.StoppedBy)
	}
	// BytesRetained is reported to the caller and recorded per run, so it has to
	// actually count the stored bodies. It once read zero on every successful
	// crawl because the counter was declared but never incremented.
	if res.BytesRetained <= 0 {
		t.Errorf("bytes retained = %d, want the stored bodies to be counted", res.BytesRetained)
	}
	if res.Stats.BytesRetained != res.BytesRetained {
		t.Errorf("stats bytes = %d, want %d", res.Stats.BytesRetained, res.BytesRetained)
	}
	var wantBytes int64
	for _, d := range allStored(store) {
		wantBytes += int64(len(d.Body))
	}
	if res.BytesRetained != wantBytes {
		t.Errorf("bytes retained = %d, want %d (the sum of stored bodies)", res.BytesRetained, wantBytes)
	}
}

func TestCrawlReachesDeeperPagesAtHigherDepth(t *testing.T) {
	site := newCrawlSite(t, nil)
	cfg := testConfig()
	res, store := runCrawl(t, cfg, CrawlOptions{
		Seeds:       []string{site.URL + "/"},
		Depth:       DepthDeep,
		FollowLinks: true,
	})
	if res.Fetched != 5 {
		t.Errorf("fetched = %d, want 5 (the whole site) — stopped=%q errs=%v", res.Fetched, res.Stats.StoppedBy, res.Errors)
	}
	if !storedPaths(store)["/d"] {
		t.Error("a three-hop page was not reached at deep depth")
	}
}

func TestCrawlScreenDepthFetchesOnlySeeds(t *testing.T) {
	site := newCrawlSite(t, nil)
	cfg := testConfig()
	res, _ := runCrawl(t, cfg, CrawlOptions{
		Seeds:       []string{site.URL + "/"},
		Depth:       DepthScreen,
		FollowLinks: true,
	})
	// DepthScreen is the cheap pre-filter: the seed and nothing else, whatever
	// the page links to.
	if res.Fetched != 1 {
		t.Errorf("fetched = %d, want 1 at screen depth", res.Fetched)
	}
	if site.hitCount("/a") != 0 {
		t.Error("screen depth followed a link")
	}
}

func TestCrawlRespectsMaxPagesBudget(t *testing.T) {
	site := newCrawlSite(t, nil)
	cfg := testConfig()
	res, _ := runCrawl(t, cfg, CrawlOptions{
		Seeds:       []string{site.URL + "/"},
		Depth:       DepthDeep,
		FollowLinks: true,
		Budget:      Budget{MaxPages: 2},
	})
	if res.Fetched > 2 {
		t.Errorf("fetched = %d, want at most the 2-page budget", res.Fetched)
	}
	if res.Stats.StoppedBy != "max_pages" {
		t.Errorf("stopped by %q, want max_pages", res.Stats.StoppedBy)
	}
}

func TestCrawlRespectsMaxHostPagesBudget(t *testing.T) {
	site := newCrawlSite(t, nil)
	cfg := testConfig()
	res, _ := runCrawl(t, cfg, CrawlOptions{
		Seeds:        []string{site.URL + "/"},
		Depth:        DepthDeep,
		FollowLinks:  true,
		Budget:       Budget{MaxPages: 100, MaxHostPages: 2},
		PollInterval: time.Millisecond,
	})
	if res.Fetched > 2 {
		t.Errorf("fetched = %d, want at most 2 from a single host", res.Fetched)
	}
}

func TestCrawlWithoutFollowLinksFetchesOnlySeeds(t *testing.T) {
	site := newCrawlSite(t, nil)
	cfg := testConfig()
	res, _ := runCrawl(t, cfg, CrawlOptions{
		Seeds:        []string{site.URL + "/"},
		Depth:        DepthStandard,
		FollowLinks:  false,
		PollInterval: time.Millisecond,
	})
	if res.Fetched != 1 {
		t.Errorf("fetched = %d, want exactly the 1 seed", res.Fetched)
	}
	if site.hitCount("/a") != 0 {
		t.Error("links were followed even though FollowLinks was false")
	}
}

func TestCrawlRecordsFailedPagesWithoutAborting(t *testing.T) {
	site := newCrawlSite(t, nil)
	cfg := testConfig()
	res, _ := runCrawl(t, cfg, CrawlOptions{
		Seeds:        []string{site.URL + "/", site.URL + "/gone"},
		Depth:        Depth(1),
		FollowLinks:  true,
		PollInterval: time.Millisecond,
	})
	if res.Failed == 0 {
		t.Error("a 404 seed must be recorded as a failure")
	}
	if res.Fetched == 0 {
		t.Error("one bad seed must not stop the crawl")
	}
	if len(res.Errors) == 0 {
		t.Error("the failure detail must be reported")
	}
}

func TestCrawlSkipsRobotsDisallowedLinks(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "User-agent: *\nDisallow: /secret\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<a href="/secret/hidden">Hidden</a><a href="/open">Open</a>`)
	})
	mux.HandleFunc("/open", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "open page")
	})
	mux.HandleFunc("/secret/hidden", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "should never be fetched")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testConfig()
	res, store := runCrawl(t, cfg, CrawlOptions{
		Seeds:        []string{srv.URL + "/"},
		Depth:        Depth(1),
		FollowLinks:  true,
		PollInterval: time.Millisecond,
	})
	if res.Skipped == 0 {
		t.Error("a robots-disallowed link must be counted as skipped")
	}
	if storedPaths(store)["/secret/hidden"] {
		t.Error("a robots-disallowed page was fetched")
	}
	if !storedPaths(store)["/open"] {
		t.Error("an allowed page was not fetched")
	}
}

func TestCrawlSecondRunIsConditionalAndCheap(t *testing.T) {
	site := newCrawlSite(t, nil)
	cfg := testConfig()
	store := NewMemoryStore()
	frontier := NewMemoryFrontier(100)
	defer frontier.Close()

	fetcher, err := NewHTTPFetcher(cfg, Options{Store: store, Limiter: testLimiter()})
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(fetcher, frontier, store, cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}

	opts := CrawlOptions{Seeds: []string{site.URL + "/"}, Depth: Depth(1), FollowLinks: true, PollInterval: time.Millisecond}
	if _, err := c.Crawl(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	firstDocs := len(store.All())

	// The server does not send ETag or Last-Modified, so a repeat crawl re-fetches
	// but must not duplicate stored rows for identical content.
	if _, err := c.Crawl(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if got := len(store.All()); got != firstDocs {
		t.Errorf("stored %d documents after two identical crawls, want %d: unchanged content must not create new rows", got, firstDocs)
	}
}

func TestCrawlSeedsFromSitemap(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "User-agent: *\nAllow: /\n")
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>%s/one</loc></url>
  <url><loc>%s/two</loc></url>
</urlset>`, "http://"+r.Host, "http://"+r.Host)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "page %s", r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testConfig()
	res, store := runCrawl(t, cfg, CrawlOptions{
		Sitemaps:     []string{srv.URL + "/sitemap.xml"},
		Depth:        DepthScreen,
		PollInterval: time.Millisecond,
	})
	if res.Fetched != 2 {
		t.Errorf("fetched = %d, want the 2 sitemap URLs — errors %v", res.Fetched, res.Errors)
	}
	if !storedPaths(store)["/one"] || !storedPaths(store)["/two"] {
		t.Errorf("sitemap URLs were not crawled; stored = %v", storedPaths(store))
	}
}

func TestCrawlStopsOnContextCancellation(t *testing.T) {
	site := newCrawlSite(t, nil)
	cfg := testConfig()
	store := NewMemoryStore()
	frontier := NewMemoryFrontier(100)
	defer frontier.Close()

	fetcher, _ := NewHTTPFetcher(cfg, Options{Store: store, Limiter: testLimiter()})
	c, _ := New(fetcher, frontier, store, cfg, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Crawl(ctx, CrawlOptions{Seeds: []string{site.URL + "/"}, Depth: Depth(1), FollowLinks: true, PollInterval: time.Millisecond})
	if err == nil {
		t.Fatal("a cancelled context must stop the crawl")
	}
}

func TestCrawlRejectsEmptySeedList(t *testing.T) {
	cfg := testConfig()
	fetcher, _ := NewHTTPFetcher(cfg, Options{Limiter: testLimiter()})
	c, _ := New(fetcher, NewMemoryFrontier(10), NewMemoryStore(), cfg, Options{})
	if _, err := c.Crawl(context.Background(), CrawlOptions{}); err == nil {
		t.Error("a crawl with no seeds and no sitemaps must be refused")
	}
}

func TestCrawlExtractsTextForDownstreamUse(t *testing.T) {
	site := newCrawlSite(t, nil)
	cfg := testConfig()
	_, store := runCrawl(t, cfg, CrawlOptions{
		Seeds:        []string{site.URL + "/a"},
		Depth:        Depth(1),
		FollowLinks:  true,
		PollInterval: time.Millisecond,
	})
	for _, d := range store.All() {
		if d.URL == site.URL+"/a" && string(d.Body) == "" {
			t.Error("the body must be retained so the extractor can read it")
		}
		if d.ContentHash == "" {
			t.Error("every stored document needs a content hash")
		}
	}
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	cfg := testConfig()
	fetcher, _ := NewHTTPFetcher(cfg, Options{})
	if _, err := New(nil, NewMemoryFrontier(1), NewMemoryStore(), cfg, Options{}); err == nil {
		t.Error("a nil Fetcher must be refused")
	}
	if _, err := New(fetcher, nil, NewMemoryStore(), cfg, Options{}); err == nil {
		t.Error("a nil Frontier must be refused")
	}
	if _, err := New(fetcher, NewMemoryFrontier(1), nil, cfg, Options{}); err == nil {
		t.Error("a nil Store must be refused")
	}
}

// storedPaths indexes the stored documents by URL path, so a test asserts on
// which pages were crawled without hard-coding the test server's origin.
// allStored returns every document the store holds.
func allStored(store *MemoryStore) []Document {
	docs := store.All()
	out := make([]Document, 0, len(docs))
	for _, d := range docs {
		out = append(out, d)
	}
	return out
}

func storedPaths(store *MemoryStore) map[string]bool {
	paths := map[string]bool{}
	for _, d := range store.All() {
		u, err := url.Parse(d.URL)
		if err != nil {
			paths[d.URL] = true
			continue
		}
		paths[u.Path] = true
	}
	return paths
}
