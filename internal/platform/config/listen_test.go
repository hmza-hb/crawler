package config

import (
	"errors"
	"testing"
)

func TestValidateHTTPLoopbackWithoutKey(t *testing.T) {
	if err := ValidateHTTP(HTTPConfig{Addr: "127.0.0.1:8080"}); err != nil {
		t.Fatalf("loopback without key: %v", err)
	}
}

func TestValidateHTTPNonLoopbackRequiresKey(t *testing.T) {
	err := ValidateHTTP(HTTPConfig{Addr: ":8080"})
	if !errors.Is(err, ErrAPIKeyRequired) {
		t.Fatalf("want ErrAPIKeyRequired, got %v", err)
	}
}

func TestIsLoopbackBind(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8080": true,
		"localhost:8080": true,
		"[::1]:8080":     true,
		":8080":          false,
		"0.0.0.0:8080":   false,
	}
	for addr, want := range cases {
		if got := IsLoopbackBind(addr); got != want {
			t.Errorf("IsLoopbackBind(%q) = %v, want %v", addr, got, want)
		}
	}
}
