package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/eswan18/bifrost/internal/kube"
	"github.com/eswan18/bifrost/internal/previewapi"
)

// previewLabelSelector selects preview namespaces — the namespace itself is
// the preview's record (labels/annotations written by the 3c orchestrator).
const previewLabelSelector = "bifrost/preview=true"

// previewNSPrefix is prepended to a preview's tag to form its namespace name.
const previewNSPrefix = "preview-"

// parseBuiltImagesAnnotation reads bifrost/built-images into
// previewapi.Record.BuiltImages. It is a local reimplementation of
// internal/preview's parseBuiltImages/builtImage, not a call into them: both
// are unexported, and — more to the point — every other bifrost/* annotation
// this file reads back that is written only by internal/preview (branch,
// apps, step, error, auto-update) is likewise read here as a literal,
// per-package copy of the format rather than a shared helper (see the note
// atop that package's own annotation-key constants: a key "read only outside
// this package" stays a literal at its single point of use). The annotation's
// TEXT is the real contract between the two packages; this function is this
// package's own reader of it.
//
// Same fail-safe shape as parseBuiltImages, and for the same reason: a
// malformed or half-empty entry is dropped rather than reported, so a value
// bifrost itself wrote but can no longer parse degrades to "nothing built for
// this member" — never an error, and never surfaced any differently than an
// absent annotation. Returns nil, matching this field's documented zero
// value, whenever nothing valid was found — including for raw == "" — so a
// caller (and reflect.DeepEqual in a test) sees exactly the same thing for
// "no such annotation" and "an annotation with nothing readable in it".
func parseBuiltImagesAnnotation(raw string) map[string]previewapi.BuiltImage {
	var built map[string]previewapi.BuiltImage
	for _, pair := range strings.Split(raw, ",") {
		svc, rest, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		commit, shortSHA, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		svc, commit, shortSHA = strings.TrimSpace(svc), strings.TrimSpace(commit), strings.TrimSpace(shortSHA)
		if svc == "" || commit == "" || shortSHA == "" {
			continue
		}
		if built == nil {
			built = map[string]previewapi.BuiltImage{}
		}
		built[svc] = previewapi.BuiltImage{Commit: commit, ShortSHA: shortSHA}
	}
	return built
}

// recordFromNamespace derives everything derivable without extra cluster
// calls; Health is filled separately by the assemblers.
func recordFromNamespace(ns kube.NamespaceInfo) previewapi.Record {
	tag := strings.TrimPrefix(ns.Name, previewNSPrefix)
	rec := previewapi.Record{
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
		AutoUpdate:  ns.Annotations["bifrost/auto-update"] == "true",
		BuiltImages: parseBuiltImagesAnnotation(ns.Annotations["bifrost/built-images"]),
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
func anyPreviewCreating(records []previewapi.Record) bool {
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
func busyRecord(tag string) previewapi.Record {
	return previewapi.Record{
		Tag:    tag,
		Apps:   []string{},
		Phase:  previewapi.PhaseBusy,
		Health: "unknown",
		URLs:   map[string]string{},
		Busy:   true,
	}
}

func (h *Handlers) assemblePreviews(ctx context.Context) ([]previewapi.Record, error) {
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
	records := make([]previewapi.Record, 0, len(namespaces)+len(busy))
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
func (h *Handlers) previewByTag(ctx context.Context, tag string) (previewapi.Record, bool, error) {
	ns, found, err := h.Kube.GetNamespace(ctx, previewNSPrefix+tag)
	if err != nil || !found {
		return previewapi.Record{}, false, err
	}
	rec := recordFromNamespace(ns)
	rec.Health = h.previewHealth(ctx, ns.Name)
	rec.Busy = h.Orch != nil && h.Orch.Busy(tag)
	return rec, true, nil
}

// --- per-preview detail page --------------------------------------------------

// previewDetailState is which of four things GET /previews/{tag} is showing.
// It is an explicit enum rather than a (record, found) pair because two of the
// four are neither: a tag can be real and still have nothing to describe it
// (detailPending), and a failed cluster read is not the same answer as "no
// such preview" (detailUnavailable) — collapsing either into "missing" would
// have the page state, in plain English, something untrue.
type previewDetailState string

const (
	// detailFound: a namespace backs the tag, so Rec is the real record.
	detailFound previewDetailState = "found"
	// detailPending: the orchestrator holds the tag but no namespace exists
	// yet (an Up before EnsureNamespace, or a Down still deleting Neon
	// branches). previewByTag deliberately 404s for this — DeletePreviewJSON
	// depends on that — so the click-through from the Previews tab, where the
	// same tag DOES appear as a synthesized busy row, lands here. Rec is
	// busyRecord's synthesis, exactly what the list shows.
	detailPending previewDetailState = "pending"
	// detailMissing: no namespace and no claim. Genuinely not a preview.
	detailMissing previewDetailState = "missing"
	// detailUnavailable: the cluster read itself failed, so whether the
	// preview exists is unknown and the page must not guess.
	detailUnavailable previewDetailState = "unavailable"
)

// previewDetail is the view model behind the per-preview page and its polling
// fragment. Rec carries the record for the two states that have one and stays
// zero for the two that don't, so the template can read .Rec.Phase etc.
// unconditionally without a nil check.
type previewDetail struct {
	Tag     string
	State   previewDetailState
	Rec     previewapi.Record
	StepFor string // how long .Rec.Step has been running; "" when unknown
}

// Active reports whether something is in flight for this tag. base.html's
// poller drops from the 30s idle cadence to 4s whenever a [data-active] sits
// inside #tab-body, and this is what the detail template puts one there for —
// the same mechanism (and the same reasoning) as previewsPage's AnyActive,
// narrowed from "any preview" to this one.
//
// Busy covers the synthesized pending record (which is nothing BUT a claim)
// and a real record the orchestrator is re-running or tearing down; "creating"
// covers an Up that is past EnsureNamespace and writing steps the page exists
// to narrate. A settled preview polls at the idle cadence like any other page.
func (d previewDetail) Active() bool {
	return d.Rec.Busy || d.Rec.Phase == "creating"
}

// Status is the HTTP status the FULL page answers with. The fragment always
// answers 200 regardless — see previewPage.
func (d previewDetail) Status() int {
	switch d.State {
	case detailMissing:
		return http.StatusNotFound
	case detailUnavailable:
		return http.StatusBadGateway
	default:
		return http.StatusOK
	}
}

// previewDetailFor resolves one tag into the four-state view model above. It
// reads through previewByTag rather than around it, so the API and the page
// agree about what exists; the busy fallback below is layered on top for the
// page only, and previewByTag's own 404 behaviour is untouched.
func (h *Handlers) previewDetailFor(ctx context.Context, tag string) previewDetail {
	d := previewDetail{Tag: tag}
	rec, found, err := h.previewByTag(ctx, tag)
	switch {
	case err != nil:
		slog.Error("read preview failed", "tag", tag, "err", err)
		d.State = detailUnavailable
	case found:
		d.State = detailFound
		d.Rec = rec
		// Computed here, from a timestamp, rather than stored as a duration:
		// the page re-renders on every poll, so the elapsed time is always
		// fresh instead of going stale between them (the same reason
		// StepSince is a timestamp on the record at all).
		if !rec.StepSince.IsZero() {
			d.StepFor = runningFor(time.Since(rec.StepSince))
		}
	case h.Orch != nil && h.Orch.Busy(tag):
		d.State = detailPending
		d.Rec = busyRecord(tag)
	default:
		d.State = detailMissing
	}
	return d
}

// previewRefreshURL is the per-tag polling fragment base.html's data-refresh
// points at. Escaped because it comes straight off the request path: a tag
// bifrost itself derived is [a-z0-9-] (internal/preview's TagForBranch), but
// the one in the URL is whatever the caller typed.
func previewRefreshURL(tag string) string {
	return "/partial/previews/" + url.PathEscape(tag)
}

// Preview serves GET /previews/{tag}, the per-preview detail page.
func (h *Handlers) Preview(w http.ResponseWriter, r *http.Request) {
	h.previewPage(w, r, true)
}

// PreviewFragment serves GET /partial/previews/{tag}, the polled fragment the
// page swaps into #tab-body.
func (h *Handlers) PreviewFragment(w http.ResponseWriter, r *http.Request) {
	h.previewPage(w, r, false)
}

// previewPage renders the detail page, full or as the tab-body fragment.
//
// Both go through the "previews" template set — the detail body lives in
// previews.html beside the tab's, and "tab-body" dispatches on vm.Preview —
// so the two share preview-phase and preview-expiry outright instead of
// growing a second, subtly different copy of each in a file of their own.
func (h *Handlers) previewPage(w http.ResponseWriter, r *http.Request, full bool) {
	tag := r.PathValue("tag")
	detail := h.previewDetailFor(r.Context(), tag)
	f := h.assembleFleet(r.Context())
	vm := h.dashboardVM(r, "previews", previewRefreshURL(tag), f)
	vm.Preview = &detail
	vm.AnyActive = vm.AnyActive || detail.Active()
	if full {
		vm.Flash = TakeFlash(w, r)
		h.renderStatus(w, "previews", detail.Status(), vm)
		return
	}
	// The fragment answers 200 for every state, including the two the full
	// page 404s/502s for. base.html's refresh() treats a non-ok response as a
	// failed poll — it marks the header stale and swaps nothing — so a preview
	// torn down while its page is open would freeze on the old content instead
	// of turning into the not-found body. The status belongs to the page a
	// human (or a monitor) requested, not to a background poll.
	h.renderNamed(w, "previews", "tab-body", vm)
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
// the bif CLI (bearer-authed — may carry no session; no SessionFromContext).
func (h *Handlers) PreviewsListJSON(w http.ResponseWriter, r *http.Request) {
	records, err := h.assemblePreviews(r.Context())
	if err != nil {
		slog.Error("list previews failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "list previews failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(previewapi.ListResponse{Previews: records})
}

// PreviewJSON serves GET /api/previews/{tag}.
func (h *Handlers) PreviewJSON(w http.ResponseWriter, r *http.Request) {
	rec, found, err := h.previewByTag(r.Context(), r.PathValue("tag"))
	if err != nil {
		slog.Error("get preview failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "get preview failed")
		return
	}
	if !found {
		writeJSONError(w, http.StatusNotFound, "unknown preview")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}
