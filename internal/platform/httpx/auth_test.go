package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBearerAuthSkipsHealth(t *testing.T) {
	var hit bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true })
	h := BearerAuth("secret", "/healthz")(inner)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !hit {
		t.Fatalf("healthz: status=%d hit=%v", rr.Code, hit)
	}
}

func TestBearerAuthRejectsMissingToken(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := BearerAuth("secret")(inner)

	req := httptest.NewRequest(http.MethodPost, "/v1/fetch", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestBearerAuthAcceptsToken(t *testing.T) {
	var hit bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true })
	h := BearerAuth("secret")(inner)

	req := httptest.NewRequest(http.MethodPost, "/v1/fetch", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !hit {
		t.Fatalf("authorized: status=%d hit=%v", rr.Code, hit)
	}
}
