// Package kube wraps client-go to read pod images and patch ArgoCD
// Application objects. Callers depend on Client (the interface) so tests
// can substitute fakes.
package kube

import "context"

type Client interface {
	ListPodImages(ctx context.Context, namespace string) ([]string, error)
	PatchProdImage(ctx context.Context, app, image string) error
}
