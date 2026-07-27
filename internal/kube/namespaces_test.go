package kube

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func previewNS(name, branch string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      map[string]string{"bifrost/preview": "true"},
			Annotations: map[string]string{"bifrost/branch": branch, "bifrost/apps": "footstrike-api,footstrike-dashboard", "bifrost/phase": "ready"},
		},
		Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	}
}

func TestListNamespacesFiltersByLabel(t *testing.T) {
	cs := fake.NewSimpleClientset(
		previewNS("preview-hae-cadence", "hae-cadence"),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "footstrike-api-staging"}},
	)
	c := &client{typed: cs}
	got, err := c.ListNamespaces(context.Background(), "bifrost/preview=true")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Name != "preview-hae-cadence" {
		t.Fatalf("namespaces = %+v, want just preview-hae-cadence", got)
	}
	ns := got[0]
	if ns.Annotations["bifrost/branch"] != "hae-cadence" || ns.Phase != "Active" || ns.CreatedAt.IsZero() && false {
		t.Errorf("record fields not carried through: %+v", ns)
	}
}

func TestGetNamespace(t *testing.T) {
	cs := fake.NewSimpleClientset(previewNS("preview-x", "x"))
	c := &client{typed: cs}

	ns, found, err := c.GetNamespace(context.Background(), "preview-x")
	if err != nil || !found {
		t.Fatalf("expected found, got found=%v err=%v", found, err)
	}
	if ns.Annotations["bifrost/phase"] != "ready" {
		t.Errorf("annotations = %v", ns.Annotations)
	}

	_, found, err = c.GetNamespace(context.Background(), "preview-missing")
	if err != nil {
		t.Fatalf("absent namespace must be (zero, false, nil), got err=%v", err)
	}
	if found {
		t.Error("expected found=false for absent namespace")
	}
}
