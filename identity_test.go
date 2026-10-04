package crawler

import (
	"net/url"
	"strings"
	"testing"
)

func TestRegistrableDomain(t *testing.T) {
	cases := []struct{ in, want string }{
		{"example.com", "example.com"},
		{"www.example.com", "example.com"},
		{"WWW.Example.COM.", "example.com"},
		{"example.com:8443", "example.com"},
		{"app.example.co.uk", "example.co.uk"},
		{"shop.example.com.au", "example.com.au"},
		{"example.co.jp", "example.co.jp"},
		{"a.b.c.example.io", "example.io"},
		{"example.io", "example.io"},
		{"localhost", "localhost"},
		{"127.0.0.1", "127.0.0.1"},
		{"[::1]", "::1"},
		{"  example.com  ", "example.com"},
		{"", ""},
		// A bare IP is never a company identity.
		{"192.168.1.10", "192.168.1.10"},
	}
	for _, tc := range cases {
		if got := RegistrableDomain(tc.in); got != tc.want {
			t.Errorf("RegistrableDomain(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRegistrableDomainPunycodesUnicode(t *testing.T) {
	// A unicode domain and its punycode form are one host. Treating them as two
	// companies is a duplicate-candidate bug, not a formatting preference.
	got := RegistrableDomain("münchen.de")
	if got != "xn--mnchen-3ya.de" {
		t.Errorf("RegistrableDomain(unicode) = %q, want the punycode form", got)
	}
	if RegistrableDomain("xn--mnchen-3ya.de") != got {
		t.Error("the unicode and punycode forms must resolve to one domain")
	}
}

func TestRegistrableDomainKeepsUnrelatedSitesApart(t *testing.T) {
	// Two different companies on one ccTLD must not collapse together, or a
	// whole country's web becomes one candidate.
	if RegistrableDomain("shop.example.co.uk") == RegistrableDomain("other.co.uk") {
		t.Error("two unrelated .co.uk sites collapsed to one domain")
	}
}

func TestPublicURLAcceptsOrdinaryWebURLs(t *testing.T) {
	for _, raw := range []string{
		"https://example.com",
		"http://example.com/path?a=1#frag",
		"https://sub.example.co.uk/x",
	} {
		if _, err := PublicURL(raw); err != nil {
			t.Errorf("PublicURL(%q) = %v, want it accepted", raw, err)
		}
	}
}

func TestPublicURLRejectsUnsafeTargets(t *testing.T) {
	// Every one of these is a real SSRF or exfiltration vector. A discovered
	// URL is attacker-controlled, so all of them must be refused.
	bad := []string{
		"",
		"   ",
		"file:///etc/passwd",
		"gopher://example.com",
		"ftp://example.com",
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:5432/",
		"http://localhost/",
		"http://app.localhost/",
		"http://10.0.0.5/",
		"http://192.168.1.1/",
		"http://172.16.0.1/",
		"http://[::1]/",
		"http://user:pass@example.com/",
		"https://",
		"http://0.0.0.0/",
		// CGNAT range: routable-looking, not publicly reachable.
		"http://100.64.0.1/",
	}
	for _, raw := range bad {
		if u, err := PublicURL(raw); err == nil {
			t.Errorf("PublicURL(%q) = %v, want it rejected", raw, u)
		}
	}
}

func TestIsSameSiteAcrossHostConventions(t *testing.T) {
	mustParse := func(s string) *url.URL {
		u, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	same := [][2]string{
		{"https://example.com/", "https://www.example.com/"},
		{"https://example.com/", "https://shop.example.com/"},
		{"https://example.co.uk/", "https://www.example.co.uk/about"},
		{"https://example.com/a", "https://example.com/b"},
	}
	for _, p := range same {
		if !IsSameSite(mustParse(p[0]), mustParse(p[1])) {
			t.Errorf("IsSameSite(%q, %q) = false, want true", p[0], p[1])
		}
	}
	diff := [][2]string{
		{"https://example.com/", "https://example.org/"},
		{"https://example.com/", "https://notexample.com/"},
		{"https://a.co.uk/", "https://b.co.uk/"},
		{"https://example.com/", "https://example.com.au/"},
	}
	for _, p := range diff {
		if IsSameSite(mustParse(p[0]), mustParse(p[1])) {
			t.Errorf("IsSameSite(%q, %q) = true, want false", p[0], p[1])
		}
	}
	if IsSameSite(nil, mustParse("https://example.com/")) {
		t.Error("IsSameSite(nil, u) = true, want false")
	}
}

func TestPublicURLMessageNamesTheHost(t *testing.T) {
	// An operator reading a log needs to know which host was refused, without
	// having to correlate timestamps.
	_, err := PublicURL("http://10.1.2.3/")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "10.1.2.3") {
		t.Errorf("error %q does not name the refused host", err)
	}
}
