package kube

import (
	"context"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestListPodsAndImages(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "foo", Name: "pod-1"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "app", Image: "reg/foo:abc"},
				{Name: "sidecar", Image: "reg/foo:abc"}, // duplicate image, deduped
			}},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{
					{Name: "app", Ready: true, RestartCount: 2},
					{Name: "sidecar", Ready: false, RestartCount: 7,
						State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
							Reason: "CrashLoopBackOff",
						}}},
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "foo", Name: "pod-2"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "app", Image: "reg/foo:def"},
			}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "p"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "app", Image: "reg/bar:zzz"},
			}},
		},
	)
	c := &client{typed: cs}
	pods, err := c.ListPods(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 2 {
		t.Fatalf("got %d pods, want 2", len(pods))
	}

	var pod1 PodInfo
	for _, p := range pods {
		if p.Name == "pod-1" {
			pod1 = p
		}
	}
	if pod1.Phase != "Running" {
		t.Errorf("pod-1 phase = %q, want Running", pod1.Phase)
	}
	if len(pod1.Containers) != 2 {
		t.Fatalf("pod-1 containers = %d, want 2", len(pod1.Containers))
	}
	app, sidecar := pod1.Containers[0], pod1.Containers[1]
	if !app.Ready || app.RestartCount != 2 || app.WaitingReason != "" {
		t.Errorf("app container = %+v, want ready, 2 restarts, not waiting", app)
	}
	if sidecar.Ready || sidecar.RestartCount != 7 || sidecar.WaitingReason != "CrashLoopBackOff" {
		t.Errorf("sidecar container = %+v, want not-ready crashloop with 7 restarts", sidecar)
	}

	got := Images(pods)
	sort.Strings(got)
	want := []string{"reg/foo:abc", "reg/foo:def"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Images = %v, want %v", got, want)
	}
}
