package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eswan18/bifrost/internal/auth"
	"github.com/eswan18/bifrost/internal/config"
)

type fakeKube struct {
	mu       sync.Mutex
	imgs     map[string][]string
	patched  map[string]string
	patchErr error
}

func (f *fakeKube) ListPodImages(_ context.Context, ns string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.imgs[ns], nil
}

func (f *fakeKube) PatchProdImage(_ context.Context, app, image string) error {
	if f.patchErr != nil {
		return f.patchErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.patched == nil {
		f.patched = map[string]string{}
	}
	f.patched[app] = image
	return nil
}

func newTestHandlers(t *testing.T, k *fakeKube) (*Handlers, *auth.SessionManager, *auth.Session) {
	t.Helper()
	cfg := &config.Config{
		Services:        []string{"foo"},
		SessionSecret:   []byte("12345678901234567890123456789012"),
		ArgoCDNamespace: "argocd",
	}
	rend, err := LoadTemplates("../../templates")
	if err != nil {
		t.Fatalf("templates: %v", err)
	}
	sm := auth.NewSessionManager(cfg.SessionSecret, time.Hour)
	sess := &auth.Session{Email: "me@example.com", IssuedAt: time.Now(), ID: "sid1"}
	return &Handlers{Cfg: cfg, Kube: k, Renderer: rend}, sm, sess
}

func TestPromoteHappyPath(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:def5678"},
	}}
	h, sm, sess := newTestHandlers(t, k)
	_ = sm

	form := strings.NewReader("csrf=" + auth.CSRFToken(h.Cfg.SessionSecret, sess.ID) +
		"&expected_sha=abc1234")
	req := httptest.NewRequest("POST", "/services/foo/promote", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "foo")
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))

	rec := httptest.NewRecorder()
	h.Promote(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code = %d", rec.Code)
	}
	if got := k.patched["foo"]; got != "reg/foo:abc1234" {
		t.Errorf("patched = %q", got)
	}
}

func TestPromoteRejectsBadCSRF(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:def5678"},
	}}
	h, _, sess := newTestHandlers(t, k)
	form := strings.NewReader("csrf=wrong")
	req := httptest.NewRequest("POST", "/services/foo/promote", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "foo")
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))

	rec := httptest.NewRecorder()
	h.Promote(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestPromoteNothingToPromote(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc"},
		"foo-prod":    {"reg/foo:abc"},
	}}
	h, _, sess := newTestHandlers(t, k)
	form := strings.NewReader("csrf=" + auth.CSRFToken(h.Cfg.SessionSecret, sess.ID))
	req := httptest.NewRequest("POST", "/services/foo/promote", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "foo")
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))

	rec := httptest.NewRecorder()
	h.Promote(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code = %d", rec.Code)
	}
	if _, ok := k.patched["foo"]; ok {
		t.Error("should not have patched")
	}
}
