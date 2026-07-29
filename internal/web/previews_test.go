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
	// no step decoration, no stray separator, nothing tacked on.
	if !strings.Contains(body, `<span class="c-mut">ready</span>`) {
		t.Error("ready preview must render its bare phase with no step")
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
