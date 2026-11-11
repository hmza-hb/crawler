package httpx

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// BearerAuth requires Authorization: Bearer <token> on protected routes. Paths
// in skip are served without a token (health checks, metrics).
func BearerAuth(token string, skip ...string) func(http.Handler) http.Handler {
	skipSet := make(map[string]struct{}, len(skip))
	for _, p := range skip {
		skipSet[p] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := skipSet[r.URL.Path]; ok {
				next.ServeHTTP(w, r)
				return
			}
			got := bearerToken(r.Header.Get("Authorization"))
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="crawld"`)
				JSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func bearerToken(h string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
