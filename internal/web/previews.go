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
	// Busy reports that the orchestrator currently holds this tag for an
	// in-flight Up or Down (internal/preview's Orchestrator.Busy). Alone
	// among this struct's fields it is NOT derived from the namespace: it is
	// bifrost's own in-process state, and that is exactly why it is worth
	// reporting. A busy tag is the reason a concurrent up/down gets a 409,
	// and until now nothing in the API said so, which left "That preview is
	// busy" and "No preview environments" as the two simultaneous answers to
	// the same question. False for a preview nobody is touching, so
	// omitempty keeps it out of the JSON for almost every record; a consumer
	// must read a missing key as false.
	Busy bool `json:"busy,omitempty"`
}

// phaseBusy is the phase carried by a synthesized record — a tag the
// orchestrator holds that has no namespace to describe it (see busyRecord).
//
// It is deliberately its own phase rather than a reuse of an existing one.
// "creating" and "terminating" would both be guesses: the busy set is a bare
// claim on a tag and does not record which of Up or Down took it, and the two
// windows that produce this state are one of each (Up before EnsureNamespace,
// Down after DeleteNamespace). "terminating" would be an outright lie besides
// — recordFromNamespace reserves it for a namespace whose Kubernetes phase
// really is Terminating, and here there is no namespace at all. "unknown",
// which recordFromNamespace uses for a namespace missing its phase
// annotation, would throw away the one thing that IS known. So: busy, which
// claims exactly what bifrost can see.
const phaseBusy = "busy"

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

// busyRecord synthesizes the record for a tag the orchestrator holds but that
// has no namespace behind it, so the state is visible in listings at all
// instead of reading as "no such preview".
//
// Everything a namespace would have supplied is genuinely unknown here, and
// is left at the zero value rather than invented: no branch, no apps, no
// URLs, no creation time (the UI's relativeTime renders "" for the zero time,
// so the AGE cell simply stays blank). Health is "unknown" for the same
// reason the read path degrades to it elsewhere — there is no namespace to
// list pods in, and asking would be a wasted call with a foregone answer.
// Apps and URLs are non-nil so they marshal as [] and {} like every other
// record's.
func busyRecord(tag string) previewRecord {
	return previewRecord{
		Tag:    tag,
		Apps:   []string{},
		Phase:  phaseBusy,
		Health: "unknown",
		URLs:   map[string]string{},
		Busy:   true,
	}
}

func (h *Handlers) assemblePreviews(ctx context.Context) ([]previewRecord, error) {
	namespaces, err := h.Kube.ListNamespaces(ctx, previewLabelSelector)
	if err != nil {
		return nil, err
	}
	// One snapshot of the claimed set for the whole response, used both for
	// the flag on each namespace-backed record and to decide what to
	// synthesize below. Re-asking per tag would let a single response
	// contradict itself — flagging a tag busy in one record while omitting
	// the synthesized one for another because the claim moved in between.
	busy := h.busyTags()
	records := make([]previewRecord, 0, len(namespaces)+len(busy))
	hasNamespace := make(map[string]bool, len(namespaces))
	for _, ns := range namespaces {
		rec := recordFromNamespace(ns)
		rec.Health = h.previewHealth(ctx, ns.Name)
		rec.Busy = busy[rec.Tag]
		hasNamespace[rec.Tag] = true
		records = append(records, rec)
	}
	// Only the claimed tags with nothing to describe them: a busy tag that
	// does have a namespace already got a real record above, carrying the
	// same flag, and must not also get a phantom second one.
	for tag := range busy {
		if !hasNamespace[tag] {
			records = append(records, busyRecord(tag))
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Tag < records[j].Tag })
	return records, nil
}

// busyTags snapshots the orchestrator's claimed-tag set as a lookup table. A
// nil Orch — previews not configured, see Handlers.Orch — has no claims to
// report, so every record reads busy=false and nothing is synthesized,
// exactly as the read path behaved before any of this existed.
func (h *Handlers) busyTags() map[string]bool {
	if h.Orch == nil {
		return nil
	}
	tags := h.Orch.BusyTags()
	set := make(map[string]bool, len(tags))
	for _, tag := range tags {
		set[tag] = true
	}
	return set
}

// previewByTag reads one preview. Unlike assemblePreviews it deliberately
// does NOT synthesize a record for a busy tag with no namespace: found=false
// here means "no namespace", and DeletePreviewJSON depends on that to answer
// 404 for a tag that doesn't exist. Synthesizing would turn every in-flight
// teardown into a 200 and let a DELETE proceed against a preview that is
// already gone. The busy flag is still reported, so a caller polling a
// specific tag sees the claim as soon as there is a namespace to hang it on.
func (h *Handlers) previewByTag(ctx context.Context, tag string) (previewRecord, bool, error) {
	ns, found, err := h.Kube.GetNamespace(ctx, previewNSPrefix+tag)
	if err != nil || !found {
		return previewRecord{}, false, err
	}
	rec := recordFromNamespace(ns)
	rec.Health = h.previewHealth(ctx, ns.Name)
	rec.Busy = h.Orch != nil && h.Orch.Busy(tag)
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
