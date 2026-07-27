package kube

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
)

// NamespaceInfo is the slice of namespace state the preview control plane
// reads: a preview's record lives in its namespace's labels/annotations.
type NamespaceInfo struct {
	Name        string
	Labels      map[string]string
	Annotations map[string]string
	CreatedAt   time.Time
	Phase       string // "Active" | "Terminating"
}

func (c *client) ListNamespaces(ctx context.Context, labelSelector string) ([]NamespaceInfo, error) {
	list, err := c.typed.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}
	out := make([]NamespaceInfo, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, namespaceInfo(&list.Items[i]))
	}
	return out, nil
}

func (c *client) GetNamespace(ctx context.Context, name string) (NamespaceInfo, bool, error) {
	ns, err := c.typed.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return NamespaceInfo{}, false, nil
	}
	if err != nil {
		return NamespaceInfo{}, false, err
	}
	return namespaceInfo(ns), true, nil
}

func namespaceInfo(ns *corev1.Namespace) NamespaceInfo {
	return NamespaceInfo{
		Name:        ns.Name,
		Labels:      ns.Labels,
		Annotations: ns.Annotations,
		CreatedAt:   ns.CreationTimestamp.Time,
		Phase:       string(ns.Status.Phase),
	}
}
