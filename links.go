package crawler

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// LinkExtractor pulls outgoing links out of a document. It is its own type so
// the HTML parsing rules can be unit tested without a network, and so a future
// extractor module can supply a richer implementation behind the same
// interface.
type LinkExtractor interface {
	// Links returns the outgoing links of a document, absolute and
	// deduplicated, capped at max.
	Links(doc Document, max int) []Link
}

// HTMLExtractor extracts links from HTML, plus link targets worth fetching
// even when a page does not link to them in the body.
type HTMLExtractor struct {
	// MaxLinks caps the returned set. Zero means DefaultMaxLinks.
	MaxLinks int
	// IncludeNoFollow keeps rel=nofollow links. They are links a site chose
	// not to endorse, but they are still real pages; the caller decides.
	IncludeNoFollow bool
}

// DefaultMaxLinks bounds link extraction so a navigation-heavy page cannot
// dominate a crawl.
const DefaultMaxLinks = 500

// Links implements LinkExtractor.
func (e HTMLExtractor) Links(doc Document, max int) []Link {
	if max <= 0 {
		max = e.MaxLinks
	}
	if max <= 0 {
		max = DefaultMaxLinks
	}
	if len(doc.Body) == 0 {
		return nil
	}

	base, err := url.Parse(doc.URL)
	if err != nil {
		base = nil
	}

	node, err := html.Parse(strings.NewReader(string(doc.Body)))
	if err != nil {
		// A malformed document still yields whatever was parsed before the
		// error; a partial link set is better than none.
		if node == nil {
			return nil
		}
	}

	seen := make(map[string]struct{}, 64)
	out := make([]Link, 0, 32)

	// done stops the whole walk, not just the current subtree: returning from
	// one level only would let a parent's remaining siblings push the set past
	// max.
	done := false
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if done {
			return
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.A && n.Data == "a" {
			href, rel, text := anchorInfo(n)
			if href != "" {
				if abs, ok := resolve(base, href); ok {
					if _, dup := seen[abs]; !dup {
						seen[abs] = struct{}{}
						noFollow := strings.Contains(strings.ToLower(rel), "nofollow")
						if !noFollow || e.IncludeNoFollow {
							out = append(out, Link{
								Href:     abs,
								Text:     text,
								Rel:      strings.ToLower(strings.TrimSpace(rel)),
								Internal: base != nil && isInternalURL(base, abs),
								NoFollow: noFollow,
							})
							if len(out) >= max {
								done = true
								return
							}
						}
					}
				}
			}
		}
		for c := n.FirstChild; c != nil && !done; c = c.NextSibling {
			walk(c)
		}
	}
	walk(node)

	return out
}

// anchorInfo returns the href, rel and collapsed anchor text of an <a> element.
func anchorInfo(n *html.Node) (href, rel, text string) {
	for _, attr := range n.Attr {
		switch strings.ToLower(attr.Key) {
		case "href":
			href = strings.TrimSpace(attr.Val)
		case "rel":
			rel = strings.TrimSpace(attr.Val)
		}
	}
	var b strings.Builder
	collectText(n, &b, 0)
	return href, rel, collapseSpace(b.String(), 200)
}

func collectText(n *html.Node, b *strings.Builder, depth int) {
	if depth > 12 {
		return
	}
	if n.Type == html.TextNode {
		b.WriteString(n.Data)
		b.WriteByte(' ')
		return
	}
	if n.Type == html.ElementNode && n.DataAtom == atom.Script {
		return // script bodies are not visible text
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		collectText(c, b, depth+1)
	}
}

func collapseSpace(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max]
	}
	return s
}

func isInternalURL(base *url.URL, abs string) bool {
	u, err := url.Parse(abs)
	if err != nil {
		return false
	}
	return sameHost(base, u)
}

func resolve(base *url.URL, href string) (string, bool) {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") {
		return "", false
	}
	switch strings.ToLower(href[:min(4, len(href))]) {
	case "java", "data", "tel:", "sms:", "call", "skype", "whats", "fax:":
		return "", false
	}
	if strings.HasPrefix(href, "//") {
		// A protocol-relative URL inherits the page's scheme.
		scheme := "https"
		if base != nil && base.Scheme != "" {
			scheme = base.Scheme
		}
		href = scheme + ":" + href
	}
	ref, err := url.Parse(href)
	if err != nil {
		return "", false
	}
	var abs *url.URL
	if base != nil {
		abs = base.ResolveReference(ref)
	} else {
		abs = ref
	}
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return "", false
	}
	normalised, err := NormalizeURL(abs)
	if err != nil {
		return "", false
	}
	return normalised, true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// SitemapEntry is one <url> in a sitemap.
type SitemapEntry struct {
	Loc        string
	LastMod    string
	ChangeFreq string
	Priority   string
}

// SitemapIndexEntry is one <sitemap> in a sitemap index.
type SitemapIndexEntry struct {
	Loc     string
	LastMod string
}

// ParseSitemap reads either a urlset or a sitemapindex. Anything it cannot parse
// returns an error rather than a partial result, so a caller can distinguish
// "not a sitemap" from "a sitemap with three entries".
func ParseSitemap(r io.Reader) (entries []SitemapEntry, index []SitemapIndexEntry, err error) {
	dec := xml.NewDecoder(r)
	// Real-world sitemaps contain unescaped ampersands. Strict mode would
	// reject an entire 50k-URL sitemap over one typo, which loses far more than
	// it protects.
	dec.Strict = false

	var (
		kind    string
		inEntry bool
		field   string
		loc     strings.Builder
		lastMod strings.Builder
		freq    strings.Builder
		prio    strings.Builder
	)

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("crawler: parse sitemap: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			name := strings.ToLower(t.Name.Local)
			switch name {
			case "urlset":
				kind = "urlset"
			case "sitemapindex":
				kind = "sitemapindex"
			case "url", "sitemap":
				inEntry = true
				loc.Reset()
				lastMod.Reset()
				freq.Reset()
				prio.Reset()
			case "loc", "lastmod", "changefreq", "priority":
				field = name
			}
		case xml.CharData:
			if !inEntry || field == "" {
				continue
			}
			b := &loc
			switch field {
			case "lastmod":
				b = &lastMod
			case "changefreq":
				b = &freq
			case "priority":
				b = &prio
			}
			b.Write(t)
		case xml.EndElement:
			switch strings.ToLower(t.Name.Local) {
			case "url", "sitemap":
				inEntry = false
				e := SitemapEntry{
					Loc:        collapseSpace(loc.String(), 2048),
					LastMod:    collapseSpace(lastMod.String(), 64),
					ChangeFreq: collapseSpace(freq.String(), 32),
					Priority:   collapseSpace(prio.String(), 8),
				}
				if kind == "sitemapindex" {
					index = append(index, SitemapIndexEntry{Loc: e.Loc, LastMod: e.LastMod})
				} else {
					entries = append(entries, e)
				}
			case "loc", "lastmod", "changefreq", "priority":
				field = ""
			}
		}
	}
	return entries, index, nil
}
