package kube

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
)

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
