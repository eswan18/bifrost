package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/eswan18/bifrost/internal/auth"
	"github.com/eswan18/bifrost/internal/config"
	"github.com/eswan18/bifrost/internal/gcb"
	"github.com/eswan18/bifrost/internal/kube"
	"github.com/eswan18/bifrost/internal/promote"
)

type Handlers struct {
	Cfg        *config.Config
	Kube       kube.Client
	Builds     gcb.Client        // nil → build badges disabled
	TriggerIDs map[string]string // service → Cloud Build trigger ID, for pipeline links; nil → links omitted
	Renderer   *Renderer
}

// pageVM is the view model handed to every rendered template. The dashboard
// pages share the header/tab chrome (hence the tab counts and theme on the
// same struct); login/error set Dashboard=false and use only Theme/Message.
type pageVM struct {
	Title      string
	Theme      string // "light" | "dark"
	ThemeLabel string
	Session    *auth.Session
	Flash      *Flash
	CSRF       string
	Dashboard  bool
	Tab        string // "overview" | "apps" | "jobs"
	RefreshURL string // polling fragment endpoint for this page
	AnyActive  bool   // something in flight → fast poll cadence

	AppCount  int
	AppIssues int
	JobCount  int
	JobIssues int

	Apps     []appView
	Overview *overviewData
	Jobs     *jobsPage

	Message string // error page
}

type jobsPage struct {
	Jobs         []jobView
	Filter       string
	SummaryLabel string
	Options      []jobOption
	TotalApps    int
}

type jobOption struct {
	Name     string
	Label    string
	Selected bool
}

// --- page handlers -----------------------------------------------------------

func (h *Handlers) Overview(w http.ResponseWriter, r *http.Request) {
	// "GET /" is a catch-all pattern: 404 anything that isn't the root so
	// favicon/scanner hits don't trigger a full fleet collection.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	f := h.assembleFleet(r.Context())
	vm := h.dashboardVM(r, "overview", "/partial/overview", f)
	vm.Flash = TakeFlash(w, r)
	h.render(w, "overview", vm)
}

func (h *Handlers) Apps(w http.ResponseWriter, r *http.Request) {
	f := h.assembleFleet(r.Context())
	vm := h.dashboardVM(r, "apps", "/partial/apps", f)
	vm.Flash = TakeFlash(w, r)
	h.render(w, "apps", vm)
}

func (h *Handlers) Jobs(w http.ResponseWriter, r *http.Request) {
	f := h.assembleFleet(r.Context())
	filter := h.jobFilter(r)
	vm := h.dashboardVM(r, "jobs", h.jobsRefreshURL(filter), f)
	vm.Jobs = buildJobsPage(f, filter)
	vm.Flash = TakeFlash(w, r)
	h.render(w, "jobs", vm)
}

// --- polling fragments -------------------------------------------------------

func (h *Handlers) OverviewFragment(w http.ResponseWriter, r *http.Request) {
	f := h.assembleFleet(r.Context())
	vm := h.dashboardVM(r, "overview", "/partial/overview", f)
	h.renderNamed(w, "overview", "tab-body", vm)
}

func (h *Handlers) AppsFragment(w http.ResponseWriter, r *http.Request) {
	f := h.assembleFleet(r.Context())
	vm := h.dashboardVM(r, "apps", "/partial/apps", f)
	h.renderNamed(w, "apps", "tab-body", vm)
}

func (h *Handlers) JobsFragment(w http.ResponseWriter, r *http.Request) {
	f := h.assembleFleet(r.Context())
	filter := h.jobFilter(r)
	vm := h.dashboardVM(r, "jobs", h.jobsRefreshURL(filter), f)
	vm.Jobs = buildJobsPage(f, filter)
	h.renderNamed(w, "jobs", "tab-body", vm)
}

// jobFilter returns the ?app= filter, or "" when absent or not a known service
// (an app that no longer exists silently reverts to "all apps").
func (h *Handlers) jobFilter(r *http.Request) string {
	app := r.URL.Query().Get("app")
	if app != "" && h.knownService(app) {
		return app
	}
	return ""
}

func (h *Handlers) jobsRefreshURL(filter string) string {
	if filter == "" {
		return "/partial/jobs"
	}
	return "/partial/jobs?app=" + url.QueryEscape(filter)
}

func buildJobsPage(f *fleet, filter string) *jobsPage {
	jp := &jobsPage{Filter: filter}
	for _, a := range f.Apps {
		if !a.HasJobs {
			continue
		}
		jp.TotalApps++
		jp.Options = append(jp.Options, jobOption{
			Name:     a.Name,
			Label:    fmt.Sprintf("%s (%d)", a.Name, len(a.Jobs)),
			Selected: a.Name == filter,
		})
	}
	var jobs []jobView
	for _, j := range f.Jobs {
		if filter == "" || j.App == filter {
			jobs = append(jobs, j)
		}
	}
	sortJobs(jobs)
	jp.Jobs = jobs
	if filter != "" {
		noun := "jobs"
		if len(jobs) == 1 {
			noun = "job"
		}
		jp.SummaryLabel = fmt.Sprintf("%d %s · %s", len(jobs), noun, filter)
	} else {
		jp.SummaryLabel = fmt.Sprintf("%d jobs across %d apps", f.JobCount, jp.TotalApps)
	}
	return jp
}

// dashboardVM builds the shared page model. The flash is taken separately by
// the full-page handlers — a fragment poll must never consume it, or the user
// would never see the promote/rollback result.
func (h *Handlers) dashboardVM(r *http.Request, tab, refresh string, f *fleet) pageVM {
	sess := auth.SessionFromContext(r.Context())
	theme := themeFrom(r)
	csrf := ""
	if sess != nil {
		csrf = auth.CSRFToken(h.Cfg.SessionSecret, sess.ID)
	}
	return pageVM{
		Title:      "Bifrost",
		Theme:      theme,
		ThemeLabel: themeLabel(theme),
		Session:    sess,
		CSRF:       csrf,
		Dashboard:  true,
		Tab:        tab,
		RefreshURL: refresh,
		AnyActive:  f.anyActive(),
		AppCount:   f.AppCount,
		AppIssues:  f.AppIssues,
		JobCount:   f.JobCount,
		JobIssues:  f.JobIssues,
		Apps:       f.Apps,
		Overview:   &f.Overview,
	}
}

func (h *Handlers) render(w http.ResponseWriter, name string, vm pageVM) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.Renderer.Render(w, name, vm); err != nil {
		slog.Error("render failed", "template", name, "error", err)
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

func (h *Handlers) renderNamed(w http.ResponseWriter, page, block string, vm pageVM) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.Renderer.RenderNamed(w, page, block, vm); err != nil {
		slog.Error("render failed", "template", block, "error", err)
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// --- per-service status JSON -------------------------------------------------

// StatusJSON returns one service's status as JSON. The browser polls this
// after a promote or rollback to detect when the target environment has
// actually rolled out. It keeps the flat fields promote polling has always
// used and adds per-env objects so rollback polling can watch either env.
func (h *Handlers) StatusJSON(w http.ResponseWriter, r *http.Request) {
	svc := r.PathValue("name")
	if !h.knownService(svc) {
		http.Error(w, "unknown service", http.StatusNotFound)
		return
	}
	org, repo := h.Cfg.GitHubOrg, h.Cfg.RepoFor(svc)
	loc := h.Cfg.DisplayLocation

	var sRaw, pRaw envRaw
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); sRaw.pods, sRaw.rsets = h.readPodsRS(r.Context(), svc+"-staging") }()
	go func() { defer wg.Done(); pRaw.pods, pRaw.rsets = h.readPodsRS(r.Context(), svc+"-prod") }()
	wg.Wait()

	staging := deriveEnv("staging", sRaw, kube.AppStatus{}, org, repo, "", loc)
	prod := deriveEnv("prod", pRaw, kube.AppStatus{}, org, repo, "", loc)
	s := promote.StatusOf(kube.Images(sRaw.pods), kube.Images(pRaw.pods))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"state":      string(s.State),
		"stagingTag": s.StagingTag,
		"prodTag":    s.ProdTag,
		"newProdTag": s.NewProdTag,
		"staging":    envJSON(staging),
		"prod":       envJSON(prod),
	})
}

func envJSON(e envView) map[string]any {
	return map[string]any{"tag": e.Tag, "sha": e.SHA, "status": e.Status}
}

func (h *Handlers) readPodsRS(ctx context.Context, ns string) ([]kube.PodInfo, []kube.ReplicaSetInfo) {
	var pods []kube.PodInfo
	var rsets []kube.ReplicaSetInfo
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		p, err := h.Kube.ListPods(ctx, ns)
		if err != nil {
			slog.Warn("list pods failed", "namespace", ns, "error", err)
		}
		pods = p
	}()
	go func() {
		defer wg.Done()
		rs, err := h.Kube.ListReplicaSets(ctx, ns)
		if err != nil {
			slog.Warn("list replicasets failed", "namespace", ns, "error", err)
		}
		rsets = rs
	}()
	wg.Wait()
	return pods, rsets
}

// --- promote -----------------------------------------------------------------

// authorizeMutation runs the guards every mutating endpoint shares: the path
// must name a known service, the form must parse, and the request must carry a
// valid CSRF token for the session. It writes the matching error response and
// returns ok=false when any guard fails.
func (h *Handlers) authorizeMutation(w http.ResponseWriter, r *http.Request) (*auth.Session, bool) {
	if !h.knownService(r.PathValue("name")) {
		http.Error(w, "unknown service", http.StatusNotFound)
		return nil, false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return nil, false
	}
	sess := auth.SessionFromContext(r.Context())
	if !auth.VerifyCSRF(h.Cfg.SessionSecret, sess.ID, r.FormValue("csrf")) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return nil, false
	}
	return sess, true
}

func (h *Handlers) Promote(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("name")
	sess, ok := h.authorizeMutation(w, r)
	if !ok {
		return
	}

	// Re-derive the current state to make sure we promote what the user saw.
	stagingPods, err := h.Kube.ListPods(r.Context(), app+"-staging")
	if err != nil {
		slog.Error("promote: read staging failed", "user", sess.Email, "service", app, "error", err)
		h.respondMutation(w, r, http.StatusBadGateway, false, fmt.Sprintf("read staging: %v", err), "", "prod")
		return
	}
	prodPods, err := h.Kube.ListPods(r.Context(), app+"-prod")
	if err != nil {
		slog.Error("promote: read prod failed", "user", sess.Email, "service", app, "error", err)
		h.respondMutation(w, r, http.StatusBadGateway, false, fmt.Sprintf("read prod: %v", err), "", "prod")
		return
	}
	staging := kube.Images(stagingPods)
	s := promote.StatusOf(staging, kube.Images(prodPods))
	if s.State != promote.OutOfSync {
		slog.Warn("promote refused: nothing to promote", "user", sess.Email, "service", app, "state", string(s.State))
		h.respondMutation(w, r, http.StatusConflict, false, fmt.Sprintf("%s: nothing to promote (state=%s)", app, s.State), "", "prod")
		return
	}
	if r.FormValue("expected_sha") != "" && r.FormValue("expected_sha") != s.NewProdTag {
		slog.Warn("promote refused: staging changed since page load",
			"user", sess.Email, "service", app,
			"expected", r.FormValue("expected_sha"), "current", s.NewProdTag)
		h.respondMutation(w, r, http.StatusConflict, false, fmt.Sprintf("%s: staging changed since page load — refresh and retry", app), "", "prod")
		return
	}
	slog.Info("promote attempt", "user", sess.Email, "service", app,
		"env", "prod", "from", s.ProdTag, "to", s.NewProdTag)

	newImage := replaceTag(staging[0], s.NewProdTag)
	if err := h.Kube.PatchAppImage(r.Context(), app, "prod", newImage); err != nil {
		slog.Error("promote failed", "user", sess.Email, "service", app,
			"from", s.ProdTag, "to", s.NewProdTag, "error", err)
		h.respondMutation(w, r, http.StatusBadGateway, false, fmt.Sprintf("patch failed: %v", err), "", "prod")
		return
	}
	slog.Info("promote succeeded", "user", sess.Email, "service", app,
		"env", "prod", "from", s.ProdTag, "to", s.NewProdTag)
	h.respondMutation(w, r, http.StatusOK, true,
		fmt.Sprintf("Promoted %s prod → %s", app, s.NewProdTag), s.NewProdTag, "prod")
}

// --- rollback ----------------------------------------------------------------

// Rollback reverts one environment to its previous image. It re-validates
// against live cluster state before patching: the environment must be settled
// on exactly the image the user saw, and its ReplicaSet history must still
// offer the same previous image the user was shown, or the request is refused
// (mirroring promote's staleness guard).
func (h *Handlers) Rollback(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("name")
	sess, ok := h.authorizeMutation(w, r)
	if !ok {
		return
	}

	env := r.FormValue("env")
	if env != "staging" && env != "prod" {
		slog.Warn("rollback refused: bad env", "user", sess.Email, "service", app, "env", env)
		h.respondMutation(w, r, http.StatusBadRequest, false, fmt.Sprintf("%s: invalid environment %q", app, env), "", env)
		return
	}
	toSHA := r.FormValue("to_sha")
	expectedCurrent := r.FormValue("expected_current_sha")
	ns := app + "-" + env

	pods, err := h.Kube.ListPods(r.Context(), ns)
	if err != nil {
		slog.Error("rollback: read pods failed", "user", sess.Email, "service", app, "env", env, "error", err)
		h.respondMutation(w, r, http.StatusBadGateway, false, fmt.Sprintf("read %s: %v", env, err), "", env)
		return
	}
	images := kube.Images(pods)
	if len(images) != 1 {
		slog.Warn("rollback refused: env not settled", "user", sess.Email, "service", app, "env", env, "distinctImages", len(images))
		h.respondMutation(w, r, http.StatusConflict, false, fmt.Sprintf("%s %s: mid-deploy or unknown — refresh and retry", app, env), "", env)
		return
	}
	currentImage := images[0]
	currentSHA := promote.ExtractSHA(promote.ExtractTag(currentImage))
	if currentSHA == "" || (expectedCurrent != "" && expectedCurrent != currentSHA) {
		slog.Warn("rollback refused: env changed since page load",
			"user", sess.Email, "service", app, "env", env,
			"expected", expectedCurrent, "current", currentSHA)
		h.respondMutation(w, r, http.StatusConflict, false, fmt.Sprintf("%s %s: changed since page load — refresh and retry", app, env), "", env)
		return
	}

	sets, err := h.Kube.ListReplicaSets(r.Context(), ns)
	if err != nil {
		slog.Error("rollback: read replicasets failed", "user", sess.Email, "service", app, "env", env, "error", err)
		h.respondMutation(w, r, http.StatusBadGateway, false, fmt.Sprintf("read %s history: %v", env, err), "", env)
		return
	}
	prevImage := kube.PreviousImage(sets, currentImage)
	if prevImage == "" {
		slog.Warn("rollback refused: no previous image", "user", sess.Email, "service", app, "env", env)
		h.respondMutation(w, r, http.StatusConflict, false, fmt.Sprintf("%s %s: no previous image known", app, env), "", env)
		return
	}
	prevTag := promote.ExtractTag(prevImage)
	prevSHA := promote.ExtractSHA(prevTag)
	if toSHA != "" && prevSHA != toSHA {
		slog.Warn("rollback refused: target changed since page load",
			"user", sess.Email, "service", app, "env", env,
			"expected", toSHA, "current", prevSHA)
		h.respondMutation(w, r, http.StatusConflict, false, fmt.Sprintf("%s %s: rollback target changed — refresh and retry", app, env), "", env)
		return
	}

	slog.Info("rollback attempt", "user", sess.Email, "service", app,
		"env", env, "from", currentSHA, "to", prevSHA)
	if err := h.Kube.PatchAppImage(r.Context(), app, env, prevImage); err != nil {
		slog.Error("rollback failed", "user", sess.Email, "service", app,
			"env", env, "from", currentSHA, "to", prevSHA, "error", err)
		h.respondMutation(w, r, http.StatusBadGateway, false, fmt.Sprintf("patch failed: %v", err), "", env)
		return
	}
	slog.Info("rollback succeeded", "user", sess.Email, "service", app,
		"env", env, "from", currentSHA, "to", prevSHA)
	// newTag is the FULL previous tag (e.g. "def5678-staging"), not the bare
	// SHA: the browser poll compares it against the env's live tag, which carries
	// the {sha}-{env} suffix for suffix-tagged services. Passing the bare SHA
	// would never match and the poll would spin until timeout.
	h.respondMutation(w, r, http.StatusOK, true,
		fmt.Sprintf("Rolled back %s %s → %s", app, env, prevSHA), prevTag, env)
}

// respondMutation sets the flash (shown after the page reloads) and replies in
// the caller's preferred format: JSON for the fetch()-driven UI, or a 303
// redirect back to the originating page for the plain-form no-JS fallback.
func (h *Handlers) respondMutation(w http.ResponseWriter, r *http.Request, status int, ok bool, msg, newTag, env string) {
	if ok {
		SetFlash(w, FlashSuccess, msg)
	} else {
		SetFlash(w, FlashError, msg)
	}
	if wantsJSON(r) {
		errMsg := ""
		if !ok {
			errMsg = msg
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": ok, "error": errMsg, "newTag": newTag, "env": env})
		return
	}
	http.Redirect(w, r, backTo(r), http.StatusSeeOther)
}

// --- small helpers -----------------------------------------------------------

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func (h *Handlers) knownService(name string) bool {
	for _, s := range h.Cfg.Services {
		if s == name {
			return true
		}
	}
	return false
}

func themeFrom(r *http.Request) string {
	if c, err := r.Cookie("bifrost_theme"); err == nil && c.Value == "dark" {
		return "dark"
	}
	return "light"
}

func themeLabel(theme string) string {
	if theme == "dark" {
		return "◐ dark"
	}
	return "☀ light"
}

// backTo returns the dashboard page a no-JS mutation should redirect to,
// preserving the Jobs filter. It only trusts known dashboard paths so the
// Referer can't bounce the user off-site.
func backTo(r *http.Request) string {
	ref := r.Referer()
	if ref == "" {
		return "/"
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "/"
	}
	switch u.Path {
	case "/", "/apps", "/jobs":
		if u.RawQuery != "" {
			return u.Path + "?" + u.RawQuery
		}
		return u.Path
	default:
		return "/"
	}
}

// replaceTag swaps the tag on a full image ref, keeping the registry path.
func replaceTag(image, tag string) string {
	return promote.ImageBase(image) + ":" + tag
}

// buildPipelineURL links a service to its Cloud Build trigger's build history
// in the GCP console. Returns "" when the project or trigger ID is unknown so
// the template omits the link.
func buildPipelineURL(project, triggerID string) string {
	if project == "" || triggerID == "" {
		return ""
	}
	return "https://console.cloud.google.com/cloud-build/builds;region=global?project=" +
		url.QueryEscape(project) + "&query=" + url.QueryEscape(`trigger_id="`+triggerID+`"`)
}

// repoURL links to a service's GitHub source repo.
func repoURL(org, repo string) string {
	if org == "" || repo == "" {
		return ""
	}
	return "https://github.com/" + org + "/" + repo
}

// commitURL links an image tag to the commit it was built from. Returns ""
// when the tag carries no recognizable SHA.
func commitURL(org, repo, tag string) string {
	sha := promote.ExtractSHA(tag)
	if sha == "" || org == "" || repo == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/%s/commit/%s", org, repo, sha)
}
