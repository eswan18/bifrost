package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eswan18/bifrost/internal/auth"
	"github.com/eswan18/bifrost/internal/config"
	"github.com/eswan18/bifrost/internal/gcb"
	"github.com/eswan18/bifrost/internal/kube"
)

// fakeKube serves per-namespace fixtures. imgs is a shorthand that expands each
// image into one healthy running pod; pods/rsets/cronjobs/jobs override or
// supplement it for a namespace when set. Like the real client, an empty
// namespace lists across all namespaces, with Namespace stamped on each item
// so the fleet's grouping works.
type fakeKube struct {
	mu       sync.Mutex
	imgs     map[string][]string
	pods     map[string][]kube.PodInfo
	rsets    map[string][]kube.ReplicaSetInfo
	cronjobs map[string][]kube.CronJobInfo
	jobs     map[string][]kube.JobInfo
	argoApps map[string]kube.AppStatus
	argoErr  error
	patched  map[string]string
	patchErr error
}

func (f *fakeKube) ListPods(_ context.Context, ns string) ([]kube.PodInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ns != "" {
		return f.podsIn(ns), nil
	}
	// Cluster-wide: the union of explicit pod fixtures and imgs-shorthand
	// namespaces.
	seen := map[string]bool{}
	var out []kube.PodInfo
	for ns := range f.pods {
		seen[ns] = true
		out = append(out, f.podsIn(ns)...)
	}
	for ns := range f.imgs {
		if !seen[ns] {
			out = append(out, f.podsIn(ns)...)
		}
	}
	return out, nil
}

// podsIn expands the imgs shorthand unless explicit pods override the
// namespace, stamping Namespace on copies so fixtures stay untouched.
func (f *fakeKube) podsIn(ns string) []kube.PodInfo {
	src, ok := f.pods[ns]
	if !ok {
		for i, img := range f.imgs[ns] {
			src = append(src, kube.PodInfo{
				Name:       fmt.Sprintf("pod-%d", i),
				Phase:      "Running",
				Containers: []kube.ContainerInfo{{Image: img, Ready: true}},
			})
		}
	}
	out := make([]kube.PodInfo, len(src))
	for i, p := range src {
		p.Namespace = ns
		out[i] = p
	}
	return out
}

func (f *fakeKube) ListArgoApps(_ context.Context) (map[string]kube.AppStatus, error) {
	if f.argoErr != nil {
		return nil, f.argoErr
	}
	return f.argoApps, nil
}

func (f *fakeKube) PatchAppImage(_ context.Context, app, env, image string) error {
	if f.patchErr != nil {
		return f.patchErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.patched == nil {
		f.patched = map[string]string{}
	}
	f.patched[app+"-"+env] = image
	return nil
}

func (f *fakeKube) ListCronJobs(_ context.Context, ns string) ([]kube.CronJobInfo, error) {
	if ns != "" {
		return f.cronjobs[ns], nil
	}
	var out []kube.CronJobInfo
	for ns, items := range f.cronjobs {
		for _, cj := range items {
			cj.Namespace = ns
			out = append(out, cj)
		}
	}
	return out, nil
}

func (f *fakeKube) ListJobs(_ context.Context, ns string) ([]kube.JobInfo, error) {
	if ns != "" {
		return f.jobs[ns], nil
	}
	var out []kube.JobInfo
	for ns, items := range f.jobs {
		for _, j := range items {
			j.Namespace = ns
			out = append(out, j)
		}
	}
	return out, nil
}

func (f *fakeKube) ListReplicaSets(_ context.Context, ns string) ([]kube.ReplicaSetInfo, error) {
	if ns != "" {
		return f.rsets[ns], nil
	}
	var out []kube.ReplicaSetInfo
	for ns, items := range f.rsets {
		for _, rs := range items {
			rs.Namespace = ns
			out = append(out, rs)
		}
	}
	return out, nil
}

func i32(v int32) *int32 { return &v }

func newTestHandlers(t *testing.T, k *fakeKube) (*Handlers, *auth.Session) {
	t.Helper()
	cfg := &config.Config{
		Services:        []string{"foo"},
		SessionSecret:   []byte("12345678901234567890123456789012"),
		ArgoCDNamespace: "argocd",
		GitHubOrg:       "eswan18",
		RepoOverrides:   map[string]string{"foo": "foo_repo"},
		DisplayLocation: time.UTC,
	}
	rend, err := LoadTemplates("../../templates")
	if err != nil {
		t.Fatalf("templates: %v", err)
	}
	sess := &auth.Session{Email: "me@example.com", IssuedAt: time.Now(), ID: "sid1"}
	return &Handlers{Cfg: cfg, Kube: k, Renderer: rend}, sess
}

func authed(t *testing.T, method, target string, body string, sess *auth.Session) *http.Request {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	return req.WithContext(auth.WithSessionForTest(req.Context(), sess))
}

func csrf(h *Handlers, sess *auth.Session) string {
	return auth.CSRFToken(h.Cfg.SessionSecret, sess.ID)
}

// --- promote (headline guards, preserved) ------------------------------------

func TestPromoteHappyPath(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:def5678"},
	}}
	h, sess := newTestHandlers(t, k)

	req := authed(t, "POST", "/services/foo/promote", "csrf="+csrf(h, sess)+"&expected_sha=abc1234", sess)
	req.SetPathValue("name", "foo")
	rec := httptest.NewRecorder()
	h.Promote(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code = %d", rec.Code)
	}
	if got := k.patched["foo-prod"]; got != "reg/foo:abc1234" {
		t.Errorf("patched = %q", got)
	}
}

func TestPromoteRejectsBadCSRF(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:def5678"},
	}}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "POST", "/services/foo/promote", "csrf=wrong", sess)
	req.SetPathValue("name", "foo")
	rec := httptest.NewRecorder()
	h.Promote(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestPromoteRefusesStaleExpectedSHA(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:def5678"},
	}}
	h, sess := newTestHandlers(t, k)

	req := authed(t, "POST", "/services/foo/promote", "csrf="+csrf(h, sess)+"&expected_sha=fff0000", sess)
	req.SetPathValue("name", "foo")
	rec := httptest.NewRecorder()
	h.Promote(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if got, ok := k.patched["foo-prod"]; ok {
		t.Fatalf("patched prod to %q despite stale expected_sha", got)
	}
	fl := flashFrom(t, rec)
	if fl == nil || fl.Kind != FlashError || !strings.Contains(fl.Msg, "staging changed") {
		t.Errorf("expected staleness error flash, got %+v", fl)
	}
}

func TestPromoteNothingToPromote(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:abc1234"},
	}}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "POST", "/services/foo/promote", "csrf="+csrf(h, sess), sess)
	req.SetPathValue("name", "foo")
	rec := httptest.NewRecorder()
	h.Promote(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code = %d", rec.Code)
	}
	if _, ok := k.patched["foo-prod"]; ok {
		t.Error("should not have patched")
	}
}

func TestPromoteJSONSuccess(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:def5678"},
	}}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "POST", "/services/foo/promote", "csrf="+csrf(h, sess)+"&expected_sha=abc1234", sess)
	req.Header.Set("Accept", "application/json")
	req.SetPathValue("name", "foo")
	rec := httptest.NewRecorder()
	h.Promote(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	got := decodeJSON(t, rec)
	if got["ok"] != true {
		t.Errorf("ok = %v", got["ok"])
	}
	if got["newTag"] != "abc1234" {
		t.Errorf("newTag = %v", got["newTag"])
	}
	if got["env"] != "prod" {
		t.Errorf("env = %v, want prod", got["env"])
	}
	if k.patched["foo-prod"] != "reg/foo:abc1234" {
		t.Errorf("patched = %q", k.patched["foo-prod"])
	}
}

func TestPromoteJSONStaleExpectedSHA(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:def5678"},
	}}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "POST", "/services/foo/promote", "csrf="+csrf(h, sess)+"&expected_sha=fff0000", sess)
	req.Header.Set("Accept", "application/json")
	req.SetPathValue("name", "foo")
	rec := httptest.NewRecorder()
	h.Promote(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", rec.Code)
	}
	got := decodeJSON(t, rec)
	if got["ok"] != false {
		t.Errorf("ok = %v", got["ok"])
	}
	if _, ok := k.patched["foo-prod"]; ok {
		t.Error("should not have patched")
	}
	if msg, _ := got["error"].(string); !strings.Contains(msg, "staging changed") {
		t.Errorf("error = %q", msg)
	}
}

// --- rollback ----------------------------------------------------------------

func rollbackKube() *fakeKube {
	// staging settled on abc1234 with def5678 as the prior revision.
	return &fakeKube{
		pods: map[string][]kube.PodInfo{
			"foo-staging": {{Name: "p", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/foo:abc1234", Ready: true}}}},
			"foo-prod":    {{Name: "p", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/foo:abc1234", Ready: true}}}},
		},
		rsets: map[string][]kube.ReplicaSetInfo{
			"foo-staging": {
				{Name: "rs2", Revision: 2, Image: "reg/foo:abc1234"},
				{Name: "rs1", Revision: 1, Image: "reg/foo:def5678"},
			},
			"foo-prod": {
				{Name: "rs2", Revision: 2, Image: "reg/foo:abc1234"},
				{Name: "rs1", Revision: 1, Image: "reg/foo:def5678"},
			},
		},
	}
}

func TestRollbackHappyPath(t *testing.T) {
	k := rollbackKube()
	h, sess := newTestHandlers(t, k)

	body := "csrf=" + csrf(h, sess) + "&env=staging&to_sha=def5678&expected_current_sha=abc1234"
	req := authed(t, "POST", "/services/foo/rollback", body, sess)
	req.SetPathValue("name", "foo")
	rec := httptest.NewRecorder()
	h.Rollback(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code = %d", rec.Code)
	}
	if got := k.patched["foo-staging"]; got != "reg/foo:def5678" {
		t.Errorf("patched staging = %q, want reg/foo:def5678", got)
	}
	if _, ok := k.patched["foo-prod"]; ok {
		t.Error("prod should not have been patched")
	}
}

func TestRollbackProdPatchesProd(t *testing.T) {
	k := rollbackKube()
	h, sess := newTestHandlers(t, k)
	body := "csrf=" + csrf(h, sess) + "&env=prod&to_sha=def5678&expected_current_sha=abc1234"
	req := authed(t, "POST", "/services/foo/rollback", body, sess)
	req.SetPathValue("name", "foo")
	rec := httptest.NewRecorder()
	h.Rollback(rec, req)
	if got := k.patched["foo-prod"]; got != "reg/foo:def5678" {
		t.Errorf("patched prod = %q, want reg/foo:def5678", got)
	}
}

func TestRollbackJSONSuccess(t *testing.T) {
	k := rollbackKube()
	h, sess := newTestHandlers(t, k)
	body := "csrf=" + csrf(h, sess) + "&env=staging&to_sha=def5678&expected_current_sha=abc1234"
	req := authed(t, "POST", "/services/foo/rollback", body, sess)
	req.Header.Set("Accept", "application/json")
	req.SetPathValue("name", "foo")
	rec := httptest.NewRecorder()
	h.Rollback(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	got := decodeJSON(t, rec)
	if got["ok"] != true || got["newTag"] != "def5678" || got["env"] != "staging" {
		t.Errorf("unexpected JSON: %v", got)
	}
}

func TestRollbackJSONReturnsFullSuffixedTag(t *testing.T) {
	// Suffix-tagged service: staging is settled on abc1234-staging with
	// def5678-staging as the prior revision. The browser poll compares newTag
	// against the env's FULL tag, so the JSON must carry "def5678-staging" — the
	// full previous tag — not the bare SHA, or the poll would never match.
	k := &fakeKube{
		pods: map[string][]kube.PodInfo{
			"foo-staging": {{Name: "p", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/foo:abc1234-staging", Ready: true}}}},
			"foo-prod":    {{Name: "p", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/foo:abc1234-staging", Ready: true}}}},
		},
		rsets: map[string][]kube.ReplicaSetInfo{
			"foo-staging": {
				{Name: "rs2", Revision: 2, Image: "reg/foo:abc1234-staging"},
				{Name: "rs1", Revision: 1, Image: "reg/foo:def5678-staging"},
			},
		},
	}
	h, sess := newTestHandlers(t, k)
	// to_sha / expected_current_sha stay bare SHAs (correct for form validation).
	body := "csrf=" + csrf(h, sess) + "&env=staging&to_sha=def5678&expected_current_sha=abc1234"
	req := authed(t, "POST", "/services/foo/rollback", body, sess)
	req.Header.Set("Accept", "application/json")
	req.SetPathValue("name", "foo")
	rec := httptest.NewRecorder()
	h.Rollback(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	got := decodeJSON(t, rec)
	if got["newTag"] != "def5678-staging" {
		t.Errorf("newTag = %v, want def5678-staging (full suffixed tag for the poll)", got["newTag"])
	}
	if k.patched["foo-staging"] != "reg/foo:def5678-staging" {
		t.Errorf("patched = %q, want reg/foo:def5678-staging", k.patched["foo-staging"])
	}
}

func TestRollbackRejectsBadCSRF(t *testing.T) {
	k := rollbackKube()
	h, sess := newTestHandlers(t, k)
	req := authed(t, "POST", "/services/foo/rollback", "csrf=wrong&env=staging", sess)
	req.SetPathValue("name", "foo")
	rec := httptest.NewRecorder()
	h.Rollback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
	if len(k.patched) != 0 {
		t.Error("should not have patched")
	}
}

func TestRollbackRejectsBadEnv(t *testing.T) {
	k := rollbackKube()
	h, sess := newTestHandlers(t, k)
	body := "csrf=" + csrf(h, sess) + "&env=production&to_sha=def5678&expected_current_sha=abc1234"
	req := authed(t, "POST", "/services/foo/rollback", body, sess)
	req.Header.Set("Accept", "application/json")
	req.SetPathValue("name", "foo")
	rec := httptest.NewRecorder()
	h.Rollback(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	if got := decodeJSON(t, rec); !strings.Contains(fmt.Sprint(got["error"]), "invalid environment") {
		t.Errorf("error = %v", got["error"])
	}
}

func TestRollbackRefusesStaleCurrent(t *testing.T) {
	k := rollbackKube()
	h, sess := newTestHandlers(t, k)
	// The user saw fff0000 but staging is actually on abc1234.
	body := "csrf=" + csrf(h, sess) + "&env=staging&to_sha=def5678&expected_current_sha=fff0000"
	req := authed(t, "POST", "/services/foo/rollback", body, sess)
	req.Header.Set("Accept", "application/json")
	req.SetPathValue("name", "foo")
	rec := httptest.NewRecorder()
	h.Rollback(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", rec.Code)
	}
	if _, ok := k.patched["foo-staging"]; ok {
		t.Error("should not have patched on stale current sha")
	}
	if got := decodeJSON(t, rec); !strings.Contains(fmt.Sprint(got["error"]), "changed since page load") {
		t.Errorf("error = %v", got["error"])
	}
}

func TestRollbackRefusesNoPreviousImage(t *testing.T) {
	k := rollbackKube()
	// Single revision → no previous image to roll back to.
	k.rsets["foo-staging"] = []kube.ReplicaSetInfo{{Name: "rs1", Revision: 1, Image: "reg/foo:abc1234"}}
	h, sess := newTestHandlers(t, k)
	body := "csrf=" + csrf(h, sess) + "&env=staging&to_sha=def5678&expected_current_sha=abc1234"
	req := authed(t, "POST", "/services/foo/rollback", body, sess)
	req.Header.Set("Accept", "application/json")
	req.SetPathValue("name", "foo")
	rec := httptest.NewRecorder()
	h.Rollback(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", rec.Code)
	}
	if got := decodeJSON(t, rec); !strings.Contains(fmt.Sprint(got["error"]), "no previous image") {
		t.Errorf("error = %v", got["error"])
	}
}

func TestRollbackRefusesMidDeployEnv(t *testing.T) {
	k := rollbackKube()
	// Two distinct images in staging → mid-deploy, must refuse.
	k.pods["foo-staging"] = []kube.PodInfo{
		{Name: "a", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/foo:abc1234", Ready: true}}},
		{Name: "b", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/foo:def5678", Ready: true}}},
	}
	h, sess := newTestHandlers(t, k)
	body := "csrf=" + csrf(h, sess) + "&env=staging&to_sha=def5678&expected_current_sha=abc1234"
	req := authed(t, "POST", "/services/foo/rollback", body, sess)
	req.Header.Set("Accept", "application/json")
	req.SetPathValue("name", "foo")
	rec := httptest.NewRecorder()
	h.Rollback(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", rec.Code)
	}
	if _, ok := k.patched["foo-staging"]; ok {
		t.Error("should not have patched mid-deploy")
	}
	if got := decodeJSON(t, rec); !strings.Contains(fmt.Sprint(got["error"]), "mid-deploy") {
		t.Errorf("error = %v", got["error"])
	}
}

// --- status JSON -------------------------------------------------------------

func TestStatusJSON(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:def5678"},
	}}
	h, _ := newTestHandlers(t, k)
	req := httptest.NewRequest("GET", "/services/foo/status", nil)
	req.SetPathValue("name", "foo")
	rec := httptest.NewRecorder()
	h.StatusJSON(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	got := decodeJSON(t, rec)
	if got["state"] != "out_of_sync" {
		t.Errorf("state = %v", got["state"])
	}
	if got["prodTag"] != "def5678" || got["newProdTag"] != "abc1234" {
		t.Errorf("tags = %v / %v", got["prodTag"], got["newProdTag"])
	}
	// Per-env objects for rollback polling.
	prod, _ := got["prod"].(map[string]any)
	if prod == nil || prod["tag"] != "def5678" || prod["status"] != "ok" {
		t.Errorf("prod env object = %v", got["prod"])
	}
	staging, _ := got["staging"].(map[string]any)
	if staging == nil || staging["tag"] != "abc1234" {
		t.Errorf("staging env object = %v", got["staging"])
	}
}

func TestStatusJSONUnknownService(t *testing.T) {
	h, _ := newTestHandlers(t, &fakeKube{})
	req := httptest.NewRequest("GET", "/services/nope/status", nil)
	req.SetPathValue("name", "nope")
	rec := httptest.NewRecorder()
	h.StatusJSON(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}

// --- apps page ---------------------------------------------------------------

func TestOverview404sNonRootPaths(t *testing.T) {
	h, sess := newTestHandlers(t, &fakeKube{})
	req := authed(t, "GET", "/favicon.ico", "", sess)
	rec := httptest.NewRecorder()
	h.Overview(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}

func TestAppsPageRendersPromoteModal(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:def5678"},
	}}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "GET", "/apps", "", sess)
	rec := httptest.NewRecorder()
	h.Apps(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="modal-promote-foo"`) {
		t.Error("promote modal missing")
	}
	if !strings.Contains(body, `action="/services/foo/promote"`) {
		t.Error("promote form missing")
	}
	if !strings.Contains(body, `name="csrf" value="`+csrf(h, sess)+`"`) {
		t.Error("CSRF token missing from promote form")
	}
	if !strings.Contains(body, `name="expected_sha" value="abc1234"`) {
		t.Error("expected_sha missing from promote form")
	}
	// Drift → primary "Promote ↗" trigger in the row.
	if !strings.Contains(body, `href="#modal-promote-foo"`) {
		t.Error("Promote trigger missing from row")
	}
}

func TestAppsPageRendersRollbackModal(t *testing.T) {
	k := rollbackKube()
	h, sess := newTestHandlers(t, k)
	req := authed(t, "GET", "/apps", "", sess)
	rec := httptest.NewRecorder()
	h.Apps(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `id="modal-rollback-foo"`) {
		t.Error("rollback modal missing")
	}
	if !strings.Contains(body, `action="/services/foo/rollback"`) {
		t.Error("rollback form missing")
	}
	if !strings.Contains(body, `name="to_sha" value="def5678"`) {
		t.Error("rollback to_sha (previous image) missing")
	}
	if !strings.Contains(body, `name="expected_current_sha" value="abc1234"`) {
		t.Error("rollback expected_current_sha missing")
	}
	// Staging caveat about image-updater re-pinning.
	if !strings.Contains(body, "pinned by image-updater") {
		t.Error("staging rollback caveat missing")
	}
}

func TestAppsRollbackDisabledWhenNoPrevious(t *testing.T) {
	// In sync, single revision → no previous image anywhere.
	k := &fakeKube{
		pods: map[string][]kube.PodInfo{
			"foo-staging": {{Name: "p", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/foo:abc1234", Ready: true}}}},
			"foo-prod":    {{Name: "p", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/foo:abc1234", Ready: true}}}},
		},
		rsets: map[string][]kube.ReplicaSetInfo{
			"foo-staging": {{Name: "rs1", Revision: 1, Image: "reg/foo:abc1234"}},
			"foo-prod":    {{Name: "rs1", Revision: 1, Image: "reg/foo:abc1234"}},
		},
	}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "GET", "/apps", "", sess)
	rec := httptest.NewRecorder()
	h.Apps(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "no previous image known") {
		t.Error("expected 'no previous image known' when history holds one revision")
	}
	if !strings.Contains(body, "disabled") {
		t.Error("expected a disabled rollback button")
	}
}

func TestAppsRendersHealthAndCommitLinks(t *testing.T) {
	k := &fakeKube{
		imgs: map[string][]string{"foo-prod": {"reg/foo:def5678"}},
		pods: map[string][]kube.PodInfo{
			"foo-staging": {{
				Name: "pod-0", Phase: "Running",
				Containers: []kube.ContainerInfo{
					{Image: "reg/foo:abc1234", Ready: true},
					{Image: "reg/foo:abc1234", Ready: false},
				},
			}},
		},
	}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "GET", "/apps", "", sess)
	rec := httptest.NewRecorder()
	h.Apps(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `href="https://github.com/eswan18/foo_repo/commit/abc1234"`) {
		t.Error("staging tag should link to the commit using the override repo name")
	}
	// One container not ready → env reads as deploying with a progress fraction.
	if !strings.Contains(body, "deploying") {
		t.Error("partially-ready env should read as deploying")
	}
}

func TestAppsRendersOpenLinks(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:abc1234"},
	}}
	h, sess := newTestHandlers(t, k)
	h.Cfg.ProdURLs = map[string]string{"foo": "https://foo.example.com"}
	req := authed(t, "GET", "/apps", "", sess)
	rec := httptest.NewRecorder()
	h.Apps(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `href="https://foo.example.com"`) || !strings.Contains(body, `aria-label="open prod app"`) {
		t.Error("prod open link missing")
	}
	if strings.Contains(body, `aria-label="open staging app"`) {
		t.Error("no staging open link should render when STAGING_URLS lacks the service")
	}
}

func TestAppsRendersRepoAndBuildLinks(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:abc1234"},
	}}
	h, sess := newTestHandlers(t, k)
	h.Cfg.GCPProject = "ethans-services"
	h.TriggerIDs = map[string]string{"foo": "trig-123"}
	h.Builds = &fakeBuilds{builds: map[string]gcb.BuildStatus{
		"foo_repo": {Status: "SUCCESS", SHA: "abc1234", LogURL: "https://console.example/build/1", FinishTime: time.Now()},
	}}
	req := authed(t, "GET", "/apps", "", sess)
	rec := httptest.NewRecorder()
	h.Apps(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `href="https://github.com/eswan18/foo_repo"`) {
		t.Error("app name should link to the GitHub repo using the override name")
	}
	// Design always shows the last build; SUCCESS renders a ✓.
	if !strings.Contains(body, "✓ ") {
		t.Error("successful build badge (✓) missing")
	}
	if !strings.Contains(body, `href="https://console.example/build/1"`) {
		t.Error("build should link to its Cloud Build log")
	}
}

func TestAppsBuildLinksToPipelineWithoutTrackedBuild(t *testing.T) {
	// No h.Builds → no tracked build (Build.Label is empty), but the pipeline
	// history URL is still derivable from the project + trigger ID, so the BUILD
	// cell must render a link there rather than an inert em dash.
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:abc1234"},
	}}
	h, sess := newTestHandlers(t, k)
	h.Cfg.GCPProject = "ethans-services"
	h.TriggerIDs = map[string]string{"foo": "trig-123"}
	req := authed(t, "GET", "/apps", "", sess)
	rec := httptest.NewRecorder()
	h.Apps(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "console.cloud.google.com/cloud-build/builds") {
		t.Error("build cell should link to the pipeline history when no build is tracked")
	}
}

// --- fragments ---------------------------------------------------------------

func TestAppsFragment(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:def5678"},
	}}
	h, sess := newTestHandlers(t, k)
	h.Builds = &fakeBuilds{builds: map[string]gcb.BuildStatus{
		"foo_repo": {Status: "WORKING", SHA: "abc1234", StartTime: time.Now().Add(-2 * time.Minute), LogURL: "https://console.example/build/1"},
	}}
	req := authed(t, "GET", "/partial/apps", "", sess)
	rec := httptest.NewRecorder()
	h.AppsFragment(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-key="foo"`) {
		t.Error("app row missing from fragment")
	}
	if !strings.Contains(body, `href="#modal-promote-foo"`) {
		t.Error("promote trigger missing from fragment")
	}
	// Live build state renders in the fragment.
	if !strings.Contains(body, "◌ running") {
		t.Error("in-progress build badge missing from fragment")
	}
	// No page chrome.
	if strings.Contains(body, "<!DOCTYPE") || strings.Contains(body, "Sign out") {
		t.Error("fragment should not include full-page chrome")
	}
}

func TestAppsFragmentMarksActive(t *testing.T) {
	render := func(t *testing.T, k *fakeKube, builds gcb.Client) string {
		t.Helper()
		h, sess := newTestHandlers(t, k)
		h.Builds = builds
		req := authed(t, "GET", "/partial/apps", "", sess)
		rec := httptest.NewRecorder()
		h.AppsFragment(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d", rec.Code)
		}
		return rec.Body.String()
	}

	t.Run("building is active", func(t *testing.T) {
		k := &fakeKube{imgs: map[string][]string{"foo-staging": {"reg/foo:abc1234"}, "foo-prod": {"reg/foo:abc1234"}}}
		builds := &fakeBuilds{builds: map[string]gcb.BuildStatus{"foo_repo": {Status: "WORKING", SHA: "abc1234"}}}
		if !strings.Contains(render(t, k, builds), "data-active") {
			t.Error("a building service should be marked data-active")
		}
	})
	t.Run("mid-deploy is active", func(t *testing.T) {
		k := &fakeKube{imgs: map[string][]string{"foo-staging": {"reg/foo:abc1234", "reg/foo:def5678"}, "foo-prod": {"reg/foo:abc1234"}}}
		if !strings.Contains(render(t, k, nil), "data-active") {
			t.Error("a mid-deploy service should be marked data-active")
		}
	})
	t.Run("settled is not active", func(t *testing.T) {
		k := &fakeKube{imgs: map[string][]string{"foo-staging": {"reg/foo:abc1234"}, "foo-prod": {"reg/foo:abc1234"}}}
		if strings.Contains(render(t, k, nil), "data-active") {
			t.Error("an in-sync service with no build should not be marked data-active")
		}
	})
}

func TestAppsSurvivesArgoListFailure(t *testing.T) {
	k := &fakeKube{
		imgs: map[string][]string{
			"foo-staging": {"reg/foo:abc1234"},
			"foo-prod":    {"reg/foo:def5678"},
		},
		argoErr: errors.New("argocd api down"),
	}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "GET", "/apps", "", sess)
	rec := httptest.NewRecorder()
	h.Apps(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `action="/services/foo/promote"`) {
		t.Error("promote form should still render when argo list fails")
	}
}

func TestAppsSurvivesPodListFailure(t *testing.T) {
	// A namespace whose pod list errors degrades that env to unknown, not 500.
	k := &fakeKube{imgs: map[string][]string{"foo-prod": {"reg/foo:abc1234"}}}
	// staging has no fixtures at all → empty images → unknown.
	h, sess := newTestHandlers(t, k)
	req := authed(t, "GET", "/apps", "", sess)
	rec := httptest.NewRecorder()
	h.Apps(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unknown") {
		t.Error("an env with no readable pods should render as unknown")
	}
}

// --- overview ----------------------------------------------------------------

func TestOverviewAttentionAndFleetOnCrash(t *testing.T) {
	k := &fakeKube{pods: map[string][]kube.PodInfo{
		"foo-staging": {{Name: "s", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/foo:abc1234", Ready: true}}}},
		"foo-prod": {{Name: "p", Phase: "Running", Containers: []kube.ContainerInfo{
			{Image: "reg/foo:def5678", Ready: false, WaitingReason: "CrashLoopBackOff", RestartCount: 7},
		}}},
	}}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "GET", "/", "", sess)
	rec := httptest.NewRecorder()
	h.Overview(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"ATTENTION · 1", "crashlooping in prod", "7 restarts", "Roll back", `href="#modal-rollback-foo"`} {
		if !strings.Contains(body, want) {
			t.Errorf("overview missing %q", want)
		}
	}
	// Fleet: one crashed, and the tab issue badge lit.
	if !strings.Contains(body, "Crashed") {
		t.Error("fleet Crashed row missing")
	}
}

func TestOverviewAllClear(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:abc1234"},
	}}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "GET", "/", "", sess)
	rec := httptest.NewRecorder()
	h.Overview(rec, req)
	if !strings.Contains(rec.Body.String(), "All clear") {
		t.Error("in-sync fleet with no issues should show the all-clear state")
	}
}

func TestOverviewDriftAttention(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:def5678"},
	}}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "GET", "/", "", sess)
	rec := httptest.NewRecorder()
	h.Overview(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "ready to promote") || !strings.Contains(body, `href="#modal-promote-foo"`) {
		t.Error("drift should surface a Promote attention item")
	}
}

// --- jobs --------------------------------------------------------------------

func TestJobsAssembly(t *testing.T) {
	// Deterministic next run regardless of wall clock.
	orig := nextRun
	nextRun = func(schedule, tz string, after time.Time) (time.Time, error) {
		return time.Date(2030, 1, 1, 14, 0, 0, 0, time.UTC), nil
	}
	defer func() { nextRun = orig }()

	start := time.Now().Add(-time.Hour)
	k := &fakeKube{
		pods: map[string][]kube.PodInfo{
			"foo-staging": {
				{Name: "web", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/foo:abc1234", Ready: true}}},
				{Name: "nightly-123-pod", Phase: "Failed", OwnerKind: "Job", OwnerName: "nightly-123",
					Containers: []kube.ContainerInfo{{Image: "reg/foo:abc1234", ExitCode: i32(137), TerminatedReason: "OOMKilled"}}},
			},
			"foo-prod": {
				{Name: "web", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/foo:abc1234", Ready: true}}},
			},
		},
		cronjobs: map[string][]kube.CronJobInfo{
			"foo-staging": {{Name: "nightly", Schedule: "0 3 * * *", Image: "reg/foo:abc1234"}},
			"foo-prod":    {{Name: "cleanup", Schedule: "0 4 * * *", Image: "reg/foo:abc1234"}},
		},
		jobs: map[string][]kube.JobInfo{
			"foo-staging": {{Name: "nightly-123", OwnerCron: "nightly", Image: "reg/foo:abc1234", StartTime: start, Failed: true, FailReason: "BackoffLimitExceeded"}},
			"foo-prod":    {{Name: "cleanup-9", OwnerCron: "cleanup", Image: "reg/foo:abc1234", StartTime: start, CompletionTime: start.Add(90 * time.Second), Succeeded: true}},
		},
	}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "GET", "/partial/jobs", "", sess)
	rec := httptest.NewRecorder()
	h.JobsFragment(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"nightly", "cleanup",
		`class="micro-inline">stg`, `class="micro-inline">prod`, // env micro-labels
		"✗ Failed", "exit 137 (OOMKilled)", // failed job: exit code + terminated reason from its pod
		"✓ Succeeded", "1m 30s", // completed duration
		"Jan 1 14:00", // next run
	} {
		if !strings.Contains(body, want) {
			t.Errorf("jobs fragment missing %q", want)
		}
	}
}

func TestBuildJobsPageFilter(t *testing.T) {
	f := &fleet{
		Apps: []appView{
			{Name: "bar", HasJobs: true, Jobs: []jobView{{App: "bar", Name: "c"}}},
			{Name: "foo", HasJobs: true, Jobs: []jobView{{App: "foo", Name: "a"}, {App: "foo", Name: "b"}}},
		},
		Jobs: []jobView{
			{App: "foo", Name: "a", State: "ok"},
			{App: "foo", Name: "b", State: "failed"},
			{App: "bar", Name: "c", State: "running"},
		},
		JobCount: 3,
	}

	filtered := buildJobsPage(f, "foo")
	if len(filtered.Jobs) != 2 {
		t.Errorf("filtered jobs = %d, want 2", len(filtered.Jobs))
	}
	if filtered.SummaryLabel != "2 jobs · foo" {
		t.Errorf("summary = %q", filtered.SummaryLabel)
	}
	if len(filtered.Options) != 2 {
		t.Errorf("options = %d, want 2 (apps with jobs)", len(filtered.Options))
	}

	all := buildJobsPage(f, "")
	if all.SummaryLabel != "3 jobs across 2 apps" {
		t.Errorf("summary = %q", all.SummaryLabel)
	}
	// Sorted failed → running → ok.
	order := []string{all.Jobs[0].Name, all.Jobs[1].Name, all.Jobs[2].Name}
	if order[0] != "b" || order[1] != "c" || order[2] != "a" {
		t.Errorf("sort order = %v, want [b c a]", order)
	}
}

func TestJobsFilterUnknownAppIgnored(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{"foo-staging": {"reg/foo:abc1234"}, "foo-prod": {"reg/foo:abc1234"}}}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "GET", "/jobs?app=ghost", "", sess)
	if got := h.jobFilter(req); got != "" {
		t.Errorf("jobFilter(unknown) = %q, want empty", got)
	}
}

// --- helper unit tests -------------------------------------------------------

func TestBuildPipelineURL(t *testing.T) {
	got := buildPipelineURL("ethans-services", "trig-123")
	want := `https://console.cloud.google.com/cloud-build/builds;region=global?project=ethans-services&query=trigger_id%3D%22trig-123%22`
	if got != want {
		t.Errorf("buildPipelineURL = %q, want %q", got, want)
	}
	if buildPipelineURL("", "trig-123") != "" || buildPipelineURL("ethans-services", "") != "" {
		t.Error("no URL should be built without a project or trigger ID")
	}
}

func TestRepoURL(t *testing.T) {
	if got := repoURL("eswan18", "asset_manager"); got != "https://github.com/eswan18/asset_manager" {
		t.Errorf("repoURL = %q", got)
	}
	if repoURL("", "foo") != "" || repoURL("eswan18", "") != "" {
		t.Error("repoURL should be empty when org or repo is missing")
	}
}

func TestBackTo(t *testing.T) {
	cases := map[string]string{
		"":                               "/",
		"https://x.example/apps":         "/apps",
		"https://x.example/jobs?app=foo": "/jobs?app=foo",
		"https://x.example/evil":         "/",
	}
	for ref, want := range cases {
		req := httptest.NewRequest("POST", "/services/foo/promote", nil)
		if ref != "" {
			req.Header.Set("Referer", ref)
		}
		if got := backTo(req); got != want {
			t.Errorf("backTo(%q) = %q, want %q", ref, got, want)
		}
	}
}

// --- test doubles / helpers --------------------------------------------------

type fakeBuilds struct {
	builds map[string]gcb.BuildStatus
	err    error
}

func (f *fakeBuilds) LatestBuilds(_ context.Context) (map[string]gcb.BuildStatus, error) {
	return f.builds, f.err
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	return got
}

func flashFrom(t *testing.T, rec *httptest.ResponseRecorder) *Flash {
	t.Helper()
	next := httptest.NewRequest("GET", "/", nil)
	for _, c := range rec.Result().Cookies() {
		next.AddCookie(c)
	}
	return TakeFlash(httptest.NewRecorder(), next)
}
