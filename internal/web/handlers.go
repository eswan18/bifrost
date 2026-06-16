package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/eswan18/bifrost/internal/auth"
	"github.com/eswan18/bifrost/internal/config"
	"github.com/eswan18/bifrost/internal/gcb"
	"github.com/eswan18/bifrost/internal/kube"
	"github.com/eswan18/bifrost/internal/promote"
)

type Handlers struct {
	Cfg      *config.Config
	Kube     kube.Client
	Builds   gcb.Client // nil → build badges disabled
	Renderer *Renderer
}

type envStatus struct {
	Tag        string
	CommitURL  string // "" → render the tag without a link
	URL        string // public app URL; "" → no "open" link
	Health     kube.HealthSummary
	ArgoSync   string // "" → unknown; badge omitted
	ArgoHealth string
	DeployedAt time.Time // when the running revision went live; zero → omit
}

type buildInfo struct {
	State  string // "building" | "failed"
	SHA    string
	LogURL string
}

type statusRow struct {
	Name       string
	State      promote.State
	Staging    envStatus
	Prod       envStatus
	NewProdTag string
	Build      *buildInfo // nil → no badge (no recent build, or it succeeded)
}

// Active reports whether the service is in flight — a deploy rolling out or a
// build running — so the client should poll it on a fast cadence until it
// settles. Settled states (in sync, or out of sync awaiting a manual promote)
// are not active.
func (r statusRow) Active() bool {
	return r.State == promote.MidDeploy || (r.Build != nil && r.Build.State == "building")
}

func (h *Handlers) Status(w http.ResponseWriter, r *http.Request) {
	// "GET /" is a catch-all pattern: without this, every unmatched path
	// (favicon.ico, scanners) would trigger a full status collection.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	rows := h.collectStatus(r.Context())
	sess := auth.SessionFromContext(r.Context())
	data := map[string]any{
		"Rows":    rows,
		"Session": sess,
		"Flash":   TakeFlash(w, r),
		"CSRF":    auth.CSRFToken(h.Cfg.SessionSecret, sess.ID),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.Renderer.Render(w, "status", data); err != nil {
		slog.Error("render failed", "template", "status", "error", err)
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// StatusJSON returns the status of a single service as JSON. The browser polls
// this after a promote to show a spinner until prod has actually rolled out.
func (h *Handlers) StatusJSON(w http.ResponseWriter, r *http.Request) {
	svc := r.PathValue("name")
	if !h.knownService(svc) {
		http.Error(w, "unknown service", http.StatusNotFound)
		return
	}
	row := h.statusRowFor(r.Context(), svc)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"state":      string(row.State),
		"stagingTag": row.Staging.Tag,
		"prodTag":    row.Prod.Tag,
		"newProdTag": row.NewProdTag,
	})
}

// StatusFragment renders just the service rows (no page chrome) so the
// browser can poll it and swap the list in place without a full reload.
func (h *Handlers) StatusFragment(w http.ResponseWriter, r *http.Request) {
	rows := h.collectStatus(r.Context())
	sess := auth.SessionFromContext(r.Context())
	data := map[string]any{
		"Rows": rows,
		"CSRF": auth.CSRFToken(h.Cfg.SessionSecret, sess.ID),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.Renderer.RenderNamed(w, "status", "rows", data); err != nil {
		slog.Error("render failed", "template", "rows", "error", err)
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// statusRowFor reads staging+prod pods for one service and derives its
// status. Used by both the full status page and the per-service JSON
// endpoint. ArgoCD fields are stamped separately by collectStatus.
func (h *Handlers) statusRowFor(ctx context.Context, svc string) statusRow {
	row := statusRow{Name: svc, State: promote.Unknown}
	staging, err := h.Kube.ListPods(ctx, svc+"-staging")
	if err != nil {
		slog.Warn("list pods failed", "service", svc, "namespace", svc+"-staging", "error", err)
	}
	prod, err := h.Kube.ListPods(ctx, svc+"-prod")
	if err != nil {
		slog.Warn("list pods failed", "service", svc, "namespace", svc+"-prod", "error", err)
	}
	s := promote.StatusOf(kube.Images(staging), kube.Images(prod))
	row.State = s.State
	row.NewProdTag = s.NewProdTag
	repo := h.Cfg.RepoFor(svc)
	row.Staging = envStatus{
		Tag:       s.StagingTag,
		CommitURL: commitURL(h.Cfg.GitHubOrg, repo, s.StagingTag),
		URL:       h.Cfg.StagingURLs[svc],
		Health:    kube.SummarizeHealth(staging),
	}
	row.Prod = envStatus{
		Tag:       s.ProdTag,
		CommitURL: commitURL(h.Cfg.GitHubOrg, repo, s.ProdTag),
		URL:       h.Cfg.ProdURLs[svc],
		Health:    kube.SummarizeHealth(prod),
	}
	return row
}

func (h *Handlers) collectStatus(ctx context.Context) []statusRow {
	// ArgoCD state and build history are each one bulk call shared by every
	// row; fetch both concurrently with the per-service pod queries.
	argoCh := make(chan map[string]kube.AppStatus, 1)
	go func() {
		apps, err := h.Kube.ListArgoApps(ctx)
		if err != nil {
			slog.Warn("list argocd applications failed", "error", err)
		}
		argoCh <- apps // nil on error → argo badges render as unknown
	}()
	buildsCh := make(chan map[string]gcb.BuildStatus, 1)
	go func() {
		if h.Builds == nil {
			buildsCh <- nil
			return
		}
		builds, err := h.Builds.LatestBuilds(ctx)
		if err != nil {
			slog.Warn("list cloud builds failed", "error", err)
		}
		buildsCh <- builds // nil on error → no build badges
	}()

	type result struct {
		idx int
		row statusRow
	}
	results := make(chan result, len(h.Cfg.Services))
	var wg sync.WaitGroup
	for i, svc := range h.Cfg.Services {
		wg.Add(1)
		go func(i int, svc string) {
			defer wg.Done()
			results <- result{i, h.statusRowFor(ctx, svc)}
		}(i, svc)
	}
	go func() { wg.Wait(); close(results) }()

	rows := make([]statusRow, len(h.Cfg.Services))
	for r := range results {
		rows[r.idx] = r.row
	}

	apps := <-argoCh
	builds := <-buildsCh
	for i := range rows {
		if app, ok := apps[rows[i].Name+"-staging"]; ok {
			rows[i].Staging.ArgoSync = app.SyncStatus
			rows[i].Staging.ArgoHealth = app.HealthStatus
			rows[i].Staging.DeployedAt = app.DeployedAt
		}
		if app, ok := apps[rows[i].Name+"-prod"]; ok {
			rows[i].Prod.ArgoSync = app.SyncStatus
			rows[i].Prod.ArgoHealth = app.HealthStatus
			rows[i].Prod.DeployedAt = app.DeployedAt
		}
		rows[i].Build = buildBadge(builds[h.Cfg.RepoFor(rows[i].Name)])
	}
	return rows
}

// buildBadge maps a build to its badge, or nil when no badge should show
// (no recent build, success, or cancelled).
func buildBadge(b gcb.BuildStatus) *buildInfo {
	switch {
	case b.InProgress():
		return &buildInfo{State: "building", SHA: b.SHA, LogURL: b.LogURL}
	case b.Failed():
		return &buildInfo{State: "failed", SHA: b.SHA, LogURL: b.LogURL}
	}
	return nil
}

// commitURL links an image tag to the commit it was built from. Returns ""
// when the tag carries no recognizable SHA (GitHub resolves short SHAs).
func commitURL(org, repo, tag string) string {
	sha := promote.ExtractSHA(tag)
	if sha == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/%s/commit/%s", org, repo, sha)
}

func (h *Handlers) Promote(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("name")
	if !h.knownService(app) {
		http.Error(w, "unknown service", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	sess := auth.SessionFromContext(r.Context())
	if !auth.VerifyCSRF(h.Cfg.SessionSecret, sess.ID, r.FormValue("csrf")) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return
	}

	// Re-derive the current state to make sure we promote what the user saw.
	stagingPods, err := h.Kube.ListPods(r.Context(), app+"-staging")
	if err != nil {
		slog.Error("promote: read staging failed", "user", sess.Email, "service", app, "error", err)
		h.respondPromote(w, r, http.StatusBadGateway, false, fmt.Sprintf("read staging: %v", err), "")
		return
	}
	prodPods, err := h.Kube.ListPods(r.Context(), app+"-prod")
	if err != nil {
		slog.Error("promote: read prod failed", "user", sess.Email, "service", app, "error", err)
		h.respondPromote(w, r, http.StatusBadGateway, false, fmt.Sprintf("read prod: %v", err), "")
		return
	}
	staging := kube.Images(stagingPods)
	s := promote.StatusOf(staging, kube.Images(prodPods))
	if s.State != promote.OutOfSync {
		slog.Warn("promote refused: nothing to promote", "user", sess.Email, "service", app, "state", string(s.State))
		h.respondPromote(w, r, http.StatusConflict, false, fmt.Sprintf("%s: nothing to promote (state=%s)", app, s.State), "")
		return
	}
	if r.FormValue("expected_sha") != "" && r.FormValue("expected_sha") != s.NewProdTag {
		slog.Warn("promote refused: staging changed since page load",
			"user", sess.Email, "service", app,
			"expected", r.FormValue("expected_sha"), "current", s.NewProdTag)
		h.respondPromote(w, r, http.StatusConflict, false, fmt.Sprintf("%s: staging changed since page load — refresh and retry", app), "")
		return
	}
	slog.Info("promote attempt", "user", sess.Email, "service", app,
		"from", s.ProdTag, "to", s.NewProdTag)

	// Image base = same registry path as the current staging image.
	stagingImage := staging[0]
	imageBase := stagingImage
	for i := len(stagingImage) - 1; i >= 0; i-- {
		if stagingImage[i] == ':' {
			imageBase = stagingImage[:i]
			break
		}
	}
	newImage := imageBase + ":" + s.NewProdTag

	if err := h.Kube.PatchProdImage(r.Context(), app, newImage); err != nil {
		slog.Error("promote failed", "user", sess.Email, "service", app,
			"from", s.ProdTag, "to", s.NewProdTag, "error", err)
		h.respondPromote(w, r, http.StatusBadGateway, false, fmt.Sprintf("patch failed: %v", err), "")
		return
	}
	slog.Info("promote succeeded", "user", sess.Email, "service", app,
		"from", s.ProdTag, "to", s.NewProdTag)
	h.respondPromote(w, r, http.StatusOK, true,
		fmt.Sprintf("Promoted %s prod → %s", app, s.NewProdTag), s.NewProdTag)
}

// respondPromote sets the flash (shown after the page reloads) and replies in
// the caller's preferred format: JSON for the fetch()-driven UI, or a 303
// redirect to "/" for the plain-form no-JS fallback.
func (h *Handlers) respondPromote(w http.ResponseWriter, r *http.Request, status int, ok bool, msg, newTag string) {
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
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": ok, "error": errMsg, "newTag": newTag})
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

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
