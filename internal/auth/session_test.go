package auth

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	sm := NewSessionManager([]byte("12345678901234567890123456789012"), time.Hour)
	rec := httptest.NewRecorder()
	sm.Set(rec, "me@example.com")
	cookie := rec.Result().Cookies()[0]
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	sess, err := sm.Get(req)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Email != "me@example.com" {
		t.Errorf("got %q", sess.Email)
	}
}

func TestTamperedCookieFails(t *testing.T) {
	sm := NewSessionManager([]byte("12345678901234567890123456789012"), time.Hour)
	rec := httptest.NewRecorder()
	sm.Set(rec, "me@example.com")
	cookie := rec.Result().Cookies()[0]
	cookie.Value = cookie.Value[:len(cookie.Value)-2] + "xx"
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	if _, err := sm.Get(req); err == nil {
		t.Fatal("expected tamper to fail")
	}
}

func TestExpiredFails(t *testing.T) {
	sm := NewSessionManager([]byte("12345678901234567890123456789012"), -time.Hour)
	rec := httptest.NewRecorder()
	sm.Set(rec, "me@example.com")
	cookie := rec.Result().Cookies()[0]
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	if _, err := sm.Get(req); err == nil {
		t.Fatal("expected expired to fail")
	}
}
