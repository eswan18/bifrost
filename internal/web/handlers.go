package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/eswan18/bifrost/internal/auth"
	"github.com/eswan18/bifrost/internal/config"
	"github.com/eswan18/bifrost/internal/kube"
	"github.com/eswan18/bifrost/internal/promote"
)

type Handlers struct {
	Cfg      *config.Config
	Kube     kube.Client
	Renderer *Renderer
}

type statusRow struct {
	Name       string
	State      promote.State
	StagingTag string
	ProdTag    string
	NewProdTag string
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
		"stagingTag": row.StagingTag,
		"prodTag":    row.ProdTag,
		"newProdTag": row.NewProdTag,
	})
}

// statusRowFor reads staging+prod pod images for one service and derives its
// status. Used by both the full status page and the per-service JSON endpoint.
func (h *Handlers) statusRowFor(ctx context.Context, svc string) statusRow {
	row := statusRow{Name: svc, State: promote.Unknown}
	staging, err := h.Kube.ListPodImages(ctx, svc+"-staging")
	if err != nil {
		slog.Warn("list pod images failed", "service", svc, "namespace", svc+"-staging", "error", err)
	}
	prod, err := h.Kube.ListPodImages(ctx, svc+"-prod")
	if err != nil {
		slog.Warn("list pod images failed", "service", svc, "namespace", svc+"-prod", "error", err)
	}
	s := promote.StatusOf(staging, prod)
	row.State = s.State
	row.StagingTag = s.StagingTag
	row.ProdTag = s.ProdTag
	row.NewProdTag = s.NewProdTag
	return row
}

func (h *Handlers) collectStatus(ctx context.Context) []statusRow {
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
	return rows
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
	staging, err := h.Kube.ListPodImages(r.Context(), app+"-staging")
	if err != nil {
		slog.Error("promote: read staging failed", "user", sess.Email, "service", app, "error", err)
		h.respondPromote(w, r, http.StatusBadGateway, false, fmt.Sprintf("read staging: %v", err), "")
		return
	}
	prod, err := h.Kube.ListPodImages(r.Context(), app+"-prod")
	if err != nil {
		slog.Error("promote: read prod failed", "user", sess.Email, "service", app, "error", err)
		h.respondPromote(w, r, http.StatusBadGateway, false, fmt.Sprintf("read prod: %v", err), "")
		return
	}
	s := promote.StatusOf(staging, prod)
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
