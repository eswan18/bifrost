package web

import (
	"context"
	"encoding/json"
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

// TestPromoteRefusesStaleExpectedSHA covers the headline safety feature:
// if staging moved between page load and button press, the promote must be
// refused rather than shipping a SHA the user never saw.
func TestPromoteRefusesStaleExpectedSHA(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:def5678"},
	}}
	h, _, sess := newTestHandlers(t, k)

	// The page was rendered when staging was at fff0000; it's now abc1234.
	form := strings.NewReader("csrf=" + auth.CSRFToken(h.Cfg.SessionSecret, sess.ID) +
		"&expected_sha=fff0000")
	req := httptest.NewRequest("POST", "/services/foo/promote", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "foo")
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))

	rec := httptest.NewRecorder()
	h.Promote(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if got, ok := k.patched["foo"]; ok {
		t.Fatalf("patched prod to %q despite stale expected_sha", got)
	}

	// The user should see the staleness message on the next page load.
	next := httptest.NewRequest("GET", "/", nil)
	for _, c := range rec.Result().Cookies() {
		next.AddCookie(c)
	}
	fl := TakeFlash(httptest.NewRecorder(), next)
	if fl == nil {
		t.Fatal("no flash set")
	}
	if fl.Kind != FlashError {
		t.Errorf("flash kind = %q, want %q", fl.Kind, FlashError)
	}
	if !strings.Contains(fl.Msg, "staging changed") {
		t.Errorf("flash msg = %q, want staleness message", fl.Msg)
	}
}

// TestStatusRendersPromoteForm smoke-tests the rendered HTML: an
// out-of-sync service must get a promote form carrying the CSRF token and
// the expected_sha the user is approving.
func TestStatusRendersPromoteForm(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:def5678"},
	}}
	h, _, sess := newTestHandlers(t, k)

	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))
	rec := httptest.NewRecorder()
	h.Status(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `action="/services/foo/promote"`) {
		t.Error("promote form for foo missing from rendered HTML")
	}
	csrf := auth.CSRFToken(h.Cfg.SessionSecret, sess.ID)
	if !strings.Contains(body, `name="csrf" value="`+csrf+`"`) {
		t.Error("CSRF token missing from promote form")
	}
	if !strings.Contains(body, `name="expected_sha" value="abc1234"`) {
		t.Error("expected_sha missing from promote form")
	}
}

// TestStatusRendersDeployingSpinner: a mid-deploy service (>1 distinct image
// in a namespace) renders an animated spinner in its badge. The asserted
// substring is contiguous only in the rendered badge — base.html's JS copy is
// split across string concatenation, so it can't false-match.
func TestStatusRendersDeployingSpinner(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234", "reg/foo:def5678"}, // 2 distinct => mid-deploy
		"foo-prod":    {"reg/foo:abc1234"},
	}}
	h, _, sess := newTestHandlers(t, k)

	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))
	rec := httptest.NewRecorder()
	h.Status(rec, req)

	want := `badge badge-info gap-1"><span class="loading loading-spinner loading-xs"></span>deploying`
	if !strings.Contains(rec.Body.String(), want) {
		t.Error("mid-deploy badge should render an inline spinner")
	}
}

func TestStatus404sNonRootPaths(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{}}
	h, _, sess := newTestHandlers(t, k)

	req := httptest.NewRequest("GET", "/favicon.ico", nil)
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))
	rec := httptest.NewRecorder()
	h.Status(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}

// TestStatusJSON covers the per-service polling endpoint the promote spinner
// uses to detect when prod has rolled out.
func TestStatusJSON(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:def5678"},
	}}
	h, _, _ := newTestHandlers(t, k)

	req := httptest.NewRequest("GET", "/services/foo/status", nil)
	req.SetPathValue("name", "foo")
	rec := httptest.NewRecorder()
	h.StatusJSON(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["state"] != "out_of_sync" {
		t.Errorf("state = %v, want out_of_sync", got["state"])
	}
	if got["prodTag"] != "def5678" {
		t.Errorf("prodTag = %v, want def5678", got["prodTag"])
	}
	if got["newProdTag"] != "abc1234" {
		t.Errorf("newProdTag = %v, want abc1234", got["newProdTag"])
	}
}

func TestStatusJSONUnknownService(t *testing.T) {
	h, _, _ := newTestHandlers(t, &fakeKube{imgs: map[string][]string{}})
	req := httptest.NewRequest("GET", "/services/nope/status", nil)
	req.SetPathValue("name", "nope")
	rec := httptest.NewRecorder()
	h.StatusJSON(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}

// TestPromoteJSONSuccess covers the AJAX response: with Accept: application/json
// the handler patches prod and returns {ok:true, newTag:...} (so the client can
// poll for that tag) instead of redirecting.
func TestPromoteJSONSuccess(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:def5678"},
	}}
	h, _, sess := newTestHandlers(t, k)
	form := strings.NewReader("csrf=" + auth.CSRFToken(h.Cfg.SessionSecret, sess.ID) +
		"&expected_sha=abc1234")
	req := httptest.NewRequest("POST", "/services/foo/promote", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetPathValue("name", "foo")
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))

	rec := httptest.NewRecorder()
	h.Promote(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["ok"] != true {
		t.Errorf("ok = %v, want true", got["ok"])
	}
	if got["newTag"] != "abc1234" {
		t.Errorf("newTag = %v, want abc1234", got["newTag"])
	}
	if k.patched["foo"] != "reg/foo:abc1234" {
		t.Errorf("patched = %q, want reg/foo:abc1234", k.patched["foo"])
	}
}

// TestPromoteJSONStaleExpectedSHA: the refusal path also speaks JSON and does
// not patch.
func TestPromoteJSONStaleExpectedSHA(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:def5678"},
	}}
	h, _, sess := newTestHandlers(t, k)
	form := strings.NewReader("csrf=" + auth.CSRFToken(h.Cfg.SessionSecret, sess.ID) +
		"&expected_sha=fff0000")
	req := httptest.NewRequest("POST", "/services/foo/promote", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetPathValue("name", "foo")
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))

	rec := httptest.NewRecorder()
	h.Promote(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["ok"] != false {
		t.Errorf("ok = %v, want false", got["ok"])
	}
	if _, ok := k.patched["foo"]; ok {
		t.Error("should not have patched on stale expected_sha")
	}
	if msg, _ := got["error"].(string); !strings.Contains(msg, "staging changed") {
		t.Errorf("error = %q, want staleness message", msg)
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
