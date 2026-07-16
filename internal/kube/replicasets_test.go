package kube

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func int32Ptr(i int32) *int32 { return &i }

func TestListReplicaSets(t *testing.T) {
	ctrl := true
	created := metav1.NewTime(time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC))

	cs := fake.NewSimpleClientset(
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo", Name: "app-6858d77994",
				CreationTimestamp: created,
				Annotations:       map[string]string{"deployment.kubernetes.io/revision": "3"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "Deployment", Name: "app", Controller: &ctrl,
				}},
			},
			Spec: appsv1.ReplicaSetSpec{
				Replicas: int32Ptr(2),
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{
						{Name: "app", Image: "reg/foo:new"},
					}},
				},
			},
			Status: appsv1.ReplicaSetStatus{ReadyReplicas: 2},
		},
		&appsv1.ReplicaSet{
			// missing revision annotation, nil replicas (defaults to 1), no owner
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo", Name: "unowned",
				CreationTimestamp: created,
			},
			Spec: appsv1.ReplicaSetSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{
						{Name: "app", Image: "reg/foo:old"},
					}},
				},
			},
		},
		&appsv1.ReplicaSet{
			// unparseable revision annotation
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo", Name: "bad-annotation",
				Annotations: map[string]string{"deployment.kubernetes.io/revision": "not-a-number"},
			},
			Spec: appsv1.ReplicaSetSpec{Replicas: int32Ptr(0)},
		},
	)
	c := &client{typed: cs}

	got, err := c.ListReplicaSets(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d replicasets, want 3", len(got))
	}

	byName := map[string]ReplicaSetInfo{}
	for _, rs := range got {
		byName[rs.Name] = rs
	}

	owned := byName["app-6858d77994"]
	if owned.Deployment != "app" {
		t.Errorf("owned Deployment = %q, want app", owned.Deployment)
	}
	if owned.Revision != 3 {
		t.Errorf("owned Revision = %d, want 3", owned.Revision)
	}
	if owned.Image != "reg/foo:new" {
		t.Errorf("owned Image = %q, want reg/foo:new", owned.Image)
	}
	if owned.Replicas != 2 || owned.ReadyReplicas != 2 {
		t.Errorf("owned Replicas/ReadyReplicas = %d/%d, want 2/2", owned.Replicas, owned.ReadyReplicas)
	}
	if !owned.CreatedAt.Equal(created.Time) {
		t.Errorf("owned CreatedAt = %v, want %v", owned.CreatedAt, created.Time)
	}

	unowned := byName["unowned"]
	if unowned.Deployment != "" {
		t.Errorf("unowned Deployment = %q, want \"\"", unowned.Deployment)
	}
	if unowned.Revision != 0 {
		t.Errorf("unowned Revision = %d, want 0 (missing annotation)", unowned.Revision)
	}
	if unowned.Replicas != 1 {
		t.Errorf("unowned Replicas = %d, want 1 (nil spec.replicas defaults to 1)", unowned.Replicas)
	}

	bad := byName["bad-annotation"]
	if bad.Revision != 0 {
		t.Errorf("bad-annotation Revision = %d, want 0 (unparseable)", bad.Revision)
	}
	if bad.Replicas != 0 {
		t.Errorf("bad-annotation Replicas = %d, want 0 (explicit spec.replicas=0)", bad.Replicas)
	}
}

func TestPreviousImage(t *testing.T) {
	cases := []struct {
		name    string
		sets    []ReplicaSetInfo
		current string
		want    string
	}{
		{
			name:    "single revision, nothing previous",
			sets:    []ReplicaSetInfo{{Name: "a", Revision: 1, Image: "reg/app:v1"}},
			current: "reg/app:v1",
			want:    "",
		},
		{
			name: "picks highest differing revision",
			sets: []ReplicaSetInfo{
				{Name: "a", Revision: 1, Image: "reg/app:v1"},
				{Name: "b", Revision: 2, Image: "reg/app:v2"},
				{Name: "c", Revision: 3, Image: "reg/app:v3"},
			},
			current: "reg/app:v3",
			want:    "reg/app:v2",
		},
		{
			name: "ignores empty images",
			sets: []ReplicaSetInfo{
				{Name: "a", Revision: 1, Image: "reg/app:v1"},
				{Name: "b", Revision: 2, Image: ""},
				{Name: "c", Revision: 3, Image: "reg/app:v3"},
			},
			current: "reg/app:v3",
			want:    "reg/app:v1",
		},
		{
			name:    "no history at all",
			sets:    nil,
			current: "reg/app:v1",
			want:    "",
		},
		{
			name: "revision order doesn't matter, only the max wins",
			sets: []ReplicaSetInfo{
				{Name: "c", Revision: 3, Image: "reg/app:v3"},
				{Name: "a", Revision: 1, Image: "reg/app:v1"},
				{Name: "b", Revision: 2, Image: "reg/app:v2"},
			},
			current: "reg/app:v3",
			want:    "reg/app:v2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PreviousImage(tc.sets, tc.current); got != tc.want {
				t.Errorf("PreviousImage = %q, want %q", got, tc.want)
			}
		})
	}
}
