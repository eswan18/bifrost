package web

import (
	"context"
	"fmt"
	"net/http"
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
		http.Error(w, "render error", http.StatusInternalServerError)
	}
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
			row := statusRow{Name: svc, State: promote.Unknown}
			staging, _ := h.Kube.ListPodImages(ctx, svc+"-staging")
			prod, _ := h.Kube.ListPodImages(ctx, svc+"-prod")
			s := promote.StatusOf(staging, prod)
			row.State = s.State
			row.StagingTag = s.StagingTag
			row.ProdTag = s.ProdTag
			row.NewProdTag = s.NewProdTag
			results <- result{i, row}
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
		SetFlash(w, FlashError, fmt.Sprintf("read staging: %v", err))
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	prod, err := h.Kube.ListPodImages(r.Context(), app+"-prod")
	if err != nil {
		SetFlash(w, FlashError, fmt.Sprintf("read prod: %v", err))
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s := promote.StatusOf(staging, prod)
	if s.State != promote.OutOfSync {
		SetFlash(w, FlashError, fmt.Sprintf("%s: nothing to promote (state=%s)", app, s.State))
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if r.FormValue("expected_sha") != "" && r.FormValue("expected_sha") != s.NewProdTag {
		SetFlash(w, FlashError, fmt.Sprintf("%s: staging changed since page load — refresh and retry", app))
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

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
		SetFlash(w, FlashError, fmt.Sprintf("patch failed: %v", err))
	} else {
		SetFlash(w, FlashSuccess, fmt.Sprintf("Promoted %s prod → %s", app, s.NewProdTag))
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handlers) knownService(name string) bool {
	for _, s := range h.Cfg.Services {
		if s == name {
			return true
		}
	}
	return false
}
