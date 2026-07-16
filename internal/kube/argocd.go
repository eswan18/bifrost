package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/eswan18/bifrost/internal/promote"
)

var argocdAppGVR = schema.GroupVersionResource{
	Group: "argoproj.io", Version: "v1alpha1", Resource: "applications",
}

// AppStatus is ArgoCD's own view of an Application. String fields are "" when
// the Application hasn't reported status yet (e.g. just created); DeployedAt
// is the zero time in that case.
type AppStatus struct {
	SyncStatus   string // "Synced", "OutOfSync"
	HealthStatus string // "Healthy", "Progressing", "Degraded", "Missing", "Suspended"
	// DeployedAt is when the currently-running revision went live — the newest
	// entry in the Application's sync history. Zero when unknown.
	DeployedAt time.Time
}

// ListArgoApps returns the status of every Application in the argocd
// namespace, keyed by name (e.g. "fitness-api-prod"). One List call covers
// all services and both environments.
func (c *client) ListArgoApps(ctx context.Context) (map[string]AppStatus, error) {
	list, err := c.dyn.Resource(argocdAppGVR).Namespace(c.argoNS).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make(map[string]AppStatus, len(list.Items))
	for _, item := range list.Items {
		out[item.GetName()] = appStatusFrom(item.Object)
	}
	return out, nil
}

func appStatusFrom(obj map[string]any) AppStatus {
	sync, _, _ := unstructured.NestedString(obj, "status", "sync", "status")
	health, _, _ := unstructured.NestedString(obj, "status", "health", "status")
	return AppStatus{
		SyncStatus:   sync,
		HealthStatus: health,
		DeployedAt:   lastDeployedAt(obj),
	}
}

// lastDeployedAt returns when the running revision was deployed. It scans
// status.history (each entry carries a deployedAt) for the newest timestamp
// rather than trusting array order, since "newest deploy" is exactly what we
// want and is robust to any reordering. ArgoCD appends a history entry on each
// sync that changes the deployed image, so this tracks image promotions, not
// just git-revision changes. When history is absent (an app that has synced
// but not recorded history yet) it falls back to the last sync operation's
// finish time, and the zero time when neither is present.
func lastDeployedAt(obj map[string]any) time.Time {
	var latest time.Time
	history, _, _ := unstructured.NestedSlice(obj, "status", "history")
	for _, h := range history {
		entry, ok := h.(map[string]any)
		if !ok {
			continue
		}
		ts, _ := entry["deployedAt"].(string)
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			continue
		}
		if t.After(latest) {
			latest = t
		}
	}
	if !latest.IsZero() {
		return latest
	}
	if ts, _, _ := unstructured.NestedString(obj, "status", "operationState", "finishedAt"); ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			return t
		}
	}
	return time.Time{}
}

// PatchAppImage mirrors `ib.py:240-261`: merge-patches the ArgoCD
// Application <app>-<env>, setting spec.source.kustomize.images to
// "<image-base>=<full-image>". Note: image-updater owns the staging override
// and re-pins it to the newest build on its next cycle, so a staging patch is
// temporary by design.
func (c *client) PatchAppImage(ctx context.Context, app, env, image string) error {
	imageBase := promote.ImageBase(image)
	patch := map[string]any{
		"spec": map[string]any{
			"source": map[string]any{
				"kustomize": map[string]any{
					"images": []string{fmt.Sprintf("%s=%s", imageBase, image)},
				},
			},
		},
	}
	b, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = c.dyn.Resource(argocdAppGVR).Namespace(c.argoNS).
		Patch(ctx, app+"-"+env, types.MergePatchType, b, metav1.PatchOptions{})
	return err
}
