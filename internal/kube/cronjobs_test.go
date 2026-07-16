package kube

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

func TestListCronJobs(t *testing.T) {
	lastSched := metav1.NewTime(time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC))
	cs := fake.NewSimpleClientset(
		&batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{Namespace: "foo", Name: "with-tz"},
			Spec: batchv1.CronJobSpec{
				Schedule: "0 9 * * *",
				TimeZone: strPtr("America/New_York"),
				JobTemplate: batchv1.JobTemplateSpec{
					Spec: batchv1.JobSpec{
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{Containers: []corev1.Container{
								{Name: "app", Image: "reg/foo:abc"},
							}},
						},
					},
				},
			},
			Status: batchv1.CronJobStatus{LastScheduleTime: &lastSched},
		},
		&batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{Namespace: "foo", Name: "suspended-no-containers"},
			Spec: batchv1.CronJobSpec{
				Schedule: "*/5 * * * *",
				Suspend:  boolPtr(true),
				JobTemplate: batchv1.JobTemplateSpec{
					Spec: batchv1.JobSpec{
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{Containers: nil},
						},
					},
				},
			},
		},
		&batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "elsewhere"},
			Spec:       batchv1.CronJobSpec{Schedule: "@daily"},
		},
	)
	c := &client{typed: cs}

	got, err := c.ListCronJobs(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d cronjobs, want 2", len(got))
	}

	byName := map[string]CronJobInfo{}
	for _, cj := range got {
		byName[cj.Name] = cj
	}

	withTZ := byName["with-tz"]
	if withTZ.Schedule != "0 9 * * *" {
		t.Errorf("with-tz Schedule = %q, want %q", withTZ.Schedule, "0 9 * * *")
	}
	if withTZ.TimeZone != "America/New_York" {
		t.Errorf("with-tz TimeZone = %q, want America/New_York", withTZ.TimeZone)
	}
	if withTZ.Suspended {
		t.Errorf("with-tz Suspended = true, want false")
	}
	if withTZ.Image != "reg/foo:abc" {
		t.Errorf("with-tz Image = %q, want reg/foo:abc", withTZ.Image)
	}
	if !withTZ.LastScheduleTime.Equal(lastSched.Time) {
		t.Errorf("with-tz LastScheduleTime = %v, want %v", withTZ.LastScheduleTime, lastSched.Time)
	}

	suspended := byName["suspended-no-containers"]
	if !suspended.Suspended {
		t.Errorf("suspended-no-containers Suspended = false, want true")
	}
	if suspended.TimeZone != "" {
		t.Errorf("suspended-no-containers TimeZone = %q, want \"\"", suspended.TimeZone)
	}
	if suspended.Image != "" {
		t.Errorf("suspended-no-containers Image = %q, want \"\" (no containers)", suspended.Image)
	}
	if !suspended.LastScheduleTime.IsZero() {
		t.Errorf("suspended-no-containers LastScheduleTime = %v, want zero", suspended.LastScheduleTime)
	}
}

func TestListCronJobsAllNamespaces(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{Namespace: "foo", Name: "a"},
			Spec:       batchv1.CronJobSpec{Schedule: "@daily"},
		},
		&batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{Namespace: "bar", Name: "b"},
			Spec:       batchv1.CronJobSpec{Schedule: "@daily"},
		},
	)
	c := &client{typed: cs}
	got, err := c.ListCronJobs(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d cronjobs across all namespaces, want 2", len(got))
	}
}

func TestListJobs(t *testing.T) {
	ctrl := true
	start := metav1.NewTime(time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC))
	completion := metav1.NewTime(time.Date(2026, 7, 16, 9, 1, 0, 0, time.UTC))

	cs := fake.NewSimpleClientset(
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo", Name: "backup-29735100",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "batch/v1", Kind: "CronJob", Name: "backup", Controller: &ctrl,
				}},
			},
			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{
						{Name: "app", Image: "reg/foo:abc"},
					}},
				},
			},
			Status: batchv1.JobStatus{
				StartTime:      &start,
				CompletionTime: &completion,
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
				},
			},
		},
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo", Name: "backup-29735200",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "batch/v1", Kind: "CronJob", Name: "backup", Controller: &ctrl,
				}},
			},
			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{
						{Name: "app", Image: "reg/foo:def"},
					}},
				},
			},
			Status: batchv1.JobStatus{
				StartTime: &start,
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
				},
			},
		},
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo", Name: "backup-29735300",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "batch/v1", Kind: "CronJob", Name: "backup", Controller: &ctrl,
				}},
			},
			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{
						{Name: "app", Image: "reg/foo:ghi"},
					}},
				},
			},
			Status: batchv1.JobStatus{
				StartTime: &start,
				Active:    1,
			},
		},
		&batchv1.Job{
			// one-off Job, no CronJob owner
			ObjectMeta: metav1.ObjectMeta{Namespace: "foo", Name: "one-off-migration"},
			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{
						{Name: "app", Image: "reg/foo:jkl"},
					}},
				},
			},
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
				},
			},
		},
	)
	c := &client{typed: cs}

	got, err := c.ListJobs(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d jobs, want 4", len(got))
	}

	byName := map[string]JobInfo{}
	for _, j := range got {
		byName[j.Name] = j
	}

	succeeded := byName["backup-29735100"]
	if succeeded.OwnerCron != "backup" {
		t.Errorf("succeeded OwnerCron = %q, want backup", succeeded.OwnerCron)
	}
	if !succeeded.Succeeded || succeeded.Failed || succeeded.Active {
		t.Errorf("succeeded flags = %+v, want only Succeeded", succeeded)
	}
	if !succeeded.StartTime.Equal(start.Time) || !succeeded.CompletionTime.Equal(completion.Time) {
		t.Errorf("succeeded times = %v/%v, want %v/%v",
			succeeded.StartTime, succeeded.CompletionTime, start.Time, completion.Time)
	}

	failed := byName["backup-29735200"]
	if !failed.Failed || failed.Succeeded || failed.Active {
		t.Errorf("failed flags = %+v, want only Failed", failed)
	}
	if failed.FailReason != "BackoffLimitExceeded" {
		t.Errorf("failed FailReason = %q, want BackoffLimitExceeded", failed.FailReason)
	}
	if !failed.CompletionTime.IsZero() {
		t.Errorf("failed CompletionTime = %v, want zero", failed.CompletionTime)
	}

	active := byName["backup-29735300"]
	if !active.Active || active.Succeeded || active.Failed {
		t.Errorf("active flags = %+v, want only Active", active)
	}

	oneOff := byName["one-off-migration"]
	if oneOff.OwnerCron != "" {
		t.Errorf("one-off-migration OwnerCron = %q, want \"\"", oneOff.OwnerCron)
	}
	if !oneOff.Succeeded {
		t.Errorf("one-off-migration Succeeded = false, want true")
	}
	if !oneOff.StartTime.IsZero() {
		t.Errorf("one-off-migration StartTime = %v, want zero (never set)", oneOff.StartTime)
	}
}

func TestNextRun(t *testing.T) {
	after := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)

	t.Run("UTC default", func(t *testing.T) {
		got, err := NextRun("0 9 * * *", "", after)
		if err != nil {
			t.Fatal(err)
		}
		want := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("NextRun = %v, want %v", got, want)
		}
	})

	t.Run("named timezone", func(t *testing.T) {
		// 9am America/New_York in July (EDT, UTC-4) is 13:00 UTC.
		got, err := NextRun("0 9 * * *", "America/New_York", after)
		if err != nil {
			t.Fatal(err)
		}
		loc, err := time.LoadLocation("America/New_York")
		if err != nil {
			t.Fatal(err)
		}
		want := time.Date(2026, 7, 16, 9, 0, 0, 0, loc)
		if !got.Equal(want) {
			t.Errorf("NextRun = %v, want %v", got, want)
		}
		if got.UTC().Hour() != 13 {
			t.Errorf("NextRun in UTC = %v, want hour 13", got.UTC())
		}
	})

	t.Run("descriptor schedule", func(t *testing.T) {
		got, err := NextRun("@daily", "", after)
		if err != nil {
			t.Fatal(err)
		}
		want := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("NextRun(@daily) = %v, want %v", got, want)
		}
	})

	t.Run("bad schedule", func(t *testing.T) {
		if _, err := NextRun("not a schedule", "", after); err == nil {
			t.Error("NextRun with bad schedule: want error, got nil")
		}
	})

	t.Run("bad timezone", func(t *testing.T) {
		if _, err := NextRun("0 9 * * *", "Not/AZone", after); err == nil {
			t.Error("NextRun with bad timezone: want error, got nil")
		}
	})
}
