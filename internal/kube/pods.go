package kube

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

type client struct {
	typed  kubernetes.Interface
	dyn    dynamic.Interface
	argoNS string
}

func (c *client) ListPodImages(ctx context.Context, namespace string) ([]string, error) {
	pods, err := c.typed.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var out []string
	for _, p := range pods.Items {
		for _, ctr := range p.Spec.Containers {
			if _, ok := seen[ctr.Image]; !ok {
				seen[ctr.Image] = struct{}{}
				out = append(out, ctr.Image)
			}
		}
	}
	return out, nil
}
