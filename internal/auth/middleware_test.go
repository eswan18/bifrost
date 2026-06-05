package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequireAuthRedirectsWhenAnonymous(t *testing.T) {
	sm := NewSessionManager([]byte("12345678901234567890123456789012"), time.Hour)
	h := RequireAuth(sm, "me@example.com", "/auth/login")(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("handler should not be called")
		}),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/auth/login" {
		t.Errorf("Location = %q", loc)
	}
}

func TestRequireAuthRejectsWrongEmail(t *testing.T) {
	sm := NewSessionManager([]byte("12345678901234567890123456789012"), time.Hour)
	setupRec := httptest.NewRecorder()
	sm.Set(setupRec, "intruder@example.com")
	cookie := setupRec.Result().Cookies()[0]

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	RequireAuth(sm, "me@example.com", "/auth/login")(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("handler should not be called")
		}),
	).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestRequireAuthAllowsCorrectEmail(t *testing.T) {
	sm := NewSessionManager([]byte("12345678901234567890123456789012"), time.Hour)
	setupRec := httptest.NewRecorder()
	sm.Set(setupRec, "me@example.com")
	cookie := setupRec.Result().Cookies()[0]

	called := false
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	RequireAuth(sm, "me@example.com", "/auth/login")(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }),
	).ServeHTTP(rec, req)

	if !called {
		t.Fatal("handler was not called")
	}
}
