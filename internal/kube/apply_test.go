package kube

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/restmapper"
)

// --- EnsureNamespace ---------------------------------------------------

func TestEnsureNamespaceCreatesWhenAbsent(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := &client{typed: cs}

	err := c.EnsureNamespace(context.Background(), "preview-x",
		map[string]string{"bifrost/preview": "true"},
		map[string]string{"bifrost/branch": "x", "bifrost/phase": "creating"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ns, err := cs.CoreV1().Namespaces().Get(context.Background(), "preview-x", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("namespace not created: %v", err)
	}
	if ns.Labels["bifrost/preview"] != "true" {
		t.Errorf("labels = %v", ns.Labels)
	}
	if ns.Annotations["bifrost/branch"] != "x" || ns.Annotations["bifrost/phase"] != "creating" {
		t.Errorf("annotations = %v", ns.Annotations)
	}
}

func TestEnsureNamespaceMergesOntoExisting(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "preview-x",
			Labels:      map[string]string{"other-label": "keep"},
			Annotations: map[string]string{"bifrost/phase": "creating", "other-annotation": "keep"},
		},
	})
	c := &client{typed: cs}

	err := c.EnsureNamespace(context.Background(), "preview-x",
		map[string]string{"bifrost/preview": "true"},
		map[string]string{"bifrost/phase": "ready"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ns, err := cs.CoreV1().Namespaces().Get(context.Background(), "preview-x", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ns.Labels["other-label"] != "keep" || ns.Labels["bifrost/preview"] != "true" {
		t.Errorf("labels not merged: %v", ns.Labels)
	}
	if ns.Annotations["other-annotation"] != "keep" {
		t.Errorf("existing annotation dropped: %v", ns.Annotations)
	}
	if ns.Annotations["bifrost/phase"] != "ready" {
		t.Errorf("overlapping annotation not overwritten: %v", ns.Annotations)
	}
}

// --- AnnotateNamespace ---------------------------------------------------

func TestAnnotateNamespaceMergesOntoExisting(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "preview-x",
			Labels:      map[string]string{"bifrost/preview": "true"},
			Annotations: map[string]string{"bifrost/phase": "creating", "bifrost/branch": "x"},
		},
	})
	c := &client{typed: cs}

	err := c.AnnotateNamespace(context.Background(), "preview-x",
		map[string]string{"bifrost/phase": "ready"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ns, err := cs.CoreV1().Namespaces().Get(context.Background(), "preview-x", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ns.Annotations["bifrost/phase"] != "ready" {
		t.Errorf("annotation not updated: %v", ns.Annotations)
	}
	if ns.Annotations["bifrost/branch"] != "x" {
		t.Errorf("unrelated annotation dropped: %v", ns.Annotations)
	}
	if ns.Labels["bifrost/preview"] != "true" {
		t.Errorf("labels should be untouched: %v", ns.Labels)
	}
}

func TestAnnotateNamespaceAbsentIsError(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := &client{typed: cs}

	err := c.AnnotateNamespace(context.Background(), "preview-missing", map[string]string{"a": "b"})
	if err == nil {
		t.Fatal("expected error annotating an absent namespace")
	}
}

// --- DeleteNamespace ---------------------------------------------------

func TestDeleteNamespaceDeletesExisting(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "preview-x"}})
	c := &client{typed: cs}

	if err := c.DeleteNamespace(context.Background(), "preview-x"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err := cs.CoreV1().Namespaces().Get(context.Background(), "preview-x", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected namespace to be gone, get err = %v", err)
	}
}

func TestDeleteNamespaceAbsentIsNotError(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := &client{typed: cs}

	if err := c.DeleteNamespace(context.Background(), "preview-missing"); err != nil {
		t.Errorf("absent namespace must not be an error, got %v", err)
	}
}

// --- CopySecret ---------------------------------------------------

func TestCopySecretCreatesWithOverridesAndKeepSource(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "foo-staging", Name: "foo-staging-secrets"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"DATABASE_URL": []byte("staging-uri"), "API_KEY": []byte("shared-key")},
	})
	c := &client{typed: cs}

	err := c.CopySecret(context.Background(), "foo-staging", "foo-staging-secrets",
		"preview-x", "foo-preview-secrets",
		map[string][]byte{"DATABASE_URL": []byte("preview-uri"), "API_KEY": nil})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dst, err := cs.CoreV1().Secrets("preview-x").Get(context.Background(), "foo-preview-secrets", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("dest secret not created: %v", err)
	}
	if string(dst.Data["DATABASE_URL"]) != "preview-uri" {
		t.Errorf("DATABASE_URL = %q, want override applied", dst.Data["DATABASE_URL"])
	}
	if string(dst.Data["API_KEY"]) != "shared-key" {
		t.Errorf("API_KEY = %q, want source value kept (nil override)", dst.Data["API_KEY"])
	}
}

// CopySecret's destination data is a full copy-plus-overrides of the source,
// not a merge with whatever the destination previously held — a stale key
// left over from an old preview run must not survive a re-copy.
func TestCopySecretUpdateFullyReplacesDestinationData(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "foo-staging", Name: "foo-staging-secrets"},
			Data:       map[string][]byte{"DATABASE_URL": []byte("staging-uri")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "preview-x", Name: "foo-preview-secrets"},
			Data:       map[string][]byte{"DATABASE_URL": []byte("old-uri"), "STALE_KEY": []byte("leftover")},
		},
	)
	c := &client{typed: cs}

	err := c.CopySecret(context.Background(), "foo-staging", "foo-staging-secrets",
		"preview-x", "foo-preview-secrets", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dst, err := cs.CoreV1().Secrets("preview-x").Get(context.Background(), "foo-preview-secrets", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(dst.Data["DATABASE_URL"]) != "staging-uri" {
		t.Errorf("DATABASE_URL = %q, want re-copied from source", dst.Data["DATABASE_URL"])
	}
	if _, ok := dst.Data["STALE_KEY"]; ok {
		t.Errorf("stale destination-only key survived the copy: %v", dst.Data)
	}
}

func TestCopySecretSourceMissingIsError(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := &client{typed: cs}

	err := c.CopySecret(context.Background(), "foo-staging", "missing-secret",
		"preview-x", "foo-preview-secrets", nil)
	if err == nil {
		t.Fatal("expected error when source secret is absent")
	}
}

// --- ApplyObjects / mapping resolution ---------------------------------

// handBuiltMapper builds a real *meta.PriorityRESTMapper the same way
// production does — restmapper.NewDiscoveryRESTMapper — but from
// hand-written discovery data covering the five kinds bifrost renders
// (Deployment/Service/ConfigMap/CronJob/Ingress), instead of a live
// restmapper.GetAPIGroupResources discovery round-trip. That makes mapping
// resolution below a test of the real client-go mapping logic, not a stub.
func handBuiltMapper() meta.RESTMapper {
	groupResources := []*restmapper.APIGroupResources{
		{
			Group: metav1.APIGroup{
				Name:             "",
				Versions:         []metav1.GroupVersionForDiscovery{{Version: "v1"}},
				PreferredVersion: metav1.GroupVersionForDiscovery{Version: "v1"},
			},
			VersionedResources: map[string][]metav1.APIResource{
				"v1": {
					{Name: "services", SingularName: "service", Namespaced: true, Kind: "Service"},
					{Name: "configmaps", SingularName: "configmap", Namespaced: true, Kind: "ConfigMap"},
				},
			},
		},
		{
			Group: metav1.APIGroup{
				Name:             "apps",
				Versions:         []metav1.GroupVersionForDiscovery{{GroupVersion: "apps/v1", Version: "v1"}},
				PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "apps/v1", Version: "v1"},
			},
			VersionedResources: map[string][]metav1.APIResource{
				"v1": {
					{Name: "deployments", SingularName: "deployment", Namespaced: true, Kind: "Deployment"},
				},
			},
		},
		{
			Group: metav1.APIGroup{
				Name:             "batch",
				Versions:         []metav1.GroupVersionForDiscovery{{GroupVersion: "batch/v1", Version: "v1"}},
				PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "batch/v1", Version: "v1"},
			},
			VersionedResources: map[string][]metav1.APIResource{
				"v1": {
					{Name: "cronjobs", SingularName: "cronjob", Namespaced: true, Kind: "CronJob"},
				},
			},
		},
		{
			Group: metav1.APIGroup{
				Name:             "networking.k8s.io",
				Versions:         []metav1.GroupVersionForDiscovery{{GroupVersion: "networking.k8s.io/v1", Version: "v1"}},
				PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "networking.k8s.io/v1", Version: "v1"},
			},
			VersionedResources: map[string][]metav1.APIResource{
				"v1": {
					{Name: "ingresses", SingularName: "ingress", Namespaced: true, Kind: "Ingress"},
				},
			},
		},
	}
	return restmapper.NewDiscoveryRESTMapper(groupResources)
}

func TestRESTMapperResolvesAllRenderedKinds(t *testing.T) {
	mapper := handBuiltMapper()
	cases := []struct {
		kind string
		gvr  schema.GroupVersionResource
	}{
		{"Deployment", schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}},
		{"Service", schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}},
		{"ConfigMap", schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}},
		{"CronJob", schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}},
		{"Ingress", schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			mapping, err := mapper.RESTMapping(schema.GroupKind{Group: tc.gvr.Group, Kind: tc.kind}, tc.gvr.Version)
			if err != nil {
				t.Fatalf("RESTMapping(%s): %v", tc.kind, err)
			}
			if mapping.Resource != tc.gvr {
				t.Errorf("Resource = %+v, want %+v", mapping.Resource, tc.gvr)
			}
			if mapping.Scope.Name() != meta.RESTScopeNameNamespace {
				t.Errorf("Scope = %v, want namespaced", mapping.Scope.Name())
			}
		})
	}
}

func TestRESTMapperUnknownKindErrors(t *testing.T) {
	mapper := handBuiltMapper()
	_, err := mapper.RESTMapping(schema.GroupKind{Kind: "FrobnicatorWidget"})
	if err == nil {
		t.Fatal("expected an error for an unregistered kind")
	}
}

func newConfigMap(ns, name string, data map[string]string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"namespace": ns, "name": name},
		"data":       toAnyMap(data),
	}}
}

func toAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// resourceInterfaceFor is the seam the brief sanctions for testing SSA
// dispatch against a fake dynamic client. It turns out the fake can't
// exercise a real Apply at all for unstructured content: client-go's
// dynamicfake always uses a plain (non-managed-fields) ObjectTracker, whose
// Apply merges via strategicpatch.StrategicMergePatch against a Go
// dataStruct — but every object the fake dynamic client hands it is
// *unstructured.Unstructured, which has no json-tagged fields to merge into.
// That produces a hard error ("unable to find api field in struct
// Unstructured for the json field ...") on *every* Apply call regardless of
// whether the target pre-exists — confirmed empirically while writing these
// tests, a stronger caveat than "doesn't support create". Create/Update
// don't go through that merge path, so these tests drive
// resourceInterfaceFor's return value with Create to verify namespace vs.
// cluster-scope dispatch, and separately assert (below) that ApplyObjects
// wraps a real Apply failure with useful context.
func TestResourceInterfaceForNamespacedScopesToObjectNamespace(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "ConfigMapList"},
	)
	mapping := &meta.RESTMapping{Resource: gvr, Scope: meta.RESTScopeNamespace}

	obj := newConfigMap("preview-a", "app-preview-env", map[string]string{"ENV": "a"})
	if _, err := resourceInterfaceFor(dyn, mapping, obj).
		Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := dyn.Resource(gvr).Namespace("preview-a").
		Get(context.Background(), "app-preview-env", metav1.GetOptions{}); err != nil {
		t.Errorf("object not found in its own namespace: %v", err)
	}
	if _, err := dyn.Resource(gvr).Namespace("preview-b").
		Get(context.Background(), "app-preview-env", metav1.GetOptions{}); err == nil {
		t.Error("object leaked into a namespace other than obj's own")
	}
}

func TestResourceInterfaceForClusterScopedIgnoresObjectNamespace(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}
	existing := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": "preview-x"},
	}}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "NamespaceList"},
		existing,
	)
	mapping := &meta.RESTMapping{Resource: gvr, Scope: meta.RESTScopeRoot}

	// decoy's own metadata.namespace is nonsensical for a cluster-scoped
	// kind — it's only here so a bug that keys dispatch off
	// obj.GetNamespace() instead of mapping.Scope gets caught: such a bug
	// would route the Get below into a "should-be-ignored" namespace bucket
	// that doesn't exist, rather than the root scope where existing lives.
	decoy := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": "preview-x", "namespace": "should-be-ignored"},
	}}

	got, err := resourceInterfaceFor(dyn, mapping, decoy).
		Get(context.Background(), "preview-x", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get via resourceInterfaceFor: %v", err)
	}
	if got.GetName() != "preview-x" {
		t.Errorf("got = %v", got.Object)
	}
}

func TestApplyObjectsUnknownKindErrors(t *testing.T) {
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), nil)
	c := &client{dyn: dyn, mapper: handBuiltMapper()}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "bifrost.example/v1",
		"kind":       "FrobnicatorWidget",
		"metadata":   map[string]any{"namespace": "preview-x", "name": "thing"},
	}}
	err := c.ApplyObjects(context.Background(), []*unstructured.Unstructured{obj})
	if err == nil {
		t.Fatal("expected an error for an unmappable kind")
	}
}

// TestApplyObjectsWrapsUnderlyingApplyError leans on the fake's Apply
// limitation (documented above) on purpose: since every real Apply call
// against dynamicfake fails, this exercises ApplyObjects' error-wrapping path
// with a real error out of the dynamic client, and asserts the wrapped
// message carries the kind/namespace/name an operator needs to find the
// broken object in a multi-object preview apply.
func TestApplyObjectsWrapsUnderlyingApplyError(t *testing.T) {
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			{Group: "", Version: "v1", Resource: "configmaps"}: "ConfigMapList",
		},
	)
	c := &client{dyn: dyn, mapper: handBuiltMapper()}

	obj := newConfigMap("preview-x", "app-preview-env", map[string]string{"ENV": "v"})
	err := c.ApplyObjects(context.Background(), []*unstructured.Unstructured{obj})
	if err == nil {
		t.Fatal("expected the fake's Apply limitation to surface as an error")
	}
	for _, want := range []string{"ConfigMap", "preview-x", "app-preview-env"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing context %q", err, want)
		}
	}
}

// restMapper resolution itself (the lazy discovery + caching) is covered via
// the client's real constructor path (New) being exercised at the
// integration level; here we confirm the cache is actually used — typed is
// left nil, so if restMapper() ignored the cache and tried discovery it
// would panic on a nil interface rather than returning cleanly.
func TestRestMapperCachesAfterFirstResolution(t *testing.T) {
	c := &client{mapper: handBuiltMapper()}

	if _, err := c.restMapper(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
