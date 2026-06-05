package auth

import (
	"context"
	"net/http"
)

type ctxKey string

const sessionCtxKey ctxKey = "session"

// RequireAuth returns middleware that ensures the request carries a valid
// session and the session's email matches allowedEmail. Anonymous → redirect
// to loginPath. Wrong email → 403.
func RequireAuth(sm *SessionManager, allowedEmail, loginPath string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sess, err := sm.Get(r)
			if err != nil {
				http.Redirect(w, r, loginPath, http.StatusSeeOther)
				return
			}
			if sess.Email != allowedEmail {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			ctx := context.WithValue(r.Context(), sessionCtxKey, sess)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func SessionFromContext(ctx context.Context) *Session {
	s, _ := ctx.Value(sessionCtxKey).(*Session)
	return s
}

// WithSessionForTest is exported for use in other packages' tests. Do not
// use in production code paths.
func WithSessionForTest(ctx context.Context, s *Session) context.Context {
	return context.WithValue(ctx, sessionCtxKey, s)
}
