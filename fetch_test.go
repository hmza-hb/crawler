package crawler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hmza-hb/crawler/internal/platform/ratelimit"
)

// testConfig returns a config that runs fast against a local test server.
func testConfig() Config {
	c := DefaultConfig()
	c.UserAgent = "UpvistaTestBot/1.0"
	c.Concurrency = 4
	c.PerHostDelay = 0
	c.FetchTimeout = 5 * time.Second
	c.MaxRetries = 0
	c.AllowPrivateHosts = true
	c.MaxCrawlTime = 30 * time.Second
	c.MaxPages = 100
	return c
}

func newTestFetcher(t *testing.T, cfg Config, store Store) *HTTPFetcher {
	t.Helper()
	f, err := NewHTTPFetcher(cfg, Options{Store: store, Clock: time.Now, Limiter: testLimiter()})
	if err != nil {
		t.Fatalf("NewHTTPFetcher: %v", err)
	}
	return f
}

// testLimiter removes the production one-request-per-second politeness delay,
// which is the right default against the public internet and pure dead time
// against a local test server.
func testLimiter() *ratelimit.Keyed {
	return ratelimit.NewKeyed(1000, 1000, 64)
}

func TestFetchStoresDocumentMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			fmt.Fprint(w, "User-agent: *\nAllow: /\n")
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
			fmt.Fprint(w, "<html><body><h1>Hello</h1></body></html>")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := testConfig()
	f := newTestFetcher(t, cfg, nil)
	res := f.Fetch(context.Background(), Request{URL: srv.URL + "/page"})

	if res.Outcome != OutcomeFetched {
		t.Fatalf("outcome = %q, want fetched (err %v)", res.Outcome, res.Err)
	}
	if res.Document.Status != 200 {
		t.Errorf("status = %d, want 200", res.Document.Status)
	}
	if res.Document.ETag != `"v1"` {
		t.Errorf("etag = %q, want %q", res.Document.ETag, `"v1"`)
	}
	if res.Document.LastModified.Year() != 2006 {
		t.Errorf("last-modified = %v, want 2006", res.Document.LastModified)
	}
	if res.Document.ContentType != "text/html" {
		t.Errorf("content type = %q, want text/html", res.Document.ContentType)
	}
	if res.Document.ContentHash == "" || res.Document.RawHash == "" {
		t.Error("both content and raw hashes must be set")
	}
	if !strings.Contains(string(res.Document.Body), "Hello") {
		t.Errorf("body = %q, want it to contain Hello", res.Document.Body)
	}
	if res.Document.Elapsed <= 0 {
		t.Error("elapsed must be measured")
	}
}

func TestFetchSendsHonestUserAgent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	cfg := testConfig()
	f := newTestFetcher(t, cfg, nil)
	if res := f.Fetch(context.Background(), Request{URL: srv.URL}); res.Outcome != OutcomeFetched {
		t.Fatalf("outcome = %q, want fetched", res.Outcome)
	}
	if got != cfg.UserAgent {
		t.Errorf("User-Agent = %q, want %q", got, cfg.UserAgent)
	}
}

func TestFetchRespectsRobotsDisallow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, "User-agent: *\nDisallow: /private\n")
			return
		}
		fmt.Fprint(w, "secret")
	}))
	defer srv.Close()

	f := newTestFetcher(t, testConfig(), nil)
	res := f.Fetch(context.Background(), Request{URL: srv.URL + "/private/thing"})
	if res.Outcome != OutcomeSkipped {
		t.Fatalf("outcome = %q, want skipped", res.Outcome)
	}
	if !strings.Contains(res.Document.StatusText, "robots") {
		t.Errorf("reason = %q, want it to mention robots", res.Document.StatusText)
	}
}

func TestFetchAllowsWhenNoRobotsFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, "open")
	}))
	defer srv.Close()

	f := newTestFetcher(t, testConfig(), nil)
	if res := f.Fetch(context.Background(), Request{URL: srv.URL + "/x"}); res.Outcome != OutcomeFetched {
		t.Fatalf("outcome = %q, want fetched when robots.txt is absent (err %v)", res.Outcome, res.Err)
	}
}

func TestFetchFailsClosedWhenRobotsIsUnreadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, "content")
	}))
	defer srv.Close()

	f := newTestFetcher(t, testConfig(), nil)
	res := f.Fetch(context.Background(), Request{URL: srv.URL + "/x"})
	if res.Outcome != OutcomeSkipped {
		t.Fatalf("outcome = %q, want skipped: an unreadable robots.txt is not permission", res.Outcome)
	}
}

func TestFetchSendsConditionalRequestAndReports304(t *testing.T) {
	var sawIfNoneMatch, sawIfModifiedSince atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, "User-agent: *\nAllow: /\n")
			return
		}
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			sawIfNoneMatch.Store(true)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if r.Header.Get("If-Modified-Since") != "" {
			sawIfModifiedSince.Store(true)
		}
		fmt.Fprint(w, "body")
	}))
	defer srv.Close()

	cfg := testConfig()
	store := NewMemoryStore()
	f := newTestFetcher(t, cfg, store)

	first := f.Fetch(context.Background(), Request{URL: srv.URL + "/p"})
	if first.Outcome != OutcomeFetched {
		t.Fatalf("first outcome = %q, want fetched", first.Outcome)
	}
	if _, err := store.Save(context.Background(), first.Document); err != nil {
		t.Fatalf("Save: %v", err)
	}

	second := f.Fetch(context.Background(), Request{URL: srv.URL + "/p"})
	if second.Outcome != OutcomeNotModified {
		t.Fatalf("second outcome = %q, want not_modified (err %v)", second.Outcome, second.Err)
	}
	if !sawIfNoneMatch.Load() && !sawIfModifiedSince.Load() {
		t.Error("the second request must be conditional")
	}
	if !second.Document.NotModified {
		t.Error("document must be flagged as not modified")
	}
}

func TestFetchFiltersUnwantedContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, "User-agent: *\nAllow: /\n")
			return
		}
		w.Header().Set("Content-Type", "image/png")
		fmt.Fprint(w, "\x89PNG binary")
	}))
	defer srv.Close()

	f := newTestFetcher(t, testConfig(), nil)
	res := f.Fetch(context.Background(), Request{URL: srv.URL + "/logo.png"})
	if res.Outcome != OutcomeFiltered {
		t.Fatalf("outcome = %q, want filtered", res.Outcome)
	}
	if len(res.Document.Body) != 0 {
		t.Errorf("a filtered body must not be retained, got %d bytes", len(res.Document.Body))
	}
	if !res.Document.Filtered {
		t.Error("document must be flagged as filtered")
	}
}

func TestFetchTruncatesOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, "User-agent: *\nAllow: /\n")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, strings.Repeat("a", 5000))
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxBytes = 1000
	f := newTestFetcher(t, cfg, nil)
	res := f.Fetch(context.Background(), Request{URL: srv.URL + "/big"})
	if res.Outcome != OutcomeFetched {
		t.Fatalf("outcome = %q, want fetched", res.Outcome)
	}
	if len(res.Document.Body) != 1000 {
		t.Errorf("body length = %d, want the 1000-byte cap", len(res.Document.Body))
	}
	if !res.Document.BodyTruncated {
		t.Error("truncation must be flagged, not silent")
	}
}

func TestFetchRetriesRetryableStatusThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, "User-agent: *\nAllow: /\n")
			return
		}
		if hits.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "recovered")
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxRetries = 3
	f, err := NewHTTPFetcher(cfg, Options{Store: nil, Limiter: testLimiter()})
	if err != nil {
		t.Fatal(err)
	}
	// Collapse the backoff so the test does not sleep for seconds.
	f.policy.Base = time.Millisecond
	f.policy.Max = 2 * time.Millisecond

	res := f.Fetch(context.Background(), Request{URL: srv.URL + "/flaky"})
	if res.Outcome != OutcomeFetched {
		t.Fatalf("outcome = %q, want fetched after retries (err %v)", res.Outcome, res.Err)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("server saw %d requests, want 3", got)
	}
}

func TestFetchDoesNotRetry404(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, "User-agent: *\nAllow: /\n")
			return
		}
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxRetries = 3
	f, _ := NewHTTPFetcher(cfg, Options{Limiter: testLimiter()})
	f.policy.Base = time.Millisecond

	res := f.Fetch(context.Background(), Request{URL: srv.URL + "/missing"})
	if res.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", res.Outcome)
	}
	if res.Retryable {
		t.Error("a 404 must not be reported as retryable")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server saw %d requests, want exactly 1 for a 404", got)
	}
}

func TestFetchRecordsRedirectChain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			fmt.Fprint(w, "User-agent: *\nAllow: /\n")
		case "/old":
			http.Redirect(w, r, "/new", http.StatusMovedPermanently)
		case "/new":
			fmt.Fprint(w, "arrived")
		}
	}))
	defer srv.Close()

	f := newTestFetcher(t, testConfig(), nil)
	res := f.Fetch(context.Background(), Request{URL: srv.URL + "/old"})
	if res.Outcome != OutcomeFetched {
		t.Fatalf("outcome = %q, want fetched (err %v)", res.Outcome, res.Err)
	}
	if !strings.HasSuffix(res.Document.URL, "/new") {
		t.Errorf("final URL = %q, want the /new destination", res.Document.URL)
	}
	if len(res.Document.RedirectChain) == 0 {
		t.Error("the redirect chain must be recorded")
	}
}

func TestFetchRefusesRedirectLoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, "User-agent: *\nAllow: /\n")
			return
		}
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer srv.Close()

	cfg := testConfig()
	f := newTestFetcher(t, cfg, nil)
	res := f.Fetch(context.Background(), Request{URL: srv.URL + "/loop"})
	if res.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %q, want failed on a redirect loop", res.Outcome)
	}
}

func TestFetchConvertsLatin1ToUTF8(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, "User-agent: *\nAllow: /\n")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=ISO-8859-1")
		// "Café" in latin-1, with a Windows-1252 curly apostrophe.
		w.Write([]byte{'C', 'a', 'f', 0xE9, ' ', 0x92, 'x'})
	}))
	defer srv.Close()

	f := newTestFetcher(t, testConfig(), nil)
	res := f.Fetch(context.Background(), Request{URL: srv.URL + "/latin"})
	if res.Outcome != OutcomeFetched {
		t.Fatalf("outcome = %q, want fetched", res.Outcome)
	}
	got := string(res.Document.Body)
	if !strings.Contains(got, "Café") {
		t.Errorf("body = %q, want it to decode to UTF-8 Café", got)
	}
	if !strings.Contains(got, "’") {
		t.Errorf("body = %q, want the cp1252 apostrophe mapped", got)
	}
}

func TestFetchRejectsNonHTTPSchemes(t *testing.T) {
	f := newTestFetcher(t, testConfig(), nil)
	for _, u := range []string{"file:///etc/passwd", "gopher://example.com/", "ftp://example.com/x"} {
		res := f.Fetch(context.Background(), Request{URL: u})
		if res.Outcome == OutcomeFetched {
			t.Errorf("%s was fetched; non-HTTP schemes must be refused", u)
		}
	}
}

func TestFetchSendsIdentifiableAcceptHeader(t *testing.T) {
	var accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, "User-agent: *\nAllow: /\n")
			return
		}
		accept = r.Header.Get("Accept")
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	f := newTestFetcher(t, testConfig(), nil)
	f.Fetch(context.Background(), Request{URL: srv.URL})
	if !strings.Contains(accept, "text/html") {
		t.Errorf("Accept = %q, want it to advertise the formats we process", accept)
	}
}
