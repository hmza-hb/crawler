package crawler

import (
	"net/url"
	"testing"
)

func TestNormalizeURLConformance(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://example.com", "https://example.com/"},
		{"HTTPS://Example.COM:443/x", "https://example.com/x"},
		{"http://example.com:80/./a/../b/", "http://example.com/b/"},
		{"https://example.com/a/", "https://example.com/a/"},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		got, err := NormalizeURL(u)
		if err != nil {
			t.Fatalf("NormalizeURL(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
