package robotstxt

import (
	"strings"
	"testing"
)

func FuzzParse(f *testing.F) {
	seeds := []string{
		"User-agent: *\nDisallow: /",
		"User-agent: *\nAllow: /public\nDisallow: /",
		"User-agent: Googlebot\nCrawl-delay: 2\nDisallow: /admin",
		"",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, body string) {
		if len(body) > 64*1024 {
			return
		}
		r, err := Parse(body, "https://example.com", Default("TestBot/1.0"))
		if err != nil {
			return
		}
		path := "/x"
		if n := strings.Count(body, "\n"); n > 0 {
			path = "/line" + strings.Repeat("/y", n%5)
		}
		_ = r.Allowed(path)
		_ = r.Delay()
	})
}
