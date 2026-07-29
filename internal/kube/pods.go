package kube

import (
	"context"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

type client struct {
	typed  kubernetes.Interface
	dyn    dynamic.Interface
	argoNS string

	// mapper is the RESTMapper ApplyObjects resolves lazily and caches here
	// (see restMapper in apply.go).
	mapperMu sync.Mutex
	mapper   meta.RESTMapper
}

type ContainerInfo struct {
	// Name is the container's name as declared in the pod spec. It's what
	// lets a caller tell one container apart from another in the same pod —
	// e.g. the preview orchestrator naming the "migrate" initContainer
	// specifically when a branch's migrations are what's wedging a pod.
	Name string
	// Image comes from spec.containers, not status.containerStatuses: during
	// ImagePullBackOff the status-side image can be empty or stale, which
	// would corrupt promote/mid-deploy detection.
	Image         string
	Ready         bool
	RestartCount  int32
	WaitingReason string // e.g. "CrashLoopBackOff", "ImagePullBackOff"; "" when not waiting
	// ExitCode is the container's most recent termination exit code (current
	// state if terminated, else last termination); nil when it has never
	// terminated. Lets failed job pods surface "exit 137".
	ExitCode *int32
	// TerminatedReason accompanies ExitCode, e.g. "OOMKilled", "Error".
	TerminatedReason string
}

type PodInfo struct {
	// Namespace groups cluster-wide List results back to their {svc}-{env}
	// namespace.
	Namespace string
	Name      string
	// OwnerKind is the pod's controller kind ("ReplicaSet", "Job", ...), ""
	// for bare pods. Job-owned pods run to completion on whatever image the
	// Job was created with, so they don't reflect what's deployed.
	OwnerKind string
	// OwnerName is the controller's name — for Job-owned pods, the Job name,
	// which joins a pod's exit code back to its JobInfo.
	OwnerName  string
	Phase      string
	Containers []ContainerInfo
	// InitContainers mirrors Containers for spec.initContainers /
	// status.initContainerStatuses. It's deliberately a separate field
	// rather than folded into Containers: Images and SummarizeHealth (and
	// through them promote's mid-deploy detection and every health readout)
	// must keep seeing only the long-running app containers, since an init
	// container is expected to terminate and would otherwise read as a
	// permanently unhealthy container. Callers that care about init
	// containers — a preview waiting out its "migrate" step — opt in by
	// reading this field.
	InitContainers []ContainerInfo
}

// ListPods returns the pods in a namespace. An empty namespace lists across
// all namespaces.
func (c *client) ListPods(ctx context.Context, namespace string) ([]PodInfo, error) {
	pods, err := c.typed.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]PodInfo, 0, len(pods.Items))
	for _, p := range pods.Items {
		info := PodInfo{Namespace: p.Namespace, Name: p.Name, Phase: string(p.Status.Phase)}
		if ref := metav1.GetControllerOf(&p); ref != nil {
			info.OwnerKind = ref.Kind
			info.OwnerName = ref.Name
		}
		for _, ctr := range p.Spec.Containers {
			info.Containers = append(info.Containers, containerInfo(ctr, p.Status.ContainerStatuses))
		}
		for _, ctr := range p.Spec.InitContainers {
			info.InitContainers = append(info.InitContainers, containerInfo(ctr, p.Status.InitContainerStatuses))
		}
		out = append(out, info)
	}
	return out, nil
}

// containerInfo pairs one spec container with its matching status entry (by
// name — the only join key the two lists share). A container with no status
// yet (nothing scheduled, or an init container the kubelet hasn't reached)
// keeps its zero-value status fields: not ready, no waiting reason, no exit
// code.
func containerInfo(ctr corev1.Container, statuses []corev1.ContainerStatus) ContainerInfo {
	ci := ContainerInfo{Name: ctr.Name, Image: ctr.Image}
	for _, cs := range statuses {
		if cs.Name != ctr.Name {
			continue
		}
		ci.Ready = cs.Ready
		ci.RestartCount = cs.RestartCount
		if cs.State.Waiting != nil {
			ci.WaitingReason = cs.State.Waiting.Reason
		}
		if t := cs.State.Terminated; t != nil {
			ci.ExitCode = &t.ExitCode
			ci.TerminatedReason = t.Reason
		} else if t := cs.LastTerminationState.Terminated; t != nil {
			ci.ExitCode = &t.ExitCode
			ci.TerminatedReason = t.Reason
		}
		break
	}
	return ci
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
