package crawler

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Depth is a research depth. The same crawl is run at different depths
// depending on how much a candidate is worth: screen everything cheaply, then
// spend on the survivors.
type Depth int

const (
	// DepthScreen fetches only the URLs a source handed us. One page, no
	// link following, no sitemap expansion. This is the filter that runs
	// before any spending.
	DepthScreen Depth = iota
	// DepthStandard follows in-site links to a bounded depth and expands
	// sitemaps, which is enough to find a company page, its about page and its
	// contact page.
	DepthStandard
	// DepthDeep follows links more widely, expands sitemaps aggressively, and
	// fetches every discovered page kind that tends to carry facts (team,
	// careers, pricing, news, changelog).
	DepthDeep
)

// String renders the depth for logs and run metadata.
func (d Depth) String() string {
	switch d {
	case DepthScreen:
		return "screen"
	case DepthStandard:
		return "standard"
	case DepthDeep:
		return "deep"
	default:
		return fmt.Sprintf("depth(%d)", int(d))
	}
}

// ParseDepth reads a depth name.
func ParseDepth(s string) (Depth, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "screen", "0":
		return DepthScreen, nil
	case "standard", "1":
		return DepthStandard, nil
	case "deep", "2":
		return DepthDeep, nil
	default:
		return DepthScreen, fmt.Errorf("crawler: unknown depth %q, want screen, standard or deep", s)
	}
}

// MaxDepthFor is the hop limit a depth implies.
func (d Depth) MaxDepth() int {
	switch d {
	case DepthScreen:
		return 0
	case DepthStandard:
		return 2
	case DepthDeep:
		return 4
	default:
		return int(d)
	}
}

// PageBudgetFor is the default page ceiling a depth implies, before the
// caller's own budget is applied.
func (d Depth) PageBudgetFor() int {
	switch d {
	case DepthScreen:
		return 5
	case DepthStandard:
		return 60
	case DepthDeep:
		return 300
	default:
		return 20
	}
}

// DefaultContentTypes are the media types this crawler will store and parse.
// Everything else is fetched for its status and then discarded, which is how a
// crawl stays cheap on sites that serve a hundred megabytes of media.
func DefaultContentTypes() []string {
	return []string{
		"text/html",
		"application/xhtml+xml",
		"text/plain",
		"application/json",
		"application/ld+json",
		"text/xml",
		"application/xml",
		"application/rss+xml",
		"application/pdf",
	}
}

// Config is the crawler's static configuration. It is validated once at
// construction; an invalid crawler is never built.
type Config struct {
	// UserAgent identifies the crawler to servers. It must be honest: a real
	// contact address is part of being a good citizen and most operators
	// require one.
	UserAgent string

	// Concurrency is the number of in-flight fetches.
	Concurrency int
	// PerHostDelay is the minimum gap between two requests to the same host.
	// A site's Crawl-delay, when it declares one, overrides this upward.
	PerHostDelay time.Duration
	// FetchTimeout bounds a single request, including its body read.
	FetchTimeout time.Duration
	// MaxRetries is how many times a retryable failure is retried.
	MaxRetries int

	// MaxBytes caps a single response body. Larger responses are truncated and
	// flagged rather than downloaded in full.
	MaxBytes int64
	// MaxRedirects caps redirect chains. Zero means "do not follow".
	MaxRedirects int

	// RespectRobots disables nothing but is recorded in run metadata, because
	// turning it off is a decision an operator has to be able to audit.
	RespectRobots bool
	// RobotsCacheTTL is how long a host's robots.txt is trusted.
	RobotsCacheTTL time.Duration

	// ContentTypes lists the media types to retain.
	ContentTypes []string
	// DenyHostSuffixes blocks hosts by suffix, e.g. ".cdn.example" to skip an
	// asset host entirely.
	DenyHostSuffixes []string
	// AllowPrivateHosts permits fetching 10.x, 192.168.x, localhost and
	// link-local addresses. It is off by default and exists for local fixtures
	// and self-hosted targets; enabling it in production is an SSRF risk.
	AllowPrivateHosts bool

	// RawRetention is how long a stored body is kept. After that only
	// metadata, the hash and the extracted facts remain. Zero disables body
	// retention entirely, which is the right setting for a crawl whose output
	// is facts rather than archives.
	RawRetention time.Duration
	// KeepBodyInMemory controls whether Document.Body is populated. The store
	// decides separately whether to persist it.
	KeepBodyInMemory bool

	// MaxCrawlTime bounds a whole Crawl call.
	MaxCrawlTime time.Duration
	// MaxPages bounds a whole Crawl call.
	MaxPages int

	// LinkFilter decides whether a discovered URL is worth enqueuing. It runs
	// after the host policy. Returning false for a URL is a policy decision,
	// not an error.
	LinkFilter func(u *url.URL) bool
}

// DefaultConfig returns a configuration that is safe to run against the public
// internet: one connection, one request per second per host, robots honoured.
func DefaultConfig() Config {
	return Config{
		UserAgent:         "UpvistaBot/1.0 (+https://upvista.example/bot)",
		Concurrency:       4,
		PerHostDelay:      time.Second,
		FetchTimeout:      20 * time.Second,
		MaxRetries:        2,
		MaxBytes:          2 << 20,
		MaxRedirects:      5,
		RespectRobots:     true,
		RobotsCacheTTL:    time.Hour,
		ContentTypes:      DefaultContentTypes(),
		RawRetention:      72 * time.Hour,
		KeepBodyInMemory:  true,
		MaxCrawlTime:      10 * time.Minute,
		MaxPages:          500,
		AllowPrivateHosts: false,
	}
}

// ScreenConfig tightens the defaults for the cheap pre-filter pass: fewer
// bytes, no retention, no link following beyond a single hop.
func ScreenConfig() Config {
	c := DefaultConfig()
	c.MaxBytes = 512 << 10
	c.RawRetention = 0
	c.MaxPages = 5
	c.MaxCrawlTime = 2 * time.Minute
	c.Concurrency = 2
	return c
}

// Validate reports every problem at once.
func (c Config) Validate() error {
	var errs []error

	if strings.TrimSpace(c.UserAgent) == "" {
		errs = append(errs, fmt.Errorf("crawler: UserAgent is required and must identify the crawler"))
	}
	if c.Concurrency < 1 {
		errs = append(errs, fmt.Errorf("crawler: Concurrency must be >= 1, got %d", c.Concurrency))
	}
	if c.FetchTimeout <= 0 {
		errs = append(errs, fmt.Errorf("crawler: FetchTimeout must be positive"))
	}
	if c.MaxBytes <= 0 {
		errs = append(errs, fmt.Errorf("crawler: MaxBytes must be positive"))
	}
	if c.MaxRetries < 0 {
		errs = append(errs, fmt.Errorf("crawler: MaxRetries must be >= 0"))
	}
	if c.MaxRedirects < 0 {
		errs = append(errs, fmt.Errorf("crawler: MaxRedirects must be >= 0"))
	}
	if len(c.ContentTypes) == 0 {
		errs = append(errs, fmt.Errorf("crawler: ContentTypes must list at least one media type"))
	}
	if c.MaxCrawlTime < 0 {
		errs = append(errs, fmt.Errorf("crawler: MaxCrawlTime must be >= 0"))
	}
	if c.MaxPages < 0 {
		errs = append(errs, fmt.Errorf("crawler: MaxPages must be >= 0"))
	}
	if len(errs) == 0 {
		return nil
	}
	return joinErrors(errs)
}

// AllowsContentType reports whether a media type is retained.
func (c Config) AllowsContentType(mediaType string) bool {
	mediaType = ContentTypeOf(mediaType)
	for _, allowed := range c.ContentTypes {
		if allowed == mediaType {
			return true
		}
	}
	return false
}

// AllowsHost reports whether a URL passes the suffix deny list. An empty host
// is rejected: a relative or malformed URL must never reach the network.
func (c Config) AllowsHost(u *url.URL) bool {
	if u == nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if !c.AllowPrivateHosts && isPrivateHost(host) {
		return false
	}
	for _, suffix := range c.DenyHostSuffixes {
		if strings.HasSuffix(host, strings.ToLower(suffix)) {
			return false
		}
	}
	return true
}

func joinErrors(errs []error) error {
	msg := make([]string, len(errs))
	for i, e := range errs {
		msg[i] = e.Error()
	}
	return fmt.Errorf("%s", strings.Join(msg, "; "))
}
