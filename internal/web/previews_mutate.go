package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/eswan18/bifrost/internal/auth"
	"github.com/eswan18/bifrost/internal/preview"
)

// orchestration is the subset of *preview.Orchestrator the mutating preview
// endpoints need. It exists so handler tests can supply a trivial fake
// (Up/Down/Busy) rather than standing up a real Orchestrator, which needs
// five real clients (kube, GitHub, Neon, Cloud Build) to even construct.
type orchestration interface {
	Up(ctx context.Context, branch string) error
	Down(ctx context.Context, tag string) error
	Busy(tag string) bool
}

// asyncOrchestrationTimeout bounds the detached goroutine that actually runs
// Up/Down after the handler has already answered 202. It is deliberately
// NOT derived from the request's context — the request returns immediately,
// long before a real build+deploy (or teardown) could finish — so this is a
// fresh, generous budget of its own. The orchestrator's own fail() already
// detaches its compensating annotation writes from whatever context it's
// given (see internal/preview/orchestrator.go), so handing it this
// timeout-bounded context as the run context is safe: a deadline firing
// mid-run still leaves the namespace correctly annotated failed.
const asyncOrchestrationTimeout = 30 * time.Minute

// createPreviewRequest is POST /api/previews's JSON body.
type createPreviewRequest struct {
	Branch string `json:"branch"`
}

// CreatePreviewJSON serves POST /api/previews. It validates just enough to
// answer synchronously (branch present, tag derivable, not already busy),
// then kicks off Orchestrator.Up in the background and returns 202
// immediately. Up's own errors land on the preview namespace's
// bifrost/phase / bifrost/error annotations — that's the orchestrator's
// job, not this handler's — so the only thing done here on failure is a
// slog line; the HTTP response has already been sent.
func (h *Handlers) CreatePreviewJSON(w http.ResponseWriter, r *http.Request) {
	if !h.verifyMutationCSRF(w, r) {
		return
	}
	if h.Orch == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "previews are not configured")
		return
	}

	var req createPreviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		writeJSONError(w, http.StatusBadRequest, "branch is required")
		return
	}
	tag := preview.TagForBranch(branch)
	if tag == "" {
		writeJSONError(w, http.StatusBadRequest, "branch does not produce a usable preview tag")
		return
	}
	if h.Orch.Busy(tag) {
		writeJSONError(w, http.StatusConflict, "preview "+tag+" is busy")
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), asyncOrchestrationTimeout)
		defer cancel()
		if err := h.Orch.Up(ctx, branch); err != nil {
			slog.Error("preview create failed", "tag", tag, "branch", branch, "err", err)
			return
		}
		slog.Info("preview create completed", "tag", tag, "branch", branch)
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"tag": tag, "phase": "creating"})
}

// DeletePreviewJSON serves DELETE /api/previews/{tag}. Orchestrator.Down
// can't itself distinguish "this tag never existed" from "delete succeeded"
// (its DeleteNamespace treats NotFound as success — see the 3c orchestrator
// report), so this handler does its own existence check via the same
// previewByTag/GetNamespace path the read endpoints use before ever calling
// Down, to give a real 404. Like Create, teardown continues in the
// background; failures are logged, never surfaced to the (already-answered)
// HTTP caller.
func (h *Handlers) DeletePreviewJSON(w http.ResponseWriter, r *http.Request) {
	if !h.verifyMutationCSRF(w, r) {
		return
	}
	if h.Orch == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "previews are not configured")
		return
	}

	tag := r.PathValue("tag")
	_, found, err := h.previewByTag(r.Context(), tag)
	if err != nil {
		slog.Error("delete preview: lookup failed", "tag", tag, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "delete preview failed")
		return
	}
	if !found {
		writeJSONError(w, http.StatusNotFound, "unknown preview")
		return
	}
	if h.Orch.Busy(tag) {
		writeJSONError(w, http.StatusConflict, "preview "+tag+" is busy")
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), asyncOrchestrationTimeout)
		defer cancel()
		if err := h.Orch.Down(ctx, tag); err != nil {
			slog.Error("preview delete failed", "tag", tag, "err", err)
			return
		}
		slog.Info("preview delete completed", "tag", tag)
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"tag": tag, "deleting": true})
}

// verifyMutationCSRF enforces the mutation guard shared by both preview
// endpoints, on top of requirePreviewAuth: a session-authed request
// (browser) must carry a valid X-CSRF-Token header; a bearer-authed request
// (nil session — the CLI) is never asked for one. The nil check runs before
// anything else touches the session so a bearer request never exercises
// session/CSRF code at all. On failure it writes the 403 response itself
// and returns false.
func (h *Handlers) verifyMutationCSRF(w http.ResponseWriter, r *http.Request) bool {
	sess := auth.SessionFromContext(r.Context())
	if sess == nil {
		return true
	}
	if !auth.VerifyCSRF(h.Cfg.SessionSecret, sess.ID, r.Header.Get("X-CSRF-Token")) {
		writeJSONError(w, http.StatusForbidden, "bad csrf")
		return false
	}
	return true
}

// writeJSONError writes a JSON {"error": msg} body with status, matching the
// read endpoints' error shape (PreviewsListJSON/PreviewJSON in previews.go).
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg})
}
