package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hmza-hb/crawler/internal/platform/httpx"
)

func TestBearerAuthWrongToken401(t *testing.T) {
	h := httpx.BearerAuth("secret")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/fetch", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestBearerAuthRightToken200(t *testing.T) {
	h := httpx.BearerAuth("secret")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/fetch", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestValidateStartupNonLoopbackNoKey(t *testing.T) {
	t.Setenv("HTTP_ADDR", "0.0.0.0:8080")
	t.Setenv("CRAWLD_API_KEY", "")
	cfg, err := loadTestConfig()
	if err != nil {
		t.Fatal(err)
	}
	err = validateTestHTTP(cfg)
	if !errors.Is(err, errAPIKeyRequired) {
		t.Fatalf("got %v", err)
	}
}

func TestValidateStartupLoopbackNoKey(t *testing.T) {
	t.Setenv("HTTP_ADDR", "127.0.0.1:8080")
	t.Setenv("CRAWLD_API_KEY", "")
	cfg, err := loadTestConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateTestHTTP(cfg); err != nil {
		t.Fatalf("loopback: %v", err)
	}
}
