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
	Up(ctx context.Context, branch string, opts preview.UpOptions) error
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

// maxPreviewTTL caps the ttl POST /api/previews will accept. It is a typo
// guard, not a policy limit: it exists so a fat-fingered "8760h" (a year)
// reads as an error instead of quietly creating a preview that outlives
// everyone's memory of it. Nothing enforces a maximum preview lifetime —
// omitting ttl entirely still means "never expires".
const maxPreviewTTL = 30 * 24 * time.Hour

// createPreviewRequest is POST /api/previews's JSON body. TTL is an optional
// Go duration string ("8h", "90m"); absent or empty means the preview never
// expires, which is the default by design (see the plan's constraints).
// AutoUpdate opts the preview into following its branch — absent means false,
// and a re-POST without it turns auto-update back off, exactly as a re-POST
// without a ttl clears the expiry.
type createPreviewRequest struct {
	Branch     string `json:"branch"`
	TTL        string `json:"ttl,omitempty"`
	AutoUpdate bool   `json:"autoUpdate,omitempty"`
}

// CreatePreviewJSON serves POST /api/previews. It validates just enough to
// answer synchronously (branch present, tag derivable, not already busy),
// then kicks off Orchestrator.Up in the background and returns 202
// immediately. Up's own errors land on the preview namespace's
// bifrost/phase / bifrost/error annotations — that's the orchestrator's
// job, not this handler's — so the only thing done here on failure is a
// slog line; the HTTP response has already been sent.
//
// The Busy(tag) pre-check is advisory, not a reservation: it and the
// goroutine's own Orchestrator.acquire() below are non-atomic, so two
// near-simultaneous POSTs for the same branch can both pass the check and
// both get a 202. This is accepted as a bounded, documented limitation
// (adjudicated in task 5 review) rather than fixed with a synchronous
// reservation API: the orchestrator's mutex still prevents duplicate work
// (the loser's Up simply returns preview.ErrBusy, logged and discarded —
// see the goroutine below), and the 202's "creating" claim remains true for
// the tag either way, since the winner is in fact creating it. Callers that
// need ground truth for a specific tag should poll
// GET /api/previews/{tag} rather than trust 202 as a uniqueness guarantee.
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
	// Rejected here, synchronously, rather than clamped or deferred to Up: a
	// caller who mistyped their TTL should learn about it from the 400, not
	// from a preview that vanishes (or doesn't) hours later.
	//
	// Turning the validated duration into the absolute instant Up records is
	// this handler's job too, and deliberately so: Up takes an instant
	// because the auto-update watcher re-runs it and must carry an existing
	// expiry through UNCHANGED (a duration would be recomputed from "now" on
	// every re-run, so a preview that keeps getting commits would keep
	// renewing itself and never expire). Here is where a user-supplied
	// relative ttl enters the system, so here is where it stops being one.
	var ttl time.Duration
	if s := strings.TrimSpace(req.TTL); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "ttl must be a Go duration like 8h or 90m")
			return
		}
		if d <= 0 {
			writeJSONError(w, http.StatusBadRequest, "ttl must be positive")
			return
		}
		if d > maxPreviewTTL {
			writeJSONError(w, http.StatusBadRequest, "ttl must be at most "+maxPreviewTTL.String())
			return
		}
		ttl = d
	}
	if h.Orch.Busy(tag) {
		writeJSONError(w, http.StatusConflict, "preview "+tag+" is busy")
		return
	}
	// Zero (no expiry) unless a ttl was given, since UpOptions.ExpiresAt's
	// zero value is what "never expires" means all the way down.
	opts := preview.UpOptions{AutoUpdate: req.AutoUpdate}
	if ttl > 0 {
		opts.ExpiresAt = time.Now().UTC().Add(ttl)
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), asyncOrchestrationTimeout)
		defer cancel()
		// err may be preview.ErrBusy here if this request lost the race
		// against a concurrent POST for the same tag (see the Busy(tag)
		// pre-check note above) — that's a no-op loser, not a real failure,
		// but it's still logged rather than silently discarded.
		if err := h.Orch.Up(ctx, branch, opts); err != nil {
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
//
// Same advisory-Busy-pre-check caveat as CreatePreviewJSON: this handler's
// Busy(tag) check and the goroutine's Orchestrator.acquire() below are
// non-atomic, so two near-simultaneous DELETEs (or a DELETE racing a POST)
// for the same tag can both 202; the loser's Down returns preview.ErrBusy,
// logged and discarded. Accepted as a bounded limitation (task 5
// adjudication) — poll GET /api/previews/{tag} for ground truth.
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
		// err may be preview.ErrBusy here if this request lost the race
		// against a concurrent POST/DELETE for the same tag (see the
		// Busy(tag) pre-check note above) — a no-op loser, not a real
		// failure, but still logged rather than silently discarded.
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
