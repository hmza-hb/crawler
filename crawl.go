package crawler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"
)

// ErrCrawlStopped is returned by Crawl when a budget, the context, or a fatal
// dependency error ends the run. A stopped crawl is not a failed crawl:
// everything fetched before the stop is valid and already stored.
var ErrCrawlStopped = errors.New("crawler: crawl stopped")

// CrawlOptions configure one Crawl call.
type CrawlOptions struct {
	// Seeds are the URLs to start from. They are normalised, checked against
	// host policy, and enqueued at the highest priority.
	Seeds []string
	// Sitemaps are sitemap URLs read for additional seeds.
	Sitemaps []string
	// RunID scopes stored rows. Empty generates one.
	RunID string
	// Depth is the research depth; it caps how far links are followed.
	Depth Depth
	// Budget caps the run. Zero fields fall back to the limits in Config.
	Budget Budget
	// FollowLinks enables link discovery. Off means the seeds are fetched and
	// nothing else, which is the cheap path a screening pass uses.
	FollowLinks bool
	// OnDocument is called for every stored document, inline. It must not
	// block; the pipeline passes an enqueue, not a network call.
	OnDocument func(Document) error
	// OnError is called for every non-fatal fetch failure.
	OnError func(FetchError)
	// PollInterval is how long Claim waits before re-checking when the frontier
	// has nothing claimable but workers are still running. Zero selects 250ms.
	PollInterval time.Duration
}

// Result summarises a finished crawl. It is valid even when Crawl returns
// ErrCrawlStopped.
type Result struct {
	RunID         string        `json:"run_id"`
	Seeds         int           `json:"seeds"`
	Enqueued      int           `json:"enqueued"`
	Fetched       int           `json:"fetched"`
	NotModified   int           `json:"not_modified"`
	Skipped       int           `json:"skipped"`
	Filtered      int           `json:"filtered"`
	Failed        int           `json:"failed"`
	LinksSeen     int           `json:"links_seen"`
	BytesRetained int64         `json:"bytes_retained"`
	Stats         Stats         `json:"stats"`
	Duration      time.Duration `json:"-"`
	Errors        []FetchError  `json:"-"`

	// mu guards the counters and Errors, which workers update in place. It is
	// a pointer so a Result stays copyable and JSON-marshallable.
	mu *sync.Mutex
}

// add applies a counter update under the result lock.
func (r *Result) add(f func(*Result)) {
	if r.mu == nil {
		f(r)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f(r)
}

// reset initialises the parts of a Result that are not settable by a caller.
func (r *Result) reset() {
	r.RunID = newID()
	r.Errors = []FetchError{}
	r.mu = &sync.Mutex{}
}

// Crawler is the frontier-driven loop. It is safe for concurrent use.
type Crawler struct {
	fetch     Fetcher
	frontier  Frontier
	store     Store
	extractor LinkExtractor
	cfg       Config
	metrics   *Metrics
	log       Logger
	now       func() time.Time

	mu       sync.Mutex
	state    RunState
	inflight int
	// wake carries a one-slot signal from a worker to the supervisor. Without
	// it the supervisor only notices newly discovered links on its next poll,
	// which adds that interval to the latency of every BFS level.
	wake chan struct{}
}

// New builds a Crawler over the given dependencies. Nothing is global: a caller
// wanting a second independent crawler passes a second Store and Frontier.
func New(fetch Fetcher, frontier Frontier, store Store, cfg Config, opt Options) (*Crawler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if fetch == nil || frontier == nil || store == nil {
		return nil, errors.New("crawler: fetch, frontier and store are all required")
	}
	log := opt.Log
	if log == nil {
		log = nopLogger{}
	}
	now := opt.Clock
	if now == nil {
		now = time.Now
	}
	return &Crawler{
		fetch:     fetch,
		frontier:  frontier,
		store:     store,
		extractor: HTMLExtractor{MaxLinks: DefaultMaxLinks},
		cfg:       cfg,
		metrics:   opt.Metrics,
		log:       log,
		now:       now,
		wake:      make(chan struct{}, 1),
	}, nil
}

// Crawl runs until the frontier drains or a budget is spent.
func (c *Crawler) Crawl(ctx context.Context, opts CrawlOptions) (Result, error) {
	if len(opts.Seeds) == 0 && len(opts.Sitemaps) == 0 {
		return Result{}, errors.New("crawler: a crawl needs at least one seed or sitemap")
	}
	depth := opts.Depth
	if depth <= 0 {
		depth = DepthStandard
	}
	budget := opts.Budget
	if budget.MaxPages == 0 {
		budget.MaxPages = c.cfg.MaxPages
	}
	if budget.MaxDuration == 0 {
		budget.MaxDuration = c.cfg.MaxCrawlTime
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 250 * time.Millisecond
	}

	c.mu.Lock()
	c.state = RunState{StartedAt: c.now(), PerHost: map[string]int{}, Consecutive: map[string]int{}}
	c.inflight = 0
	c.mu.Unlock()

	res := Result{mu: &sync.Mutex{}}
	res.reset()
	if opts.RunID != "" {
		res.RunID = opts.RunID
	}
	res.Seeds = len(opts.Seeds)
	started := c.now()

	stoppedBy, err := c.run(ctx, opts, depth, budget, &res)
	finished := c.now()

	stats, _ := c.store.Stats(ctx, res.RunID)
	stats.RunID = res.RunID
	stats.StartedAt = started.UTC()
	stats.FinishedAt = finished.UTC()
	stats.DurationMillis = finished.Sub(started).Milliseconds()
	stats.StoppedBy = stoppedBy
	stats.Documents = res.Fetched
	stats.NotModified = res.NotModified
	stats.Skipped = res.Skipped
	stats.Filtered = res.Filtered
	stats.Failed = res.Failed
	stats.BytesRetained = res.BytesRetained
	res.Stats = stats
	res.Duration = finished.Sub(started)

	if err != nil && !errors.Is(err, context.Canceled) {
		return res, fmt.Errorf("%w (%s): %w", ErrCrawlStopped, stoppedBy, err)
	}
	return res, err
}

// run seeds the frontier and then supervises the worker pool.
func (c *Crawler) run(ctx context.Context, opts CrawlOptions, depth Depth, budget Budget, res *Result) (string, error) {
	if err := c.seed(ctx, res.RunID, opts, depth, res); err != nil {
		return "seed_error", err
	}

	workers := c.cfg.Concurrency
	if workers < 1 {
		workers = 1
	}
	// FetchTimeout bounds one request, not a run, so a lease must outlive a slow
	// response plus its retries, or a healthy item gets reclaimed while in flight.
	lease := c.cfg.FetchTimeout*time.Duration(c.cfg.MaxRetries+2) + time.Minute

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		badStreak int
	)
	// Every exit path must wait for the workers. Returning while a worker is
	// still saving means Crawl hands the caller a Result that keeps changing
	// underneath it, and leaves a goroutine writing to a store the caller may
	// already have closed.
	defer wg.Wait()

	for {
		if reason, spent := budget.exhausted(c.snapshot(), c.now()); spent {
			return reason, nil
		}
		if err := ctx.Err(); err != nil {
			return "cancelled", err
		}

		batch, err := c.frontier.Claim(ctx, workers, lease)
		if err != nil {
			return "frontier_error", err
		}
		if len(batch) == 0 {
			// Ask the frontier what it is waiting for instead of inferring it
			// from an empty claim. An empty claim cannot distinguish "done" from
			// "a worker holds the only remaining item", and guessing wrong
			// truncates a crawl mid-tree.
			wait, pending, err := c.frontier.Pending(ctx)
			if err != nil {
				return "frontier_error", err
			}
			if !pending && c.inFlight() == 0 {
				return "frontier_drained", nil
			}
			if c.inFlight() == 0 && wait > 0 {
				// Every remaining item is inside a backoff window. Sleeping for
				// exactly that long is both faster and more accurate than
				// polling, and it cannot be starved by a lost wake-up.
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return "cancelled", ctx.Err()
				case <-c.wake:
					timer.Stop()
				case <-timer.C:
				}
				continue
			}
			select {
			case <-ctx.Done():
				return "cancelled", ctx.Err()
			case <-c.wake:
			case <-time.After(opts.PollInterval):
			}
			continue
		}
		for _, item := range batch {
			wg.Add(1)
			c.addInFlight(1)
			go func(it Item) {
				// One deferred call, registered first so it runs last: the
				// in-flight count must not drop until after the worker has
				// enqueued its discovered links, or the supervisor sees an
				// empty frontier with no work and declares the crawl drained.
				defer func() {
					c.addInFlight(-1)
					wg.Done()
				}()
				retryLater := c.process(ctx, it, opts, budget, res)
				// A failed item is released with backoff rather than dropped, so
				// a transient 503 gets another attempt inside this run.
				var releaseErr error
				if retryLater {
					releaseErr = errRelease
				}
				_ = c.frontier.Complete(ctx, []Item{it}, releaseErr)
				// Tell the supervisor there may be new work, so it does not have
				// to wait out a poll interval to notice this item's links.
				c.signal()
				mu.Lock()
				if retryLater {
					badStreak++
				} else {
					badStreak = 0
				}
				mu.Unlock()
			}(item)
		}

		mu.Lock()
		streak := badStreak
		mu.Unlock()
		if streak >= workers*3 {
			// Every recent item failed. The dependency is sick; spending the
			// rest of the budget on 503s helps nobody.
			return "repeated_fetch_failures", nil
		}
	}
}

// errRelease tells the frontier to back an item off rather than drop it.
var errRelease = errors.New("crawler: release for retry")

// seed enqueues the starting URLs and any URLs the sitemaps advertise.
func (c *Crawler) seed(ctx context.Context, runID string, opts CrawlOptions, depth Depth, res *Result) error {
	items := make([]Item, 0, len(opts.Seeds))
	seen := map[string]bool{}

	add := func(raw, hint string) {
		u, err := url.Parse(raw)
		if err != nil {
			c.metrics.incError("seed_parse")
			return
		}
		norm, err := NormalizeURL(u)
		if err != nil {
			c.metrics.incError("seed_policy")
			return
		}
		key := dedupeKey(u)
		if seen[key] {
			return
		}
		seen[key] = true
		items = append(items, Item{
			URL:        norm,
			Key:        key,
			Depth:      0,
			Priority:   (depth.MaxDepth() + 1) * 10,
			SourceHint: hint,
			RunID:      runID,
		})
	}

	for _, s := range opts.Seeds {
		add(s, "seed")
	}
	for _, sm := range opts.Sitemaps {
		entries, index, err := c.readSitemap(ctx, sm)
		if err != nil {
			// A missing sitemap is normal. Losing the seeds it advertised is
			// not fatal, so the run continues with what it has.
			c.log.Warn("sitemap unreadable; continuing with seeds", "sitemap", sm, "err", err)
			continue
		}
		for _, e := range entries {
			if e.Loc == "" {
				continue
			}
			add(e.Loc, "sitemap")
		}
		for _, e := range index {
			// A sitemap index points at more sitemaps; follow one level so a
			// sharded sitemap is not silently reduced to its first shard.
			if e.Loc == "" {
				continue
			}
			more, moreIndex, err := c.readSitemap(ctx, e.Loc)
			if err != nil {
				c.log.Warn("nested sitemap unreadable", "sitemap", e.Loc, "err", err)
				continue
			}
			for _, m := range more {
				if m.Loc != "" {
					add(m.Loc, "sitemap-index")
				}
			}
			_ = moreIndex
		}
	}
	n, err := c.frontier.Enqueue(ctx, items)
	if err != nil {
		return fmt.Errorf("crawler: enqueue seeds: %w", err)
	}
	res.add(func(r *Result) { r.Enqueued += n })
	return nil
}

// readSitemap fetches and parses a sitemap without storing it as a document:
// a sitemap is a directory, not content.
func (c *Crawler) readSitemap(ctx context.Context, sitemapURL string) ([]SitemapEntry, []SitemapIndexEntry, error) {
	res := c.fetch.Fetch(ctx, Request{URL: sitemapURL, SourceHint: "sitemap"})
	if res.Outcome != OutcomeFetched {
		if res.Err != nil {
			return nil, nil, res.Err
		}
		return nil, nil, fmt.Errorf("crawler: sitemap %s: %s", sitemapURL, res.Document.StatusText)
	}
	// The fetcher already decoded the body; a sitemap is text bounded by
	// MaxBytes, so streaming the in-memory copy is cheap.
	return ParseSitemap(bytes.NewReader(res.Document.Body))
}

// process fetches one item, stores what came back, and enqueues its links. It
// reports whether the item should be retried within this run.
func (c *Crawler) process(ctx context.Context, item Item, opts CrawlOptions, budget Budget, res *Result) bool {
	// Only the counter updates take the result lock. Holding it across the
	// fetch would serialise the whole worker pool behind one request.
	if !c.cfg.AllowsHost(parseOrEmpty(item.URL)) {
		c.metrics.incSkipped("host_policy")
		res.add(func(r *Result) { r.Skipped++ })
		return false
	}
	// Both allowances are claimed together, immediately before the request.
	// Checking them at claim time lets every worker in a batch read the same
	// pre-fetch counters and collectively overshoot the limit by a whole batch.
	host := hostOf(item.URL)
	if !c.claimHostSlot(item.URL, budget.MaxHostPages) {
		c.log.Debug("host page budget spent; skipping url", "url", item.URL, "host", host)
		c.metrics.incSkipped("host_budget")
		res.add(func(r *Result) { r.Skipped++ })
		return false
	}
	if !c.claimPageSlot(budget.MaxPages) {
		// The run-wide page budget is gone. Hand the host reservation back so
		// a later host is not starved by a page slot we never used.
		c.releaseHostSlot(host)
		c.log.Debug("page budget spent; stopping", "url", item.URL)
		c.metrics.incSkipped("page_budget")
		res.add(func(r *Result) { r.Skipped++ })
		return false
	}

	result := c.fetch.Fetch(ctx, Request{
		URL:        item.URL,
		Depth:      item.Depth,
		Priority:   item.Priority,
		SourceHint: item.SourceHint,
	})

	switch result.Outcome {
	case OutcomeSkipped:
		c.metrics.incSkipped("policy")
		res.add(func(r *Result) { r.Skipped++ })
		return false

	case OutcomeNotModified:
		c.bumpPages(0)
		res.add(func(r *Result) { r.NotModified++ })
		// No new row: the stored copy is still current, so a daily crawl of a
		// static site must not grow the table by a row per URL per day.
		return false

	case OutcomeFiltered:
		c.metrics.incSkipped("content_type")
		res.add(func(r *Result) { r.Filtered++ })
		return false

	case OutcomeFailed:
		c.metrics.incError("fetch")
		res.add(func(r *Result) {
			r.Failed++
			if len(r.Errors) < maxRecordedErrors {
				r.Errors = append(r.Errors, FetchError{
					URL:     result.Document.URL,
					Status:  result.Document.Status,
					Reason:  result.Document.StatusText,
					Attempt: result.Document.Attempt,
					Err:     result.Err,
				})
			}
		})
		if opts.OnError != nil {
			opts.OnError(FetchError{
				URL:    result.Document.URL,
				Status: result.Document.Status,
				Reason: result.Document.StatusText,
				Err:    result.Err,
			})
		}
		return result.Retryable
	}

	doc := result.Document
	doc.Depth = item.Depth
	doc.SourceHint = item.SourceHint
	doc.RunID = res.RunID

	if opts.FollowLinks && !doc.BodyTruncated && IsHTMLContentType(doc.ContentType) {
		if links := c.extractor.Links(doc, DefaultMaxLinks); len(links) > 0 {
			doc.Links = links
		}
	}

	saved, err := c.store.Save(ctx, doc)
	if err != nil {
		c.metrics.incError("store")
		res.add(func(r *Result) { r.Failed++ })
		c.log.Warn("store save failed", "url", doc.URL, "err", err)
		return true
	}

	c.bumpPages(int64(len(saved.Body)))
	res.add(func(r *Result) {
		r.Fetched++
		r.BytesRetained += int64(len(saved.Body))
	})
	c.metrics.incPage(int64(len(saved.Body)), saved.ContentType)

	if opts.OnDocument != nil {
		if err := opts.OnDocument(saved); err != nil {
			c.log.Warn("OnDocument handler failed", "url", saved.URL, "err", err)
		}
	}
	if opts.FollowLinks {
		c.enqueueLinks(ctx, saved, opts, res)
	}
	return false
}

// maxRecordedErrors bounds the error list on a Result. A crawl that fails a
// thousand times should report a thousand failures in the count and a hundred
// in the detail; the full set belongs in logs.
const maxRecordedErrors = 100

// enqueueLinks applies depth, host policy, and the caller's LinkFilter before
// adding anything to the queue. Every item carries the run ID: a durable frontier
// scopes its dedupe keys by run, and an item without one would land in a
// different scope from the seed that discovered it, letting a page be re-crawled
// through its own back-link. A link that fails a policy is not an error.
func (c *Crawler) enqueueLinks(ctx context.Context, doc Document, opts CrawlOptions, res *Result) {
	maxDepth := opts.Depth.MaxDepth()
	if doc.Depth >= maxDepth {
		return
	}
	items := make([]Item, 0, len(doc.Links))
	for _, l := range doc.Links {
		if l.NoFollow {
			continue
		}
		u, err := url.Parse(l.Href)
		if err != nil {
			continue
		}
		if !c.cfg.AllowsHost(u) {
			continue
		}
		if c.cfg.LinkFilter != nil && !c.cfg.LinkFilter(u) {
			continue
		}
		items = append(items, Item{
			URL: l.Href,
			Key: dedupeKey(u),
			// The run ID must travel with every item. A durable frontier scopes
			// its dedupe keys by run, so an item without one lands in a
			// different scope from the seed that discovered it, and a page
			// becomes re-crawlable through its own back-link.
			RunID: res.RunID,
			Depth: doc.Depth + 1,
			// Shallower links first: breadth gives more distinct evidence per
			// page budget than depth does.
			Priority:   (maxDepth - doc.Depth - 1) * 10,
			SourceHint: "link:" + doc.URL,
		})
	}
	if len(items) == 0 {
		res.add(func(r *Result) { r.LinksSeen += len(doc.Links) })
		return
	}
	n, err := c.frontier.Enqueue(ctx, items)
	if err != nil {
		c.log.Warn("enqueue links failed", "url", doc.URL, "err", err)
		return
	}
	res.add(func(r *Result) {
		r.LinksSeen += len(doc.Links)
		r.Enqueued += n
	})
}

// claimHostSlot reserves one page of a host's allowance. It is the only place
// PerHost is written, which is what makes the limit a limit rather than a
// suggestion. A non-positive limit means unlimited.
// signal nudges the supervisor without blocking. The channel holds one token,
// so several discoveries in a row collapse into a single wake-up.
func (c *Crawler) signal() {
	if c.wake == nil {
		return
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Crawler) claimHostSlot(rawURL string, maxPerHost int) bool {
	if maxPerHost <= 0 {
		return true
	}
	host := hostOf(rawURL)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.PerHost[host] >= maxPerHost {
		return false
	}
	c.state.PerHost[host]++
	return true
}

// claimPageSlot reserves one page of the run-wide budget. A non-positive limit
// means unlimited. PagesFetched counts reservations, not completions, so the
// supervisor stops claiming work as soon as the budget is gone.
func (c *Crawler) claimPageSlot(maxPages int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if maxPages > 0 && c.state.PagesFetched >= maxPages {
		return false
	}
	c.state.PagesFetched++
	return true
}

// releaseHostSlot hands back a host reservation taken for work that never ran.
func (c *Crawler) releaseHostSlot(host string) {
	if host == "" {
		return
	}
	c.mu.Lock()
	if c.state.PerHost[host] > 0 {
		c.state.PerHost[host]--
	}
	c.mu.Unlock()
}

// bumpPages records the byte cost of a fetch. The page itself was already
// counted by claimPageSlot.
func (c *Crawler) bumpPages(bytes int64) {
	c.mu.Lock()
	c.state.BytesRetained += bytes
	c.mu.Unlock()
}

// snapshot copies the run state so budget checks read it without holding the
// lock across a fetch.
func (c *Crawler) snapshot() *RunState {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.state
	s.PerHost = make(map[string]int, len(c.state.PerHost))
	for k, v := range c.state.PerHost {
		s.PerHost[k] = v
	}
	return &s
}

func (c *Crawler) inFlight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inflight
}

func (c *Crawler) addInFlight(n int) {
	c.mu.Lock()
	c.inflight += n
	c.mu.Unlock()
}

// hostOf returns the hostname of a URL, or the raw string when it will not
// parse. Budget accounting must never panic on a bad URL.
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Hostname()
}

func parseOrEmpty(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		return &url.URL{}
	}
	return u
}

// SlogLogger adapts a *slog.Logger to the crawler's Logger interface.
type SlogLogger struct{ L *slog.Logger }

func (s SlogLogger) Debug(msg string, args ...any) { s.L.Debug(msg, args...) }
func (s SlogLogger) Warn(msg string, args ...any)  { s.L.Warn(msg, args...) }
