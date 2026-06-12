package kube

import "testing"

func pod(phase string, ctrs ...ContainerInfo) PodInfo {
	return PodInfo{Name: "p", Phase: phase, Containers: ctrs}
}

func TestSummarizeHealth(t *testing.T) {
	cases := []struct {
		name   string
		pods   []PodInfo
		state  HealthState
		detail string
	}{
		{"no pods", nil, HealthUnknown, "no pods"},
		{
			"all ready",
			[]PodInfo{pod("Running", ContainerInfo{Ready: true}, ContainerInfo{Ready: true})},
			Healthy, "2/2 ready",
		},
		{
			"partial ready",
			[]PodInfo{pod("Running", ContainerInfo{Ready: true}, ContainerInfo{Ready: false})},
			Degraded, "1/2 ready",
		},
		{
			"crashloop wins over degraded, max restarts reported",
			[]PodInfo{
				pod("Running", ContainerInfo{Ready: false, RestartCount: 3, WaitingReason: "CrashLoopBackOff"}),
				pod("Running", ContainerInfo{Ready: false, RestartCount: 14, WaitingReason: "CrashLoopBackOff"}),
			},
			Crashlooping, "14 restarts",
		},
		{
			"image pull backoff",
			[]PodInfo{pod("Pending", ContainerInfo{Ready: false, WaitingReason: "ImagePullBackOff"})},
			Degraded, "0/1 ready — ImagePullBackOff",
		},
		{
			"succeeded pods ignored",
			[]PodInfo{
				pod("Succeeded", ContainerInfo{Ready: false}),
				pod("Running", ContainerInfo{Ready: true}),
			},
			Healthy, "1/1 ready",
		},
		{
			"only succeeded pods means no workload",
			[]PodInfo{pod("Succeeded", ContainerInfo{Ready: false})},
			HealthUnknown, "no pods",
		},
		{
			"pending phase degrades even when containers report ready",
			[]PodInfo{pod("Pending", ContainerInfo{Ready: true})},
			Degraded, "1/1 ready",
		},
		{
			"recovered crashloop with high restarts is healthy",
			[]PodInfo{pod("Running", ContainerInfo{Ready: true, RestartCount: 42})},
			Healthy, "1/1 ready",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SummarizeHealth(tc.pods)
			if got.State != tc.state {
				t.Errorf("state = %q, want %q", got.State, tc.state)
			}
			if got.Detail != tc.detail {
				t.Errorf("detail = %q, want %q", got.Detail, tc.detail)
			}
		})
	}
}
