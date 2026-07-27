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
}

// --- UI tab ------------------------------------------------------------------

func TestPreviewsPage(t *testing.T) {
	k := &fakeKube{namespaces: []kube.NamespaceInfo{
		nsInfo("preview-hae-cadence", "hae-cadence", "foo", "ready"),
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
	// One preview: the nav badge should render its count.
	if !strings.Contains(body, `Previews<span class="tab-count">1</span>`) {
		t.Error("nav badge should show PreviewCount when non-zero")
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
