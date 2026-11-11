package config

import (
	"os"
	"testing"
)

func TestLoadDefaultHTTPAddrLoopback(t *testing.T) {
	t.Setenv("HTTP_ADDR", "")
	t.Setenv("DATABASE_URL", "postgres://x")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Addr != "127.0.0.1:8080" {
		t.Errorf("Addr = %q, want 127.0.0.1:8080", cfg.HTTP.Addr)
	}
}

func TestLoadAPIKey(t *testing.T) {
	t.Setenv("CRAWLD_API_KEY", "test-key")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.APIKey != "test-key" {
		t.Errorf("APIKey = %q", cfg.HTTP.APIKey)
	}
}

func TestMain(m *testing.M) {
	// Avoid polluting other tests if config tests run in parallel packages.
	os.Exit(m.Run())
}
