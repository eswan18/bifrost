package kube

import (
	"context"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestListPodImages(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "foo", Name: "pod-1"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Image: "reg/foo:abc"},
				{Image: "reg/foo:abc"}, // duplicate, deduped
			}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "foo", Name: "pod-2"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Image: "reg/foo:def"},
			}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "p"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Image: "reg/bar:zzz"},
			}},
		},
	)
	c := &client{typed: cs}
	got, err := c.ListPodImages(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{"reg/foo:abc", "reg/foo:def"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v, want %v", got, want)
	}
}
