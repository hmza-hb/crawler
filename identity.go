package crawler

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// This file exposes the crawler's two identity primitives to sibling modules
// that must agree with it exactly: which host belongs to which company, and
// which hosts are never safe to contact.
//
// Duplicating either table inside another module is how a private-network guard
// and a company-identity rule drift apart until the two disagree about the same
// URL. discovery needs both, so they are exported here rather than reimplemented.

// RegistrableDomain reduces a host to the domain a human would name as "the
// company's domain": the registrable label plus its public suffix. It is a
// pragmatic approximation, not a public-suffix implementation — it covers the
// multi-part suffixes that appear in practice and treats anything else as
// two-label. It never makes a security decision; it only groups hosts that
// probably belong to one operator.
//
// A leading "www.", any port, and a trailing dot are removed, and the result is
// lowercased and punycoded. An IP literal is returned unchanged, because an
// address is never a company domain.
func RegistrableDomain(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return ""
	}
	// Strip a port. Hostname() is not usable here because callers pass bare
	// hosts, not URL objects, and "example.com:8080" is a legitimate input.
	if !strings.HasPrefix(h, "[") {
		if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i+1:], ":") {
			if _, err := net.LookupPort("tcp", h[i+1:]); err == nil {
				h = h[:i]
			} else if isAllDigits(h[i+1:]) {
				h = h[:i]
			}
		}
	}
	h = strings.Trim(h, "[]")
	if net.ParseIP(h) != nil {
		return h
	}
	// Punycode: a caller may hand us a unicode domain straight from a search
	// result, and "münchen.de" and "xn--mnchen-3ya.de" are one host.
	if ascii, err := idnaASCII(h); err == nil {
		h = ascii
	}
	// "www" is a hostname convention, not a registrable label. Stripping it is
	// safe for identity purposes even though www.example.co.uk is technically a
	// distinct name.
	if strings.HasPrefix(h, "www.") {
		h = h[4:]
	}
	return registrableDomain(h)
}

// IsSameSite reports whether two URLs are plausibly the same site: the same
// host, or hosts sharing a registrable domain. It is the test a caller should
// use before treating two discovered URLs as two companies.
func IsSameSite(a, b *url.URL) bool { return isSameSite(a, b) }

// PublicURL validates that a URL discovered from an untrusted source is one the
// crawler is willing to contact, and returns it normalized.
//
// Discovered URLs arrive from search results, sitemaps, certificate logs and
// third-party pages, none of which are trustworthy: any of them can name
// http://169.254.169.254/, http://localhost:5432/ or
// file:///etc/passwd. Every discovered URL must pass through this function
// before a caller acts on it.
//
// It rejects non-http(s) schemes, embedded credentials, and hosts inside
// private, loopback, link-local, CGNAT and other unroutable ranges. A hostname
// that is not an IP literal passes here and is re-checked after DNS resolution
// by the crawler's transport, so this is the cheap first pass, not the last.
func PublicURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errEmptyURL
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("crawler: parse %q: %w", raw, err)
	}
	if err := ValidatePublicURL(u); err != nil {
		return nil, err
	}
	return u, nil
}

// ValidatePublicURL reports whether a parsed URL is safe to fetch. It is the
// form callers use when they already hold a *url.URL, such as a link extracted
// from a page.
func ValidatePublicURL(u *url.URL) error {
	if u == nil {
		return errEmptyURL
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return errUnsupportedScheme
	}
	if u.User != nil {
		// Credentials in a discovered URL are either a phishing attempt or a
		// mis-parsed string. Neither is worth a request.
		return errors.New("crawler: URL must not embed credentials")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("crawler: URL has no host")
	}
	if isPrivateHost(host) {
		return fmt.Errorf("crawler: host %q is private or unroutable and will not be fetched", host)
	}
	return nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
