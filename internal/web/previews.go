package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/eswan18/bifrost/internal/kube"
)

// previewLabelSelector selects preview namespaces — the namespace itself is
// the preview's record (labels/annotations written by the 3c orchestrator).
const previewLabelSelector = "bifrost/preview=true"

// previewNSPrefix is prepended to a preview's tag to form its namespace name.
const previewNSPrefix = "preview-"

type previewRecord struct {
	Tag       string            `json:"tag"`
	Branch    string            `json:"branch"`
	Apps      []string          `json:"apps"`
	Phase     string            `json:"phase"`
	Health    string            `json:"health"`
	CreatedAt time.Time         `json:"createdAt"`
	URLs      map[string]string `json:"urls"`
	// Step and StepSince narrate what the orchestrator's Up is doing right
	// now (bifrost/step, bifrost/step-since — see internal/preview's
	// Orchestrator.step). Step is "" once a preview reaches ready (cleared
	// on success) but is deliberately left in place on a failed preview, so
	// it reads as "failed while building footstrike-api" rather than going
	// silent. StepSince is a timestamp, not a duration, so elapsed time is
	// always computed fresh by whoever renders it (the CLI, the UI) instead
	// of going stale between polls.
	Step      string    `json:"step,omitempty"`
	StepSince time.Time `json:"stepSince,omitzero"`
	// Error surfaces bifrost/error (already written by Orchestrator.fail)
	// so a failed preview's cause is visible to API consumers instead of
	// only the cluster-side annotation.
	Error string `json:"error,omitempty"`
	// ExpiresAt is the optional reclaim time recorded at creation
	// (bifrost/expires-at — see internal/preview's expiresAtAnnotation).
	// Zero, and so omitted from the JSON, means the preview never expires:
	// most previews carry no TTL, and that is the default by design.
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
	// AutoUpdate reports whether the preview follows its branch
	// (bifrost/auto-update = "true" — see internal/preview's
	// autoUpdateAnnotation and PollAutoUpdates). Opt-in and false by
	// default, so it is omitted from the JSON for almost every preview.
	AutoUpdate bool `json:"autoUpdate,omitempty"`
}

// recordFromNamespace derives everything derivable without extra cluster
// calls; Health is filled separately by the assemblers.
func recordFromNamespace(ns kube.NamespaceInfo) previewRecord {
	tag := strings.TrimPrefix(ns.Name, previewNSPrefix)
	rec := previewRecord{
		Tag:       tag,
		Branch:    ns.Annotations["bifrost/branch"],
		Apps:      []string{},
		Phase:     ns.Annotations["bifrost/phase"],
		CreatedAt: ns.CreatedAt,
		URLs:      map[string]string{},
		Step:      ns.Annotations["bifrost/step"],
		Error:     ns.Annotations["bifrost/error"],
		// Exactly the test the watcher itself applies (internal/preview's
		// autoUpdatable): "true" and nothing else means on, so absent, ""
		// (what an Up without auto-update writes) and any other value all
		// read as off.
		AutoUpdate: ns.Annotations["bifrost/auto-update"] == "true",
	}
	if since := ns.Annotations["bifrost/step-since"]; since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			rec.StepSince = t
		}
	}
	// Same swallow-the-parse-error shape as step-since directly above, for the
	// same reason: one malformed annotation must not fail the whole read. Here
	// it also fails safe — an unreadable expiry leaves ExpiresAt zero, which
	// everything downstream reads as "no expiry", never "expired long ago".
	if expires := ns.Annotations["bifrost/expires-at"]; expires != "" {
		if t, err := time.Parse(time.RFC3339, expires); err == nil {
			rec.ExpiresAt = t
		}
	}
	for _, app := range strings.Split(ns.Annotations["bifrost/apps"], ",") {
		if app = strings.TrimSpace(app); app != "" {
			rec.Apps = append(rec.Apps, app)
		}
	}
	if rec.Phase == "" {
		rec.Phase = "unknown"
	}
	if ns.Phase == "Terminating" {
		rec.Phase = "terminating"
		// A namespace being deleted isn't running its retained step any
		// more, and Down's own failures are reported to the caller rather
		// than annotated — so whatever step/error the annotations still hold
		// describes a run that is over. Left in place they'd read as work in
		// flight: "terminating · building footstrike-api (1/1) — build ended
		// with status FAILURE" for a failed preview being torn down.
		rec.Step = ""
		rec.StepSince = time.Time{}
		rec.Error = ""
	}
	for _, app := range rec.Apps {
		rec.URLs[app] = fmt.Sprintf("https://%s-%s.preview.footstrike.run", app, tag)
	}
	return rec
}

// anyPreviewCreating reports whether any preview is mid-creation, i.e.
// whether an Up is (as far as the cluster's annotations say) still running
// and writing new steps. previewsPage ORs this into the page's AnyActive so
// the Previews tab polls on the fast cadence while that's true; see the
// comment there for why the fleet-derived signal alone isn't enough.
func anyPreviewCreating(records []previewRecord) bool {
	for _, rec := range records {
		if rec.Phase == "creating" {
			return true
		}
	}
	return false
}

func (h *Handlers) assemblePreviews(ctx context.Context) ([]previewRecord, error) {
	namespaces, err := h.Kube.ListNamespaces(ctx, previewLabelSelector)
	if err != nil {
		return nil, err
	}
	records := make([]previewRecord, 0, len(namespaces))
	for _, ns := range namespaces {
		rec := recordFromNamespace(ns)
		rec.Health = h.previewHealth(ctx, ns.Name)
		records = append(records, rec)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Tag < records[j].Tag })
	return records, nil
}

func (h *Handlers) previewByTag(ctx context.Context, tag string) (previewRecord, bool, error) {
	ns, found, err := h.Kube.GetNamespace(ctx, previewNSPrefix+tag)
	if err != nil || !found {
		return previewRecord{}, false, err
	}
	rec := recordFromNamespace(ns)
	rec.Health = h.previewHealth(ctx, ns.Name)
	return rec, true, nil
}

// previewHealth summarizes pod health namespace-wide; errors degrade to
// "unknown" rather than failing the read (consistent with how the fleet
// view tolerates partial reads).
func (h *Handlers) previewHealth(ctx context.Context, namespace string) string {
	pods, err := h.Kube.ListPods(ctx, namespace)
	if err != nil {
		return "unknown"
	}
	return strings.ToLower(string(kube.SummarizeHealth(pods).State))
}

// PreviewsListJSON serves GET /api/previews for both the UI's consumers and
// the ib CLI (bearer-authed — may carry no session; no SessionFromContext).
func (h *Handlers) PreviewsListJSON(w http.ResponseWriter, r *http.Request) {
	records, err := h.assemblePreviews(r.Context())
	if err != nil {
		slog.Error("list previews failed", "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "list previews failed"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"previews": records})
}

// PreviewJSON serves GET /api/previews/{tag}.
func (h *Handlers) PreviewJSON(w http.ResponseWriter, r *http.Request) {
	rec, found, err := h.previewByTag(r.Context(), r.PathValue("tag"))
	if err != nil {
		slog.Error("get preview failed", "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "get preview failed"})
		return
	}
	if !found {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "unknown preview"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}
