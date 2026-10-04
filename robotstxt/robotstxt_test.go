package robotstxt

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, body, agent string) *Robot {
	t.Helper()
	r, err := Parse(body, "https://example.com", Default(agent))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return r
}

func TestAbsentRulesAllowEverything(t *testing.T) {
	r := mustParse(t, "", "MyBot")
	if !r.Allowed("/anything") || !r.Allowed("/private/secret") {
		t.Error("an empty robots.txt must allow everything")
	}
}

func TestNilRobotAllows(t *testing.T) {
	var r *Robot
	if !r.Allowed("/x") || !r.AllowedURL(&url.URL{Path: "/x"}) {
		t.Error("a nil Robot must allow, because it means 'no robots.txt'")
	}
	if r.Delay() != 0 {
		t.Error("a nil Robot has no delay")
	}
}

func TestBasicDisallow(t *testing.T) {
	r := mustParse(t, "User-agent: *\nDisallow: /private", "MyBot")
	if !r.Allowed("/public") {
		t.Error("/public should be allowed")
	}
	if r.Allowed("/private") || r.Allowed("/private/x") {
		t.Error("/private and below should be disallowed")
	}
	// Matching is textual prefix matching, not path-segment matching: a rule of
	// /private also covers /privateish. Crawlers must over-block, not under-block.
	if r.Allowed("/privateish") {
		t.Error("/privateish starts with /private and must be disallowed by prefix match")
	}
}

func TestEmptyDisallowMeansAllowAll(t *testing.T) {
	r := mustParse(t, "User-agent: *\nDisallow:", "MyBot")
	if !r.Allowed("/anything") {
		t.Error("an empty Disallow directive means allow everything")
	}
}

func TestLongestRuleWins(t *testing.T) {
	// RFC 9309 §2.2.2: the most specific (longest) matching rule decides, with
	// Allow winning ties.
	body := "User-agent: *\nDisallow: /a\nAllow: /a/b\n"
	r := mustParse(t, body, "MyBot")
	if !r.Allowed("/a/b") {
		t.Error("the longer Allow must beat the shorter Disallow")
	}
	if r.Allowed("/a") {
		t.Error("/a should remain disallowed")
	}
}

func TestAllowWinsTies(t *testing.T) {
	body := "User-agent: *\nDisallow: /x\nAllow: /x\n"
	r := mustParse(t, body, "MyBot")
	if !r.Allowed("/x") {
		t.Error("Allow must win when both rules are the same length")
	}
}

func TestWildcardMatching(t *testing.T) {
	cases := []struct {
		rule    string
		path    string
		allowed bool
	}{
		{"/*.php$", "/index.php", false},
		{"/*.php$", "/index.php?x=1", true}, // $ anchors the path, not the query
		{"/*.php$", "/a/index.php", false},
		{"/a/*/b", "/a/x/b", false},
		{"/a/*/b", "/a/x/y/b", false},
		{"/a/*", "/a/anything/deep", false},
		{"/a/*", "/a/", false},
		{"/a/*", "/a", true},
		{"*", "/anything", false},
	}
	for _, tc := range cases {
		body := "User-agent: *\nDisallow: " + tc.rule + "\n"
		r := mustParse(t, body, "MyBot")
		if got := r.Allowed(tc.path); got != tc.allowed {
			t.Errorf("Disallow %q, path %q: allowed=%v want %v", tc.rule, tc.path, got, tc.allowed)
		}
	}
}

func TestUserAgentGroupSelection(t *testing.T) {
	body := "User-agent: BadBot\nDisallow: /\n\nUser-agent: MyBot\nDisallow: /nope\n\nUser-agent: *\nDisallow: /generic\n"
	r := mustParse(t, body, "MyBot/1.0")
	if r.Allowed("/nope") {
		t.Error("MyBot's own group should apply")
	}
	if !r.Allowed("/generic") {
		t.Error("a more specific group must fully replace the wildcard group")
	}
}

func TestMostSpecificGroupWins(t *testing.T) {
	body := "User-agent: *\nDisallow: /\n\nUser-agent: MyBot\nAllow: /\n"
	if r := mustParse(t, body, "MyBot/1.0"); !r.Allowed("/anything") {
		t.Error("the more specific MyBot group must win over *")
	}
	other := mustParse(t, body, "SomeOtherBot")
	if other.Allowed("/anything") {
		t.Error("a bot that does not match MyBot must fall back to the * group")
	}
}

func TestConsecutiveUserAgentLinesShareAGroup(t *testing.T) {
	body := "User-agent: MyBot\nUser-agent: OtherBot\nDisallow: /shared\n"
	r := mustParse(t, body, "MyBot/1.0")
	if r.Allowed("/shared") {
		t.Error("both agents declared before a rule belong to one group")
	}
}

func TestCaseInsensitiveAgentMatch(t *testing.T) {
	r := mustParse(t, "User-agent: mybot\nDisallow: /x", "MYBOT/1.0")
	if r.Allowed("/x") {
		t.Error("user agent matching must be case-insensitive")
	}
}

func TestCommentsAndBlankLines(t *testing.T) {
	body := "# a comment\nUser-agent: *\n\nDisallow: /admin # trailing comment\n\n"
	r := mustParse(t, body, "MyBot")
	if r.Allowed("/admin") {
		t.Error("trailing comments must be stripped")
	}
	if !r.Allowed("/public") {
		t.Error("/public should be allowed")
	}
}

func TestSitemaps(t *testing.T) {
	body := "Sitemap: https://example.com/sitemap.xml\nUser-agent: *\nSitemap: /sitemap-products.xml\nDisallow: /x\n"
	r := mustParse(t, body, "MyBot")
	got := r.Sitemaps()
	if len(got) != 2 || got[0] != "https://example.com/sitemap.xml" || got[1] != "/sitemap-products.xml" {
		t.Fatalf("Sitemaps = %v", got)
	}
	// Sitemaps are independent of agent matching.
	if len(r.Rules()) == 0 {
		t.Error("Rules should report the matched group")
	}
}

func TestCrawlDelay(t *testing.T) {
	body := "User-agent: *\nCrawl-delay: 7\nDisallow: /x\n"
	r := mustParse(t, body, "MyBot")
	if got := r.Delay(); got != 7*time.Second {
		t.Errorf("Delay = %s, want 7s", got)
	}
}

func TestCrawlDelayPerAgent(t *testing.T) {
	body := "User-agent: SlowBot\nCrawl-delay: 30\nDisallow: /\n\nUser-agent: FastBot\nCrawl-delay: 1\nDisallow: /\n"
	slow := mustParse(t, body, "SlowBot/1.0")
	if got := slow.Delay(); got != 30*time.Second {
		t.Errorf("SlowBot delay = %s, want 30s", got)
	}
	fast := mustParse(t, body, "FastBot/1.0")
	if got := fast.Delay(); got != time.Second {
		t.Errorf("FastBot delay = %s, want 1s", got)
	}
}

func TestInvalidCrawlDelayFallsBack(t *testing.T) {
	r := mustParse(t, "User-agent: *\nCrawl-delay: soon\nDisallow: /x", "MyBot")
	if got := r.Delay(); got != 0 {
		t.Errorf("Delay = %s, want 0 for an unparseable value", got)
	}
}

func TestRulesBeforeAnyAgentAreIgnored(t *testing.T) {
	// RFC 9309 §2.2.1: a group must start with User-agent.
	r := mustParse(t, "Disallow: /\nUser-agent: MyBot\nDisallow: /only-this\n", "MyBot")
	if !r.Allowed("/") {
		t.Error("a rule before any User-agent line must be discarded")
	}
	if r.Allowed("/only-this") {
		t.Error("the valid group that follows should still apply")
	}
}

func TestAllowedURLUsesPath(t *testing.T) {
	r := mustParse(t, "User-agent: *\nDisallow: /admin", "MyBot")
	u, _ := url.Parse("https://example.com/admin/panel?x=1#frag")
	if r.AllowedURL(u) {
		t.Error("AllowedURL should use the URL path")
	}
	ok, _ := url.Parse("https://example.com/home")
	if !r.AllowedURL(ok) {
		t.Error("/home should be allowed")
	}
}

func TestParseReaderEnforcesByteCap(t *testing.T) {
	// A Disallow that starts past the cap must not take effect: the crawler
	// only honours what it actually read.
	var b strings.Builder
	b.WriteString("User-agent: *\n")
	for b.Len() < 4000 {
		b.WriteString("# padding to push the rule past the cap\n")
	}
	b.WriteString("Disallow: /secret\n")
	if b.Len() < 4000 {
		t.Fatalf("fixture is only %d bytes", b.Len())
	}
	r, err := ParseReader(strings.NewReader(b.String()), "https://example.com", Options{UserAgent: "MyBot", MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Allowed("/secret") {
		t.Error("a rule beyond the byte cap must be ignored")
	}
}

func TestParseReaderDoesNotSplitRunes(t *testing.T) {
	body := "User-agent: *\nDisallow: /a\n# " + strings.Repeat("é", 100)
	_, err := ParseReader(strings.NewReader(body), "https://example.com", Options{UserAgent: "MyBot", MaxBytes: 120})
	if err != nil {
		t.Fatalf("ParseReader returned an error on a rune boundary: %v", err)
	}
}

func TestEmptyUserAgentMatchesNothing(t *testing.T) {
	r := mustParse(t, "User-agent: *\nDisallow: /\n", "")
	if !r.Allowed("/") {
		t.Error("an empty user agent should match no group, so nothing is disallowed")
	}
}

func TestRulesIsStableAcrossCalls(t *testing.T) {
	r := mustParse(t, "User-agent: *\nDisallow: /z\nDisallow: /a\nAllow: /a/b\n", "MyBot")
	first := strings.Join(r.Rules(), "|")
	second := strings.Join(r.Rules(), "|")
	if first != second {
		t.Errorf("Rules is not stable: %q vs %q", first, second)
	}
}
