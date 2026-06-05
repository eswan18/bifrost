package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var argocdAppGVR = schema.GroupVersionResource{
	Group: "argoproj.io", Version: "v1alpha1", Resource: "applications",
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
