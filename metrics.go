package crawler

import (
	"time"

	"github.com/hmza-hb/crawler/internal/platform/observe"
)

// Metrics are the crawler's counters and histograms. Names are stable: they are
// part of the module's contract with whoever operates it.
var (
	requestsTotal   = "crawler_requests_total"
	bytesTotal      = "crawler_response_bytes_total"
	latencySeconds  = "crawler_fetch_duration_seconds"
	frontierDepth   = "crawler_frontier_depth"
	crawlPagesTotal = "crawler_pages_total"
	skippedTotal    = "crawler_skipped_total"
	errorsTotal     = "crawler_errors_total"
)

// Metrics wraps the platform registry with the crawler's own series. A nil
// *Metrics is valid and does nothing, so the crawler can run with no telemetry.
type Metrics struct {
	requests   *observe.Counter
	bytes      *observe.Counter
	latency    *observe.Histogram
	depth      *observe.Gauge
	pages      *observe.Counter
	skipped    *observe.Counter
	errors     *observe.Counter
	registered bool
}

// NewMetrics registers the crawler's series on reg.
func NewMetrics(reg *observe.Registry) *Metrics {
	if reg == nil {
		return &Metrics{}
	}
	return &Metrics{
		requests:   reg.Counter(requestsTotal, "HTTP responses by outcome", "outcome"),
		bytes:      reg.Counter(bytesTotal, "Response bytes retained", "content_type"),
		latency:    reg.Histogram(latencySeconds, "Fetch latency", observe.DefBuckets, "outcome"),
		depth:      reg.Gauge(frontierDepth, "URLs waiting in the frontier", "run"),
		pages:      reg.Counter(crawlPagesTotal, "Documents stored", "content_type"),
		skipped:    reg.Counter(skippedTotal, "URLs skipped before fetching", "reason"),
		errors:     reg.Counter(errorsTotal, "Fetch errors", "reason"),
		registered: true,
	}
}

func (m *Metrics) incFetch(outcome, _ string) {
	if m == nil || !m.registered {
		return
	}
	m.requests.Inc(outcome)
}

func (m *Metrics) observeLatency(d time.Duration) {
	if m == nil || !m.registered {
		return
	}
	m.latency.Observe(d.Seconds(), "fetch")
}

func (m *Metrics) incSkipped(reason string) {
	if m == nil || !m.registered {
		return
	}
	m.skipped.Inc(reason)
}

func (m *Metrics) incError(reason string) {
	if m == nil || !m.registered {
		return
	}
	m.errors.Inc(reason)
}

func (m *Metrics) incPage(bytes int64, contentType string) {
	if m == nil || !m.registered {
		return
	}
	m.pages.Inc(contentType)
	if bytes > 0 {
		m.bytes.Add(float64(bytes), contentType)
	}
}

func (m *Metrics) setDepth(runID string, n int) {
	if m == nil || !m.registered {
		return
	}
	m.depth.Set(float64(n), runID)
}

// Budget is the cost envelope of a crawl. It is enforced, not advisory: the
// loop checks it before every fetch and stops when it is spent. This is the
// mechanism that keeps a run from quietly costing 400x its estimate.
type Budget struct {
	// MaxPages caps documents fetched. Zero means unlimited.
	MaxPages int
	// MaxBytes caps total retained body bytes. Zero means unlimited.
	MaxBytes int64
	// MaxDuration caps wall-clock time. Zero means unlimited.
	MaxDuration time.Duration
	// MaxErrors caps consecutive failures before the crawl gives up on a host.
	MaxErrors int
	// MaxHostPages caps pages fetched from any single host, so one enormous
	// site cannot consume the whole run.
	MaxHostPages int
}

// exhausted reports which budget dimension is spent, if any. now is passed in
// rather than read from the clock so a crawl with an injected clock has a
// deterministic budget.
func (b Budget) exhausted(state *RunState, now time.Time) (string, bool) {
	if state == nil {
		return "", false
	}
	if b.MaxPages > 0 && state.PagesFetched >= b.MaxPages {
		return "max_pages", true
	}
	if b.MaxBytes > 0 && state.BytesRetained >= b.MaxBytes {
		return "max_bytes", true
	}
	if b.MaxDuration > 0 && !state.StartedAt.IsZero() && now.Sub(state.StartedAt) > b.MaxDuration {
		return "max_duration", true
	}
	return "", false
}

// RunState is the mutable accounting for one crawl. It is owned by the Crawler
// and guarded by its mutex; Budget reads it under that lock.
type RunState struct {
	StartedAt     time.Time
	PagesFetched  int
	BytesRetained int64
	PerHost       map[string]int
	Consecutive   map[string]int
}

// hostSpent reports whether a host has used its page allowance, and which host
// that was. One enormous site must not be able to consume the whole run.
func (b Budget) hostSpent(state *RunState, rawURL string) (string, bool) {
	if b.MaxHostPages <= 0 || state == nil {
		return "", false
	}
	host := hostOf(rawURL)
	return host, state.PerHost[host] >= b.MaxHostPages
}
