package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// RequireSessionOrBearer gates a route for both browsers (session cookie)
// and CLIs (static bearer token). Any request presenting an Authorization
// Bearer header is judged solely as a bearer request — a bad token gets a
// 401, never a login redirect — so misconfigured CLIs fail loudly. Bearer
// requests carry no session in context; handlers must tolerate a nil
// SessionFromContext. An empty configured token disables the bearer path.
func RequireSessionOrBearer(sm *SessionManager, allowedEmail, loginPath, token string) func(http.Handler) http.Handler {
	sessionAuth := RequireAuth(sm, allowedEmail, loginPath)
	return func(next http.Handler) http.Handler {
		sessionHandler := sessionAuth(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented, isBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !isBearer {
				sessionHandler.ServeHTTP(w, r)
				return
			}
			if token == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
