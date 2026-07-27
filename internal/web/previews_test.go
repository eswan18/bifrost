package web

import (
	"context"
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
