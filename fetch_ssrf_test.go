package crawler

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestHTTPClientDoesNotUseProxyEnv(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")

	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "User-agent: *\nAllow: /\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testConfig()
	cfg.AllowPrivateHosts = true
	f := newTestFetcher(t, cfg, nil)
	res := f.Fetch(context.Background(), Request{URL: srv.URL + "/"})
	if res.Outcome != OutcomeFetched {
		t.Fatalf("outcome = %q err %v (proxy env must not steal the connection)", res.Outcome, res.Err)
	}
	tr, ok := f.client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if tr.Proxy != nil {
		t.Fatal("Transport.Proxy must be nil")
	}
}

func TestSafeDialRejectsAnyPrivateResolution(t *testing.T) {
	orig := lookupIPAddr
	defer func() { lookupIPAddr = orig }()
	lookupIPAddr = func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host == "evil.example" {
			return []net.IPAddr{
				{IP: net.ParseIP("93.184.216.34")},
				{IP: net.ParseIP("10.0.0.1")},
			}, nil
		}
		return orig(context.Background(), host)
	}

	cfg := DefaultConfig()
	cfg.AllowPrivateHosts = false
	client := NewHTTPClient(cfg)
	dial := client.Transport.(*http.Transport).DialContext
	_, err := dial(context.Background(), "tcp", "evil.example:80")
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("dial = %v, want private rejection when any A/AAAA is blocked", err)
	}
}

func TestFetchTLSVerifiesHostnameViaIPDial(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "User-agent: *\nAllow: /\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "tls-ok")
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	cfg := testConfig()
	cfg.AllowPrivateHosts = true
	base := srv.Client()
	tr := NewHTTPClient(cfg).Transport.(*http.Transport).Clone()
	if st, ok := base.Transport.(*http.Transport); ok {
		tr.TLSClientConfig = st.TLSClientConfig.Clone()
	}
	client := &http.Client{Transport: tr, Timeout: cfg.FetchTimeout}
	f, err := NewHTTPFetcher(cfg, Options{HTTPClient: client, Limiter: testLimiter()})
	if err != nil {
		t.Fatal(err)
	}
	res := f.Fetch(context.Background(), Request{URL: srv.URL + "/"})
	if res.Outcome != OutcomeFetched {
		t.Fatalf("TLS fetch failed: outcome=%q err=%v", res.Outcome, res.Err)
	}
}

func TestRedirectToLoopbackBlocked(t *testing.T) {
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, "User-agent: *\nAllow: /\n")
			return
		}
		http.Redirect(w, r, "http://127.0.0.1/secret", http.StatusFound)
	}))
	defer redirector.Close()

	cfg := testConfig()
	cfg.AllowPrivateHosts = true // reach the local redirector; redirect targets stay blocked
	cfg.MaxRedirects = 5
	f := newTestFetcher(t, cfg, nil)
	res := f.Fetch(context.Background(), Request{URL: redirector.URL + "/start"})
	if res.Outcome == OutcomeFetched || res.Outcome == OutcomeNotModified {
		t.Fatalf("expected redirect to loopback to be refused, outcome=%q", res.Outcome)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "private") {
		t.Fatalf("want private redirect error, got outcome=%q err=%v", res.Outcome, res.Err)
	}
}

func TestRedirectToPrivateDNSNameBlocked(t *testing.T) {
	orig := lookupIPAddr
	defer func() { lookupIPAddr = orig }()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, "User-agent: *\nAllow: /\n")
			return
		}
		http.Redirect(w, r, "http://rebind.example/private", http.StatusFound)
	}))
	defer redirector.Close()

	lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		switch host {
		case "rebind.example":
			return []net.IPAddr{{IP: net.ParseIP("10.0.0.5")}}, nil
		default:
			return orig(ctx, host)
		}
	}

	cfg := testConfig()
	cfg.AllowPrivateHosts = true
	cfg.MaxRedirects = 5
	f := newTestFetcher(t, cfg, nil)
	_ = os.Unsetenv("HTTP_PROXY")
	res := f.Fetch(context.Background(), Request{URL: redirector.URL + "/start"})
	if res.Outcome == OutcomeFetched || res.Outcome == OutcomeNotModified {
		t.Fatalf("expected private redirect target to be refused, outcome=%q", res.Outcome)
	}
}
