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

type ContainerInfo struct {
	// Image comes from spec.containers, not status.containerStatuses: during
	// ImagePullBackOff the status-side image can be empty or stale, which
	// would corrupt promote/mid-deploy detection.
	Image         string
	Ready         bool
	RestartCount  int32
	WaitingReason string // e.g. "CrashLoopBackOff", "ImagePullBackOff"; "" when not waiting
}

type PodInfo struct {
	Name       string
	Phase      string
	Containers []ContainerInfo
}

func (c *client) ListPods(ctx context.Context, namespace string) ([]PodInfo, error) {
	pods, err := c.typed.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]PodInfo, 0, len(pods.Items))
	for _, p := range pods.Items {
		info := PodInfo{Name: p.Name, Phase: string(p.Status.Phase)}
		for _, ctr := range p.Spec.Containers {
			ci := ContainerInfo{Image: ctr.Image}
			for _, cs := range p.Status.ContainerStatuses {
				if cs.Name != ctr.Name {
					continue
				}
				ci.Ready = cs.Ready
				ci.RestartCount = cs.RestartCount
				if cs.State.Waiting != nil {
					ci.WaitingReason = cs.State.Waiting.Reason
				}
				break
			}
			info.Containers = append(info.Containers, ci)
		}
		out = append(out, info)
	}
	return out, nil
}

// Images returns the deduped container images across all pods, including
// completed ones — promote.StatusOf depends on seeing every image present in
// the namespace, so this must not filter by phase or health.
func Images(pods []PodInfo) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, p := range pods {
		for _, ctr := range p.Containers {
			if _, ok := seen[ctr.Image]; !ok {
				seen[ctr.Image] = struct{}{}
				out = append(out, ctr.Image)
			}
		}
	}
	return out
}
