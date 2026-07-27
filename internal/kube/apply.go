package kube

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
)

// applyFieldManager is the SSA field manager bifrost uses for every preview
// object it applies. Force-conflicts under this manager are safe: previews
// are the sole owner of their own rendered manifests.
const applyFieldManager = "bifrost-preview"

// EnsureNamespace creates the namespace if absent, or updates it if present.
// Either way, labels/annotations are merged onto whatever's already there:
// new values win on key collision, existing keys not mentioned are left
// alone.
func (c *client) EnsureNamespace(ctx context.Context, name string, labels, annotations map[string]string) error {
	ns, err := c.typed.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		ns.Labels = mergeStringMaps(ns.Labels, labels)
		ns.Annotations = mergeStringMaps(ns.Annotations, annotations)
		_, err = c.typed.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	ns.Labels = mergeStringMaps(ns.Labels, labels)
	ns.Annotations = mergeStringMaps(ns.Annotations, annotations)
	_, err = c.typed.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{})
	return err
}

// AnnotateNamespace merges annotations onto an existing namespace (new
// values win on key collision, existing keys not mentioned are left alone).
// Unlike DeleteNamespace, an absent namespace here is a real error: callers
// use this mid-flow, after EnsureNamespace has already run, to record phase
// transitions on a namespace that's expected to exist.
func (c *client) AnnotateNamespace(ctx context.Context, name string, annotations map[string]string) error {
	ns, err := c.typed.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	ns.Annotations = mergeStringMaps(ns.Annotations, annotations)
	_, err = c.typed.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{})
	return err
}

// DeleteNamespace deletes the namespace. An already-absent namespace is not
// an error, so Down can be retried freely.
func (c *client) DeleteNamespace(ctx context.Context, name string) error {
	err := c.typed.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// CopySecret reads srcNS/srcName and creates or updates dstNS/dstName with
// its data, then applies overrides on top: a non-nil override value replaces
// that key, a nil value means "keep whatever the source has for this key"
// (lets callers build the overrides map uniformly without conditionally
// omitting keys). The destination's data is otherwise a full copy of the
// source, not a merge with whatever the destination previously held — a
// stale key from a prior copy does not survive a re-copy.
func (c *client) CopySecret(ctx context.Context, srcNS, srcName, dstNS, dstName string, overrides map[string][]byte) error {
	src, err := c.typed.CoreV1().Secrets(srcNS).Get(ctx, srcName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	data := make(map[string][]byte, len(src.Data))
	for k, v := range src.Data {
		data[k] = v
	}
	for k, v := range overrides {
		if v == nil {
			continue
		}
		data[k] = v
	}
	dst := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: dstName, Namespace: dstNS},
		Type:       src.Type,
		Data:       data,
	}

	existing, err := c.typed.CoreV1().Secrets(dstNS).Get(ctx, dstName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = c.typed.CoreV1().Secrets(dstNS).Create(ctx, dst, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	dst.ResourceVersion = existing.ResourceVersion
	_, err = c.typed.CoreV1().Secrets(dstNS).Update(ctx, dst, metav1.UpdateOptions{})
	return err
}

// mergeStringMaps overlays overrides onto base, without mutating either
// input; nil maps are handled like empty ones.
func mergeStringMaps(base, overrides map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(overrides))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// ApplyObjects server-side-applies each object with fieldManager
// "bifrost-preview", forcing ownership of conflicting fields. Each object's
// REST mapping is resolved via the client's cached RESTMapper (see
// restMapper). Objects are applied in the given order; the first failure
// aborts the rest.
func (c *client) ApplyObjects(ctx context.Context, objs []*unstructured.Unstructured) error {
	mapper, err := c.restMapper()
	if err != nil {
		return fmt.Errorf("resolve REST mapper: %w", err)
	}
	for _, obj := range objs {
		gvk := obj.GroupVersionKind()
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return fmt.Errorf("map %s %s/%s: %w", gvk.Kind, obj.GetNamespace(), obj.GetName(), err)
		}
		if err := c.applyOne(ctx, mapping, obj); err != nil {
			return fmt.Errorf("apply %s %s/%s: %w", gvk.Kind, obj.GetNamespace(), obj.GetName(), err)
		}
	}
	return nil
}

// applyOne issues the actual SSA call for one object. resourceInterfaceFor is
// split out as the seam under test: the fake dynamic client can't drive a
// real Apply end-to-end for unstructured content (see resourceInterfaceFor's
// doc comment and apply_test.go), so tests exercise the mapping/namespace
// dispatch logic directly instead of round-tripping through Apply.
func (c *client) applyOne(ctx context.Context, mapping *meta.RESTMapping, obj *unstructured.Unstructured) error {
	ri := resourceInterfaceFor(c.dyn, mapping, obj)
	_, err := ri.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{
		FieldManager: applyFieldManager,
		Force:        true,
	})
	return err
}

// resourceInterfaceFor scopes dyn to obj's resource mapping: namespaced
// resources are scoped to obj's namespace, cluster-scoped resources are not
// scoped at all.
//
// This is deliberately its own function (rather than inlined in applyOne) so
// it can be tested without going through Apply: client-go's fake dynamic
// client (dynamicfake.NewSimpleDynamicClient and friends) always builds a
// plain, non-managed-fields ObjectTracker, whose Apply implementation merges
// via strategicpatch.StrategicMergePatch against a Go dataStruct — but the
// fake only ever hands it *unstructured.Unstructured, which has no
// json-tagged fields to merge into. The result is a hard error
// ("unable to find api field in struct Unstructured for the json field ...")
// for *any* object on *any* Apply call, not merely a "doesn't support
// create" quirk — confirmed empirically while writing this package's tests.
// Create/Update calls don't go through that merge path (they just store the
// object), so tests drive this function's return value with Create/Update to
// verify namespace/cluster-scope dispatch, and separately assert that
// ApplyObjects wraps the (fake-induced, but shaped like a real one) Apply
// failure with useful context.
func resourceInterfaceFor(dyn dynamic.Interface, mapping *meta.RESTMapping, obj *unstructured.Unstructured) dynamic.ResourceInterface {
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		return dyn.Resource(mapping.Resource).Namespace(obj.GetNamespace())
	}
	return dyn.Resource(mapping.Resource)
}

// restMapper resolves and caches the client's RESTMapper on first use, so a
// preview run's several ApplyObjects calls (one per rendered member) share a
// single discovery round-trip instead of paying it per apply.
func (c *client) restMapper() (meta.RESTMapper, error) {
	c.mapperMu.Lock()
	defer c.mapperMu.Unlock()
	if c.mapper != nil {
		return c.mapper, nil
	}
	groupResources, err := restmapper.GetAPIGroupResources(c.typed.Discovery())
	if err != nil {
		return nil, err
	}
	c.mapper = restmapper.NewDiscoveryRESTMapper(groupResources)
	return c.mapper, nil
}
