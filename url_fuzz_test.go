package crawler

import (
	"net/url"
	"testing"
)

func FuzzNormalizeURL(f *testing.F) {
	seeds := []string{
		"https://example.com/",
		"https://EXAMPLE.COM:443/path/../x",
		"http://example.com:80/a/",
		"https://example.com?q=1&q=2",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		u, err := url.Parse(raw)
		if err != nil {
			return
		}
		out, err := NormalizeURL(u)
		if err != nil {
			return
		}
		u2, err := url.Parse(out)
		if err != nil {
			t.Fatalf("normalized url.Parse: %v", err)
		}
		out2, err := NormalizeURL(u2)
		if err != nil {
			t.Fatalf("second normalize: %v", err)
		}
		if out != out2 {
			t.Fatalf("normalize not idempotent: %q -> %q", out, out2)
		}
		if u2.Scheme != "http" && u2.Scheme != "https" {
			return
		}
		if u2.Host == "" {
			return
		}
		if u2.Fragment != "" {
			t.Fatalf("fragment retained: %q", out)
		}
	})
}
