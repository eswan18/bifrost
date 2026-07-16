package kube

import (
	"context"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// revisionAnnotation is set by the Deployment controller on every ReplicaSet
// it owns to record the rollout revision it corresponds to.
const revisionAnnotation = "deployment.kubernetes.io/revision"

// ReplicaSetInfo is one ReplicaSet revision of a Deployment's rollout
// history. Old revisions (replicas 0) are the source for "previous image"
// when rolling back.
type ReplicaSetInfo struct {
	// Namespace groups cluster-wide List results back to their {svc}-{env}
	// namespace.
	Namespace  string
	Name       string
	Deployment string // owning Deployment name; "" for unowned ReplicaSets
	// Revision is the deployment.kubernetes.io/revision annotation; 0 when
	// missing/unparseable.
	Revision int64
	// Image is the first container image in the pod template.
	Image         string
	Replicas      int32 // spec.replicas
	ReadyReplicas int32 // status.readyReplicas
	CreatedAt     time.Time
}

// ListReplicaSets returns the ReplicaSets in a namespace, including scaled-down
// historical revisions. An empty namespace lists across all namespaces.
func (c *client) ListReplicaSets(ctx context.Context, namespace string) ([]ReplicaSetInfo, error) {
	list, err := c.typed.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]ReplicaSetInfo, 0, len(list.Items))
	for _, rs := range list.Items {
		info := ReplicaSetInfo{
			Namespace:     rs.Namespace,
			Name:          rs.Name,
			Replicas:      1, // K8s default when spec.replicas is unset
			ReadyReplicas: rs.Status.ReadyReplicas,
			CreatedAt:     rs.CreationTimestamp.Time,
		}
		if ref := metav1.GetControllerOf(&rs); ref != nil && ref.Kind == "Deployment" {
			info.Deployment = ref.Name
		}
		if rev, err := strconv.ParseInt(rs.Annotations[revisionAnnotation], 10, 64); err == nil {
			info.Revision = rev
		}
		if containers := rs.Spec.Template.Spec.Containers; len(containers) > 0 {
			info.Image = containers[0].Image
		}
		if rs.Spec.Replicas != nil {
			info.Replicas = *rs.Spec.Replicas
		}
		out = append(out, info)
	}
	return out, nil
}

// NewestReplicaSet returns the highest-revision ReplicaSet for which keep
// returns true, and whether any matched. A nil keep matches every set. It is
// the shared "newest rollout" reduction behind both PreviousImage (keep =
// differs from current) and the deploy-progress lookup (keep = nil).
func NewestReplicaSet(sets []ReplicaSetInfo, keep func(ReplicaSetInfo) bool) (ReplicaSetInfo, bool) {
	var best ReplicaSetInfo
	found := false
	for _, rs := range sets {
		if keep != nil && !keep(rs) {
			continue
		}
		if !found || rs.Revision > best.Revision {
			best, found = rs, true
		}
	}
	return best, found
}

// PreviousImage returns the image an environment ran before the current one:
// the image of the highest-revision ReplicaSet whose image differs from
// current. "" when the history holds no other image (e.g. a fresh deploy with
// a single revision).
func PreviousImage(sets []ReplicaSetInfo, current string) string {
	rs, ok := NewestReplicaSet(sets, func(rs ReplicaSetInfo) bool {
		return rs.Image != "" && rs.Image != current
	})
	if !ok {
		return ""
	}
	return rs.Image
}
