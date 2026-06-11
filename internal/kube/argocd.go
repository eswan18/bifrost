package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var argocdAppGVR = schema.GroupVersionResource{
	Group: "argoproj.io", Version: "v1alpha1", Resource: "applications",
}

// AppStatus is ArgoCD's own view of an Application. Fields are "" when the
// Application hasn't reported status yet (e.g. just created).
type AppStatus struct {
	SyncStatus   string // "Synced", "OutOfSync"
	HealthStatus string // "Healthy", "Progressing", "Degraded", "Missing", "Suspended"
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
	return AppStatus{SyncStatus: sync, HealthStatus: health}
}

// PatchProdImage mirrors `ib.py:240-261`: merge-patches the ArgoCD
// Application <app>-prod, setting spec.source.kustomize.images to
// "<image-base>=<full-image>".
func (c *client) PatchProdImage(ctx context.Context, app, image string) error {
	imageBase := image
	if i := strings.LastIndex(image, ":"); i >= 0 {
		imageBase = image[:i]
	}
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
		Patch(ctx, app+"-prod", types.MergePatchType, b, metav1.PatchOptions{})
	return err
}
