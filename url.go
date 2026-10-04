package crawler

import (
	"errors"
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
)

var (
	errEmptyURL          = errors.New("crawler: URL is empty")
	errUnsupportedScheme = errors.New("crawler: only http and https are supported")
)

// isPrivateHost reports whether a host is inside a private or otherwise
// unroutable range. This is the crawler's SSRF guard: without it, a hostile
// page could enqueue http://169.254.169.254/ and the crawler would fetch a
// cloud metadata credential and hand the contents to whoever seeded the URL.
func isPrivateHost(host string) bool {
	h := strings.Trim(host, "[]")
	if strings.EqualFold(h, "localhost") || strings.HasSuffix(strings.ToLower(h), ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		// A name that is not an IP literal could still resolve to a private
		// address. The transport re-checks after DNS resolution; this is the
		// cheap first pass.
		return false
	}
	return isPrivateIP(ip)
}

func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() {
		return true
	}
	// Carrier-grade NAT, benchmarking, documentation and reserved ranges are all
	// places a lead-generation crawl has no business reaching.
	for _, cidr := range unroutableCIDRs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

var unroutableCIDRs = func() []*net.IPNet {
	cidrs := []string{
		"100.64.0.0/10",   // RFC 6598 carrier-grade NAT
		"192.0.0.0/24",    // RFC 6890 IETF protocol assignments
		"192.0.2.0/24",    // TEST-NET-1
		"198.18.0.0/15",   // RFC 2544 benchmarking
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"240.0.0.0/4",     // reserved
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// normalizeURL canonicalises a URL for storage and dedupe.
func normalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	return NormalizeURL(u)
}

// NormalizeURL canonicalises a parsed URL: lowercased scheme and host, default
// port removed, fragment dropped, dot segments resolved. The query is preserved
// because for some sites it selects the content.
//
// Exported because other modules dedupe against the crawler's keys and must
// produce byte-identical strings.
func NormalizeURL(u *url.URL) (string, error) {
	if u == nil {
		return "", errEmptyURL
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", errUnsupportedScheme
	}
	if u.Host == "" {
		return "", errEmptyURL
	}

	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if (u.Scheme == "http" && strings.HasSuffix(u.Host, ":80")) ||
		(u.Scheme == "https" && strings.HasSuffix(u.Host, ":443")) {
		u.Host = strings.TrimSuffix(strings.TrimSuffix(u.Host, ":80"), ":443")
	}
	u.Fragment = ""
	u.RawFragment = ""
	if u.Path == "" {
		u.Path = "/"
	}
	u.Path = cleanPath(u.Path)
	u.RawPath = ""

	return u.String(), nil
}

// cleanPath resolves "." and ".." segments per RFC 3986 §5.2.4, preserving an
// explicit trailing slash: /a and /a/ are different pages to a server and
// collapsing them silently loses content.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	trailingSlash := strings.HasSuffix(p, "/")
	segments := strings.Split(p, "/")
	out := make([]string, 0, len(segments))
	for _, seg := range segments {
		switch seg {
		case "", ".":
			// Collapsed by the split/join below.
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	joined := "/" + strings.Join(out, "/")
	if trailingSlash && joined != "/" {
		joined += "/"
	}
	return joined
}

// dedupeKey is the frontier's identity for a URL. Tracking parameters are
// stripped: a single page tagged with a dozen analytics parameters would
// otherwise occupy a dozen queue slots and a dozen conditional requests.
func dedupeKey(u *url.URL) string {
	if u == nil {
		return ""
	}
	clone := *u
	clone.Fragment = ""
	clone.RawFragment = ""
	if clone.RawQuery != "" {
		q := clone.Query()
		for _, drop := range trackingParams {
			q.Del(drop)
		}
		clone.RawQuery = q.Encode()
	}
	clone.Scheme = strings.ToLower(clone.Scheme)
	clone.Host = strings.ToLower(clone.Host)
	if clone.Path == "" {
		clone.Path = "/"
	}
	clone.Path = cleanPath(clone.Path)
	return clone.String()
}

// trackingParams are dropped from a frontier identity but preserved on the URL
// that is actually fetched.
var trackingParams = []string{
	"utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content", "utm_id",
	"gclid", "fbclid", "msclkid", "mc_cid", "mc_eid", "ref", "referrer",
	"_ga", "_gl", "yclid", "igshid", "s_kwcid", "vero_id", "wickedid",
}

// sameHost compares two URLs by normalised host and port.
func sameHost(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Host, b.Host)
}

// registrableDomain reduces a host to its registrable domain using a pragmatic
// two-label rule plus a list of common multi-part public suffixes. It is not a
// public-suffix implementation and never makes a security decision — it only
// groups hosts that probably belong to the same operator.
func registrableDomain(host string) string {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "" {
		return ""
	}
	if strings.Count(h, ":") >= 1 { // IPv6 literal
		return h
	}
	if net.ParseIP(h) != nil {
		return h
	}
	parts := strings.Split(h, ".")
	if len(parts) <= 2 {
		return h
	}
	suffix := parts[len(parts)-2] + "." + parts[len(parts)-1]
	if _, isMulti := multiPartSuffixes[suffix]; isMulti && len(parts) >= 3 {
		return parts[len(parts)-3] + "." + suffix
	}
	return suffix
}

// multiPartSuffixes covers the public suffixes that would otherwise make two
// unrelated sites on one ccTLD look like a single company.
var multiPartSuffixes = map[string]struct{}{
	"co.uk": {}, "org.uk": {}, "ac.uk": {}, "gov.uk": {}, "me.uk": {}, "net.uk": {},
	"com.au": {}, "net.au": {}, "org.au": {}, "edu.au": {}, "gov.au": {}, "id.au": {},
	"co.nz": {}, "com.br": {}, "com.mx": {}, "com.ar": {}, "com.cn": {}, "com.tw": {},
	"co.jp": {}, "co.kr": {}, "co.in": {}, "co.za": {},
	"com.sg": {}, "com.tr": {}, "com.pl": {}, "com.ua": {}, "com.hk": {},
}

// isSameSite reports whether two URLs are plausibly the same site: the same
// host, or two hosts sharing a registrable domain.
func isSameSite(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	if sameHost(a, b) {
		return true
	}
	return registrableDomain(a.Hostname()) == registrableDomain(b.Hostname())
}

// idnaASCII punycodes a possibly-unicode domain. It is separated out so the
// exported identity helpers and the internal ones cannot disagree about
// encoding: "münchen.de" and "xn--mnchen-3ya.de" are the same host, and treating
// them as two companies is a duplicate-candidate bug.
func idnaASCII(host string) (string, error) {
	if isASCIIDomain(host) {
		return host, nil
	}
	return idna.Lookup.ToASCII(host)
}

func isASCIIDomain(h string) bool {
	for i := 0; i < len(h); i++ {
		if h[i] >= 0x80 {
			return false
		}
	}
	return true
}
