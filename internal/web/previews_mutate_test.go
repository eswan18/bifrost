package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eswan18/bifrost/internal/auth"
	"github.com/eswan18/bifrost/internal/kube"
)

// syncBuffer is a concurrency-safe bytes.Buffer: the background goroutine
// under test writes to it (via slog) on its own goroutine while the test
// polls it from the main one.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureSlog swaps the default slog logger for one writing into a buffer,
// restoring the previous logger on test cleanup. Used to pin that a failed
// background Up/Down is actually logged, not silently dropped, since the
// HTTP response has already gone out by the time it fails (see the task
// brief: "goroutine failures land in namespace annotations + one slog line
// here").
func captureSlog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// waitForLog polls buf until it contains substr or the deadline passes. The
// handler's failure log happens on the background goroutine strictly after
// Up/Down returns (which fakeOrchestration signals via doneCh/waitForCall),
// so a short poll — rather than a fixed sleep — closes that small remaining
// window deterministically.
func waitForLog(t *testing.T, buf *syncBuffer, substr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), substr) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("log output never contained %q; got %q", substr, buf.String())
}

// fakeOrchestration is the trivial fake that previews_mutate.go's
// orchestration interface exists to enable: no real clients, just recorded
// calls and scriptable errors/busy state. Up/Down signal doneCh after
// recording their args so tests can deterministically wait for the
// handler's background goroutine instead of racing it.
type fakeOrchestration struct {
	busyTags map[string]bool
	upErr    error
	downErr  error

	gotBranch string
	gotTTL    time.Duration
	gotTag    string
	gotCtx    context.Context
	doneCh    chan struct{}

	// releaseCh, if set, makes Up/Down block after capturing their args and
	// signaling doneCh, until the test closes it. Tests that need to inspect
	// gotCtx *before* the handler's own `defer cancel()` fires (i.e. before
	// Up/Down returns) use this to make that window deterministic instead of
	// racing the goroutine's own completion.
	releaseCh chan struct{}
}

func newFakeOrchestration() *fakeOrchestration {
	return &fakeOrchestration{doneCh: make(chan struct{}, 1)}
}

func (f *fakeOrchestration) Busy(tag string) bool { return f.busyTags[tag] }

func (f *fakeOrchestration) Up(ctx context.Context, branch string, ttl time.Duration) error {
	f.gotBranch = branch
	f.gotTTL = ttl
	f.gotCtx = ctx
	f.doneCh <- struct{}{}
	if f.releaseCh != nil {
		<-f.releaseCh
	}
	return f.upErr
}

func (f *fakeOrchestration) Down(ctx context.Context, tag string) error {
	f.gotTag = tag
	f.gotCtx = ctx
	f.doneCh <- struct{}{}
	if f.releaseCh != nil {
		<-f.releaseCh
	}
	return f.downErr
}

// waitForCall blocks until the fake's Up or Down has been invoked, failing
// the test if the handler never kicked off the background goroutine at all.
func (f *fakeOrchestration) waitForCall(t *testing.T) {
	t.Helper()
	select {
	case <-f.doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("orchestrator method was never invoked")
	}
}

// mustNotCall asserts the fake's Up/Down was never invoked (used for guard
// paths — CSRF, 400, 409, 404, 503 — that must all short-circuit before
// ever reaching the orchestrator).
func (f *fakeOrchestration) mustNotCall(t *testing.T) {
	t.Helper()
	select {
	case <-f.doneCh:
		t.Fatal("orchestrator method was invoked, want it skipped")
	case <-time.After(50 * time.Millisecond):
	}
}

func jsonPost(target, body string) *http.Request {
	return httptest.NewRequest("POST", target, strings.NewReader(body))
}

func withSession(r *http.Request, sess *auth.Session) *http.Request {
	return r.WithContext(auth.WithSessionForTest(r.Context(), sess))
}

// --- POST /api/previews -------------------------------------------------

func TestCreatePreview202Shape(t *testing.T) {
	fo := newFakeOrchestration()
	h, _ := newTestHandlers(t, &fakeKube{})
	h.Orch = fo

	req := jsonPost("/api/previews", `{"branch":"hae-cadence"}`)
	rec := httptest.NewRecorder()
	h.CreatePreviewJSON(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var body struct {
		Tag   string `json:"tag"`
		Phase string `json:"phase"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Tag != "hae-cadence" || body.Phase != "creating" {
		t.Errorf("body = %+v", body)
	}

	fo.waitForCall(t)
	if fo.gotBranch != "hae-cadence" {
		t.Errorf("Up called with branch = %q, want hae-cadence", fo.gotBranch)
	}
}

// TestCreatePreviewUpFailureIsLoggedNotSurfaced pins the contract from the
// task brief: a background Up failure lands in namespace annotations (the
// orchestrator's own job, not exercised here via the fake) plus exactly one
// slog line — it must never surface to the HTTP caller, who already got 202.
func TestCreatePreviewUpFailureIsLoggedNotSurfaced(t *testing.T) {
	buf := captureSlog(t)
	fo := newFakeOrchestration()
	fo.upErr = errors.New("build failed")
	h, _ := newTestHandlers(t, &fakeKube{})
	h.Orch = fo

	req := jsonPost("/api/previews", `{"branch":"hae-cadence"}`)
	rec := httptest.NewRecorder()
	h.CreatePreviewJSON(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 even though Up will fail asynchronously", rec.Code)
	}
	fo.waitForCall(t)
	waitForLog(t, buf, "build failed")
}

// TestCreatePreviewTTLReachesUp pins that a valid Go duration in the body is
// parsed and handed to Orchestrator.Up, which is the only thing that turns it
// into a bifrost/expires-at annotation.
func TestCreatePreviewTTLReachesUp(t *testing.T) {
	fo := newFakeOrchestration()
	h, _ := newTestHandlers(t, &fakeKube{})
	h.Orch = fo

	req := jsonPost("/api/previews", `{"branch":"hae-cadence","ttl":"90m"}`)
	rec := httptest.NewRecorder()
	h.CreatePreviewJSON(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	fo.waitForCall(t)
	if fo.gotTTL != 90*time.Minute {
		t.Errorf("Up called with ttl = %v, want 90m", fo.gotTTL)
	}
}

// TestCreatePreviewWithoutTTLIsNoExpiry is the plan's headline constraint: no
// implicit default TTL. A body with no ttl must reach Up as 0 (no expiry),
// not as some house default.
func TestCreatePreviewWithoutTTLIsNoExpiry(t *testing.T) {
	fo := newFakeOrchestration()
	h, _ := newTestHandlers(t, &fakeKube{})
	h.Orch = fo

	req := jsonPost("/api/previews", `{"branch":"hae-cadence"}`)
	rec := httptest.NewRecorder()
	h.CreatePreviewJSON(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	fo.waitForCall(t)
	if fo.gotTTL != 0 {
		t.Errorf("Up called with ttl = %v, want 0 (no expiry) when the field is absent", fo.gotTTL)
	}
}

// TestCreatePreviewBadTTL covers every way a ttl can be rejected. Each must
// 400 *before* the orchestrator is touched: a preview created with a
// misunderstood expiry is worse than one not created at all.
//
// wantMsg is asserted, not just the status, because the three guards overlap
// on status: an unparseable duration also yields a zero d, so dropping the
// parse check would still 400 via the "must be positive" check and a
// status-only assertion would pass for the wrong reason.
func TestCreatePreviewBadTTL(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{"unparseable", `{"branch":"hae-cadence","ttl":"eight hours"}`, "must be a Go duration"},
		{"bare number, no unit", `{"branch":"hae-cadence","ttl":"8"}`, "must be a Go duration"},
		{"zero", `{"branch":"hae-cadence","ttl":"0h"}`, "must be positive"},
		{"negative", `{"branch":"hae-cadence","ttl":"-1h"}`, "must be positive"},
		{"beyond the typo guard", `{"branch":"hae-cadence","ttl":"8760h"}`, "must be at most"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fo := newFakeOrchestration()
			h, _ := newTestHandlers(t, &fakeKube{})
			h.Orch = fo

			rec := httptest.NewRecorder()
			h.CreatePreviewJSON(rec, jsonPost("/api/previews", tc.body))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !strings.Contains(body.Error, tc.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", body.Error, tc.wantMsg)
			}
			fo.mustNotCall(t)
		})
	}
}

// TestCreatePreviewEmptyTTLIsNoExpiry: an explicit empty/whitespace ttl is
// the CLI's natural rendering of "flag not passed", so it must mean no expiry
// rather than 400.
func TestCreatePreviewEmptyTTLIsNoExpiry(t *testing.T) {
	fo := newFakeOrchestration()
	h, _ := newTestHandlers(t, &fakeKube{})
	h.Orch = fo

	req := jsonPost("/api/previews", `{"branch":"hae-cadence","ttl":"  "}`)
	rec := httptest.NewRecorder()
	h.CreatePreviewJSON(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	fo.waitForCall(t)
	if fo.gotTTL != 0 {
		t.Errorf("Up called with ttl = %v, want 0", fo.gotTTL)
	}
}

func TestCreatePreviewEmptyBranch(t *testing.T) {
	fo := newFakeOrchestration()
	h, _ := newTestHandlers(t, &fakeKube{})
	h.Orch = fo

	req := jsonPost("/api/previews", `{"branch":"   "}`)
	rec := httptest.NewRecorder()
	h.CreatePreviewJSON(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("body = %q, want an error field", rec.Body.String())
	}
	fo.mustNotCall(t)
}

func TestCreatePreviewMissingBranchField(t *testing.T) {
	fo := newFakeOrchestration()
	h, _ := newTestHandlers(t, &fakeKube{})
	h.Orch = fo

	req := jsonPost("/api/previews", `{}`)
	rec := httptest.NewRecorder()
	h.CreatePreviewJSON(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	fo.mustNotCall(t)
}

func TestCreatePreviewInvalidJSON(t *testing.T) {
	fo := newFakeOrchestration()
	h, _ := newTestHandlers(t, &fakeKube{})
	h.Orch = fo

	req := jsonPost("/api/previews", `not json`)
	rec := httptest.NewRecorder()
	h.CreatePreviewJSON(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	fo.mustNotCall(t)
}

// TestCreatePreviewUnslugabbleBranch pins the defensive guard for a branch
// that TagForBranch reduces to "" (e.g. symbols-only) — must 400, not
// proceed with an empty preview tag.
func TestCreatePreviewUnslugabbleBranch(t *testing.T) {
	fo := newFakeOrchestration()
	h, _ := newTestHandlers(t, &fakeKube{})
	h.Orch = fo

	req := jsonPost("/api/previews", `{"branch":"---"}`)
	rec := httptest.NewRecorder()
	h.CreatePreviewJSON(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	fo.mustNotCall(t)
}

func TestCreatePreviewBusy(t *testing.T) {
	fo := newFakeOrchestration()
	fo.busyTags = map[string]bool{"hae-cadence": true}
	h, _ := newTestHandlers(t, &fakeKube{})
	h.Orch = fo

	req := jsonPost("/api/previews", `{"branch":"hae-cadence"}`)
	rec := httptest.NewRecorder()
	h.CreatePreviewJSON(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	fo.mustNotCall(t)
}

func TestCreatePreviewNoOrchestrator(t *testing.T) {
	h, _ := newTestHandlers(t, &fakeKube{})
	h.Orch = nil

	req := jsonPost("/api/previews", `{"branch":"hae-cadence"}`)
	rec := httptest.NewRecorder()
	h.CreatePreviewJSON(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestCreatePreviewSessionRequiresCSRF(t *testing.T) {
	fo := newFakeOrchestration()
	h, sess := newTestHandlers(t, &fakeKube{})
	h.Orch = fo

	req := withSession(jsonPost("/api/previews", `{"branch":"hae-cadence"}`), sess)
	rec := httptest.NewRecorder()
	h.CreatePreviewJSON(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	fo.mustNotCall(t)
}

func TestCreatePreviewSessionWithValidCSRF(t *testing.T) {
	fo := newFakeOrchestration()
	h, sess := newTestHandlers(t, &fakeKube{})
	h.Orch = fo

	req := withSession(jsonPost("/api/previews", `{"branch":"hae-cadence"}`), sess)
	req.Header.Set("X-CSRF-Token", csrf(h, sess))
	rec := httptest.NewRecorder()
	h.CreatePreviewJSON(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	fo.waitForCall(t)
}

// TestCreatePreviewBearerSkipsCSRF pins the bearer-path half of the guard:
// no session in context (as a bearer-authed request carries), no
// X-CSRF-Token header, and the request still succeeds.
func TestCreatePreviewBearerSkipsCSRF(t *testing.T) {
	fo := newFakeOrchestration()
	h, _ := newTestHandlers(t, &fakeKube{})
	h.Orch = fo

	req := jsonPost("/api/previews", `{"branch":"hae-cadence"}`) // no session, no header
	rec := httptest.NewRecorder()
	h.CreatePreviewJSON(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	fo.waitForCall(t)
}

// TestCreatePreviewGoroutineOutlivesRequestCancellation pins the goroutine
// context contract from the brief: Up must run on
// context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute),
// NOT the bare request context, so the request's own cancellation (which
// fires the instant net/http finishes writing the 202) can never abort an
// in-flight orchestration run.
func TestCreatePreviewGoroutineOutlivesRequestCancellation(t *testing.T) {
	fo := newFakeOrchestration()
	fo.releaseCh = make(chan struct{})
	h, _ := newTestHandlers(t, &fakeKube{})
	h.Orch = fo

	reqCtx, cancel := context.WithCancel(context.Background())
	req := jsonPost("/api/previews", `{"branch":"hae-cadence"}`).WithContext(reqCtx)
	rec := httptest.NewRecorder()
	h.CreatePreviewJSON(rec, req)

	// Wait until Up has captured its ctx and is blocked inside the fake
	// (not yet returned — so the handler's own `defer cancel()` on the
	// derived context hasn't fired yet either), then cancel the *request's*
	// context and check the derived one is unaffected.
	fo.waitForCall(t)
	cancel()

	if err := fo.gotCtx.Err(); err != nil {
		t.Errorf("Up's context was canceled by the request context: %v", err)
	}
	if _, ok := fo.gotCtx.Deadline(); !ok {
		t.Error("Up's context has no deadline, want the 30-minute timeout")
	}
	close(fo.releaseCh)
}

// --- DELETE /api/previews/{tag} ------------------------------------------

func deleteReq(tag string) *http.Request {
	r := httptest.NewRequest("DELETE", "/api/previews/"+tag, nil)
	r.SetPathValue("tag", tag)
	return r
}

func TestDeletePreview202Shape(t *testing.T) {
	fo := newFakeOrchestration()
	k := &fakeKube{namespaces: []kube.NamespaceInfo{nsInfo("preview-hae-cadence", "hae-cadence", "footstrike-api", "ready")}}
	h, _ := newTestHandlers(t, k)
	h.Orch = fo

	req := deleteReq("hae-cadence")
	rec := httptest.NewRecorder()
	h.DeletePreviewJSON(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var body struct {
		Tag      string `json:"tag"`
		Deleting bool   `json:"deleting"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Tag != "hae-cadence" || !body.Deleting {
		t.Errorf("body = %+v", body)
	}

	fo.waitForCall(t)
	if fo.gotTag != "hae-cadence" {
		t.Errorf("Down called with tag = %q, want hae-cadence", fo.gotTag)
	}
}

// TestDeletePreviewDownFailureIsLoggedNotSurfaced mirrors
// TestCreatePreviewUpFailureIsLoggedNotSurfaced for teardown.
func TestDeletePreviewDownFailureIsLoggedNotSurfaced(t *testing.T) {
	buf := captureSlog(t)
	fo := newFakeOrchestration()
	fo.downErr = errors.New("neon cleanup failed")
	k := &fakeKube{namespaces: []kube.NamespaceInfo{nsInfo("preview-hae-cadence", "hae-cadence", "footstrike-api", "ready")}}
	h, _ := newTestHandlers(t, k)
	h.Orch = fo

	req := deleteReq("hae-cadence")
	rec := httptest.NewRecorder()
	h.DeletePreviewJSON(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 even though Down will fail asynchronously", rec.Code)
	}
	fo.waitForCall(t)
	waitForLog(t, buf, "neon cleanup failed")
}

func TestDeletePreviewUnknownTag(t *testing.T) {
	fo := newFakeOrchestration()
	h, _ := newTestHandlers(t, &fakeKube{})
	h.Orch = fo

	req := deleteReq("nope")
	rec := httptest.NewRecorder()
	h.DeletePreviewJSON(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	fo.mustNotCall(t)
}

func TestDeletePreviewBusy(t *testing.T) {
	fo := newFakeOrchestration()
	fo.busyTags = map[string]bool{"hae-cadence": true}
	k := &fakeKube{namespaces: []kube.NamespaceInfo{nsInfo("preview-hae-cadence", "hae-cadence", "footstrike-api", "ready")}}
	h, _ := newTestHandlers(t, k)
	h.Orch = fo

	req := deleteReq("hae-cadence")
	rec := httptest.NewRecorder()
	h.DeletePreviewJSON(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	fo.mustNotCall(t)
}

func TestDeletePreviewNoOrchestrator(t *testing.T) {
	h, _ := newTestHandlers(t, &fakeKube{})
	h.Orch = nil

	req := deleteReq("hae-cadence")
	rec := httptest.NewRecorder()
	h.DeletePreviewJSON(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestDeletePreviewSessionRequiresCSRF(t *testing.T) {
	fo := newFakeOrchestration()
	k := &fakeKube{namespaces: []kube.NamespaceInfo{nsInfo("preview-hae-cadence", "hae-cadence", "footstrike-api", "ready")}}
	h, sess := newTestHandlers(t, k)
	h.Orch = fo

	req := withSession(deleteReq("hae-cadence"), sess)
	rec := httptest.NewRecorder()
	h.DeletePreviewJSON(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	fo.mustNotCall(t)
}

func TestDeletePreviewSessionWithValidCSRF(t *testing.T) {
	fo := newFakeOrchestration()
	k := &fakeKube{namespaces: []kube.NamespaceInfo{nsInfo("preview-hae-cadence", "hae-cadence", "footstrike-api", "ready")}}
	h, sess := newTestHandlers(t, k)
	h.Orch = fo

	req := withSession(deleteReq("hae-cadence"), sess)
	req.Header.Set("X-CSRF-Token", csrf(h, sess))
	rec := httptest.NewRecorder()
	h.DeletePreviewJSON(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	fo.waitForCall(t)
}

func TestDeletePreviewBearerSkipsCSRF(t *testing.T) {
	fo := newFakeOrchestration()
	k := &fakeKube{namespaces: []kube.NamespaceInfo{nsInfo("preview-hae-cadence", "hae-cadence", "footstrike-api", "ready")}}
	h, _ := newTestHandlers(t, k)
	h.Orch = fo

	req := deleteReq("hae-cadence") // no session, no header
	rec := httptest.NewRecorder()
	h.DeletePreviewJSON(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	fo.waitForCall(t)
}

func TestDeletePreviewGoroutineOutlivesRequestCancellation(t *testing.T) {
	fo := newFakeOrchestration()
	fo.releaseCh = make(chan struct{})
	k := &fakeKube{namespaces: []kube.NamespaceInfo{nsInfo("preview-hae-cadence", "hae-cadence", "footstrike-api", "ready")}}
	h, _ := newTestHandlers(t, k)
	h.Orch = fo

	reqCtx, cancel := context.WithCancel(context.Background())
	req := deleteReq("hae-cadence").WithContext(reqCtx)
	rec := httptest.NewRecorder()
	h.DeletePreviewJSON(rec, req)

	fo.waitForCall(t)
	cancel()

	if err := fo.gotCtx.Err(); err != nil {
		t.Errorf("Down's context was canceled by the request context: %v", err)
	}
	if _, ok := fo.gotCtx.Deadline(); !ok {
		t.Error("Down's context has no deadline, want the 30-minute timeout")
	}
	close(fo.releaseCh)
}

func TestDeletePreviewLookupError(t *testing.T) {
	fo := newFakeOrchestration()
	h, _ := newTestHandlers(t, &fakeKube{namespacesErr: errors.New("boom")})
	h.Orch = fo

	req := deleteReq("hae-cadence")
	rec := httptest.NewRecorder()
	h.DeletePreviewJSON(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	fo.mustNotCall(t)
}
