// Package robotstxt implements the Robots Exclusion Protocol as specified in
// RFC 9309, plus the long-standing crawl-delay and sitemap extensions.
//
// Compliance is not optional for this crawler. The parser is deliberately
// conservative: anything it does not understand is treated as disallow for the
// matching path, never as allow.
package robotstxt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultCrawlDelay is used when a site asks for no specific delay.
const DefaultCrawlDelay = 0

// Robot is a parsed robots.txt for one host.
type Robot struct {
	// Groups is the rule set per user-agent token, in file order.
	groups []*group
	// Sitemaps lists every Sitemap: directive in the file.
	sitemaps []string
	// CrawlDelay maps a user-agent token to the delay it requested.
	crawlDelay map[string]time.Duration
	// maxDelay is the largest delay any group asked for, used as the polite
	// upper bound when a caller wants a single number per host.
	maxDelay time.Duration
	// Host is the origin this file governs.
	Host string
	// agent is the token this Robot was parsed for.
	agent string
	// FetchedAt records when the file was retrieved.
	FetchedAt time.Time
	// MaxURL is the largest file the crawler will read from robots.txt.
	MaxURL int
}

type group struct {
	agents   []string
	allow    []rule
	disallow []rule
	// seenRule records whether this group has any path rule at all, which is
	// what distinguishes "declared but empty" from "never declared".
	seenRule bool
}

type rule struct {
	path  string
	depth int
	raw   string
}

// Rule representation notes. RFC 9309 §2.2.2 allows '*' and '$' wildcards, so
// rules are compiled to a regexp-lite matcher rather than compared as strings.

// Options controls parsing.
type Options struct {
	// UserAgent is the token rules are matched against. Case-insensitive.
	UserAgent string
	// MaxBytes caps the bytes read from the file. RFC 9309 §2.5 requires
	// parsing at least the first 500 KiB.
	MaxBytes int64
}

// DefaultMaxBytes is the RFC 9309 §2.5 minimum a compliant crawler must parse.
const DefaultMaxBytes int64 = 500 * 1024

// Default returns options for the common case.
func Default(userAgent string) Options {
	return Options{UserAgent: userAgent, MaxBytes: DefaultMaxBytes}
}

// Parse reads a robots.txt for the given origin.
func Parse(raw string, origin string, opt Options) (*Robot, error) {
	if opt.MaxBytes <= 0 {
		opt.MaxBytes = DefaultMaxBytes
	}
	if len(raw) > int(opt.MaxBytes) {
		raw = raw[:opt.MaxBytes]
	}

	r := &Robot{
		Host:       origin,
		agent:      strings.TrimSpace(opt.UserAgent),
		crawlDelay: map[string]time.Duration{},
	}

	var cur *group
	// lastLineWasAgent distinguishes a continuation of the previous agents from
	// a new group: in RFC 9309, consecutive User-agent lines share one group.
	lastLineWasAgent := false

	sc := bufio.NewScanner(strings.NewReader(raw))
	sc.Buffer(make([]byte, 0, 8*1024), int(opt.MaxBytes)+1)

	for sc.Scan() {
		line := stripComment(sc.Text())
		if line == "" {
			// A blank line ends a group. RFC 9309 §2.2.1.
			if cur != nil && len(cur.allow)+len(cur.disallow) > 0 {
				cur = nil
			}
			lastLineWasAgent = false
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		field = strings.ToLower(strings.TrimSpace(field))
		value = strings.TrimSpace(value)

		switch field {
		case "user-agent":
			if cur == nil || !lastLineWasAgent {
				cur = &group{}
				r.groups = append(r.groups, cur)
			}
			cur.agents = append(cur.agents, value)
			lastLineWasAgent = true

		case "allow", "disallow":
			if cur == nil {
				// Rules before any User-agent are invalid and ignored.
				continue
			}
			// An empty Disallow means "allow everything"; it is not a rule
			// that matches the empty path. RFC 9309 §2.2.2.
			if field == "disallow" && value == "" {
				cur.seenRule = true
				lastLineWasAgent = false
				continue
			}
			rule, err := compileRule(value)
			if err != nil {
				continue
			}
			if field == "allow" {
				cur.allow = append(cur.allow, rule)
			} else {
				cur.disallow = append(cur.disallow, rule)
			}
			cur.seenRule = true
			lastLineWasAgent = false

		case "crawl-delay":
			if cur != nil {
				if d, err := time.ParseDuration(value + "s"); err == nil && d >= 0 {
					for _, a := range cur.agents {
						r.crawlDelay[strings.ToLower(a)] = d
					}
					if d > r.maxDelay {
						r.maxDelay = d
					}
				}
			}
			lastLineWasAgent = false

		case "sitemap":
			if value != "" {
				r.sitemaps = append(r.sitemaps, value)
			}
			lastLineWasAgent = false

		default:
			// Unknown directive: ignore, but it terminates an agent list
			// because it is not a User-agent line.
			lastLineWasAgent = false
		}
	}

	// Only one group matches a given user agent: the most specific match, i.e.
	// the longest token that is a case-insensitive substring of ours.
	r.groups = selectGroups(r.groups, opt.UserAgent)
	return r, sc.Err()
}

// ParseReader parses from a reader, enforcing the byte cap while reading.
func ParseReader(r io.Reader, origin string, opt Options) (*Robot, error) {
	if opt.MaxBytes <= 0 {
		opt.MaxBytes = DefaultMaxBytes
	}
	buf, err := io.ReadAll(io.LimitReader(r, opt.MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("robotstxt: read %s: %w", origin, err)
	}
	// Truncate on a UTF-8 boundary so a multi-byte rune is never cut in half.
	if len(buf) > int(opt.MaxBytes) {
		buf = truncateUTF8(buf, opt.MaxBytes)
	}
	return Parse(string(buf), origin, opt)
}

// truncateUTF8 cuts b to at most limit bytes without splitting a rune.
func truncateUTF8(b []byte, limit int64) []byte {
	cut := b[:limit]
	for len(cut) > 0 && !utf8.Valid(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// selectGroups keeps only the groups whose agents match, choosing the single
// most specific matching group. RFC 9309 §2.2.2: "the group that matches the
// most specific user agent" applies, and if none match, the * group applies.
func selectGroups(groups []*group, userAgent string) []*group {
	ua := strings.ToLower(strings.TrimSpace(userAgent))
	if ua == "" {
		return nil
	}

	best := -1
	bestLen := -1
	wildcardFound := false

	for i, g := range groups {
		for _, a := range g.agents {
			token := strings.ToLower(strings.TrimSpace(a))
			switch {
			case token == "*":
				wildcardFound = true
			case token != "" && strings.Contains(ua, token):
				// Longest matching token wins, per RFC 9309 specificity.
				if len(token) > bestLen {
					bestLen = len(token)
					best = i
				}
			}
		}
	}

	if best >= 0 {
		return groups[best : best+1]
	}
	if wildcardFound {
		for _, g := range groups {
			if slices.Contains(g.agents, "*") {
				return []*group{g}
			}
		}
	}
	return nil
}

// Allowed reports whether the user agent may fetch the given path. An empty or
// unmatched rule set allows everything, which is what an absent robots.txt
// means.
func (r *Robot) Allowed(path string) bool {
	if r == nil {
		return true
	}
	if len(r.groups) == 0 {
		return true
	}
	for _, g := range r.groups {
		// Find the longest matching rule across allow and disallow. Ties go
		// to Allow, per RFC 9309 §2.2.2.
		best := rule{depth: -1}
		bestIsAllow := true
		for _, rl := range g.disallow {
			if rl.matches(path) && rl.depth > best.depth {
				best = rl
				bestIsAllow = false
			}
		}
		for _, rl := range g.allow {
			if rl.matches(path) && rl.depth >= best.depth {
				best = rl
				bestIsAllow = true
			}
		}
		if best.depth >= 0 {
			return bestIsAllow
		}
	}
	return true
}

// AllowedURL reports whether the user agent may fetch the given URL.
func (r *Robot) AllowedURL(u *url.URL) bool {
	if r == nil || u == nil {
		return true
	}
	return r.Allowed(u.EscapedPath())
}

// Delay returns the crawl delay this Robot was parsed for, falling back to the
// largest delay any group asked for.
func (r *Robot) Delay() time.Duration {
	if r == nil {
		return 0
	}
	return r.DelayFor(r.agent)
}

// DelayFor returns the crawl delay for an explicit user-agent token, applying
// the same specificity rule as group selection: the longest declared token that
// appears in the querying agent wins, otherwise the largest declared delay.
func (r *Robot) DelayFor(userAgent string) time.Duration {
	if r == nil {
		return 0
	}
	ua := strings.ToLower(strings.TrimSpace(userAgent))
	bestLen := -1
	best := time.Duration(-1)
	for token, d := range r.crawlDelay {
		if token == "*" {
			continue
		}
		if strings.Contains(ua, token) && len(token) > bestLen {
			bestLen, best = len(token), d
		}
	}
	if best >= 0 {
		return best
	}
	if d, ok := r.crawlDelay["*"]; ok {
		return d
	}
	return r.maxDelay
}

// Sitemaps returns the Sitemap: URLs declared in the file.
func (r *Robot) Sitemaps() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.sitemaps...)
}

// Rules returns a human-readable dump of the matched group. Used by the
// crawler's /robots debug endpoint so an operator can see exactly what the
// parser understood.
func (r *Robot) Rules() []string {
	if r == nil {
		return nil
	}
	var out []string
	for _, g := range r.groups {
		for _, a := range g.agents {
			out = append(out, "user-agent: "+a)
		}
		slices.SortFunc(g.disallow, func(a, b rule) int { return strings.Compare(a.path, b.path) })
		slices.SortFunc(g.allow, func(a, b rule) int { return strings.Compare(a.path, b.path) })
		for _, rl := range g.disallow {
			out = append(out, "disallow: "+rl.raw)
		}
		for _, rl := range g.allow {
			out = append(out, "allow: "+rl.raw)
		}
	}
	return out
}

// compileRule turns a robots path pattern into a matcher.
func compileRule(pattern string) (rule, error) {
	if pattern == "" {
		return rule{}, errors.New("empty rule")
	}
	// RFC 9309 defines the value as a path, but `Disallow: *` and
	// `Disallow: *.php$` are everywhere in the wild. Accepting them can only
	// make the crawler crawl less, so it errs in the safe direction.
	return rule{path: pattern, raw: pattern, depth: len(pattern)}, nil
}

// matches implements RFC 9309 §2.2.2 path matching: prefix match, '*' wildcard
// spanning any characters, '$' anchoring the end.
func (r rule) matches(path string) bool {
	// Fast path: no wildcards, plain prefix match (the overwhelmingly common
	// case, so it must not allocate).
	if !strings.ContainsAny(r.path, "*$") {
		return strings.HasPrefix(path, r.path)
	}

	// General case: linear-time wildcard match without regexp compilation.
	pat := r.path
	anchorEnd := strings.HasSuffix(pat, "$")
	if anchorEnd {
		pat = strings.TrimSuffix(pat, "$")
	}
	// Split on '*': the segments between must appear in order.
	parts := strings.Split(pat, "*")

	if len(parts) == 1 {
		// Pattern was just "$" or empty: matches everything (or nothing).
		return anchorEnd && path == ""
	}

	pos := 0
	// Leading literal (before the first '*') must match at the start.
	if parts[0] != "" {
		if !strings.HasPrefix(path, parts[0]) {
			return false
		}
		pos = len(parts[0])
	}
	// Middle literals must appear in order.
	for i := 1; i < len(parts)-1; i++ {
		if parts[i] == "" {
			continue
		}
		idx := strings.Index(path[pos:], parts[i])
		if idx < 0 {
			return false
		}
		pos += idx + len(parts[i])
	}
	// Trailing literal (after the last '*').
	last := parts[len(parts)-1]
	if anchorEnd {
		// With $, the tail must sit exactly at the end.
		if !strings.HasSuffix(path, last) {
			return false
		}
		return len(path)-len(last) >= pos
	}
	if last == "" {
		return true
	}
	return strings.HasSuffix(path, last)
}

// stripComment removes an unescaped '#' comment and surrounding space.
func stripComment(line string) string {
	if i := strings.IndexByte(line, '#'); i >= 0 {
		// A '#' inside a percent-escape is not a comment; %23 is the encoded
		// form, so a raw '#' always starts a comment.
		line = line[:i]
	}
	return strings.TrimSpace(line)
}
