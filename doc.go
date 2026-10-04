// Package crawler is a polite, resumable, horizontally scalable web crawler.
//
// It owns the canonical Document type — "a page we fetched, with its bytes, its
// HTTP metadata, and the content hash everything else keys off" — plus the
// frontier, robots compliance, politeness, conditional re-fetch, and the
// content-type and size policy that keep a crawl cheap.
//
// The module has two halves that can be used independently:
//
//   - Fetcher: fetch one URL, correctly, once. No queue, no state.
//   - Crawler: a frontier-driven loop over many URLs with budgets.
//
// cmd/crawld exposes both over HTTP so the crawler can be sold as a service
// without the rest of the platform.
package crawler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Document is a fetched page. It is the crawler's product and the only thing
// other modules need to know about it.
//
// Body is populated only when the caller asked for it or when the store is
// configured to retain raw bytes. Everything else is metadata that is cheap to
// keep forever.
type Document struct {
	// ID is assigned by the Store. Empty for an unsaved document.
	ID string
	// URL is the final URL after redirects — what the bytes actually are.
	URL string
	// RequestedURL is what we asked for. Kept so a redirect can be reported
	// and so a dedupe key can distinguish the request from the destination.
	RequestedURL string
	// RedirectChain records each hop, in order.
	RedirectChain []string

	// Status is the final HTTP status.
	Status int
	// StatusText explains a non-2xx outcome in words, for humans and logs.
	StatusText string
	// ContentType is the media type without parameters.
	ContentType string
	// Charset is the declared or detected character set, normalised to UTF-8
	// when we were able to convert.
	Charset string
	// ContentLength is the declared length, or -1 when unknown.
	ContentLength int64

	// ETag and LastModified drive conditional re-fetch, which is how a repeat
	// crawl of an unchanged site costs almost nothing.
	ETag         string
	LastModified time.Time

	// Body is the decoded response body, truncated at the configured cap.
	Body []byte
	// BodyTruncated reports that the cap was hit and the body is incomplete.
	BodyTruncated bool
	// ContentHash addresses the normalised body. Two documents with the same
	// hash are the same page as far as every downstream module is concerned.
	ContentHash string
	// RawHash is the digest of the bytes exactly as received, before charset
	// conversion. It is what conditional GET and dedupe-on-receipt use.
	RawHash string

	// Links are the links discovered in this document.
	Links []Link
	// Depth is the hop count from the seed.
	Depth int
	// FetchedAt is when the response was received.
	FetchedAt time.Time
	// NotModified reports a 304: the stored copy is still current.
	NotModified bool
	// Filtered reports that the response was received but its content type is
	// not one this crawler retains, so no body was kept.
	Filtered bool
	// FilterReason explains a Filtered document, and a skip, in one line. It is
	// empty for a normal fetch. Without it a caller cannot tell a deliberate
	// policy decision from a bug.
	FilterReason string
	// Elapsed is the wall-clock time the request took.
	Elapsed time.Duration
	// Attempt is the 1-based retry attempt that produced this document.
	Attempt int
	// UserAgent is the agent string used.
	UserAgent string
	// SourceHint records what discovered this URL, for attribution back to a
	// seed, a sitemap, or a link.
	SourceHint string
	// RunID scopes the document to a crawl, so a run's cost can be attributed.
	RunID string
}

// Link is an outgoing hyperlink discovered in a document.
type Link struct {
	// Href is the absolute, normalised URL.
	Href string
	// Text is the anchor text, whitespace-collapsed and length-capped.
	Text string
	// Rel is the anchor's rel attribute, which distinguishes navigational
	// links from stylesheets and social profiles.
	Rel string
	// Internal reports whether the link stays on the document's own host.
	Internal bool
	// NoFollow reports rel="nofollow", a signal not to treat the target as
	// endorsed.
	NoFollow bool
}

// Request asks the Fetcher for one URL.
type Request struct {
	// URL is the absolute URL to fetch.
	URL string
	// Depth is the hop count from the seed, used for budget decisions.
	Depth int
	// Priority orders the frontier. Higher is fetched sooner.
	Priority int
	// ETag and IfModifiedSince enable a conditional request when set.
	ETag            string
	IfModifiedSince time.Time
	// SourceHint records what discovered this URL, for attribution.
	SourceHint string
}

// Outcome classifies a fetch so callers can branch without string matching.
type Outcome string

const (
	// OutcomeFetched: a 2xx response with a usable body.
	OutcomeFetched Outcome = "fetched"
	// OutcomeNotModified: 304; the stored copy is still current.
	OutcomeNotModified Outcome = "not_modified"
	// OutcomeSkipped: policy declined to fetch (robots, type, scheme, budget).
	OutcomeSkipped Outcome = "skipped"
	// OutcomeFailed: the fetch was attempted and failed.
	OutcomeFailed Outcome = "failed"
	// OutcomeFiltered: the document was fetched but its content is not
	// something this crawler stores or links from.
	OutcomeFiltered Outcome = "filtered"
)

// FetchResult is a Fetcher's answer: a document, a classification, and an
// error. The document is returned even on failure whenever there is something
// worth recording (a 404 status, a truncated body).
type FetchResult struct {
	Document Document
	Outcome  Outcome
	// Err is non-nil only for OutcomeFailed.
	Err error
	// Retryable reports whether the caller should try again later.
	Retryable bool
}

// FetchError records a failed fetch without aborting a crawl.
type FetchError struct {
	URL     string
	Status  int
	Reason  string
	Attempt int
	Err     error
}

func (e FetchError) Error() string {
	status := strconv.Itoa(e.Status)
	if e.Status == 0 {
		status = "---"
	}
	if e.Err != nil {
		return fmt.Sprintf("%s -> %s: %v", e.URL, status, e.Err)
	}
	return fmt.Sprintf("%s -> %s: %s", e.URL, status, e.Reason)
}

func (e FetchError) Unwrap() error { return e.Err }

// StatusTextFor maps an HTTP status to a short, log-friendly explanation that
// distinguishes "nothing there" from "we were refused" from "they are busy".
func StatusTextFor(code int) string {
	switch {
	case code == 0:
		return "no response"
	case code >= 200 && code < 300:
		return "ok"
	case code == 304:
		return "not modified"
	case code >= 300 && code < 400:
		return "redirect not followed"
	case code == 400:
		return "bad request"
	case code == 401 || code == 403:
		return "forbidden"
	case code == 404 || code == 410:
		return "gone"
	case code == 429:
		return "rate limited"
	case code >= 500 && code < 600:
		return "server error"
	default:
		return "unexpected status"
	}
}

// IsRetryableStatus reports whether a status justifies an immediate retry.
// 4xx other than 429 will not change by asking again.
func IsRetryableStatus(code int) bool {
	switch {
	case code == 408, code == 425, code == 429:
		return true
	case code >= 500 && code < 600:
		return true
	default:
		return false
	}
}

// ContentTypeOf splits a media type from its parameters, lowercased.
func ContentTypeOf(header string) string {
	if i := strings.IndexByte(header, ';'); i >= 0 {
		header = header[:i]
	}
	return strings.ToLower(strings.TrimSpace(header))
}

// CharsetOf extracts the charset parameter from a Content-Type header.
func CharsetOf(header string) string {
	for _, part := range strings.Split(header, ";")[1:] {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(k), "charset") {
			return strings.ToLower(strings.Trim(strings.TrimSpace(v), `"'`))
		}
	}
	return ""
}

// IsHTMLContentType reports whether a media type is worth parsing for links
// and facts.
func IsHTMLContentType(ct string) bool {
	switch ct {
	case "text/html", "application/xhtml+xml":
		return true
	default:
		return false
	}
}

// IsXMLContentType reports whether a media type is a sitemap or feed.
func IsXMLContentType(ct string) bool {
	switch ct {
	case "text/xml", "application/xml", "application/rss+xml", "application/atom+xml":
		return true
	default:
		return false
	}
}
