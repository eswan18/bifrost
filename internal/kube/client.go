package kube

import (
	"context"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type Client interface {
	ListPods(ctx context.Context, namespace string) ([]PodInfo, error)
	ListArgoApps(ctx context.Context) (map[string]AppStatus, error)
	// PatchAppImage pins the ArgoCD Application <app>-<env> to a full image
	// ref via its kustomize images override. Promote patches prod; rollback
	// patches either env.
	PatchAppImage(ctx context.Context, app, env, image string) error
	ListCronJobs(ctx context.Context, namespace string) ([]CronJobInfo, error)
	ListJobs(ctx context.Context, namespace string) ([]JobInfo, error)
	ListReplicaSets(ctx context.Context, namespace string) ([]ReplicaSetInfo, error)
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
	// Bound every API call so a hung API server can't hang requests forever.
	cfg.Timeout = 15 * time.Second
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
