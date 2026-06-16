package kube

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
)

func TestAppStatusFrom(t *testing.T) {
	cases := []struct {
		name string
		obj  map[string]any
		want AppStatus
	}{
		{
			"full status",
			map[string]any{"status": map[string]any{
				"sync":   map[string]any{"status": "Synced"},
				"health": map[string]any{"status": "Healthy"},
			}},
			AppStatus{SyncStatus: "Synced", HealthStatus: "Healthy"},
		},
		{
			"missing health",
			map[string]any{"status": map[string]any{
				"sync": map[string]any{"status": "OutOfSync"},
			}},
			AppStatus{SyncStatus: "OutOfSync"},
		},
		{"no status at all", map[string]any{"spec": map[string]any{}}, AppStatus{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := appStatusFrom(tc.obj); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestAppStatusDeployedAt: the deploy time is the newest entry in the
// Application's sync history (when the running revision actually went live),
// falling back to the last sync operation's finish time for apps that have
// synced but recorded no history yet, and the zero time when neither exists.
func TestAppStatusDeployedAt(t *testing.T) {
	rfc := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return ts
	}
	cases := []struct {
		name string
		obj  map[string]any
		want time.Time
	}{
		{
			"latest history entry wins",
			map[string]any{"status": map[string]any{
				"history": []any{
					map[string]any{"deployedAt": "2026-06-14T21:42:50Z"},
					map[string]any{"deployedAt": "2026-06-14T21:47:29Z"},
				},
			}},
			rfc("2026-06-14T21:47:29Z"),
		},
		{
			"newest is chosen regardless of array order",
			map[string]any{"status": map[string]any{
				"history": []any{
					map[string]any{"deployedAt": "2026-06-14T21:47:29Z"},
					map[string]any{"deployedAt": "2026-06-14T21:42:50Z"},
				},
			}},
			rfc("2026-06-14T21:47:29Z"),
		},
		{
			"falls back to operationState when history is empty",
			map[string]any{"status": map[string]any{
				"operationState": map[string]any{"finishedAt": "2026-06-10T08:00:00Z"},
			}},
			rfc("2026-06-10T08:00:00Z"),
		},
		{
			"history beats the operationState fallback",
			map[string]any{"status": map[string]any{
				"history":        []any{map[string]any{"deployedAt": "2026-06-14T21:47:29Z"}},
				"operationState": map[string]any{"finishedAt": "2026-06-10T08:00:00Z"},
			}},
			rfc("2026-06-14T21:47:29Z"),
		},
		{
			"unparseable history entries are skipped, fallback used",
			map[string]any{"status": map[string]any{
				"history":        []any{map[string]any{"deployedAt": "not-a-time"}},
				"operationState": map[string]any{"finishedAt": "2026-06-10T08:00:00Z"},
			}},
			rfc("2026-06-10T08:00:00Z"),
		},
		{"no timestamps at all", map[string]any{"status": map[string]any{}}, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := appStatusFrom(tc.obj).DeployedAt; !got.Equal(tc.want) {
				t.Errorf("DeployedAt = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListArgoApps(t *testing.T) {
	gvr := schema.GroupVersionResource{
		Group: "argoproj.io", Version: "v1alpha1", Resource: "applications",
	}
	app := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"sync":   map[string]any{"status": "Synced"},
			"health": map[string]any{"status": "Progressing"},
		},
	}}
	app.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "argoproj.io", Version: "v1alpha1", Kind: "Application",
	})
	app.SetNamespace("argocd")
	app.SetName("foo-staging")

	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "ApplicationList"},
		app,
	)
	c := &client{dyn: dyn, argoNS: "argocd"}

	got, err := c.ListArgoApps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := AppStatus{SyncStatus: "Synced", HealthStatus: "Progressing"}
	if got["foo-staging"] != want {
		t.Errorf("foo-staging = %+v, want %+v", got["foo-staging"], want)
	}
}

func TestPatchProdImage(t *testing.T) {
	gvr := schema.GroupVersionResource{
		Group: "argoproj.io", Version: "v1alpha1", Resource: "applications",
	}
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "argoproj.io", Version: "v1alpha1", Kind: "Application",
	})
	app.SetNamespace("argocd")
	app.SetName("foo-prod")

	scheme := runtime.NewScheme()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{gvr: "ApplicationList"},
		app,
	)
	c := &client{dyn: dyn, argoNS: "argocd"}

	err := c.PatchProdImage(context.Background(), "foo",
		"reg/foo:abc1234-prod")
	if err != nil {
		t.Fatal(err)
	}

	got, err := dyn.Resource(gvr).Namespace("argocd").
		Get(context.Background(), "foo-prod", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	imgs, _, err := unstructured.NestedStringSlice(
		got.Object, "spec", "source", "kustomize", "images")
	if err != nil {
		t.Fatal(err)
	}
	want := "reg/foo=reg/foo:abc1234-prod"
	if len(imgs) != 1 || imgs[0] != want {
		out, _ := json.Marshal(got.Object)
		t.Errorf("images = %v, want [%q]; full obj = %s", imgs, want, out)
	}
}
