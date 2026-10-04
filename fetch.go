package crawler

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hmza-hb/crawler/internal/platform/hash"
	"github.com/hmza-hb/crawler/internal/platform/ratelimit"
	"github.com/hmza-hb/crawler/internal/platform/retry"

	"github.com/hmza-hb/crawler/robotstxt"
)

// Fetcher retrieves one URL. It holds no queue and no crawl state, so it can be
// used on its own as a "fetch this URL politely" library.
type Fetcher interface {
	Fetch(ctx context.Context, req Request) FetchResult
}

// Options configure a Fetcher.
type Options struct {
	// Store supplies the previous ETag/Last-Modified for a URL so a repeat
	// fetch can be conditional. Nil disables conditional requests.
	Store Store
	// UserAgent overrides Config.UserAgent for this fetcher.
	UserAgent string
	// HTTPClient overrides the constructed client. Tests use this; production
	// leaves it nil so the crawler builds a client with the right transport.
	HTTPClient *http.Client
	// Clock overrides time.Now, for deterministic tests.
	Clock func() time.Time
	// Limiter overrides the per-host rate limiter.
	Limiter *ratelimit.Keyed
	// Metrics receives counters. Nil disables metrics.
	Metrics *Metrics
	// Log is optional.
	Log Logger
}

// Logger is the minimal logging surface the crawler needs, so the module does
// not force a particular logger on its consumers.
type Logger interface {
	Debug(msg string, args ...any)
	Warn(msg string, args ...any)
}

// nopLogger discards everything.
type nopLogger struct{}

func (nopLogger) Debug(string, ...any) {}
func (nopLogger) Warn(string, ...any)  {}

// HTTPFetcher is the default Fetcher.
type HTTPFetcher struct {
	cfg     Config
	store   Store
	client  *http.Client
	agent   string
	now     func() time.Time
	limiter *ratelimit.Keyed
	metrics *Metrics
	log     Logger

	policy   retry.Policy
	robotsMu sync.RWMutex
	robots   map[string]*robotsEntry
}

type robotsEntry struct {
	robot *robotstxt.Robot
	at    time.Time
	// delay is the politeness delay this host asked for, cached alongside the
	// rules so a crawl does not re-derive it per request.
	delay time.Duration
}

// NewHTTPFetcher builds a Fetcher. It fails on an invalid config rather than
// silently crawling with nonsensical limits.
func NewHTTPFetcher(cfg Config, opt Options) (*HTTPFetcher, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	agent := opt.UserAgent
	if agent == "" {
		agent = cfg.UserAgent
	}
	client := opt.HTTPClient
	if client == nil {
		client = NewHTTPClient(cfg)
	}
	limiter := opt.Limiter
	if limiter == nil {
		// One request per second per host, bursting to two, is polite enough
		// for a general crawl and fast enough to be useful.
		limiter = ratelimit.NewKeyed(1, 2, 4096)
	}
	log := opt.Log
	if log == nil {
		log = nopLogger{}
	}
	now := opt.Clock
	if now == nil {
		now = time.Now
	}

	return &HTTPFetcher{
		cfg:     cfg,
		store:   opt.Store,
		client:  client,
		agent:   agent,
		now:     now,
		limiter: limiter,
		metrics: opt.Metrics,
		log:     log,
		policy: retry.Policy{
			MaxAttempts: cfg.MaxRetries + 1,
			Base:        500 * time.Millisecond,
			Max:         15 * time.Second,
			Multiplier:  2,
			Jitter:      0.5,
		},
		robots: map[string]*robotsEntry{},
	}, nil
}

// NewHTTPClient builds the HTTP client the crawler uses: bounded timeouts,
// compression, a connection pool sized for concurrency, and a transport that
// refuses to dial private addresses unless explicitly allowed.
func NewHTTPClient(cfg Config) *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          cfg.Concurrency * 4,
		MaxIdleConnsPerHost:   2,
		MaxConnsPerHost:       cfg.Concurrency * 2,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if !cfg.AllowPrivateHosts {
		transport.DialContext = safeDialContext(transport.DialContext)
	}
	return &http.Client{
		Transport: transport,
		Timeout:   cfg.FetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= cfg.MaxRedirects {
				return fmt.Errorf("crawler: stopped after %d redirects", len(via))
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("crawler: refusing to follow a redirect to %q", req.URL.Scheme)
			}
			if !cfg.AllowPrivateHosts && isPrivateHost(req.URL.Hostname()) {
				return fmt.Errorf("crawler: refusing to follow a redirect to a private host")
			}
			return nil
		},
	}
}

// safeDialContext re-checks the resolved address, because a public hostname can
// resolve to 127.0.0.1 and the URL check alone would not catch it.
func safeDialContext(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if isPrivateIP(ip.IP) {
				return nil, fmt.Errorf("crawler: refusing to dial %s: %s resolves to a private address", addr, ip.IP)
			}
		}
		return dial(ctx, network, addr)
	}
}

// Fetch retrieves one URL with robots compliance, politeness, conditional
// re-fetch, size and content-type policy, and bounded retries.
func (f *HTTPFetcher) Fetch(ctx context.Context, req Request) FetchResult {
	start := f.now()
	target, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil {
		return f.failed(req, 0, start, false, fmt.Errorf("crawler: parse %q: %w", req.URL, err))
	}
	if _, err := NormalizeURL(target); err != nil {
		return f.failed(req, 0, start, false, err)
	}

	robots, allowed, err := f.checkRobots(ctx, target)
	if err != nil {
		f.log.Warn("robots.txt could not be read; refusing to crawl", "url", req.URL, "err", err)
		// Fail closed. A robots.txt we cannot read is not permission to fetch.
		return f.skipped(req, start, "robots.txt unavailable")
	}
	if !allowed {
		return f.skipped(req, start, "disallowed by robots.txt")
	}

	// Politeness: the site's own Crawl-delay wins when it asks for more than we
	// were going to give anyway.
	delay := f.cfg.PerHostDelay
	if robots != nil && robots.Delay() > delay {
		delay = robots.Delay()
	}
	if err := f.limiter.Wait(ctx, hostLimitKey(target)); err != nil {
		return f.failed(req, 0, start, false, err)
	}
	if extra := delay - f.cfg.PerHostDelay; extra > 0 {
		timer := time.NewTimer(extra)
		select {
		case <-ctx.Done():
			timer.Stop()
			return f.failed(req, 0, start, false, ctx.Err())
		case <-timer.C:
		}
	}

	return f.fetchWithRetries(ctx, req, target, start)
}

func (f *HTTPFetcher) fetchWithRetries(ctx context.Context, req Request, target *url.URL, start time.Time) FetchResult {
	result, err := retry.DoValue(ctx, f.policy, func(err error) bool { return isRetryableFetchError(err) },
		func(ctx context.Context, attempt int) (FetchResult, error) {
			res, err := f.fetchOnce(ctx, req, target, start, attempt)
			if err == nil {
				return res, nil
			}
			return res, err
		})
	if err != nil {
		// retry.Do only returns an error when the last attempt failed, and the
		// result still carries the status and reason worth recording.
		if result.Outcome != "" || result.Document.Status != 0 {
			return result
		}
		return f.failed(req, result.Document.Status, start, true, err)
	}
	return result
}

func (f *HTTPFetcher) fetchOnce(ctx context.Context, req Request, target *url.URL, start time.Time, attempt int) (FetchResult, error) {
	// A fresh per-attempt context so the overall FetchTimeout is not consumed
	// by the first attempt's backoff.
	reqCtx, cancel := context.WithTimeout(ctx, f.cfg.FetchTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target.String(), nil)
	if err != nil {
		return FetchResult{}, fmt.Errorf("crawler: build request: %w", err)
	}
	setCrawlerHeaders(httpReq, f.agent)

	conditional := false
	if f.store != nil {
		prev, err := f.store.LastDocument(ctx, target.String())
		if err == nil && prev != nil {
			if prev.ETag != "" {
				httpReq.Header.Set("If-None-Match", prev.ETag)
				conditional = true
			} else if !prev.LastModified.IsZero() {
				httpReq.Header.Set("If-Modified-Since", prev.LastModified.UTC().Format(http.TimeFormat))
				conditional = true
			}
		}
	}

	resp, err := f.client.Do(httpReq)
	if err != nil {
		f.metrics.incFetch("error", "")
		return FetchResult{}, fmt.Errorf("crawler: fetch %s: %w", target, err)
	}
	defer func() {
		// Drain a little so the connection can be reused, then close.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusNotModified && conditional {
		f.metrics.incFetch("not_modified", "")
		doc := f.newDocument(req, finalURL(resp), start, attempt)
		doc.Status = resp.StatusCode
		doc.StatusText = StatusTextFor(resp.StatusCode)
		doc.NotModified = true
		doc.Elapsed = f.now().Sub(start)
		return FetchResult{Document: doc, Outcome: OutcomeNotModified}, nil
	}

	// A status error is a failure regardless of what the body looks like. A 503
	// with no Content-Type is a server in trouble, not a filtered media file,
	// and reporting it as "filtered" would hide a sick dependency.
	if resp.StatusCode >= 400 {
		mediaType := ContentTypeOf(resp.Header.Get("Content-Type"))
		doc := f.newDocument(req, finalURL(resp), start, attempt)
		doc.Status = resp.StatusCode
		doc.StatusText = StatusTextFor(resp.StatusCode)
		doc.ContentType = mediaType
		doc.RedirectChain = redirectChain(resp)
		doc.Elapsed = f.now().Sub(start)
		f.metrics.incFetch(StatusTextFor(resp.StatusCode), "")

		res := FetchResult{Document: doc, Outcome: OutcomeFailed, Retryable: IsRetryableStatus(resp.StatusCode)}
		if res.Retryable {
			// Surfaced as an error so retry.Do sees it, but the result still
			// carries the status and reason worth recording.
			return res, &httpStatusError{code: resp.StatusCode, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
		}
		return res, nil
	}

	mediaType := ContentTypeOf(resp.Header.Get("Content-Type"))
	if !f.cfg.AllowsContentType(mediaType) {
		// The status is still worth knowing, but the body is not read.
		f.metrics.incFetch("filtered_type", "")
		doc := f.newDocument(req, finalURL(resp), start, attempt)
		doc.Status = resp.StatusCode
		doc.StatusText = "content type " + mediaType + " is not retained"
		doc.ContentType = mediaType
		doc.Filtered = true
		doc.FilterReason = "content type " + mediaType + " is not retained"
		doc.RedirectChain = redirectChain(resp)
		doc.Elapsed = f.now().Sub(start)
		return FetchResult{Document: doc, Outcome: OutcomeFiltered}, nil
	}

	limit := f.cfg.MaxBytes
	body, truncated, err := readLimited(resp.Body, limit)
	if err != nil {
		f.metrics.incFetch("error", "")
		return FetchResult{}, fmt.Errorf("crawler: read %s: %w", target, err)
	}

	rawHash := hash.Bytes(body)
	body = decodeCharset(body, CharsetOf(resp.Header.Get("Content-Type")))

	doc := f.newDocument(req, finalURL(resp), start, attempt)
	doc.Status = resp.StatusCode
	doc.StatusText = StatusTextFor(resp.StatusCode)
	doc.ContentType = mediaType
	doc.ContentLength = resp.ContentLength
	doc.ETag = resp.Header.Get("ETag")
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, err := http.ParseTime(lm); err == nil {
			doc.LastModified = t.UTC()
		}
	}
	if f.cfg.KeepBodyInMemory {
		doc.Body = body
	}
	doc.BodyTruncated = truncated
	doc.RawHash = string(rawHash)
	doc.ContentHash = string(hash.Document(body))
	doc.Elapsed = f.now().Sub(start)
	doc.RedirectChain = redirectChain(resp)

	f.metrics.incFetch(StatusTextFor(resp.StatusCode), "")
	f.metrics.observeLatency(doc.Elapsed)

	return FetchResult{Document: doc, Outcome: OutcomeFetched}, nil
}

// finalURL is the URL the bytes actually came from, which is not the URL we
// asked for when the server redirected us.
func finalURL(resp *http.Response) *url.URL {
	if resp != nil && resp.Request != nil && resp.Request.URL != nil {
		return resp.Request.URL
	}
	if resp != nil {
		return &url.URL{}
	}
	return &url.URL{}
}

func (f *HTTPFetcher) newDocument(req Request, target *url.URL, start time.Time, attempt int) Document {
	return Document{
		URL:           target.String(),
		RequestedURL:  req.URL,
		Status:        0,
		ContentLength: -1,
		Depth:         req.Depth,
		FetchedAt:     f.now().UTC(),
		Attempt:       attempt,
		UserAgent:     f.agent,
		SourceHint:    req.SourceHint,
	}
}

func (f *HTTPFetcher) skipped(req Request, start time.Time, reason string) FetchResult {
	f.metrics.incFetch("skipped", "")
	doc := Document{
		URL:           req.URL,
		RequestedURL:  req.URL,
		StatusText:    reason,
		FilterReason:  reason,
		ContentLength: -1,
		Depth:         req.Depth,
		FetchedAt:     f.now().UTC(),
		UserAgent:     f.agent,
		SourceHint:    req.SourceHint,
	}
	return FetchResult{Document: doc, Outcome: OutcomeSkipped}
}

func (f *HTTPFetcher) failed(req Request, status int, start time.Time, retryable bool, err error) FetchResult {
	f.metrics.incFetch("error", "")
	doc := Document{
		URL:           req.URL,
		RequestedURL:  req.URL,
		Status:        status,
		StatusText:    StatusTextFor(status),
		ContentLength: -1,
		Depth:         req.Depth,
		FetchedAt:     f.now().UTC(),
		UserAgent:     f.agent,
	}
	return FetchResult{
		Document:  doc,
		Outcome:   OutcomeFailed,
		Err:       err,
		Retryable: retryable,
	}
}

// checkRobots returns the host's rules, whether the URL is allowed, and any
// error reading robots.txt. A host that publishes no robots.txt is allowed in
// full, per RFC 9309 §2.3.1.4.
func (f *HTTPFetcher) checkRobots(ctx context.Context, target *url.URL) (*robotstxt.Robot, bool, error) {
	if !f.cfg.RespectRobots {
		return nil, true, nil
	}
	origin := target.Scheme + "://" + target.Host
	robot, ok := f.cachedRobots(origin)
	if ok {
		return robot, robot.AllowedURL(target), nil
	}

	robotsURL := origin + "/robots.txt"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, robotsURL, nil)
	if err != nil {
		return nil, false, err
	}
	setCrawlerHeaders(req, f.agent)

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("crawler: fetch %s: %w", robotsURL, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// Absent robots.txt means no restrictions.
		f.storeRobots(origin, &robotstxt.Robot{Host: origin}, time.Duration(0))
		return nil, true, nil
	case resp.StatusCode >= 400:
		// Unreachable or forbidden robots.txt is not permission. RFC 9309
		// §2.3.1.4 treats an unreachable file as "disallow all" only for
		// 4xx that indicate unavailability; refusing is the safe reading.
		return nil, false, fmt.Errorf("crawler: %s returned %d", robotsURL, resp.StatusCode)
	}

	body, readErr := readAllLimited(resp.Body, f.cfg.MaxBytes)
	if readErr != nil {
		return nil, false, fmt.Errorf("crawler: read %s: %w", robotsURL, readErr)
	}
	robot, parseErr := robotstxt.Parse(string(body), origin, robotstxt.Options{
		UserAgent: f.agent,
		MaxBytes:  robotstxt.DefaultMaxBytes,
	})
	if parseErr != nil {
		return nil, false, fmt.Errorf("crawler: parse %s: %w", robotsURL, parseErr)
	}
	f.storeRobots(origin, robot, robot.Delay())
	f.metrics.incFetch("robots", "")
	return robot, robot.AllowedURL(target), nil
}

func (f *HTTPFetcher) cachedRobots(origin string) (*robotstxt.Robot, bool) {
	f.robotsMu.RLock()
	entry, ok := f.robots[origin]
	f.robotsMu.RUnlock()
	if !ok {
		return nil, false
	}
	if f.now().Sub(entry.at) > f.cfg.RobotsCacheTTL {
		f.robotsMu.Lock()
		delete(f.robots, origin)
		f.robotsMu.Unlock()
		return nil, false
	}
	return entry.robot, true
}

func (f *HTTPFetcher) storeRobots(origin string, robot *robotstxt.Robot, delay time.Duration) {
	f.robotsMu.Lock()
	defer f.robotsMu.Unlock()
	if len(f.robots) > 8192 {
		// Bound the cache; a crawl across tens of thousands of hosts would
		// otherwise retain every rule set for the life of the process.
		clear(f.robots)
	}
	f.robots[origin] = &robotsEntry{robot: robot, at: f.now(), delay: delay}
}

// RobotsFor returns the rules cached for a host, and whether any were cached.
// It is what a debug endpoint needs to answer "why was this URL skipped?".
func (f *HTTPFetcher) RobotsFor(origin string) (*robotstxt.Robot, bool) {
	return f.cachedRobots(origin)
}

// RobotsReporter is the optional capability of a Fetcher that can report the
// rules it holds for a host. It is separate from Fetcher because a caller with
// its own transport has no reason to implement it, and forcing the method on the
// interface would make that awkward.
type RobotsReporter interface {
	RobotsFor(origin string) (*robotstxt.Robot, bool)
}

// httpStatusError carries a retryable HTTP status through retry.Do.
type httpStatusError struct {
	code       int
	retryAfter time.Duration
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("http status %d", e.code) }

func isRetryableFetchError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var se *httpStatusError
	if errors.As(err, &se) {
		return IsRetryableStatus(se.code)
	}
	// Network-level failures are worth another attempt; a malformed URL or a
	// policy refusal is not.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsTemporary || dnsErr.IsTimeout
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	msg := err.Error()
	for _, transient := range []string{"connection reset", "broken pipe", "timeout", "EOF", "server closed"} {
		if strings.Contains(msg, transient) {
			return true
		}
	}
	return false
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// redirectChain returns the URLs visited before the final one, in the order
// they were requested. http.Response.Request.Response is the response that
// produced this request, so the chain is walked backwards and then reversed.
func redirectChain(resp *http.Response) []string {
	if resp == nil || resp.Request == nil {
		return nil
	}
	var chain []string
	for r := resp.Request; r != nil && r.Response != nil; r = r.Response.Request {
		if r.Response.Request != nil && r.Response.Request.URL != nil {
			chain = append(chain, r.Response.Request.URL.String())
		}
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// setCrawlerHeaders identifies the crawler honestly and asks for the formats it
// can actually process. Accept-Encoding is left to Go's transport, which adds
// gzip and transparently decompresses.
func setCrawlerHeaders(req *http.Request, agent string) {
	req.Header.Set("User-Agent", agent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,text/plain;q=0.8,application/json;q=0.8,*/*;q=0.5")
	req.Header.Set("Accept-Language", "en")
	req.Header.Set("Cache-Control", "no-cache")
}

// readLimited reads at most limit bytes and reports whether more was available.
func readLimited(r io.Reader, limit int64) ([]byte, bool, error) {
	body, err := readAllLimited(r, limit)
	if err != nil {
		return nil, false, err
	}
	// One extra byte distinguishes "exactly at the limit" from "over it".
	truncated := int64(len(body)) > limit
	if truncated {
		body = body[:limit]
	}
	return body, truncated, nil
}

func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(r)
	}
	return io.ReadAll(io.LimitReader(r, limit+1))
}

// decodeCharset converts a non-UTF-8 body to UTF-8. The crawler stores text, so
// an ISO-8859-1 page that is never converted produces mojibake in every
// downstream fact. Characters that cannot be mapped are dropped rather than
// replaced, because a wrong character in a person's name is worse than a
// missing one that a human can spot.
func decodeCharset(body []byte, declared string) []byte {
	charset := declared
	if charset == "" {
		charset = sniffCharset(body)
	}
	switch normaliseCharset(charset) {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return body
	case "iso-8859-1", "latin1", "latin-1", "windows-1252", "cp1252":
		return latin1ToUTF8(body)
	default:
		// An exotic charset is left alone; the extractor records it and the
		// facts are marked lower confidence rather than silently corrupted.
		return body
	}
}

func normaliseCharset(cs string) string {
	return strings.ToLower(strings.TrimSpace(strings.Trim(cs, `"'`)))
}

// sniffCharset reads a <meta charset> or <meta http-equiv content-type> from
// the first bytes of an HTML document.
func sniffCharset(body []byte) string {
	head := body
	if len(head) > 2048 {
		head = head[:2048]
	}
	lower := strings.ToLower(string(head))
	if i := strings.Index(lower, "charset="); i >= 0 {
		rest := lower[i+len("charset="):]
		rest = strings.TrimLeft(rest, " \t\"'")
		end := strings.IndexAny(rest, "\"' \t>;")
		if end >= 0 {
			rest = rest[:end]
		}
		return rest
	}
	return ""
}

// latin1ToUTF8 maps every byte to the code point of the same value. Windows-1252
// shares that mapping for the printable range; the 0x80-0x9F block is remapped
// by the caller-independent table below when present.
func latin1ToUTF8(body []byte) []byte {
	out := make([]byte, 0, len(body)+len(body)/4)
	for _, b := range body {
		if b < 0x80 {
			out = append(out, b)
			continue
		}
		r := rune(b)
		if r >= 0x80 && r <= 0x9F {
			if repl, ok := cp1252High[r]; ok {
				r = repl
			}
		}
		out = utf8AppendRune(out, r)
	}
	return out
}

func utf8AppendRune(b []byte, r rune) []byte {
	return append(b, []byte(string(r))...)
}

// cp1252High is the Windows-1252 mapping of the C1 control block, which is what
// modern "ISO-8859-1" pages actually use for curly quotes and dashes.
var cp1252High = map[rune]rune{
	0x80: '€', 0x82: '‚', 0x83: 'ƒ', 0x84: '„',
	0x85: '…', 0x86: '†', 0x87: '‡', 0x88: 'ˆ',
	0x89: '‰', 0x8A: 'Š', 0x8B: '‹', 0x8C: 'Œ',
	0x8E: 'Ž', 0x91: '‘', 0x92: '’', 0x93: '“',
	0x94: '”', 0x95: '•', 0x96: '–', 0x97: '—',
	0x98: '˜', 0x99: '™', 0x9A: 'š', 0x9B: '›',
	0x9C: 'œ', 0x9E: 'ž', 0x9F: 'Ÿ',
}

// hostLimitKey buckets the rate limiter by registrable domain rather than host,
// so www.example.com and example.com share a politeness budget. A site serving
// the same content from six hostnames should still get one request per second.
func hostLimitKey(u *url.URL) string {
	if u == nil {
		return ""
	}
	if d := registrableDomain(u.Hostname()); d != "" {
		return d
	}
	return u.Host
}
