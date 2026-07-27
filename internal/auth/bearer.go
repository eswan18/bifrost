package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// RequireBearer gates a route on a static bearer token (the preview API's
// CLI auth). An empty configured token rejects every request — the feature
// is disabled, not open. Constant-time comparison; requests authed this way
// carry no session, so downstream handlers must not use SessionFromContext.
func RequireBearer(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || token == "" ||
				subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
