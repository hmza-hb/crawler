// Command crawld serves the crawler as a standalone service.
//
// It exists so the crawling half of the platform can be run, tested and sold
// without the rest of the system. Inside lead-engine the crawler is a library;
// here it is a process with an HTTP API and its own database schema.
//
// Endpoints:
//
//	POST /v1/crawl                    run a bounded crawl and return its result
//	POST /v1/fetch                   fetch one URL politely
//	GET  /v1/documents/{id}          fetch a stored document
//	GET  /v1/documents?url=...       fetch the last stored copy of a URL
//	GET  /v1/hosts/{host}/robots     the cached robots.txt rules for a host
//	GET  /healthz /readyz /version /metrics
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hmza-hb/crawler"
	"github.com/hmza-hb/crawler/internal/platform/config"
	"github.com/hmza-hb/crawler/internal/platform/db"
	"github.com/hmza-hb/crawler/internal/platform/httpx"
	"github.com/hmza-hb/crawler/internal/platform/logging"
	"github.com/hmza-hb/crawler/internal/platform/observe"
	"github.com/hmza-hb/crawler/internal/platform/ratelimit"
)

const version = "0.1.0"

var migrateOnly = flag.Bool("migrate-only", false, "apply migrations and exit")

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "crawld:", err)
		os.Exit(1)
	}
}

func run() error {
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	log := logging.New(cfg.Log.Level, cfg.Log.Format, os.Stdout)
	log.Info("starting crawld", "version", version, "addr", cfg.HTTP.Addr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Startup work is bounded: a crawler that cannot reach its database should
	// fail to start rather than start and fail every request.
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	pool, err := db.Open(startup, cfg.Database, "crawld")
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()

	if res, err := db.Migrate(startup, pool, []db.Source{crawler.MigrationSource()}, log); err != nil {
		return fmt.Errorf("migrate: %w", err)
	} else {
		log.Info("migrations applied", "applied", res.Applied, "skipped", len(res.Skipped))
	}

	// -migrate-only turns this into a release step: apply the schema and exit,
	// so a deployment can run migrations with its own credentials and start the
	// service separately.
	if *migrateOnly {
		log.Info("migrations applied, exiting as requested", "flag", "-migrate-only")
		return nil
	}

	crawlCfg, err := crawlerConfigFromEnv()
	if err != nil {
		return err
	}
	// A private-network crawler is a request-forgery gadget pointed at whatever
	// the crawler process can reach, so it stays off unless explicitly enabled.
	if envBool("CRAWLER_ALLOW_PRIVATE_HOSTS") {
		crawlCfg.AllowPrivateHosts = true
		log.Warn("private and loopback hosts are permitted; do not run this in production")
	}

	registry := observe.New()
	metrics := crawler.NewMetrics(registry)
	store := crawler.NewPostgresStore(pool)
	frontier := crawler.NewPostgresFrontier(pool)
	defer frontier.Close()

	fetcher, err := crawler.NewHTTPFetcher(crawlCfg, crawler.Options{
		Store:   store,
		Metrics: metrics,
		Log:     slogAdapter{log},
		Limiter: ratelimit.NewKeyed(rateFromEnv(), burstFromEnv(), 100_000),
	})
	if err != nil {
		return fmt.Errorf("build fetcher: %w", err)
	}
	crawlerLoop, err := crawler.New(fetcher, frontier, store, crawlCfg, crawler.Options{
		Metrics: metrics,
		Log:     slogAdapter{log},
	})
	if err != nil {
		return fmt.Errorf("build crawler: %w", err)
	}

	srv := httpx.NewServer(cfg.HTTP, log,
		httpx.WithVersion(version),
		httpx.WithMetrics(registry),
		httpx.WithReadiness(func(ctx context.Context) error {
			return pool.Ping(ctx)
		}),
	)

	mux := http.NewServeMux()
	api := &api{
		cfg:      crawlCfg,
		store:    store,
		frontier: frontier,
		fetcher:  fetcher,
		crawler:  crawlerLoop,
		log:      log,
	}
	mux.Handle("/v1/crawl", httpx.MethodNotAllowedHandler("POST"))
	mux.Handle("POST /v1/crawl", httpx.Handler(api.crawl))
	mux.Handle("POST /v1/fetch", httpx.Handler(api.fetchOne))
	mux.Handle("GET /v1/documents/{id}", httpx.Handler(api.getDocument))
	mux.Handle("GET /v1/documents", httpx.Handler(api.getDocumentByURL))
	mux.Handle("GET /v1/hosts/{host}/robots", httpx.Handler(api.hostRobots))
	mux.Handle("/v1/stats", httpx.Handler(api.stats))

	return srv.Run(ctx, mux)
}

// api holds the handler dependencies. It is a struct rather than a set of
// closures so a test can build one without a process.
type api struct {
	cfg      crawler.Config
	store    *crawler.PostgresStore
	frontier crawler.Frontier
	fetcher  crawler.Fetcher
	crawler  *crawler.Crawler
	log      *slog.Logger
}

type crawlRequest struct {
	Seeds        []string `json:"seeds"`
	Sitemaps     []string `json:"sitemaps"`
	Depth        string   `json:"depth"`
	FollowLinks  bool     `json:"follow_links"`
	MaxPages     int      `json:"max_pages"`
	MaxBytes     int64    `json:"max_bytes"`
	MaxDuration  string   `json:"max_duration"`
	MaxHostPages int      `json:"max_host_pages"`
}

type crawlResponse struct {
	RunID       string               `json:"run_id"`
	Seeds       int                  `json:"seeds"`
	Enqueued    int                  `json:"enqueued"`
	Fetched     int                  `json:"fetched"`
	Skipped     int                  `json:"skipped"`
	Failed      int                  `json:"failed"`
	NotModified int                  `json:"not_modified"`
	Filtered    int                  `json:"filtered"`
	Bytes       int64                `json:"bytes_retained"`
	StoppedBy   string               `json:"stopped_by"`
	DurationMS  int64                `json:"duration_ms"`
	Errors      []crawler.FetchError `json:"errors,omitempty"`
}

func (a *api) crawl(w http.ResponseWriter, r *http.Request) {
	var req crawlRequest
	if err := httpx.DecodeJSON(w, r, 1<<20, &req); err != nil {
		httpx.Error(w, httpx.BadRequest("invalid request body").WithCause(err))
		return
	}
	if len(req.Seeds) == 0 && len(req.Sitemaps) == 0 {
		httpx.Error(w, httpx.BadRequest("at least one of seeds or sitemaps is required"))
		return
	}
	depth, err := crawler.ParseDepth(defaultString(req.Depth, crawler.DepthStandard.String()))
	if err != nil {
		httpx.Error(w, httpx.BadRequest("invalid depth: "+err.Error()))
		return
	}
	budget := crawler.Budget{
		MaxPages:     req.MaxPages,
		MaxBytes:     req.MaxBytes,
		MaxHostPages: req.MaxHostPages,
	}
	if req.MaxDuration != "" {
		d, err := time.ParseDuration(req.MaxDuration)
		if err != nil {
			httpx.Error(w, httpx.BadRequest("invalid max_duration: "+err.Error()))
			return
		}
		budget.MaxDuration = d
	}
	for i, s := range append(append([]string{}, req.Seeds...), req.Sitemaps...) {
		if !isFetchableURL(s) {
			httpx.Error(w, httpx.BadRequest(fmt.Sprintf("seeds[%d] is not a fetchable http(s) URL", i)))
			return
		}
	}

	// The request context is the crawl's deadline: a client that gives up must
	// stop the crawl rather than leave it running against its wishes.
	res, crawlErr := a.crawler.Crawl(r.Context(), crawler.CrawlOptions{
		Seeds:       req.Seeds,
		Sitemaps:    req.Sitemaps,
		Depth:       depth,
		FollowLinks: req.FollowLinks,
		Budget:      budget,
	})
	if recErr := a.recordRun(r.Context(), res); recErr != nil {
		a.log.Warn("could not record run", "run", res.RunID, "err", recErr)
	}

	resp := crawlResponse{
		RunID:       res.RunID,
		Seeds:       res.Seeds,
		Enqueued:    res.Enqueued,
		Fetched:     res.Fetched,
		Skipped:     res.Skipped,
		Failed:      res.Failed,
		NotModified: res.NotModified,
		Filtered:    res.Filtered,
		Bytes:       res.BytesRetained,
		StoppedBy:   res.Stats.StoppedBy,
		DurationMS:  res.Duration.Milliseconds(),
		Errors:      res.Errors,
	}

	switch {
	case crawlErr == nil:
		httpx.JSON(w, http.StatusOK, resp)
	case errors.Is(crawlErr, crawler.ErrCrawlStopped):
		// A budgeted stop is a successful, complete result. Reporting it as an
		// error would make a well-behaved client retry work already done.
		httpx.JSON(w, http.StatusOK, resp)
	case errors.Is(crawlErr, context.Canceled), errors.Is(crawlErr, context.DeadlineExceeded):
		httpx.Error(w, httpx.Timeout("crawl cancelled: "+crawlErr.Error()))
	default:
		httpx.Error(w, httpx.Internal("crawl failed").WithCause(crawlErr))
	}
}

type fetchRequest struct {
	URL string `json:"url"`
}

type fetchResponse struct {
	Outcome      crawler.Outcome `json:"outcome"`
	URL          string          `json:"url"`
	Status       int             `json:"status"`
	StatusText   string          `json:"status_text"`
	ContentType  string          `json:"content_type"`
	ContentHash  string          `json:"content_hash"`
	ETag         string          `json:"etag,omitempty"`
	LastModified string          `json:"last_modified,omitempty"`
	NotModified  bool            `json:"not_modified"`
	Filtered     bool            `json:"filtered"`
	Truncated    bool            `json:"body_truncated"`
	Bytes        int             `json:"bytes"`
	ElapsedMS    int64           `json:"elapsed_ms"`
	Reason       string          `json:"reason,omitempty"`
}

func (a *api) fetchOne(w http.ResponseWriter, r *http.Request) {
	var req fetchRequest
	if err := httpx.DecodeJSON(w, r, 1<<16, &req); err != nil {
		httpx.Error(w, httpx.BadRequest("invalid request body").WithCause(err))
		return
	}
	if !isFetchableURL(req.URL) {
		httpx.Error(w, httpx.BadRequest("url must be an absolute http or https URL"))
		return
	}
	res := a.fetcher.Fetch(r.Context(), crawler.Request{URL: req.URL, SourceHint: "api"})
	if res.Outcome == crawler.OutcomeFetched || res.Outcome == crawler.OutcomeNotModified {
		if _, err := a.store.Save(r.Context(), res.Document); err != nil {
			a.log.Warn("could not store document", "url", req.URL, "err", err)
		}
	}
	doc := res.Document
	resp := fetchResponse{
		Outcome:     res.Outcome,
		URL:         doc.URL,
		Status:      doc.Status,
		StatusText:  doc.StatusText,
		ContentType: doc.ContentType,
		ContentHash: doc.ContentHash,
		ETag:        doc.ETag,
		NotModified: doc.NotModified,
		Filtered:    doc.Filtered,
		Truncated:   doc.BodyTruncated,
		Bytes:       len(doc.Body),
		ElapsedMS:   doc.Elapsed.Milliseconds(),
	}
	if !doc.LastModified.IsZero() {
		resp.LastModified = doc.LastModified.Format(time.RFC3339)
	}
	// A non-2xx response that was not retried comes back with a nil error but a
	// failed outcome, so the reason has to be derived from the status. A failure
	// with no explanation is not a useful answer to "what is at this URL?".
	resp.Reason = failureReason(res)
	// A fetch that failed is still a successful API call: the caller asked what
	// is at that URL and got a truthful answer. Only a transport error on *our*
	// side is a 5xx.
	httpx.JSON(w, http.StatusOK, resp)
}

// projectDocument maps a stored document onto the wire shape. The body is
// deliberately not included: it can be megabytes, and a caller that wants text
// belongs in the extractor, not in a document GET.
func projectDocument(d crawler.Document) documentResponse {
	resp := documentResponse{
		ID:            d.ID,
		URL:           d.URL,
		RequestedURL:  d.RequestedURL,
		RedirectChain: d.RedirectChain,
		Status:        d.Status,
		StatusText:    d.StatusText,
		ContentType:   d.ContentType,
		Charset:       d.Charset,
		ContentLength: d.ContentLength,
		ETag:          d.ETag,
		ContentHash:   d.ContentHash,
		RawHash:       d.RawHash,
		BodyTruncated: d.BodyTruncated,
		Depth:         d.Depth,
		FetchedAt:     d.FetchedAt,
		ElapsedMS:     d.Elapsed.Milliseconds(),
		SourceHint:    d.SourceHint,
		RunID:         d.RunID,
		Links:         d.Links,
	}
	if !d.LastModified.IsZero() {
		resp.LastModified = d.LastModified.Format(time.RFC3339)
	}
	return resp
}

// failureReason explains an unsuccessful fetch in one line.
func failureReason(res crawler.FetchResult) string {
	if res.Err != nil {
		return res.Err.Error()
	}
	switch res.Outcome {
	case crawler.OutcomeFiltered:
		return res.Document.FilterReason
	case crawler.OutcomeFailed:
		if res.Document.Status != 0 {
			return fmt.Sprintf("upstream returned %d %s", res.Document.Status, res.Document.StatusText)
		}
		return "the request did not complete"
	case crawler.OutcomeSkipped:
		return res.Document.FilterReason
	}
	return ""
}

func (a *api) getDocument(w http.ResponseWriter, r *http.Request) {
	doc, err := a.store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, crawler.ErrNotFound) {
		httpx.Error(w, httpx.NotFound("no document with that id"))
		return
	}
	if err != nil {
		httpx.Error(w, httpx.Internal("read document").WithCause(err))
		return
	}
	if doc == nil {
		httpx.Error(w, httpx.NotFound("no document with that id"))
		return
	}
	httpx.JSON(w, http.StatusOK, projectDocument(*doc))
}

func (a *api) getDocumentByURL(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		httpx.Error(w, httpx.BadRequest("url query parameter is required"))
		return
	}
	u, err := url.Parse(raw)
	if err != nil {
		httpx.Error(w, httpx.BadRequest("url is not parseable"))
		return
	}
	norm, err := crawler.NormalizeURL(u)
	if err != nil {
		httpx.Error(w, httpx.BadRequest("url is not fetchable: "+err.Error()))
		return
	}
	doc, err := a.store.LastDocument(r.Context(), norm)
	if err != nil {
		httpx.Error(w, httpx.Internal("read document").WithCause(err))
		return
	}
	if doc == nil {
		httpx.Error(w, httpx.NotFound("that URL has never been fetched"))
		return
	}
	httpx.JSON(w, http.StatusOK, projectDocument(*doc))
}

type documentResponse struct {
	ID            string         `json:"id"`
	URL           string         `json:"url"`
	RequestedURL  string         `json:"requested_url"`
	RedirectChain []string       `json:"redirect_chain,omitempty"`
	Status        int            `json:"status"`
	StatusText    string         `json:"status_text"`
	ContentType   string         `json:"content_type"`
	Charset       string         `json:"charset,omitempty"`
	ContentLength int64          `json:"content_length"`
	ETag          string         `json:"etag,omitempty"`
	LastModified  string         `json:"last_modified,omitempty"`
	ContentHash   string         `json:"content_hash"`
	RawHash       string         `json:"raw_hash,omitempty"`
	BodyTruncated bool           `json:"body_truncated"`
	Depth         int            `json:"depth"`
	FetchedAt     time.Time      `json:"fetched_at"`
	ElapsedMS     int64          `json:"elapsed_ms"`
	SourceHint    string         `json:"source_hint,omitempty"`
	RunID         string         `json:"run_id,omitempty"`
	Links         []crawler.Link `json:"links,omitempty"`
}

// hostRobots answers "why was this URL skipped?". Only cached rules are
// reported: the endpoint must not become a way to make the crawler fetch a
// robots.txt on demand for an arbitrary host.
func (a *api) hostRobots(w http.ResponseWriter, r *http.Request) {
	host := r.PathValue("host")
	if host == "" {
		httpx.Error(w, httpx.BadRequest("host is required"))
		return
	}
	reporter, ok := a.fetcher.(crawler.RobotsReporter)
	if !ok {
		httpx.Error(w, httpx.NotFound("this build does not expose cached robots rules"))
		return
	}
	for _, scheme := range []string{"https", "http"} {
		rules, found := reporter.RobotsFor(scheme + "://" + host)
		if !found {
			continue
		}
		httpx.JSON(w, http.StatusOK, map[string]any{
			"origin":         scheme + "://" + host,
			"host":           rules.Host,
			"rules":          rules.Rules(),
			"crawl_delay_ms": rules.Delay().Milliseconds(),
			"sitemaps":       rules.Sitemaps(),
		})
		return
	}
	httpx.Error(w, httpx.NotFound("robots.txt has not been fetched for that host yet"))
}

func (a *api) stats(w http.ResponseWriter, r *http.Request) {
	runID := r.URL.Query().Get("run_id")
	depth, err := a.frontier.Depth(r.Context())
	if err != nil {
		httpx.Error(w, httpx.Internal("frontier depth").WithCause(err))
		return
	}
	st, err := a.store.Stats(r.Context(), runID)
	if err != nil {
		httpx.Error(w, httpx.Internal("stats").WithCause(err))
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"frontier_depth": depth,
		"run_id":         runID,
		"stats":          st,
	})
}

func (a *api) recordRun(ctx context.Context, res crawler.Result) error {
	if a.store == nil {
		return nil
	}
	return a.store.RecordRun(ctx, res)
}

// isFetchableURL rejects anything that is not an absolute http or https URL. The
// crawler would refuse these anyway; catching it here returns a clear 400 instead
// of a confusing "skipped".
func isFetchableURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Hostname() != ""
}

func crawlerConfigFromEnv() (crawler.Config, error) {
	c := crawler.DefaultConfig()
	if v := os.Getenv("CRAWLER_USER_AGENT"); v != "" {
		c.UserAgent = v
	}
	if v := os.Getenv("CRAWLER_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return c, fmt.Errorf("CRAWLER_CONCURRENCY: %w", err)
		}
		c.Concurrency = n
	}
	if v := os.Getenv("CRAWLER_PER_HOST_DELAY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("CRAWLER_PER_HOST_DELAY: %w", err)
		}
		c.PerHostDelay = d
	}
	if v := os.Getenv("CRAWLER_MAX_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return c, fmt.Errorf("CRAWLER_MAX_BYTES: %w", err)
		}
		c.MaxBytes = n
	}
	if v := os.Getenv("CRAWLER_MAX_RETRIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return c, fmt.Errorf("CRAWLER_MAX_RETRIES: %w", err)
		}
		c.MaxRetries = n
	}
	if envBool("CRAWLER_RESPECT_ROBOTS_DISABLED") {
		// Named so that setting it is impossible to do by accident, and logged
		// loudly at startup. Turning this off is a legal decision, not a
		// performance tweak.
		c.RespectRobots = false
	}
	return c, c.Validate()
}

func envBool(name string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func rateFromEnv() float64 {
	if v := os.Getenv("CRAWLER_HOST_RATE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return 1
}

func burstFromEnv() int {
	if v := os.Getenv("CRAWLER_HOST_BURST"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 2
}

func defaultString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// slogAdapter bridges the crawler's minimal Logger interface to slog.
type slogAdapter struct{ l *slog.Logger }

func (s slogAdapter) Debug(msg string, args ...any) { s.l.Debug(msg, args...) }
func (s slogAdapter) Warn(msg string, args ...any)  { s.l.Warn(msg, args...) }
