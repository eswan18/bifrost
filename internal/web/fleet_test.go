package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eswan18/bifrost/internal/kube"
)

// --- argo visibility (label-only) --------------------------------------------

func TestDeriveEnvArgoVisibility(t *testing.T) {
	healthyPods := []kube.PodInfo{
		{Name: "p", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/foo:abc1234", Ready: true}}},
	}
	cases := []struct {
		name      string
		argo      kube.AppStatus
		wantLabel string
		wantClass string
		wantBold  bool
	}{
		{"degraded", kube.AppStatus{HealthStatus: "Degraded"}, "argo degraded", "c-amb", true},
		{"missing", kube.AppStatus{HealthStatus: "Missing"}, "argo missing", "c-amb", true},
		{"out of sync", kube.AppStatus{SyncStatus: "OutOfSync"}, "argo out of sync", "c-amb", true},
		{"health beats sync", kube.AppStatus{HealthStatus: "Degraded", SyncStatus: "OutOfSync"}, "argo degraded", "c-amb", true},
		{"healthy unchanged", kube.AppStatus{HealthStatus: "Healthy", SyncStatus: "Synced"}, "healthy", "c-mut", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := deriveEnv("prod", envRaw{pods: healthyPods}, tc.argo, "eswan18", "foo", "", time.Now(), time.UTC)
			// Status is deliberately left "ok" — the fix is label-only.
			if ev.Status != "ok" {
				t.Errorf("Status = %q, want ok (visibility fix must not change status)", ev.Status)
			}
			if ev.Label != tc.wantLabel {
				t.Errorf("Label = %q, want %q", ev.Label, tc.wantLabel)
			}
			if ev.LabelClass != tc.wantClass {
				t.Errorf("LabelClass = %q, want %q", ev.LabelClass, tc.wantClass)
			}
			if ev.Bold != tc.wantBold {
				t.Errorf("Bold = %v, want %v", ev.Bold, tc.wantBold)
			}
			if tc.wantLabel != "healthy" && !strings.Contains(ev.Detail, "ArgoCD") {
				t.Errorf("Detail = %q, want it to mention the ArgoCD state", ev.Detail)
			}
		})
	}
}

func TestDeriveEnvArgoDoesNotOverrideCrash(t *testing.T) {
	// A crashing env already reads crash; argo's Degraded view must not relabel
	// it (only otherwise-healthy envs get the amber argo label).
	raw := envRaw{pods: []kube.PodInfo{
		{Name: "p", Phase: "Running", Containers: []kube.ContainerInfo{
			{Image: "reg/foo:abc1234", Ready: false, WaitingReason: "CrashLoopBackOff", RestartCount: 3},
		}},
	}}
	ev := deriveEnv("prod", raw, kube.AppStatus{HealthStatus: "Degraded"}, "eswan18", "foo", "", time.Now(), time.UTC)
	if ev.Status != "crash" {
		t.Fatalf("Status = %q, want crash", ev.Status)
	}
	if strings.HasPrefix(ev.Label, "argo") {
		t.Errorf("Label = %q; a crashing env must keep its crash label", ev.Label)
	}
}

// --- overview RECENT FAILURES 24h window -------------------------------------

func TestDeriveOverviewRecentFailuresWindow(t *testing.T) {
	now := time.Now()
	f := &fleet{Jobs: []jobView{
		{App: "foo", Name: "old-fail", State: "failed", EnvLabel: "prod", LastRunTime: now.Add(-5 * 24 * time.Hour)},
		{App: "foo", Name: "recent-fail", State: "failed", EnvLabel: "prod", LastRunTime: now.Add(-2 * time.Hour)},
	}}
	f.deriveOverview(now)

	if len(f.Overview.Failed) != 1 {
		t.Fatalf("overview Failed = %d, want 1 (only the last-24h failure)", len(f.Overview.Failed))
	}
	if f.Overview.Failed[0].Name != "recent-fail" {
		t.Errorf("Failed[0] = %q, want recent-fail", f.Overview.Failed[0].Name)
	}
	// Fleet-wide counts stay unbounded: both failures still count.
	if f.JobIssues != 2 {
		t.Errorf("JobIssues = %d, want 2 (counts are unbounded, only the column is windowed)", f.JobIssues)
	}
}

// --- LAST RUN fallback -------------------------------------------------------

func TestBuildJobsLastRunFallsBackToDash(t *testing.T) {
	now := time.Now()
	// A retained Job with no state flags and no start/completion time hits the
	// default branch; jobTime is the zero time, which dayRelative renders as "",
	// so the label must fall back to an em dash.
	raw := envRaw{
		cronjobs: []kube.CronJobInfo{{Name: "j", Schedule: "0 0 * * *"}},
		jobs:     []kube.JobInfo{{Name: "j-1", OwnerCron: "j"}},
	}
	jobs := buildJobs("foo", "prod", raw, "eswan18", "foo", now, time.UTC)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	if jobs[0].LastRunLabel != "—" {
		t.Errorf("LastRunLabel = %q, want em dash", jobs[0].LastRunLabel)
	}
	if jobs[0].LastLabel != "—" {
		t.Errorf("LastLabel = %q, want em dash", jobs[0].LastLabel)
	}
}

// --- exit detail -------------------------------------------------------------

func TestExitDetail(t *testing.T) {
	job := &kube.JobInfo{Name: "nightly-1", FailReason: "BackoffLimitExceeded"}
	cases := []struct {
		name string
		pods []kube.PodInfo
		want string
	}{
		{
			name: "exit code only",
			pods: []kube.PodInfo{{OwnerName: "nightly-1", Containers: []kube.ContainerInfo{{ExitCode: i32(2)}}}},
			want: "exit 2",
		},
		{
			name: "exit code plus informative reason",
			pods: []kube.PodInfo{{OwnerName: "nightly-1", Containers: []kube.ContainerInfo{{ExitCode: i32(137), TerminatedReason: "OOMKilled"}}}},
			want: "exit 137 (OOMKilled)",
		},
		{
			name: "generic Error reason is not appended",
			pods: []kube.PodInfo{{OwnerName: "nightly-1", Containers: []kube.ContainerInfo{{ExitCode: i32(1), TerminatedReason: "Error"}}}},
			want: "exit 1",
		},
		{
			name: "no failing pod falls back to the Job condition",
			pods: []kube.PodInfo{{OwnerName: "someone-else", Containers: []kube.ContainerInfo{{ExitCode: i32(1)}}}},
			want: "BackoffLimitExceeded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitDetail(job, tc.pods); got != tc.want {
				t.Errorf("exitDetail = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- cluster-wide grouping -----------------------------------------------------

func TestGroupByNamespace(t *testing.T) {
	raws := groupByNamespace(
		[]kube.PodInfo{
			{Namespace: "foo-staging", Name: "p1"},
			{Namespace: "kube-system", Name: "dns"},
			{Namespace: "foo-staging", Name: "p2"},
		},
		[]kube.ReplicaSetInfo{{Namespace: "foo-prod", Name: "rs1"}},
		[]kube.CronJobInfo{{Namespace: "foo-staging", Name: "cj"}},
		[]kube.JobInfo{{Namespace: "foo-prod", Name: "j"}},
	)

	s := raws["foo-staging"]
	if len(s.pods) != 2 || s.pods[0].Name != "p1" || s.pods[1].Name != "p2" {
		t.Errorf("foo-staging pods = %+v, want p1,p2 in order", s.pods)
	}
	if len(s.cronjobs) != 1 || len(s.rsets) != 0 || len(s.jobs) != 0 {
		t.Errorf("foo-staging raw = %+v, want 1 cronjob only", s)
	}
	p := raws["foo-prod"]
	if len(p.rsets) != 1 || len(p.jobs) != 1 || len(p.pods) != 0 {
		t.Errorf("foo-prod raw = %+v, want 1 replicaset + 1 job", p)
	}
	if r := raws["absent-ns"]; len(r.pods)+len(r.rsets)+len(r.cronjobs)+len(r.jobs) != 0 {
		t.Errorf("absent namespace raw = %+v, want zero envRaw", r)
	}
}

// Unrelated namespaces in the cluster-wide List results (kube-system, argocd,
// ...) must not leak into a service's derived view.
func TestAssembleFleetIgnoresUnrelatedNamespaces(t *testing.T) {
	k := &fakeKube{
		imgs: map[string][]string{
			"foo-staging": {"reg/foo:abc1234"},
			"foo-prod":    {"reg/foo:abc1234"},
			"kube-system": {"reg/dns:v1", "reg/proxy:v2"},
			"argocd":      {"reg/argo:v3"},
		},
	}
	h, _ := newTestHandlers(t, k)
	f := h.assembleFleet(context.Background())

	if len(f.Apps) != 1 {
		t.Fatalf("apps = %d, want 1", len(f.Apps))
	}
	a := f.Apps[0]
	if a.Overall != "sync" {
		t.Errorf("overall = %q, want sync", a.Overall)
	}
	if a.Staging.Image != "reg/foo:abc1234" || a.Prod.Image != "reg/foo:abc1234" {
		t.Errorf("images = %q / %q, want reg/foo:abc1234 for both envs", a.Staging.Image, a.Prod.Image)
	}
}

// --- stuck rollouts ------------------------------------------------------------

func TestDeriveEnvStuckRollout(t *testing.T) {
	now := time.Now()
	degradedPods := []kube.PodInfo{
		{Name: "old", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/foo:old1234", Ready: true}}},
		{Name: "new", Phase: "Pending", Containers: []kube.ContainerInfo{{Image: "reg/foo:bad5678", Ready: false, WaitingReason: "ImagePullBackOff"}}},
	}
	rsAt := func(created time.Time) []kube.ReplicaSetInfo {
		return []kube.ReplicaSetInfo{{Name: "rs-2", Revision: 2, Image: "reg/foo:bad5678", Replicas: 1, CreatedAt: created}}
	}

	// Inside the deploy window the env reads as an ordinary rollout.
	fresh := deriveEnv("prod", envRaw{pods: degradedPods, rsets: rsAt(now.Add(-time.Minute))}, kube.AppStatus{}, "eswan18", "foo", "", now, time.UTC)
	if fresh.Stuck || fresh.Status != "deploying" || fresh.Label != "deploying 0/1" {
		t.Errorf("fresh rollout: stuck=%v status=%q label=%q, want deploying 0/1, not stuck", fresh.Stuck, fresh.Status, fresh.Label)
	}

	// Past stuckAfter it goes amber with the waiting reason in the label,
	// keeping status "deploying" (3-state vocabulary).
	stuck := deriveEnv("prod", envRaw{pods: degradedPods, rsets: rsAt(now.Add(-2 * stuckAfter))}, kube.AppStatus{}, "eswan18", "foo", "", now, time.UTC)
	if !stuck.Stuck || stuck.Status != "deploying" {
		t.Fatalf("old degraded rollout: stuck=%v status=%q, want stuck deploying", stuck.Stuck, stuck.Status)
	}
	if stuck.Label != "stuck · ImagePullBackOff" {
		t.Errorf("label = %q, want stuck · ImagePullBackOff", stuck.Label)
	}
	if stuck.LabelClass != "c-amb" || !stuck.Bold {
		t.Errorf("label class/bold = %q/%v, want c-amb bold", stuck.LabelClass, stuck.Bold)
	}

	// A genuine mid-rollout that isn't degraded (all pods ready, old+new
	// images coexisting) must never read as stuck, however old the RS.
	rollingPods := []kube.PodInfo{
		{Name: "old", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/foo:old1234", Ready: true}}},
		{Name: "new", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/foo:new5678", Ready: true}}},
	}
	rolling := deriveEnv("prod", envRaw{pods: rollingPods, rsets: rsAt(now.Add(-2 * stuckAfter))}, kube.AppStatus{}, "eswan18", "foo", "", now, time.UTC)
	if rolling.Stuck {
		t.Errorf("healthy mid-rollout marked stuck: %+v", rolling)
	}
}

func TestActiveExcludesStuckEnvs(t *testing.T) {
	stuckEnv := envView{Status: "deploying", Stuck: true}
	rollingEnv := envView{Status: "deploying"}
	okEnv := envView{Status: "ok"}

	cases := []struct {
		name string
		app  appView
		want bool
	}{
		{"stuck env only", appView{Overall: "deploying", Staging: okEnv, Prod: stuckEnv}, false},
		{"both envs stuck", appView{Overall: "deploying", Staging: stuckEnv, Prod: stuckEnv}, false},
		{"genuine rollout", appView{Overall: "deploying", Staging: okEnv, Prod: rollingEnv}, true},
		{"stuck staging, rolling prod", appView{Overall: "deploying", Staging: stuckEnv, Prod: rollingEnv}, true},
		{"mid-deploy only", appView{Overall: "deploying", Staging: okEnv, Prod: okEnv}, true},
		{"building", appView{Overall: "sync", Staging: okEnv, Prod: okEnv, Build: buildView{State: "building"}}, true},
		{"settled", appView{Overall: "sync", Staging: okEnv, Prod: okEnv}, false},
	}
	for _, tc := range cases {
		if got := tc.app.Active(); got != tc.want {
			t.Errorf("%s: Active() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
