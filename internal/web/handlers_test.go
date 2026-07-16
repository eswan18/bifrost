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

type fakeKube struct {
	mu       sync.Mutex
	imgs     map[string][]string       // each image becomes one healthy running pod
	pods     map[string][]kube.PodInfo // overrides imgs for a namespace when set
	argoApps map[string]kube.AppStatus
	argoErr  error
	patched  map[string]string
	patchErr error
}

func (f *fakeKube) ListPods(_ context.Context, ns string) ([]kube.PodInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pods, ok := f.pods[ns]; ok {
		return pods, nil
	}
	var out []kube.PodInfo
	for i, img := range f.imgs[ns] {
		out = append(out, kube.PodInfo{
			Name:       fmt.Sprintf("pod-%d", i),
			Phase:      "Running",
			Containers: []kube.ContainerInfo{{Image: img, Ready: true}},
		})
	}
	return out, nil
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
	return nil, nil
}

func (f *fakeKube) ListJobs(_ context.Context, ns string) ([]kube.JobInfo, error) {
	return nil, nil
}

func (f *fakeKube) ListReplicaSets(_ context.Context, ns string) ([]kube.ReplicaSetInfo, error) {
	return nil, nil
}

func newTestHandlers(t *testing.T, k *fakeKube) (*Handlers, *auth.SessionManager, *auth.Session) {
	t.Helper()
	cfg := &config.Config{
		Services:        []string{"foo"},
		SessionSecret:   []byte("12345678901234567890123456789012"),
		ArgoCDNamespace: "argocd",
		GitHubOrg:       "eswan18",
		RepoOverrides:   map[string]string{"foo": "foo_repo"},
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
	if got := k.patched["foo-prod"]; got != "reg/foo:abc1234" {
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
	if got, ok := k.patched["foo-prod"]; ok {
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

// TestStatusFragment: the poller endpoint renders just the service rows
// (the same markup as the full page, including live build state and promote
// forms) with none of the page chrome, so the browser can swap it in place
// without a full reload.
func TestStatusFragment(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:def5678"},
	}}
	h, _, sess := newTestHandlers(t, k)
	h.Builds = &fakeBuilds{builds: map[string]gcb.BuildStatus{
		"foo_repo": {Status: "WORKING", SHA: "abc1234", LogURL: "https://console.example/build/1"},
	}}

	req := httptest.NewRequest("GET", "/partial/status", nil)
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))
	rec := httptest.NewRecorder()
	h.StatusFragment(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// The rows themselves, with live build state and a working promote form.
	if !strings.Contains(body, `data-service="foo"`) {
		t.Error("service row missing from fragment")
	}
	if !strings.Contains(body, `action="/services/foo/promote"`) {
		t.Error("promote form missing from fragment")
	}
	csrf := auth.CSRFToken(h.Cfg.SessionSecret, sess.ID)
	if !strings.Contains(body, `name="csrf" value="`+csrf+`"`) {
		t.Error("CSRF token missing from fragment promote form")
	}
	if !strings.Contains(body, "building abc1234") {
		t.Error("live build badge missing from fragment")
	}
	// None of the page chrome — this is a fragment, not a full document.
	if strings.Contains(body, "<!DOCTYPE") || strings.Contains(body, "Sign out") {
		t.Error("fragment should not include the full-page chrome")
	}
}

// TestStatusMarksActiveRows: rows that are mid-deploy or have an in-progress
// build are tagged data-active so the client polls them on a fast cadence;
// settled rows (in sync, out of sync) are not.
func TestStatusMarksActiveRows(t *testing.T) {
	// Render the fragment (rows only): the full page also embeds base.html's
	// JS, which mentions the [data-active] selector and would false-match.
	render := func(t *testing.T, k *fakeKube, builds gcb.Client) string {
		t.Helper()
		h, _, sess := newTestHandlers(t, k)
		h.Builds = builds
		req := httptest.NewRequest("GET", "/partial/status", nil)
		req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))
		rec := httptest.NewRecorder()
		h.StatusFragment(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", rec.Code)
		}
		return rec.Body.String()
	}

	t.Run("building is active", func(t *testing.T) {
		k := &fakeKube{imgs: map[string][]string{
			"foo-staging": {"reg/foo:abc1234"},
			"foo-prod":    {"reg/foo:abc1234"},
		}}
		builds := &fakeBuilds{builds: map[string]gcb.BuildStatus{
			"foo_repo": {Status: "WORKING", SHA: "abc1234"},
		}}
		if !strings.Contains(render(t, k, builds), "data-active") {
			t.Error("a building service should be marked data-active")
		}
	})

	t.Run("mid-deploy is active", func(t *testing.T) {
		k := &fakeKube{imgs: map[string][]string{
			"foo-staging": {"reg/foo:abc1234", "reg/foo:def5678"},
			"foo-prod":    {"reg/foo:abc1234"},
		}}
		if !strings.Contains(render(t, k, nil), "data-active") {
			t.Error("a mid-deploy service should be marked data-active")
		}
	})

	t.Run("settled is not active", func(t *testing.T) {
		k := &fakeKube{imgs: map[string][]string{
			"foo-staging": {"reg/foo:abc1234"},
			"foo-prod":    {"reg/foo:abc1234"},
		}}
		if strings.Contains(render(t, k, nil), "data-active") {
			t.Error("an in-sync service with no build should not be marked data-active")
		}
	})
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
	if k.patched["foo-prod"] != "reg/foo:abc1234" {
		t.Errorf("patched = %q, want reg/foo:abc1234", k.patched["foo-prod"])
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
	if _, ok := k.patched["foo-prod"]; ok {
		t.Error("should not have patched on stale expected_sha")
	}
	if msg, _ := got["error"].(string); !strings.Contains(msg, "staging changed") {
		t.Errorf("error = %q, want staleness message", msg)
	}
}

// TestStatusRendersHealthAndCommitLinks: env lines link tags to GitHub
// commits (honoring repo overrides) and show a health badge derived from pod
// readiness.
func TestStatusRendersHealthAndCommitLinks(t *testing.T) {
	k := &fakeKube{
		imgs: map[string][]string{
			"foo-prod": {"reg/foo:def5678"},
		},
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
	h, _, sess := newTestHandlers(t, k)

	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))
	rec := httptest.NewRecorder()
	h.Status(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `href="https://github.com/eswan18/foo_repo/commit/abc1234"`) {
		t.Error("staging tag should link to the commit using the override repo name")
	}
	if !strings.Contains(body, "1/2 ready") {
		t.Error("staging health badge should show 1/2 ready")
	}
}

// TestStatusRendersOpenLinks: a service with a configured URL gets an "open"
// link on that env line; an env with no configured URL gets none.
func TestStatusRendersOpenLinks(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:abc1234"},
	}}
	h, _, sess := newTestHandlers(t, k)
	// Prod URL configured, staging deliberately omitted (like a service with
	// no tailnet ingress): only the prod env line should get an "open" link.
	h.Cfg.ProdURLs = map[string]string{"foo": "https://foo.example.com"}

	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))
	rec := httptest.NewRecorder()
	h.Status(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `href="https://foo.example.com"`) ||
		!strings.Contains(body, `aria-label="open prod app"`) {
		t.Error("prod open link missing")
	}
	if strings.Contains(body, `aria-label="open staging app"`) {
		t.Error("no staging open link should render when STAGING_URLS lacks the service")
	}
}

func TestBuildPipelineURL(t *testing.T) {
	got := buildPipelineURL("ethans-services", "trig-123")
	want := `https://console.cloud.google.com/cloud-build/builds;region=global?project=ethans-services&query=trigger_id%3D%22trig-123%22`
	if got != want {
		t.Errorf("buildPipelineURL = %q, want %q", got, want)
	}
	if buildPipelineURL("", "trig-123") != "" {
		t.Error("no URL should be built without a project")
	}
	if buildPipelineURL("ethans-services", "") != "" {
		t.Error("no URL should be built without a trigger ID")
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

// TestStatusCardsAreCollapsible: each service renders as a <details> card so it
// can be expanded/collapsed, with the sync badge in the always-visible summary.
func TestStatusCardsAreCollapsible(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:abc1234"},
	}}
	h, _, sess := newTestHandlers(t, k)

	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))
	rec := httptest.NewRecorder()
	h.Status(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `<details class="card card-collapse`) {
		t.Error("service should render as a collapsible <details> card")
	}
	if !strings.Contains(body, "<summary") {
		t.Error("collapsible card should have a summary row")
	}
}

// TestStatusRendersRepoButton: the expanded card links to the GitHub source
// repo, honoring the repo override (foo → foo_repo).
func TestStatusRendersRepoButton(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:abc1234"},
	}}
	h, _, sess := newTestHandlers(t, k)

	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))
	rec := httptest.NewRecorder()
	h.Status(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `href="https://github.com/eswan18/foo_repo"`) {
		t.Error("Repo button should link to the GitHub repo using the override name")
	}
	if !strings.Contains(body, `aria-label="open repo for foo"`) {
		t.Error("Repo button missing")
	}
}

// TestStatusRendersBuildPipelineLink: a service with a known trigger ID gets a
// "build pipeline" link on its card, pointing at that trigger's Cloud Build
// history. The query string is HTML-attribute-escaped (& → &amp;), so the
// assertions check the structurally-stable parts either side of the separator.
func TestStatusRendersBuildPipelineLink(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:abc1234"},
	}}
	h, _, sess := newTestHandlers(t, k)
	h.Cfg.GCPProject = "ethans-services"
	h.TriggerIDs = map[string]string{"foo": "trig-123"}

	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))
	rec := httptest.NewRecorder()
	h.Status(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `aria-label="open build pipeline for foo"`) {
		t.Error("build pipeline link missing from card")
	}
	if !strings.Contains(body, "Builds</a>") {
		t.Error("build pipeline link should render as a labeled \"Builds\" button")
	}
	if !strings.Contains(body, "cloud-build/builds;region=global?project=ethans-services") {
		t.Error("pipeline link should point at the service's Cloud Build history")
	}
	if !strings.Contains(body, "query=trigger_id%3D%22trig-123%22") {
		t.Error("pipeline link should filter history by the trigger ID")
	}
}

// TestStatusOmitsBuildPipelineLinkWhenUnknown: with no trigger ID for the
// service (or Cloud Build disabled), the card renders no pipeline link rather
// than a broken one.
func TestStatusOmitsBuildPipelineLinkWhenUnknown(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:abc1234"},
	}}
	h, _, sess := newTestHandlers(t, k) // no GCPProject, no TriggerIDs

	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))
	rec := httptest.NewRecorder()
	h.Status(rec, req)

	if strings.Contains(rec.Body.String(), "build pipeline") {
		t.Error("no pipeline link should render when the trigger ID is unknown")
	}
}

// TestStatusRendersArgoBadges: argo badges appear only when interesting —
// OutOfSync/Progressing render, Synced+Healthy renders nothing.
func TestStatusRendersArgoBadges(t *testing.T) {
	k := &fakeKube{
		imgs: map[string][]string{
			"foo-staging": {"reg/foo:abc1234"},
			"foo-prod":    {"reg/foo:abc1234"},
		},
		argoApps: map[string]kube.AppStatus{
			"foo-staging": {SyncStatus: "Synced", HealthStatus: "Healthy"},
			"foo-prod":    {SyncStatus: "OutOfSync", HealthStatus: "Progressing"},
		},
	}
	h, _, sess := newTestHandlers(t, k)

	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))
	rec := httptest.NewRecorder()
	h.Status(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "argo: out of sync") {
		t.Error("prod OutOfSync argo badge missing")
	}
	if !strings.Contains(body, "argo: progressing") {
		t.Error("prod Progressing argo badge missing")
	}
	// Exactly one of each badge: the Synced+Healthy staging env renders none.
	if strings.Count(body, "argo: out of sync") != 1 || strings.Count(body, "argo: progressing") != 1 {
		t.Error("Synced+Healthy staging env should render no argo badges")
	}
}

// TestStatusRendersDeployTime: each env line shows how long ago its running
// revision was deployed (from ArgoCD history), with the absolute timestamp in
// a tooltip.
func TestStatusRendersDeployTime(t *testing.T) {
	k := &fakeKube{
		imgs: map[string][]string{
			"foo-staging": {"reg/foo:abc1234"},
			"foo-prod":    {"reg/foo:abc1234"},
		},
		argoApps: map[string]kube.AppStatus{
			"foo-staging": {SyncStatus: "Synced", HealthStatus: "Healthy", DeployedAt: time.Now().Add(-3 * time.Hour)},
			"foo-prod":    {SyncStatus: "Synced", HealthStatus: "Healthy", DeployedAt: time.Now().Add(-2 * 24 * time.Hour)},
		},
	}
	h, _, sess := newTestHandlers(t, k)

	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))
	rec := httptest.NewRecorder()
	h.Status(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "3h ago") {
		t.Error("staging deploy time (3h ago) missing")
	}
	if !strings.Contains(body, "2d ago") {
		t.Error("prod deploy time (2d ago) missing")
	}
	if !strings.Contains(body, `title="deployed `) {
		t.Error("deploy-time tooltip missing")
	}
}

// TestStatusOmitsDeployTimeWhenUnknown: with no deploy time available (e.g.
// the ArgoCD lookup failed or returned no history), the env line renders no
// deploy-time element rather than an empty or misleading one.
func TestStatusOmitsDeployTimeWhenUnknown(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:abc1234"},
	}} // no argoApps => zero DeployedAt
	h, _, sess := newTestHandlers(t, k)

	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))
	rec := httptest.NewRecorder()
	h.Status(rec, req)

	if strings.Contains(rec.Body.String(), `title="deployed `) {
		t.Error("no deploy-time element should render when the deploy time is unknown")
	}
}

// TestStatusSurvivesArgoListFailure: an ArgoCD API failure must not take down
// the status page — health and promote still work, argo badges are omitted.
func TestStatusSurvivesArgoListFailure(t *testing.T) {
	k := &fakeKube{
		imgs: map[string][]string{
			"foo-staging": {"reg/foo:abc1234"},
			"foo-prod":    {"reg/foo:def5678"},
		},
		argoErr: errors.New("argocd api down"),
	}
	h, _, sess := newTestHandlers(t, k)

	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))
	rec := httptest.NewRecorder()
	h.Status(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `action="/services/foo/promote"`) {
		t.Error("promote form should still render when argo list fails")
	}
	if strings.Contains(rec.Body.String(), "argo:") {
		t.Error("no argo badges should render when argo list fails")
	}
}

type fakeBuilds struct {
	builds map[string]gcb.BuildStatus
	err    error
}

func (f *fakeBuilds) LatestBuilds(_ context.Context) (map[string]gcb.BuildStatus, error) {
	return f.builds, f.err
}

// TestStatusRendersBuildBadges: an in-progress build shows a "building"
// badge linking to its log; build status is looked up by repo name (the
// "foo" service maps to repo "foo_repo" via the test config's override).
func TestStatusRendersBuildBadges(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:abc1234"},
	}}
	h, _, sess := newTestHandlers(t, k)
	h.Builds = &fakeBuilds{builds: map[string]gcb.BuildStatus{
		"foo_repo": {Status: "WORKING", SHA: "abc1234", LogURL: "https://console.example/build/1"},
	}}

	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))
	rec := httptest.NewRecorder()
	h.Status(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "building abc1234") {
		t.Error("building badge missing")
	}
	if !strings.Contains(body, `href="https://console.example/build/1"`) {
		t.Error("build log link missing")
	}
}

func TestStatusRendersFailedBuildBadge(t *testing.T) {
	k := &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:abc1234"},
	}}
	h, _, sess := newTestHandlers(t, k)
	h.Builds = &fakeBuilds{builds: map[string]gcb.BuildStatus{
		"foo_repo": {Status: "FAILURE", SHA: "abc1234"},
	}}

	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))
	rec := httptest.NewRecorder()
	h.Status(rec, req)

	if !strings.Contains(rec.Body.String(), "build failed abc1234") {
		t.Error("failed-build badge missing")
	}
}

// TestStatusNoBuildBadgeOnSuccessNilOrError: successful builds, a nil client
// (feature disabled), and an API error all render no badge — and never break
// the page.
func TestStatusNoBuildBadgeOnSuccessNilOrError(t *testing.T) {
	for name, builds := range map[string]gcb.Client{
		"success":  &fakeBuilds{builds: map[string]gcb.BuildStatus{"foo_repo": {Status: "SUCCESS", SHA: "abc1234"}}},
		"disabled": nil,
		"error":    &fakeBuilds{err: errors.New("cloud build api down")},
	} {
		t.Run(name, func(t *testing.T) {
			k := &fakeKube{imgs: map[string][]string{
				"foo-staging": {"reg/foo:abc1234"},
				"foo-prod":    {"reg/foo:abc1234"},
			}}
			h, _, sess := newTestHandlers(t, k)
			h.Builds = builds

			req := httptest.NewRequest("GET", "/", nil)
			req = req.WithContext(auth.WithSessionForTest(req.Context(), sess))
			rec := httptest.NewRecorder()
			h.Status(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("code = %d, want 200", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "building") || strings.Contains(rec.Body.String(), "build failed") {
				t.Error("no build badge should render")
			}
		})
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
	if _, ok := k.patched["foo-prod"]; ok {
		t.Error("should not have patched")
	}
}
