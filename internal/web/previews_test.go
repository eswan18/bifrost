package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eswan18/bifrost/internal/auth"
	"github.com/eswan18/bifrost/internal/kube"
)

func nsInfo(name, branch, apps, phase string) kube.NamespaceInfo {
	ann := map[string]string{}
	if branch != "" {
		ann["bifrost/branch"] = branch
	}
	if apps != "" {
		ann["bifrost/apps"] = apps
	}
	if phase != "" {
		ann["bifrost/phase"] = phase
	}
	return kube.NamespaceInfo{
		Name: name, Labels: map[string]string{"bifrost/preview": "true"},
		Annotations: ann, CreatedAt: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC), Phase: "Active",
	}
}

func TestRecordFromNamespace(t *testing.T) {
	r := recordFromNamespace(nsInfo("preview-hae-cadence", "hae-cadence", "footstrike-api,footstrike-dashboard", "ready"))
	if r.Tag != "hae-cadence" || r.Branch != "hae-cadence" || r.Phase != "ready" {
		t.Errorf("record = %+v", r)
	}
	if len(r.Apps) != 2 || r.Apps[0] != "footstrike-api" {
		t.Errorf("apps = %v", r.Apps)
	}
	if r.URLs["footstrike-api"] != "https://footstrike-api-hae-cadence.preview.footstrike.run" {
		t.Errorf("urls = %v", r.URLs)
	}
}

func TestRecordFromNamespaceTrailingComma(t *testing.T) {
	r := recordFromNamespace(nsInfo("preview-x", "x", "footstrike-api,", "ready"))
	if len(r.Apps) != 1 || r.Apps[0] != "footstrike-api" {
		t.Fatalf("trailing comma must not produce a phantom app, got apps=%v", r.Apps)
	}
	if len(r.URLs) != 1 || r.URLs["footstrike-api"] != "https://footstrike-api-x.preview.footstrike.run" {
		t.Errorf("trailing comma must not produce a bogus URL, got urls=%v", r.URLs)
	}
}

func TestRecordFromNamespaceDefaults(t *testing.T) {
	r := recordFromNamespace(nsInfo("preview-x", "", "", ""))
	if r.Phase != "unknown" {
		t.Errorf("absent phase must read unknown, got %q", r.Phase)
	}
	if len(r.Apps) != 0 {
		t.Errorf("absent apps must be empty, got %v", r.Apps)
	}
}

func TestRecordFromNamespaceTerminating(t *testing.T) {
	ns := nsInfo("preview-x", "x", "", "ready")
	ns.Phase = "Terminating"
	if r := recordFromNamespace(ns); r.Phase != "terminating" {
		t.Errorf("terminating namespace must override phase, got %q", r.Phase)
	}
}

// TestRecordFromNamespaceTerminatingSuppressesStepAndError covers tearing
// down a FAILED preview — the common case, since a failed preview is usually
// the next thing an operator runs `ib preview down` on. Its annotations still
// carry the retained last step and error (deliberately, for as long as it
// exists), but once the namespace is Terminating that run is over: rendering
// them would read as "terminating · building footstrike-api (1/1) — build
// ended with status FAILURE", i.e. as though a build were still in flight.
func TestRecordFromNamespaceTerminatingSuppressesStepAndError(t *testing.T) {
	ns := nsInfo("preview-x", "x", "footstrike-api", "failed")
	ns.Annotations["bifrost/step"] = "building footstrike-api (1/1)"
	ns.Annotations["bifrost/step-since"] = "2026-07-27T12:34:56Z"
	ns.Annotations["bifrost/error"] = "build ended with status FAILURE"
	ns.Phase = "Terminating"

	r := recordFromNamespace(ns)
	if r.Phase != "terminating" {
		t.Fatalf("Phase = %q, want terminating", r.Phase)
	}
	if r.Step != "" {
		t.Errorf("Step = %q, want suppressed while terminating", r.Step)
	}
	if !r.StepSince.IsZero() {
		t.Errorf("StepSince = %v, want zero while terminating", r.StepSince)
	}
	if r.Error != "" {
		t.Errorf("Error = %q, want suppressed while terminating", r.Error)
	}
}

// TestRecordFromNamespaceStep pins the step-reporting contract added for
// preview progress: bifrost/step, bifrost/step-since, and bifrost/error all
// surface onto previewRecord verbatim (the timestamp parsed, not just
// copied).
func TestRecordFromNamespaceStep(t *testing.T) {
	ns := nsInfo("preview-hae-cadence", "hae-cadence", "footstrike-api", "creating")
	ns.Annotations["bifrost/step"] = "building footstrike-api (1/2)"
	ns.Annotations["bifrost/step-since"] = "2026-07-27T12:34:56Z"

	r := recordFromNamespace(ns)
	if r.Step != "building footstrike-api (1/2)" {
		t.Errorf("Step = %q", r.Step)
	}
	wantSince := time.Date(2026, 7, 27, 12, 34, 56, 0, time.UTC)
	if !r.StepSince.Equal(wantSince) {
		t.Errorf("StepSince = %v, want %v", r.StepSince, wantSince)
	}
}

// TestRecordFromNamespaceReadyReportsEmptyStep asserts the "ready clears
// step" half of the contract: a ready preview (no bifrost/step annotation,
// since Up wipes it on success) must not display a stale step.
func TestRecordFromNamespaceReadyReportsEmptyStep(t *testing.T) {
	ns := nsInfo("preview-x", "x", "footstrike-api", "ready")
	r := recordFromNamespace(ns)
	if r.Step != "" {
		t.Errorf("Step = %q, want empty for a ready preview", r.Step)
	}
	if !r.StepSince.IsZero() {
		t.Errorf("StepSince = %v, want zero for a ready preview", r.StepSince)
	}
}

// TestRecordFromNamespaceFailedRetainsStepAndError asserts the "failed
// preview keeps its last step" diagnostic contract, plus that bifrost/error
// surfaces onto the record.
func TestRecordFromNamespaceFailedRetainsStepAndError(t *testing.T) {
	ns := nsInfo("preview-x", "x", "footstrike-api", "failed")
	ns.Annotations["bifrost/step"] = "building footstrike-api (1/2)"
	ns.Annotations["bifrost/step-since"] = "2026-07-27T12:34:56Z"
	ns.Annotations["bifrost/error"] = "build ended with status FAILURE"

	r := recordFromNamespace(ns)
	if r.Step != "building footstrike-api (1/2)" {
		t.Errorf("Step = %q, want the retained last step", r.Step)
	}
	if r.Error != "build ended with status FAILURE" {
		t.Errorf("Error = %q", r.Error)
	}
}

// TestRecordFromNamespaceInvalidStepSinceIgnored guards against a malformed
// (or absent) bifrost/step-since annotation crashing or poisoning the
// record: it should just leave StepSince zero rather than propagating a
// parse error anywhere visible.
func TestRecordFromNamespaceInvalidStepSinceIgnored(t *testing.T) {
	ns := nsInfo("preview-x", "x", "footstrike-api", "creating")
	ns.Annotations["bifrost/step"] = "branching databases"
	ns.Annotations["bifrost/step-since"] = "not-a-timestamp"

	r := recordFromNamespace(ns)
	if !r.StepSince.IsZero() {
		t.Errorf("StepSince = %v, want zero for an unparseable annotation", r.StepSince)
	}
}

// TestRecordFromNamespaceExpiresAt pins the whole bifrost/expires-at read
// contract, which is bifrost/step-since's contract exactly: the timestamp is
// parsed (not just copied), and absent/empty/malformed all degrade to the
// zero time rather than failing the read.
//
// The three degrading cases are deliberately table rows alongside the "set"
// row rather than tests of their own: on their own they would pass even with
// the parse in recordFromNamespace deleted (an unset field is zero either
// way), so they only discriminate as guards against a *future* change — a
// default TTL, or treating an unreadable expiry as "expired" — with the set
// row as the control that fails the moment the parse itself regresses.
func TestRecordFromNamespaceExpiresAt(t *testing.T) {
	cases := []struct {
		name       string
		annotation string
		setAnn     bool
		want       time.Time
	}{
		{"set", "2026-07-27T20:34:56Z", true, time.Date(2026, 7, 27, 20, 34, 56, 0, time.UTC)},
		{"absent", "", false, time.Time{}},
		{"cleared by a no-TTL re-run", "", true, time.Time{}},
		{"unparseable", "not-a-timestamp", true, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := nsInfo("preview-hae-cadence", "hae-cadence", "footstrike-api", "ready")
			if tc.setAnn {
				ns.Annotations["bifrost/expires-at"] = tc.annotation
			}
			if got := recordFromNamespace(ns).ExpiresAt; !got.Equal(tc.want) {
				t.Errorf("ExpiresAt = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRecordFromNamespaceAutoUpdate pins the bifrost/auto-update read: only
// the literal "true" turns it on, and every other reading of the annotation —
// absent, "" (what an Up that didn't ask for it writes, since that write
// merges), and any other value — is off. The negative rows discriminate here
// in a way the expires-at table's do not: a `!= ""` or `strconv.ParseBool`
// reading of this annotation would pass the "true" row and fail these.
func TestRecordFromNamespaceAutoUpdate(t *testing.T) {
	cases := []struct {
		name       string
		annotation string
		setAnn     bool
		want       bool
	}{
		{"opted in", "true", true, true},
		{"absent", "", false, false},
		{"cleared by a re-run that didn't ask for it", "", true, false},
		{"false", "false", true, false},
		{"garbage", "yes", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := nsInfo("preview-hae-cadence", "hae-cadence", "footstrike-api", "ready")
			if tc.setAnn {
				ns.Annotations["bifrost/auto-update"] = tc.annotation
			}
			if got := recordFromNamespace(ns).AutoUpdate; got != tc.want {
				t.Errorf("AutoUpdate = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPreviewJSONOmitsAutoUpdateWhenOff pins the wire shape both directions:
// present as `"autoUpdate":true` for an opted-in preview, and omitted
// entirely (omitempty) for every other one, so no existing consumer sees a
// new key on a preview that behaves exactly as it always did.
func TestPreviewJSONOmitsAutoUpdateWhenOff(t *testing.T) {
	on := nsInfo("preview-on", "on", "footstrike-api", "ready")
	on.Annotations["bifrost/auto-update"] = "true"
	off := nsInfo("preview-off", "off", "footstrike-api", "ready")

	for _, tc := range []struct {
		name string
		ns   kube.NamespaceInfo
		want bool
	}{{"on", on, true}, {"off", off, false}} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handlers{Kube: &fakeKube{namespaces: []kube.NamespaceInfo{tc.ns}}}
			rec := httptest.NewRecorder()
			h.PreviewsListJSON(rec, httptest.NewRequest("GET", "/api/previews", nil))
			body := rec.Body.String()
			if got := strings.Contains(body, `"autoUpdate":true`); got != tc.want {
				t.Errorf("body %q contains autoUpdate = %v, want %v", body, got, tc.want)
			}
			if strings.Contains(body, `"autoUpdate":false`) {
				t.Errorf("body = %q, want the field omitted rather than rendered false", body)
			}
		})
	}
}

// --- busy visibility ---------------------------------------------------------

// TestPreviewRecordCarriesBusyFlag pins the flag itself: a tag the
// orchestrator holds is reported as busy on its record, on both the list and
// the single-preview read, and a tag nobody holds is not.
func TestPreviewRecordCarriesBusyFlag(t *testing.T) {
	fk := &fakeKube{namespaces: []kube.NamespaceInfo{
		nsInfo("preview-a", "a", "footstrike-api", "creating"),
		nsInfo("preview-b", "b", "footstrike-api", "ready"),
	}}
	h := &Handlers{Kube: fk, Orch: &fakeOrchestration{busyTags: map[string]bool{"a": true}}}

	recs, err := h.assemblePreviews(context.Background())
	if err != nil {
		t.Fatalf("assemblePreviews: %v", err)
	}
	if len(recs) != 2 || recs[0].Tag != "a" || recs[1].Tag != "b" {
		t.Fatalf("records = %+v, want [a b]", recs)
	}
	if !recs[0].Busy {
		t.Error("a is claimed by the orchestrator but its record reports busy=false")
	}
	if recs[1].Busy {
		t.Error("b is claimed by nobody but its record reports busy=true")
	}
	// The flag must survive into the JSON, since the CLI and the Previews
	// tab are the consumers this exists for.
	rec := httptest.NewRecorder()
	h.PreviewsListJSON(rec, httptest.NewRequest("GET", "/api/previews", nil))
	if body := rec.Body.String(); !strings.Contains(body, `"busy":true`) {
		t.Errorf("list body = %q, want it to carry \"busy\":true", body)
	}

	// Same flag on GET /api/previews/{tag}, which polls one tag rather than
	// listing.
	got, found, err := h.previewByTag(context.Background(), "a")
	if err != nil || !found {
		t.Fatalf("previewByTag(a) = (_, %v, %v)", found, err)
	}
	if !got.Busy {
		t.Error("previewByTag dropped the busy flag")
	}
}

// TestPreviewsListSynthesizesABusyTagWithNoNamespace is the state that made
// the production incident baffling: `ib preview list` said "No preview
// environments" while `ib preview up` said "That preview is busy", and
// neither was the whole truth. A tag can be claimed with no namespace behind
// it — Down holds it after deleting the namespace while it works through the
// Neon branches, Up holds it during membership resolution before the
// namespace exists — and a listing that can only see namespaces cannot show
// that at all.
func TestPreviewsListSynthesizesABusyTagWithNoNamespace(t *testing.T) {
	h := &Handlers{
		Kube: &fakeKube{},
		Orch: &fakeOrchestration{busyTags: map[string]bool{"training-plans": true}},
	}

	recs, err := h.assemblePreviews(context.Background())
	if err != nil {
		t.Fatalf("assemblePreviews: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("records = %+v, want exactly the one synthesized record", recs)
	}
	got := recs[0]
	if got.Tag != "training-plans" {
		t.Errorf("Tag = %q, want training-plans", got.Tag)
	}
	if !got.Busy {
		t.Error("Busy = false on a synthesized record, want true")
	}
	if got.Phase != "busy" {
		t.Errorf("Phase = %q, want busy — the claim is all that's known, so the phase must not guess", got.Phase)
	}
	if got.Apps == nil || got.URLs == nil {
		t.Errorf("Apps/URLs = %v/%v, want non-nil so they marshal as [] and {}", got.Apps, got.URLs)
	}

	rec := httptest.NewRecorder()
	h.PreviewsListJSON(rec, httptest.NewRequest("GET", "/api/previews", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `"tag":"training-plans"`) || !strings.Contains(body, `"phase":"busy"`) {
		t.Errorf("body = %q, want the synthesized record to appear with phase busy", body)
	}
	// "terminating" is reserved for a namespace whose Kubernetes phase really
	// is Terminating. There is no namespace here, so claiming it would be a
	// guess — and half the time the wrong one, since Up holds the tag too.
	if strings.Contains(body, "terminating") {
		t.Errorf("body = %q, want no terminating claim for a tag with no namespace", body)
	}
}

// TestPreviewsListInventsNoPhantomRecords is the negative control for the
// synthesis above: it must add a record only for a claimed tag that has
// nothing else describing it, never a duplicate alongside a real record and
// never anything at all for a tag nobody holds.
func TestPreviewsListInventsNoPhantomRecords(t *testing.T) {
	for _, tc := range []struct {
		name string
		busy map[string]bool
	}{
		// The busy tag DOES have a namespace, so it already has a real
		// record; synthesizing on top would show the same preview twice.
		{"claimed tag that has a namespace", map[string]bool{"a": true}},
		// Nothing claimed at all — the overwhelmingly common case.
		{"nothing claimed", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fk := &fakeKube{namespaces: []kube.NamespaceInfo{nsInfo("preview-a", "a", "footstrike-api", "ready")}}
			h := &Handlers{Kube: fk, Orch: &fakeOrchestration{busyTags: tc.busy}}

			recs, err := h.assemblePreviews(context.Background())
			if err != nil {
				t.Fatalf("assemblePreviews: %v", err)
			}
			if len(recs) != 1 || recs[0].Tag != "a" {
				t.Fatalf("records = %+v, want exactly one record for a", recs)
			}
			if recs[0].Phase != "ready" {
				t.Errorf("Phase = %q, want the namespace's own phase, not a synthesized one", recs[0].Phase)
			}
		})
	}
}

// TestPreviewsListOmitsBusyWhenFalse holds the field to the same JSON
// convention autoUpdate follows: absent, not rendered false, for the
// preview nobody is touching — so a consumer reads a missing key as false.
func TestPreviewsListOmitsBusyWhenFalse(t *testing.T) {
	fk := &fakeKube{namespaces: []kube.NamespaceInfo{nsInfo("preview-a", "a", "footstrike-api", "ready")}}
	h := &Handlers{Kube: fk, Orch: &fakeOrchestration{}}

	rec := httptest.NewRecorder()
	h.PreviewsListJSON(rec, httptest.NewRequest("GET", "/api/previews", nil))
	if body := rec.Body.String(); strings.Contains(body, `"busy"`) {
		t.Errorf("body = %q, want the busy key omitted entirely rather than rendered false", body)
	}
}

// TestAssemblePreviewsWithoutAnOrchestrator covers the previews-not-configured
// wiring (main.go leaves Handlers.Orch nil when the preview control plane
// isn't set up): the read path must still work, reporting nothing busy rather
// than panicking on a nil interface.
func TestAssemblePreviewsWithoutAnOrchestrator(t *testing.T) {
	fk := &fakeKube{namespaces: []kube.NamespaceInfo{nsInfo("preview-a", "a", "footstrike-api", "ready")}}
	h := &Handlers{Kube: fk}

	recs, err := h.assemblePreviews(context.Background())
	if err != nil {
		t.Fatalf("assemblePreviews: %v", err)
	}
	if len(recs) != 1 || recs[0].Busy {
		t.Fatalf("records = %+v, want one record reporting busy=false", recs)
	}
	if _, found, err := h.previewByTag(context.Background(), "a"); err != nil || !found {
		t.Fatalf("previewByTag = (_, %v, %v), want it to work with a nil Orch", found, err)
	}
}

func TestAssemblePreviews(t *testing.T) {
	fk := &fakeKube{namespaces: []kube.NamespaceInfo{
		nsInfo("preview-b", "b", "footstrike-api", "ready"),
		nsInfo("preview-a", "a", "footstrike-api", "creating"),
	}}
	h := &Handlers{Kube: fk}
	got, err := h.assemblePreviews(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0].Tag != "a" || got[1].Tag != "b" {
		t.Fatalf("want sorted by tag [a b], got %+v", got)
	}
}

func TestPreviewByTagMissing(t *testing.T) {
	h := &Handlers{Kube: &fakeKube{}}
	_, found, err := h.previewByTag(context.Background(), "nope")
	if err != nil || found {
		t.Fatalf("want (zero,false,nil), got found=%v err=%v", found, err)
	}
}

func TestPreviewsListJSON(t *testing.T) {
	fk := &fakeKube{namespaces: []kube.NamespaceInfo{nsInfo("preview-a", "a", "footstrike-api", "ready")}}
	h := &Handlers{Kube: fk}
	req := httptest.NewRequest("GET", "/api/previews", nil)
	rec := httptest.NewRecorder()
	h.PreviewsListJSON(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var body struct {
		Previews []previewRecord `json:"previews"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Previews) != 1 || body.Previews[0].Tag != "a" {
		t.Errorf("body = %+v", body)
	}
}

func TestPreviewJSONNotFound(t *testing.T) {
	h := &Handlers{Kube: &fakeKube{}}
	req := httptest.NewRequest("GET", "/api/previews/nope", nil)
	req.SetPathValue("tag", "nope")
	rec := httptest.NewRecorder()
	h.PreviewJSON(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestPreviewsListJSONKubeError(t *testing.T) {
	h := &Handlers{Kube: &fakeKube{namespacesErr: errors.New("boom")}}
	req := httptest.NewRequest("GET", "/api/previews", nil)
	rec := httptest.NewRecorder()
	h.PreviewsListJSON(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"error"`) {
		t.Errorf("body = %q, want it to contain \"error\"", body)
	}
}

// TestPreviewsListJSONEmpty pins the JSON contract for an empty previews
// list: the field must marshal as [] (via the non-nil make() slice in
// assemblePreviews), never null.
func TestPreviewsListJSONEmpty(t *testing.T) {
	h := &Handlers{Kube: &fakeKube{}}
	req := httptest.NewRequest("GET", "/api/previews", nil)
	rec := httptest.NewRecorder()
	h.PreviewsListJSON(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"previews":[]`) {
		t.Errorf("body = %q, want it to contain \"previews\":[]", body)
	}
}

// --- UI tab ------------------------------------------------------------------

func TestPreviewsPage(t *testing.T) {
	creating := nsInfo("preview-x", "x", "footstrike-api", "creating")
	creating.Annotations["bifrost/step"] = "building footstrike-api (1/2)"
	k := &fakeKube{namespaces: []kube.NamespaceInfo{
		nsInfo("preview-hae-cadence", "hae-cadence", "foo", "ready"),
		creating,
	}}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "GET", "/previews", "", sess)
	rec := httptest.NewRecorder()
	h.Previews(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hae-cadence") {
		t.Error("preview tag missing from page")
	}
	if !strings.Contains(body, `class="tab active"`) {
		t.Error("previews tab not marked active")
	}
	// Two previews: the nav badge should render their count.
	if !strings.Contains(body, `Previews<span class="tab-count">2</span>`) {
		t.Error("nav badge should show PreviewCount when non-zero")
	}
	// The creating preview's step must be narrated next to its phase.
	if !strings.Contains(body, "building footstrike-api (1/2)") {
		t.Error("creating preview's step text missing from page")
	}
	// The ready preview (no step annotation) must render its bare phase —
	// no step decoration, no stray separator, nothing tacked on. The
	// Contains check alone is too weak to prove this on its own: the
	// mobile job-card-top phase span (untouched by this feature, always
	// bare) also matches `<span class="c-mut">ready</span>`, so it would
	// still pass even if the desktop preview-phase cell were broken (e.g.
	// unconditionally emitting "{{.Phase}} · {{.Step}}"). The negative
	// check below is what actually pins the desktop cell: it fails if a
	// dangling " · " separator leaks in for an empty Step, the same
	// pattern TestPreviewsPageCreatingWithoutStepRendersClean uses.
	if !strings.Contains(body, `<span class="c-mut">ready</span>`) {
		t.Error("ready preview must render its bare phase with no step")
	}
	if strings.Contains(body, "ready ·") {
		t.Error("ready preview must not leave a dangling separator")
	}
}

// TestPreviewsPageCreatingWithoutStepRendersClean guards the "older preview,
// no step annotation" case (a preview created before this feature existed,
// or simply between step writes): Step is "" but Phase is still "creating",
// and the PHASE cell must render just the bare phase — no dangling " · "
// separator, no "unknown" placeholder, no broken markup.
func TestPreviewsPageCreatingWithoutStepRendersClean(t *testing.T) {
	k := &fakeKube{namespaces: []kube.NamespaceInfo{
		nsInfo("preview-x", "x", "footstrike-api", "creating"),
	}}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "GET", "/previews", "", sess)
	rec := httptest.NewRecorder()
	h.Previews(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	// The exact-match here (not just Contains) is the "not unknown, no
	// broken markup" assertion: it pins the PHASE cell's entire content to
	// the bare phase, with nothing appended (no "unknown" placeholder, no
	// dangling " · " separator, no stray markup) when Step is absent — the
	// HEALTH column legitimately renders "unknown" for this pod-less
	// fixture, so a body-wide substring check for "unknown" would be a
	// false positive here.
	if !strings.Contains(body, `<span class="c-mut">creating</span>`) {
		t.Error("phase with no step must render bare, got a decorated or broken phase cell")
	}
	if strings.Contains(body, "creating ·") {
		t.Error("phase with no step must not leave a dangling separator")
	}
}

// desktopAgeCell isolates the desktop AGE grid cell's HTML: the AGE span is
// the only element on the page carrying a title="<abstime>" attribute (the
// mobile job-card-meta line renders no title at all), so scoping to it makes
// an assertion attributable to the desktop cell specifically. Without this,
// a Contains check against the whole body could be satisfied by the mobile
// card even if the desktop jobs-grid cell were broken — see
// TestPreviewsPage's comment on the same trap for the PHASE cell.
func desktopAgeCell(t *testing.T, body, title string) string {
	t.Helper()
	start := strings.Index(body, `title="`+title+`"`)
	if start == -1 {
		t.Fatalf("age cell title %q not found in body", title)
	}
	end := strings.Index(body[start:], "</span>")
	if end == -1 {
		t.Fatalf("age cell closing </span> not found after title %q", title)
	}
	return body[start : start+end+len("</span>")]
}

// desktopRow isolates one preview's desktop jobs-grid row, for the same
// reason desktopAgeCell isolates its AGE cell: the mobile job-card renders
// the same tag, branch, phase and health text, so a body-wide Contains can be
// satisfied by the mobile card while the desktop grid row is broken. Every
// assertion about the six-column layout has to be attributable to the row
// itself.
//
// It matches on the PREVIEW cell's detail-page link, which is the row's first
// grid child and (unlike the bare tag text) appears nowhere else in the
// desktop grid.
func desktopRow(t *testing.T, body, tag string) string {
	t.Helper()
	for _, chunk := range strings.Split(body, `<div class="jobs-grid jobs-row">`)[1:] {
		end := strings.Index(chunk, "</div>")
		if end == -1 {
			continue
		}
		if row := chunk[:end]; strings.Contains(row, `<span class="mono w6"><a href="/previews/`+tag+`"`) {
			return row
		}
	}
	t.Fatalf("no desktop jobs-grid row for preview %q in body", tag)
	return ""
}

// countGridCells counts a desktop row's top-level grid children. jobs-grid is
// a fixed six-column layout SHARED with jobs.html, so a row that renders a
// seventh (or fifth) child silently misaligns both tabs.
//
// It tracks nesting depth rather than matching indentation: a cell added on
// the same line as an existing one is still a seventh grid child, and an
// indentation-based count would wave it through. Nested elements (the APPS
// cell's links, preview-phase's c-red error, the teardown trigger inside the
// PREVIEW cell) are correctly not counted — keeping them nested is exactly
// how those templates stay within six columns. The row contains no void
// elements, so every open tag here really does close.
func countGridCells(row string) int {
	cells, depth := 0, 0
	for {
		i := strings.IndexByte(row, '<')
		if i < 0 {
			return cells
		}
		row = row[i+1:]
		j := strings.IndexByte(row, '>')
		if j < 0 {
			return cells
		}
		tag := row[:j]
		row = row[j+1:]
		if strings.HasPrefix(tag, "/") {
			depth--
			continue
		}
		if depth == 0 {
			cells++
		}
		if !strings.HasSuffix(tag, "/") {
			depth++
		}
	}
}

// TestPreviewsPageShowsCreateForm covers the create half of the tab's new
// controls. Until this existed the Previews tab was read-only and `ib preview
// up` was the only way to make a preview — which is a poor answer on a phone,
// the reason bifrost exists at all.
//
// The form deliberately is NOT the plain <form method="POST"> promote and
// rollback use. Those endpoints read r.FormValue("csrf"); POST /api/previews
// reads the X-CSRF-Token HEADER (verifyMutationCSRF) and takes a JSON body,
// so the submit must be intercepted — hence the js-preview-create hook the
// assertions below pin alongside the fields themselves.
func TestPreviewsPageShowsCreateForm(t *testing.T) {
	k := &fakeKube{namespaces: []kube.NamespaceInfo{nsInfo("preview-x", "x", "footstrike-api", "ready")}}
	h, sess := newTestHandlers(t, k)
	rec := httptest.NewRecorder()
	h.Previews(rec, authed(t, "GET", "/previews", "", sess))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		// The trigger and the panel are the established CSS-only modal
		// idiom (style.css's .modal:target), so the form opens with no JS
		// at all — only the POST itself needs fetch().
		`<a href="#modal-preview-create"`,
		`<div id="modal-preview-create" class="modal">`,
		`<a href="#close" class="modal-backdrop"`,
		// The fetch() hook and the endpoint it targets.
		`class="modal-rows js-preview-create"`,
		`action="/api/previews"`,
		// Branch (required), TTL (optional), auto-update (optional).
		`<input type="text" name="branch"`,
		`<input type="text" name="ttl"`,
		`<input type="checkbox" name="autoUpdate">`,
		`<button type="submit" class="pill pill-primary" data-csrf=`,
		// The slot the handler writes a 400's or 409's server-owned message
		// into. Without it every failure would be invisible.
		`class="c-red js-preview-error"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("create form is missing %s", want)
		}
	}

	// Branch is the one required field, and the browser's own validation is
	// what stops an empty submit before the fetch ever runs (the submit event
	// doesn't fire for an invalid form), so it has to be on the input rather
	// than left to the server's 400.
	start := strings.Index(body, `<input type="text" name="branch"`)
	if start == -1 {
		t.Fatal("no branch input to inspect")
	}
	branchInput := body[start : start+strings.Index(body[start:], ">")]
	if !strings.Contains(branchInput, "required") {
		t.Errorf("branch input = %q, want it marked required", branchInput)
	}
	// TTL must NOT be required — a preview with no TTL never expires, and
	// that is the default by design.
	start = strings.Index(body, `<input type="text" name="ttl"`)
	if ttlInput := body[start : start+strings.Index(body[start:], ">")]; strings.Contains(ttlInput, "required") {
		t.Errorf("ttl input = %q, want it optional", ttlInput)
	}
}

// TestPreviewsPageCarriesCSRFToken pins the token the fetch()-driven controls
// send as X-CSRF-Token. verifyMutationCSRF skips verification only when there
// is NO session — the bearer path the CLI uses; the browser always has one and
// so always needs the header, and a control that can't produce the token is a
// control that can only 403.
func TestPreviewsPageCarriesCSRFToken(t *testing.T) {
	k := &fakeKube{namespaces: []kube.NamespaceInfo{nsInfo("preview-x", "x", "footstrike-api", "ready")}}
	h, sess := newTestHandlers(t, k)
	token := csrf(h, sess)

	rec := httptest.NewRecorder()
	h.Previews(rec, authed(t, "GET", "/previews", "", sess))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()

	// data-csrf specifically, not the bare token: base.html renders the
	// promote/rollback modals on every dashboard page, and their hidden
	// "csrf" form inputs already carry the same value — so a Contains(token)
	// would pass happily with the preview controls carrying nothing at all.
	if !strings.Contains(body, `data-csrf="`+token+`"`) {
		t.Error("preview controls do not carry the session's CSRF token as data-csrf")
	}
	if strings.Contains(body, `data-csrf=""`) {
		t.Error("a preview control rendered an empty CSRF token, which can only 403")
	}

	// The fragment must carry it too. Both controls live inside #tab-body,
	// which the background poll replaces wholesale every 4–30s; a token
	// present only on the full page would leave every control dead after the
	// first swap.
	frag := httptest.NewRecorder()
	h.PreviewsFragment(frag, authed(t, "GET", "/partial/previews", "", sess))
	if frag.Code != http.StatusOK {
		t.Fatalf("fragment code = %d", frag.Code)
	}
	if !strings.Contains(frag.Body.String(), `data-csrf="`+token+`"`) {
		t.Error("the polled fragment drops the CSRF token, so controls would stop working after the first refresh")
	}
}

// TestPreviewsPageShowsTeardownControl covers the teardown half: a control on
// every real preview, on both surfaces, behind the same CSS-only confirmation
// modal the rollback flow uses.
//
// The count of 2 is what makes this mean anything — it pins the desktop grid
// row AND the mobile card. An assertion that only searched the whole body
// would be satisfied by the mobile card alone while the desktop cell was
// broken, exactly the trap TestPreviewsPage documents for the PHASE cell.
func TestPreviewsPageShowsTeardownControl(t *testing.T) {
	k := &fakeKube{namespaces: []kube.NamespaceInfo{
		nsInfo("preview-hae-cadence", "hae-cadence", "footstrike-api", "ready"),
	}}
	h, sess := newTestHandlers(t, k)
	rec := httptest.NewRecorder()
	h.Previews(rec, authed(t, "GET", "/previews", "", sess))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()

	if n := strings.Count(body, `href="#modal-preview-down-hae-cadence"`); n != 2 {
		t.Errorf("teardown trigger rendered %d times, want 2 (desktop row + mobile card)", n)
	}
	// Desktop: folded INTO the PREVIEW cell next to the tag it acts on. A
	// seventh grid column is not available — jobs-grid is a fixed six-column
	// layout shared with jobs.html.
	row := desktopRow(t, body, "hae-cadence")
	if !strings.Contains(row, `<span class="mono w6"><a href="/previews/hae-cadence" class="app-name">hae-cadence</a> <a href="#modal-preview-down-hae-cadence"`) {
		t.Errorf("desktop row = %q, want the teardown trigger inside the PREVIEW cell beside the tag link", row)
	}
	if n := countGridCells(row); n != 6 {
		t.Errorf("desktop row has %d top-level grid cells, want exactly 6 (jobs-grid is shared with jobs.html)", n)
	}
	// Mobile: a real full-size target rather than the desktop row's inline ✕
	// — the phone is the use case this whole feature exists for.
	if !strings.Contains(body, `<div class="card-actions"><a href="#modal-preview-down-hae-cadence" class="pill-block pill-ghost">`) {
		t.Error("mobile card is missing its teardown action")
	}
	// One modal for both triggers, so the two never fight over an element id.
	if n := strings.Count(body, `id="modal-preview-down-hae-cadence"`); n != 1 {
		t.Errorf("teardown modal rendered %d times, want exactly 1 shared by both triggers", n)
	}
	// The confirm is a <button> carrying the tag and the header token, not a
	// form: HTML forms cannot issue DELETE, and the endpoint will not read a
	// form-encoded csrf value.
	if !strings.Contains(body, `<button type="button" class="pill pill-primary js-preview-down" data-tag="hae-cadence" data-csrf="`+csrf(h, sess)+`">`) {
		t.Error("teardown confirm is not a js-preview-down button carrying the tag and CSRF token")
	}
	if !strings.Contains(body, `class="c-red js-preview-error"`) {
		t.Error("teardown modal has no slot for the server's 404/409 message")
	}
}

// TestPreviewsPageBusyRecordRendersWithoutTeardown covers the record
// assemblePreviews synthesizes for a tag the orchestrator holds that has NO
// namespace behind it (busyRecord): phase "busy" and almost nothing else — no
// branch, no apps, no URLs, a zero CreatedAt. It is the one record on this tab
// that isn't derived from a namespace, so it is the one that breaks a row
// renderer written against the normal case.
//
// It must also offer no teardown. DELETE /api/previews/{tag} looks the tag up
// through previewByTag, which deliberately does NOT synthesize, so it would
// answer 404: a control here could only ever fail, and offering one would be
// worse than offering none.
func TestPreviewsPageBusyRecordRendersWithoutTeardown(t *testing.T) {
	h, sess := newTestHandlers(t, &fakeKube{})
	h.Orch = &fakeOrchestration{busyTags: map[string]bool{"training-plans": true}}
	rec := httptest.NewRecorder()
	h.Previews(rec, authed(t, "GET", "/previews", "", sess))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()

	if strings.Contains(body, "<no value>") {
		t.Error("synthesized busy record rendered '<no value>' (a nil field reference)")
	}
	// No trigger on either surface, and no modal. Matching the bare tag
	// substring (rather than a scoped cell) is deliberate: it fails no matter
	// which of the three places the control leaked into.
	if strings.Contains(body, "modal-preview-down-training-plans") {
		t.Error("a busy tag with no namespace must not offer teardown — the DELETE would 404")
	}
	// The row itself is still a well-formed six-cell grid row, with the bare
	// tag in the PREVIEW cell and the claim in the PHASE cell.
	row := desktopRow(t, body, "training-plans")
	if n := countGridCells(row); n != 6 {
		t.Errorf("busy row has %d top-level grid cells, want exactly 6: %q", n, row)
	}
	if !strings.Contains(row, `<span class="mono w6"><a href="/previews/training-plans" class="app-name">training-plans</a></span>`) {
		t.Errorf("busy row = %q, want a PREVIEW cell holding only the tag link, with no teardown trigger", row)
	}
	if !strings.Contains(row, `<span class="c-mut">busy</span>`) {
		t.Errorf("busy row = %q, want the synthesized phase rendered", row)
	}
	// A zero CreatedAt renders no age, so the AGE cell must omit its tooltip
	// rather than emit an empty one (jobs.html's own idiom), and the mobile
	// card must not leave the separator that would have preceded the age.
	if strings.Contains(body, `title=""`) {
		t.Error("zero CreatedAt left an empty title attribute behind")
	}
	if strings.Contains(body, "· </div>") {
		t.Error("mobile card left a dangling separator where the empty age would be")
	}
	// The create form is still there: a tab showing only a busy tag is
	// exactly when someone wants to know what else they can do.
	if !strings.Contains(body, `<div id="modal-preview-create" class="modal">`) {
		t.Error("create form missing from a page whose only record is synthesized")
	}
}

// TestPreviewsPageFutureExpiryShowsRemainingTime covers a preview created
// with --ttl whose expiry hasn't arrived yet: the AGE cell must show the
// remaining time, not the absolute instant.
func TestPreviewsPageFutureExpiryShowsRemainingTime(t *testing.T) {
	ns := nsInfo("preview-x", "x", "footstrike-api", "ready")
	ns.Annotations["bifrost/expires-at"] = time.Now().Add(6*time.Hour + 5*time.Minute).UTC().Format(time.RFC3339)
	k := &fakeKube{namespaces: []kube.NamespaceInfo{ns}}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "GET", "/previews", "", sess)
	rec := httptest.NewRecorder()
	h.Previews(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	cell := desktopAgeCell(t, rec.Body.String(), "2026-07-27 12:00 UTC")
	if !strings.Contains(cell, "expires in 6h") {
		t.Errorf("AGE cell = %q, want it to show the remaining time", cell)
	}
}

// TestPreviewsPageNoExpiryRendersClean guards the common case — a preview
// created without --ttl, which never expires — the same way
// TestPreviewsPageCreatingWithoutStepRendersClean guards a stepless PHASE
// cell: no expiry text, and critically no dangling " · " separator, since
// that's the mistake a naive "if not zero, prepend a separator" template
// change would make even for the field it left empty.
func TestPreviewsPageNoExpiryRendersClean(t *testing.T) {
	k := &fakeKube{namespaces: []kube.NamespaceInfo{
		nsInfo("preview-x", "x", "footstrike-api", "ready"),
	}}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "GET", "/previews", "", sess)
	rec := httptest.NewRecorder()
	h.Previews(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	cell := desktopAgeCell(t, rec.Body.String(), "2026-07-27 12:00 UTC")
	if strings.Contains(cell, "expir") {
		t.Errorf("AGE cell = %q, want no expiry text for a preview with no TTL", cell)
	}
	if strings.Contains(cell, " · ") {
		t.Errorf("AGE cell = %q, want no dangling separator when there is no expiry", cell)
	}
}

// TestPreviewsPageShowsAutoUpdate covers the tab's whole auto-update surface,
// both directions, in one place — the second half being what makes the first
// mean anything.
//
// The marker is folded into the BRANCH cell rather than given a seventh
// jobs-grid column (that grid is a fixed six-column layout shared with
// jobs.html), so the assertion checks it renders INSIDE that cell —
// `<span class="mono">{branch}<span class="c-mut"` — and not merely somewhere
// on the page. A body-wide Contains would pass just as happily if the marker
// leaked into the tag cell or landed outside the grid entirely.
//
// The count of 2 pins both renderings: the desktop grid row and the mobile
// card's meta line. A marker added to only one of them would leave half the
// UI silently lying about whether a preview redeploys itself.
func TestPreviewsPageShowsAutoUpdate(t *testing.T) {
	auto := nsInfo("preview-auto", "auto-branch", "footstrike-api", "ready")
	auto.Annotations["bifrost/auto-update"] = "true"
	manual := nsInfo("preview-manual", "manual-branch", "footstrike-api", "ready")

	k := &fakeKube{namespaces: []kube.NamespaceInfo{auto, manual}}
	h, sess := newTestHandlers(t, k)
	rec := httptest.NewRecorder()
	h.Previews(rec, authed(t, "GET", "/previews", "", sess))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()

	if n := strings.Count(body, "· auto<"); n != 2 {
		t.Errorf("auto-update marker rendered %d times, want 2 (desktop row + mobile card)", n)
	}
	if !strings.Contains(body, `<span class="mono">auto-branch <span class="c-mut"`) {
		t.Error("auto-update marker is not inside the desktop BRANCH cell")
	}
	// The opted-out preview is the control: its branch cell must be exactly
	// what it was before this feature existed — no marker, and no dangling
	// separator, which is the mistake an unconditional " · auto" would make.
	if !strings.Contains(body, `<span class="mono">manual-branch</span>`) {
		t.Error("a preview that doesn't auto-update must render a bare BRANCH cell")
	}
}

// TestPreviewsPagePastDueExpiryReadsAsExpired covers a preview whose expiry
// has passed but the hourly sweep hasn't reclaimed it yet (up to an hour —
// see internal/preview's RunReaper): it must read as expired, not as a
// negative duration.
func TestPreviewsPagePastDueExpiryReadsAsExpired(t *testing.T) {
	ns := nsInfo("preview-x", "x", "footstrike-api", "ready")
	ns.Annotations["bifrost/expires-at"] = time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339)
	k := &fakeKube{namespaces: []kube.NamespaceInfo{ns}}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "GET", "/previews", "", sess)
	rec := httptest.NewRecorder()
	h.Previews(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	cell := desktopAgeCell(t, rec.Body.String(), "2026-07-27 12:00 UTC")
	if !strings.Contains(cell, "expired") {
		t.Errorf("AGE cell = %q, want a past-due expiry to read as expired", cell)
	}
	// Note the title attribute itself ("2026-07-27 12:00 UTC") has hyphens,
	// so this must check for a negative *duration* specifically, not "-"
	// anywhere in the cell.
	if strings.Contains(cell, "-30m") || strings.Contains(cell, "expires in -") {
		t.Errorf("AGE cell = %q, must not show a negative remaining time", cell)
	}
}

// TestPreviewsPageShowsFailedStepAndError covers the third phase in the
// contract: a failed preview keeps narrating its last step (task 1 retains
// it rather than clearing it) and additionally surfaces the error, in
// c-red — the same color class fleet.go already uses for a failed job's
// StateClass — so a failed row reads as "failed while building X" plus why.
func TestPreviewsPageShowsFailedStepAndError(t *testing.T) {
	failed := nsInfo("preview-x", "x", "footstrike-api", "failed")
	failed.Annotations["bifrost/step"] = "building footstrike-api (1/2)"
	failed.Annotations["bifrost/error"] = "build ended with status FAILURE"
	k := &fakeKube{namespaces: []kube.NamespaceInfo{failed}}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "GET", "/previews", "", sess)
	rec := httptest.NewRecorder()
	h.Previews(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "building footstrike-api (1/2)") {
		t.Error("failed preview must retain and show its last step")
	}
	if !strings.Contains(body, `<span class="c-red">build ended with status FAILURE</span>`) {
		t.Error("failed preview must show its error in c-red")
	}
}

// TestPreviewsPageDegradesOnKubeError mirrors TestPreviewsListJSONKubeError
// for the UI path: assemblePreviews failing must not 500 the whole dashboard
// tab — it degrades to the empty-state UI (matching how AppsSurvives*
// failures degrade elsewhere), and the raw error must never leak into the
// rendered page.
func TestPreviewsPageDegradesOnKubeError(t *testing.T) {
	k := &fakeKube{namespacesErr: errors.New("boom")}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "GET", "/previews", "", sess)
	rec := httptest.NewRecorder()
	h.Previews(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (degrade, not fail)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No preview environments") {
		t.Error("expected empty-state copy when assemblePreviews fails")
	}
	if strings.Contains(body, "boom") {
		t.Error("raw kube error must not leak into the rendered page")
	}
}

// TestPreviewsPageCreatingDrivesFastPoll pins the polling cadence half of
// the feature: base.html's JS picks ACTIVE (4s) over IDLE (30s) purely from
// a data-active attribute inside #tab-body, and dashboardVM's AnyActive is
// fleet-derived only (an app deploying/building, a job running) — a preview
// in "creating" contributes nothing to it. Without previewsPage ORing in a
// creating preview, this tab would poll every 30 seconds while narrating
// steps that each last a few seconds, and would usually display none of
// them. Both fixtures here run against an idle fleet (no app or job
// activity), so the attribute's presence can only come from the preview.
func TestPreviewsPageCreatingDrivesFastPoll(t *testing.T) {
	render := func(t *testing.T, namespaces []kube.NamespaceInfo) string {
		t.Helper()
		h, sess := newTestHandlers(t, &fakeKube{namespaces: namespaces})
		req := authed(t, "GET", "/previews", "", sess)
		rec := httptest.NewRecorder()
		h.Previews(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d", rec.Code)
		}
		return rec.Body.String()
	}

	t.Run("creating", func(t *testing.T) {
		creating := nsInfo("preview-x", "x", "footstrike-api", "creating")
		creating.Annotations["bifrost/step"] = "branching databases"
		body := render(t, []kube.NamespaceInfo{
			nsInfo("preview-hae-cadence", "hae-cadence", "footstrike-api", "ready"),
			creating,
		})
		if !strings.Contains(body, `<div class="previews" data-active>`) {
			t.Error("a creating preview must mark the tab data-active so it polls on the fast cadence")
		}
	})

	t.Run("ready only", func(t *testing.T) {
		body := render(t, []kube.NamespaceInfo{
			nsInfo("preview-hae-cadence", "hae-cadence", "footstrike-api", "ready"),
		})
		if !strings.Contains(body, `<div class="previews">`) {
			t.Error("a settled previews tab must render the container without data-active")
		}
		// Nothing else on an idle fleet should carry the attribute either —
		// this is what makes the positive case above attributable to the
		// creating preview rather than to page chrome. The leading space
		// matters: a bare "data-active" substring also matches base.html's
		// polling JS ('data-active' / '[data-active]'), which is on every
		// page, so it would false-positive here.
		if strings.Contains(body, " data-active") {
			t.Error("no preview is in flight and the fleet is idle: nothing should be data-active")
		}
	})
}

func TestPreviewsFragment(t *testing.T) {
	k := &fakeKube{namespaces: []kube.NamespaceInfo{
		nsInfo("preview-hae-cadence", "hae-cadence", "foo", "ready"),
	}}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "GET", "/partial/previews", "", sess)
	rec := httptest.NewRecorder()
	h.PreviewsFragment(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hae-cadence") {
		t.Error("preview tag missing from fragment")
	}
	// No page chrome: a poll response is swapped into #tab-body verbatim.
	if strings.Contains(body, "<!DOCTYPE") || strings.Contains(body, "Sign out") {
		t.Error("fragment should not include full-page chrome")
	}
}

// --- per-preview detail page --------------------------------------------------

// detailPage renders GET /previews/{tag}. The path value is set explicitly
// because these tests call the handler directly rather than through the mux
// pattern that would otherwise populate it.
func detailPage(t *testing.T, h *Handlers, sess *auth.Session, tag string) string {
	t.Helper()
	req := authed(t, "GET", "/previews/"+tag, "", sess)
	req.SetPathValue("tag", tag)
	rec := httptest.NewRecorder()
	h.Preview(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// TestPreviewDetailPageShowsEverythingWithURLsProminent is the page's reason
// to exist: the tab can only afford one cramped APPS cell for a preview's
// URLs, and the URLs are the thing anyone actually wants from a preview.
//
// The assertion on each URL pins the FULL anchor — href, target and the URL
// itself as the visible link text — because a name-only link (which is all
// the tab's preview-apps block renders) would satisfy a looser "is the URL on
// the page somewhere" check while leaving the URL itself unreadable and
// uncopyable, i.e. leaving the page pointless.
func TestPreviewDetailPageShowsEverythingWithURLsProminent(t *testing.T) {
	ns := nsInfo("preview-hae-cadence", "hae-cadence", "footstrike-api,footstrike-dashboard", "ready")
	ns.Annotations["bifrost/auto-update"] = "true"
	ns.Annotations["bifrost/expires-at"] = time.Now().Add(6*time.Hour + 5*time.Minute).UTC().Format(time.RFC3339)
	h, sess := newTestHandlers(t, &fakeKube{namespaces: []kube.NamespaceInfo{ns}})

	body := detailPage(t, h, sess, "hae-cadence")

	for _, app := range []string{"footstrike-api", "footstrike-dashboard"} {
		u := "https://" + app + "-hae-cadence.preview.footstrike.run"
		want := `<a href="` + u + `" target="_blank" rel="noopener" class="app-name">` + u + `</a>`
		if !strings.Contains(body, want) {
			t.Errorf("%s's URL is not rendered as a prominent link (want %s)", app, want)
		}
		// Each URL is a labelled tile, not a run-together list.
		if !strings.Contains(body, `<div class="tile-label">`+app+`</div>`) {
			t.Errorf("%s has no labelled URL tile", app)
		}
	}

	// Every remaining field of previewRecord, plus the page chrome that makes
	// this a real dashboard page rather than a bare fragment.
	for _, want := range []string{
		`<div class="modal-title mono">hae-cadence</div>`,
		`<span>Branch</span><span class="mono c-ink">hae-cadence</span>`,
		`<span>Phase</span><span class="mono c-ink">ready</span>`,
		`<span>Health</span><span class="mono c-ink">unknown</span>`,
		`<span>Auto-update</span><span class="mono c-ink">on</span>`,
		`title="2026-07-27 12:00 UTC"`, // created-at, as the tab renders it
		"expires in 6h",                // expiry, via the shared preview-expiry block
		`<a href="/previews" class="jobs-link">`,
		`<a href="/previews" class="tab active">Previews`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail page is missing %s", want)
		}
	}
	if strings.Contains(body, "<no value>") {
		t.Error("detail page rendered '<no value>' (a nil field reference)")
	}
}

// TestPreviewDetailPageCreatingNarratesStepAndPollsFast is the live-progress
// half of the feature. base.html's poller drops from 30s to 4s purely because
// a [data-active] sits inside #tab-body, and re-fetches whatever <body>'s
// data-refresh points at — so a per-tag fragment URL plus that attribute is
// the entire mechanism by which this page narrates `ib preview up` live.
//
// The settled subtest is what makes the active one mean anything: on an idle
// fleet nothing else may carry the attribute. The leading space matters —
// base.html's polling JS mentions 'data-active' and '[data-active]' on every
// page, so a bare substring would false-positive.
func TestPreviewDetailPageCreatingNarratesStepAndPollsFast(t *testing.T) {
	t.Run("creating", func(t *testing.T) {
		ns := nsInfo("preview-x", "x", "footstrike-api", "creating")
		ns.Annotations["bifrost/step"] = "branching databases"
		ns.Annotations["bifrost/step-since"] = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
		h, sess := newTestHandlers(t, &fakeKube{namespaces: []kube.NamespaceInfo{ns}})

		body := detailPage(t, h, sess, "x")

		if !strings.Contains(body, "creating · branching databases") {
			t.Error("the current step is not narrated beside the phase")
		}
		if !strings.Contains(body, "· running 2m") {
			t.Error("the step does not report how long it has been running")
		}
		if !strings.Contains(body, `<div class="preview-detail" data-active>`) {
			t.Error("a creating preview must mark the page data-active so it polls on the fast cadence")
		}
		if !strings.Contains(body, `data-refresh="/partial/previews/x"`) {
			t.Error("the page must poll its own per-tag fragment, not the whole Previews tab")
		}
	})

	t.Run("settled", func(t *testing.T) {
		h, sess := newTestHandlers(t, &fakeKube{namespaces: []kube.NamespaceInfo{
			nsInfo("preview-x", "x", "footstrike-api", "ready"),
		}})

		body := detailPage(t, h, sess, "x")

		if !strings.Contains(body, `<div class="preview-detail">`) {
			t.Error("a settled preview must render the container without data-active")
		}
		if strings.Contains(body, " data-active") {
			t.Error("nothing is in flight and the fleet is idle: nothing should be data-active")
		}
		// No step, so no elapsed time and no dangling separator either.
		if strings.Contains(body, "· running") {
			t.Error("a ready preview has no running step and must not claim one")
		}
	})
}

// TestPreviewDetailPageFailedShowsError covers the third phase: a failed
// preview keeps its last step (recordFromNamespace retains it deliberately)
// and surfaces bifrost/error in c-red, so the page answers "failed doing
// what, and why" rather than just "failed".
func TestPreviewDetailPageFailedShowsError(t *testing.T) {
	ns := nsInfo("preview-x", "x", "footstrike-api", "failed")
	ns.Annotations["bifrost/step"] = "building footstrike-api (1/2)"
	ns.Annotations["bifrost/error"] = "build ended with status FAILURE"
	h, sess := newTestHandlers(t, &fakeKube{namespaces: []kube.NamespaceInfo{ns}})

	body := detailPage(t, h, sess, "x")

	if !strings.Contains(body, `<span class="c-red">build ended with status FAILURE</span>`) {
		t.Error("a failed preview must show its error in c-red")
	}
	if !strings.Contains(body, "failed · building footstrike-api (1/2)") {
		t.Error("a failed preview must still narrate the step it died on")
	}
	if !strings.Contains(body, `<span class="dot dot-red"></span>`) {
		t.Error("a failed preview must read as failed at a glance, not as amber in-progress")
	}
}

// TestPreviewDetailPageBusyTagClickThrough is the edge the list creates and
// the API deliberately refuses to smooth over. A tag can be claimed with no
// namespace behind it (an Up before EnsureNamespace, a Down still deleting
// Neon branches); assemblePreviews synthesizes a row for it so the state is
// visible, but previewByTag still 404s — DeletePreviewJSON depends on that
// lookup meaning "there is a namespace". So the user can click a row and land
// on a tag the by-tag read says doesn't exist.
//
// The page must say what is actually happening, and keep itself data-active so
// it turns into the real preview on its own.
func TestPreviewDetailPageBusyTagClickThrough(t *testing.T) {
	h, sess := newTestHandlers(t, &fakeKube{})
	h.Orch = &fakeOrchestration{busyTags: map[string]bool{"training-plans": true}}

	body := detailPage(t, h, sess, "training-plans")

	if !strings.Contains(body, "This preview is mid-operation") {
		t.Error("a claimed tag with no namespace must be explained, not left looking broken")
	}
	if strings.Contains(body, "No preview environment with this tag") {
		t.Error("a tag the list just showed must not read as nonexistent")
	}
	if !strings.Contains(body, `<div class="preview-detail" data-active>`) {
		t.Error("a mid-operation tag must poll fast so the page becomes the real preview on its own")
	}
	if strings.Contains(body, "<no value>") {
		t.Error("the synthesized record rendered '<no value>' (a nil field reference)")
	}
	// Nothing is known beyond the claim, so nothing may be invented: no empty
	// URL tiles, no 0001-01-01 creation time, no blank tooltip.
	if strings.Contains(body, `class="tile-label"`) {
		t.Error("a tag with no namespace has no URLs and must not render an empty URL block")
	}
	if strings.Contains(body, "0001-01-01") || strings.Contains(body, `title=""`) {
		t.Error("a zero CreatedAt leaked into the page")
	}
}

// TestPreviewDetailPageUnknownTag covers the two ways there is nothing to
// show, which must not be conflated: a tag that genuinely isn't a preview,
// and a cluster read that failed so nobody knows whether it is. The first is
// a 404 with a real page around it; the second must not claim the preview is
// gone, and must not leak the raw error.
func TestPreviewDetailPageUnknownTag(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		h, sess := newTestHandlers(t, &fakeKube{})
		req := authed(t, "GET", "/previews/nope", "", sess)
		req.SetPathValue("tag", "nope")
		rec := httptest.NewRecorder()
		h.Preview(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404 for a tag that is not a preview", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "No preview environment with this tag") {
			t.Error("an unknown tag must get a clean not-found page")
		}
		// A real page, not an empty shell: full chrome plus a way back.
		for _, want := range []string{
			`<div class="brand">Bifrost</div>`,
			`<a href="/previews" class="tab active">Previews`,
			`<div class="card-actions"><a href="/previews" class="pill pill-ghost">All previews</a></div>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("not-found page is missing %s", want)
			}
		}
		if strings.Contains(body, "<no value>") {
			t.Error("not-found page rendered '<no value>'")
		}
		if strings.Contains(body, `class="tile-label"`) || strings.Contains(body, "<span>Branch</span>") {
			t.Error("not-found page rendered the detail blocks for a preview that does not exist")
		}
	})

	t.Run("cluster read failed", func(t *testing.T) {
		h, sess := newTestHandlers(t, &fakeKube{namespacesErr: errors.New("boom")})
		req := authed(t, "GET", "/previews/x", "", sess)
		req.SetPathValue("tag", "x")
		rec := httptest.NewRecorder()
		h.Preview(rec, req)

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("code = %d, want 502 — the read failed, so whether the preview exists is unknown", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "Could not read this preview from the cluster") {
			t.Error("a failed read must say so")
		}
		if strings.Contains(body, "No preview environment with this tag") {
			t.Error("a failed read must not be reported as a preview that doesn't exist")
		}
		if strings.Contains(body, "boom") {
			t.Error("raw kube error leaked into the rendered page")
		}
	})
}

// TestPreviewDetailPageZeroCreatedAt pins the tab's own idiom on this page: a
// record with no creation time omits the title attribute entirely rather than
// rendering the zero instant, and the value falls back to an em dash so the
// DETAILS row is never blank.
func TestPreviewDetailPageZeroCreatedAt(t *testing.T) {
	ns := nsInfo("preview-x", "x", "footstrike-api", "ready")
	ns.CreatedAt = time.Time{}
	h, sess := newTestHandlers(t, &fakeKube{namespaces: []kube.NamespaceInfo{ns}})

	body := detailPage(t, h, sess, "x")

	if strings.Contains(body, "0001-01-01") {
		t.Error("a zero CreatedAt rendered as 0001-01-01")
	}
	if strings.Contains(body, `title=""`) {
		t.Error("a zero CreatedAt left an empty title attribute behind")
	}
	if !strings.Contains(body, `<span>Created</span><span class="mono c-ink">—</span>`) {
		t.Error("a zero CreatedAt must leave a dashed row, not a blank one")
	}
}

// TestPreviewsTabLinksRowsToTheDetailPage pins the way in. The count of 2 is
// what makes it mean anything: it pins the desktop grid row AND the mobile
// card, and a body-wide check alone would be satisfied by the mobile card
// while the desktop cell was broken — the trap TestPreviewsPage documents for
// the PHASE cell.
//
// The link is folded INTO the existing PREVIEW cell, so the desktop row still
// renders exactly six top-level grid children: jobs-grid is a fixed
// six-column layout shared with jobs.html, and a seventh would misalign both
// tabs.
func TestPreviewsTabLinksRowsToTheDetailPage(t *testing.T) {
	h, sess := newTestHandlers(t, &fakeKube{namespaces: []kube.NamespaceInfo{
		nsInfo("preview-hae-cadence", "hae-cadence", "footstrike-api", "ready"),
	}})
	rec := httptest.NewRecorder()
	h.Previews(rec, authed(t, "GET", "/previews", "", sess))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()

	if n := strings.Count(body, `<a href="/previews/hae-cadence" class="app-name">hae-cadence</a>`); n != 2 {
		t.Errorf("tag link rendered %d times, want 2 (desktop row + mobile card)", n)
	}
	row := desktopRow(t, body, "hae-cadence")
	if n := countGridCells(row); n != 6 {
		t.Errorf("desktop row has %d top-level grid cells, want exactly 6 (jobs-grid is shared with jobs.html): %q", n, row)
	}
	if !strings.Contains(body, `<div class="job-card-top"><span class="mono w6"><a href="/previews/hae-cadence" class="app-name">hae-cadence</a></span>`) {
		t.Error("the mobile card's top line does not link the tag to its detail page")
	}
}

// TestPreviewDetailFragmentIsBodyOnlyAndAlwaysOK covers the polled half.
//
// The fragment answers 200 even for a tag with nothing behind it, unlike the
// full page's 404. base.html's refresh() treats a non-ok response as a failed
// poll — it marks the header stale and swaps nothing — so a preview torn down
// while its page is open would freeze on stale content instead of turning
// into the not-found body.
func TestPreviewDetailFragmentIsBodyOnlyAndAlwaysOK(t *testing.T) {
	fragment := func(t *testing.T, h *Handlers, sess *auth.Session, tag string) *httptest.ResponseRecorder {
		t.Helper()
		req := authed(t, "GET", "/partial/previews/"+tag, "", sess)
		req.SetPathValue("tag", tag)
		rec := httptest.NewRecorder()
		h.PreviewFragment(rec, req)
		return rec
	}

	h, sess := newTestHandlers(t, &fakeKube{namespaces: []kube.NamespaceInfo{
		nsInfo("preview-hae-cadence", "hae-cadence", "footstrike-api", "ready"),
	}})

	rec := fragment(t, h, sess, "hae-cadence")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "https://footstrike-api-hae-cadence.preview.footstrike.run") {
		t.Error("fragment is missing the preview's URLs")
	}
	// No page chrome: the response is swapped into #tab-body verbatim.
	if strings.Contains(body, "<!DOCTYPE") || strings.Contains(body, "Sign out") {
		t.Error("fragment should not include full-page chrome")
	}

	gone := fragment(t, h, sess, "torn-down")
	if gone.Code != http.StatusOK {
		t.Fatalf("gone fragment code = %d, want 200 so the poll swaps rather than going stale", gone.Code)
	}
	if !strings.Contains(gone.Body.String(), "No preview environment with this tag") {
		t.Error("a preview torn down while its page is open must swap to the not-found body")
	}
}

// TestPreviewsNavBadgeConditional asserts the cheap path: other tabs never
// call assemblePreviews (an extra namespace list) just to size the Previews
// nav badge, so PreviewCount is the zero value there and the badge span must
// not render at all (vs. AppCount/JobCount, which always render their span).
func TestPreviewsNavBadgeConditional(t *testing.T) {
	k := &fakeKube{namespaces: []kube.NamespaceInfo{
		nsInfo("preview-hae-cadence", "hae-cadence", "foo", "ready"),
	}}
	h, sess := newTestHandlers(t, k)
	req := authed(t, "GET", "/apps", "", sess)
	rec := httptest.NewRecorder()
	h.Apps(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `href="/previews"`) {
		t.Error("previews nav link missing")
	}
	if strings.Contains(body, `Previews<span class="tab-count">`) {
		t.Error("previews nav badge should not render on other tabs (would require an extra namespace list)")
	}
}
