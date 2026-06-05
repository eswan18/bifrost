package kube

import (
	"context"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type Client interface {
	ListPodImages(ctx context.Context, namespace string) ([]string, error)
	PatchProdImage(ctx context.Context, app, image string) error
}

// New returns an in-cluster Client. Falls back to KUBECONFIG / ~/.kube/config
// when not running in a pod (for local development).
func New(argoNS string) (Client, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		loader := clientcmd.NewDefaultClientConfigLoadingRules()
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			loader, &clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return nil, err
		}
	}
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &client{typed: typed, dyn: dyn, argoNS: argoNS}, nil
}
