package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequireSessionOrBearer(t *testing.T) {
	sm := NewSessionManager([]byte(strings.Repeat("k", 32)), 12*time.Hour)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if SessionFromContext(r.Context()) != nil {
			w.Header().Set("X-Had-Session", "yes")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler := RequireSessionOrBearer(sm, "me@example.com", "/auth/login", "s3cret")(next)

	t.Run("valid bearer passes with no session", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/previews", nil)
		req.Header.Set("Authorization", "Bearer s3cret")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
		if rec.Header().Get("X-Had-Session") != "" {
			t.Error("bearer request must carry no session in context")
		}
	})

	t.Run("bad bearer 401s and never redirects", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/previews", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("WWW-Authenticate = %q, want Bearer", got)
		}
	})

	t.Run("no auth header falls through to session redirect", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/previews", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303 login redirect", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/auth/login" {
			t.Errorf("Location = %q", loc)
		}
	})

	t.Run("empty configured token rejects bearer attempts", func(t *testing.T) {
		h := RequireSessionOrBearer(sm, "me@example.com", "/auth/login", "")(next)
		req := httptest.NewRequest("GET", "/api/previews", nil)
		req.Header.Set("Authorization", "Bearer anything")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
}
