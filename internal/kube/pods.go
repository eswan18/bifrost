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
	Name string
	// OwnerKind is the pod's controller kind ("ReplicaSet", "Job", ...), ""
	// for bare pods. Job-owned pods run to completion on whatever image the
	// Job was created with, so they don't reflect what's deployed.
	OwnerKind  string
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
		if ref := metav1.GetControllerOf(&p); ref != nil {
			info.OwnerKind = ref.Kind
		}
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

// Images returns the deduped container images across the namespace's
// long-running pods. Job-owned pods (cron/one-off jobs) are excluded: a
// completed job keeps the image it ran with, which would read as a permanent
// mid-deploy once the deployment moves past it. Everything else must be
// included regardless of phase or health — promote.StatusOf detects
// mid-deploy by seeing old+new (or pending/backoff) pods side by side.
func Images(pods []PodInfo) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, p := range pods {
		if p.OwnerKind == "Job" {
			continue
		}
		for _, ctr := range p.Containers {
			if _, ok := seen[ctr.Image]; !ok {
				seen[ctr.Image] = struct{}{}
				out = append(out, ctr.Image)
			}
		}
	}
	return out
}
