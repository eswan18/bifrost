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

// A completed CronJob pod keeps the image it ran with; it must not make the
// namespace look mid-deploy after the deployment moves to a newer image.
func TestImagesExcludesJobPods(t *testing.T) {
	ctrl := true
	cs := fake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo", Name: "app-6858d77994-9s6c5",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "app-6858d77994", Controller: &ctrl,
				}},
			},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "reg/foo:new"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo", Name: "app-purge-29735100-8wrsp",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "batch/v1", Kind: "Job", Name: "app-purge-29735100", Controller: &ctrl,
				}},
			},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "reg/foo:old"}}},
			Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
		},
	)
	c := &client{typed: cs}
	pods, err := c.ListPods(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 2 {
		t.Fatalf("got %d pods, want 2 (job pods stay visible in ListPods)", len(pods))
	}
	for _, p := range pods {
		switch p.Name {
		case "app-6858d77994-9s6c5":
			if p.OwnerKind != "ReplicaSet" {
				t.Errorf("%s OwnerKind = %q, want ReplicaSet", p.Name, p.OwnerKind)
			}
			if p.OwnerName != "app-6858d77994" {
				t.Errorf("%s OwnerName = %q, want app-6858d77994", p.Name, p.OwnerName)
			}
		case "app-purge-29735100-8wrsp":
			if p.OwnerKind != "Job" {
				t.Errorf("%s OwnerKind = %q, want Job", p.Name, p.OwnerKind)
			}
			if p.OwnerName != "app-purge-29735100" {
				t.Errorf("%s OwnerName = %q, want app-purge-29735100", p.Name, p.OwnerName)
			}
		}
	}

	got := Images(pods)
	if len(got) != 1 || got[0] != "reg/foo:new" {
		t.Errorf("Images = %v, want [reg/foo:new] (job pod image excluded)", got)
	}
}

// TestListPodsExitCodes: a failed job pod's exit code comes from the current
// terminated state; a crashlooping (currently waiting) pod's exit code comes
// from its last termination state; a healthy running container has no exit
// code at all.
func TestListPodsExitCodes(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "foo", Name: "failed-job-pod"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "reg/foo:abc"}}},
			Status: corev1.PodStatus{
				Phase: corev1.PodFailed,
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name: "app",
						State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 137,
							Reason:   "OOMKilled",
						}},
					},
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "foo", Name: "crashlooping-pod"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "reg/foo:abc"}}},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name: "app",
						State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
							Reason: "CrashLoopBackOff",
						}},
						LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 1,
							Reason:   "Error",
						}},
					},
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "foo", Name: "healthy-pod"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "reg/foo:abc"}}},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:  "app",
						Ready: true,
						State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
					},
				},
			},
		},
	)
	c := &client{typed: cs}
	pods, err := c.ListPods(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]PodInfo{}
	for _, p := range pods {
		byName[p.Name] = p
	}

	failed := byName["failed-job-pod"].Containers[0]
	if failed.ExitCode == nil || *failed.ExitCode != 137 {
		t.Errorf("failed-job-pod ExitCode = %v, want 137", failed.ExitCode)
	}
	if failed.TerminatedReason != "OOMKilled" {
		t.Errorf("failed-job-pod TerminatedReason = %q, want OOMKilled", failed.TerminatedReason)
	}

	crashlooping := byName["crashlooping-pod"].Containers[0]
	if crashlooping.ExitCode == nil || *crashlooping.ExitCode != 1 {
		t.Errorf("crashlooping-pod ExitCode = %v, want 1 (from LastTerminationState)", crashlooping.ExitCode)
	}
	if crashlooping.TerminatedReason != "Error" {
		t.Errorf("crashlooping-pod TerminatedReason = %q, want Error", crashlooping.TerminatedReason)
	}
	if crashlooping.WaitingReason != "CrashLoopBackOff" {
		t.Errorf("crashlooping-pod WaitingReason = %q, want CrashLoopBackOff", crashlooping.WaitingReason)
	}

	healthy := byName["healthy-pod"].Containers[0]
	if healthy.ExitCode != nil {
		t.Errorf("healthy-pod ExitCode = %v, want nil", healthy.ExitCode)
	}
	if healthy.TerminatedReason != "" {
		t.Errorf("healthy-pod TerminatedReason = %q, want \"\"", healthy.TerminatedReason)
	}
}
