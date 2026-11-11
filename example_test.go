package crawler_test

import (
	"fmt"
	"net/url"

	"github.com/hmza-hb/crawler"
)

func ExampleNormalizeURL() {
	u, _ := url.Parse("HTTPS://Example.COM:443/a/../b/")
	out, err := crawler.NormalizeURL(u)
	if err != nil {
		panic(err)
	}
	fmt.Println(out)
	// Output: https://example.com/b/
}

func ExamplePublicURL_rejectPrivate() {
	_, err := crawler.PublicURL("http://127.0.0.1/")
	if err != nil {
		fmt.Println("rejected")
	}
	// Output: rejected
}

func ExampleRegistrableDomain() {
	fmt.Println(crawler.RegistrableDomain("www.shop.example.co.uk"))
	// Output: example.co.uk
}
