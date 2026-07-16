package kube

import (
	"context"
	"time"

	"github.com/robfig/cron/v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CronJobInfo is one CronJob in a service namespace.
type CronJobInfo struct {
	// Namespace groups cluster-wide List results back to their {svc}-{env}
	// namespace.
	Namespace string
	Name      string
	Schedule  string // raw cron expression, e.g. "0 9 * * *"
	// TimeZone is spec.timeZone; "" means the cluster default (UTC on GKE).
	TimeZone  string
	Suspended bool
	// Image is the first container image in the job template — the image the
	// next run will use.
	Image string
	// LastScheduleTime is status.lastScheduleTime; zero if the CronJob has
	// never been scheduled. Fallback for "last run" when all Jobs have been
	// garbage-collected.
	LastScheduleTime time.Time
}

// JobInfo is one Job in a service namespace, including finished Jobs still
// retained by history limits.
type JobInfo struct {
	// Namespace groups cluster-wide List results back to their {svc}-{env}
	// namespace.
	Namespace string
	Name      string
	// OwnerCron is the owning CronJob's name; "" for one-off Jobs.
	OwnerCron string
	// Image is the first container image in the Job's pod template — the image
	// this run actually used.
	Image          string
	StartTime      time.Time // zero if the Job hasn't started
	CompletionTime time.Time // zero if the Job hasn't finished
	Succeeded      bool      // condition Complete=True
	Failed         bool      // condition Failed=True
	Active         bool      // status.active > 0
	// FailReason is the Failed condition's reason (e.g. "BackoffLimitExceeded",
	// "DeadlineExceeded"); "" otherwise. Exit codes come from the Job's pods
	// (PodInfo.OwnerName / ContainerInfo.ExitCode).
	FailReason string
}

// ListCronJobs returns the CronJobs in a namespace. An empty namespace lists
// across all namespaces.
func (c *client) ListCronJobs(ctx context.Context, namespace string) ([]CronJobInfo, error) {
	list, err := c.typed.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]CronJobInfo, 0, len(list.Items))
	for _, cj := range list.Items {
		info := CronJobInfo{
			Namespace: cj.Namespace,
			Name:      cj.Name,
			Schedule:  cj.Spec.Schedule,
			Suspended: cj.Spec.Suspend != nil && *cj.Spec.Suspend,
		}
		if cj.Spec.TimeZone != nil {
			info.TimeZone = *cj.Spec.TimeZone
		}
		if containers := cj.Spec.JobTemplate.Spec.Template.Spec.Containers; len(containers) > 0 {
			info.Image = containers[0].Image
		}
		if cj.Status.LastScheduleTime != nil {
			info.LastScheduleTime = cj.Status.LastScheduleTime.Time
		}
		out = append(out, info)
	}
	return out, nil
}

// ListJobs returns the Jobs in a namespace, including completed Jobs retained
// by history limits. An empty namespace lists across all namespaces.
func (c *client) ListJobs(ctx context.Context, namespace string) ([]JobInfo, error) {
	list, err := c.typed.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]JobInfo, 0, len(list.Items))
	for _, j := range list.Items {
		info := JobInfo{
			Namespace: j.Namespace,
			Name:      j.Name,
			Active:    j.Status.Active > 0,
		}
		if ref := metav1.GetControllerOf(&j); ref != nil && ref.Kind == "CronJob" {
			info.OwnerCron = ref.Name
		}
		if containers := j.Spec.Template.Spec.Containers; len(containers) > 0 {
			info.Image = containers[0].Image
		}
		if j.Status.StartTime != nil {
			info.StartTime = j.Status.StartTime.Time
		}
		if j.Status.CompletionTime != nil {
			info.CompletionTime = j.Status.CompletionTime.Time
		}
		for _, cond := range j.Status.Conditions {
			switch {
			case cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue:
				info.Succeeded = true
			case cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue:
				info.Failed = true
				info.FailReason = cond.Reason
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// NextRun returns the next time a cron schedule fires after the given time.
// timeZone is an IANA name; "" means UTC (the GKE cluster default).
func NextRun(schedule, timeZone string, after time.Time) (time.Time, error) {
	sched, err := cron.ParseStandard(schedule)
	if err != nil {
		return time.Time{}, err
	}
	loc := time.UTC
	if timeZone != "" {
		loc, err = time.LoadLocation(timeZone)
		if err != nil {
			return time.Time{}, err
		}
	}
	return sched.Next(after.In(loc)), nil
}
