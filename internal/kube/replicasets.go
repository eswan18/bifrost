package kube

import (
	"context"
	"errors"
	"time"
)

// ReplicaSetInfo is one ReplicaSet revision of a Deployment's rollout
// history. Old revisions (replicas 0) are the source for "previous image"
// when rolling back.
type ReplicaSetInfo struct {
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
// historical revisions.
func (c *client) ListReplicaSets(ctx context.Context, namespace string) ([]ReplicaSetInfo, error) {
	return nil, errors.New("not implemented")
}

// PreviousImage returns the image an environment ran before the current one:
// the image of the highest-revision ReplicaSet whose image differs from
// current. "" when the history holds no other image (e.g. a fresh deploy with
// a single revision).
func PreviousImage(sets []ReplicaSetInfo, current string) string {
	var best ReplicaSetInfo
	found := false
	for _, rs := range sets {
		if rs.Image == "" || rs.Image == current {
			continue
		}
		if !found || rs.Revision > best.Revision {
			best, found = rs, true
		}
	}
	if !found {
		return ""
	}
	return best.Image
}
