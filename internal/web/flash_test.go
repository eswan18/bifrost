package web

import (
	"net/http/httptest"
	"testing"
)

// roundTripFlash sets a flash, copies the resulting cookie onto a fresh
// request (as a browser would), and takes it back.
func roundTripFlash(t *testing.T, kind FlashKind, msg string) *Flash {
	t.Helper()
	rec := httptest.NewRecorder()
	SetFlash(rec, kind, msg)

	req := httptest.NewRequest("GET", "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	return TakeFlash(httptest.NewRecorder(), req)
}

func TestFlashRoundTripPreservesMessage(t *testing.T) {
	cases := []struct {
		name string
		kind FlashKind
		msg  string
	}{
		// The promote success message: "→" is non-ASCII and net/http would
		// silently strip it from a raw cookie value.
		{"promote arrow", FlashSuccess, "Promoted fitness-api prod → abc1234"},
		// Kube error strings carry spaces, quotes, and punctuation.
		{"kube error", FlashError, `read staging: pods is forbidden: User "system:serviceaccount:x" cannot list resource "pods"`},
		{"semicolons and commas", FlashError, "a;b,c=d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fl := roundTripFlash(t, tc.kind, tc.msg)
			if fl == nil {
				t.Fatal("no flash returned")
			}
			if fl.Kind != tc.kind {
				t.Errorf("Kind = %q, want %q", fl.Kind, tc.kind)
			}
			if fl.Msg != tc.msg {
				t.Errorf("Msg = %q, want %q", fl.Msg, tc.msg)
			}
		})
	}
}

func TestTakeFlashNoCookie(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	if fl := TakeFlash(httptest.NewRecorder(), req); fl != nil {
		t.Errorf("expected nil flash, got %+v", fl)
	}
}
