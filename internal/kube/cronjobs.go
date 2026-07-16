package kube

import (
	"context"
	"errors"
	"time"
)

// CronJobInfo is one CronJob in a service namespace.
type CronJobInfo struct {
	Name     string
	Schedule string // raw cron expression, e.g. "0 9 * * *"
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
	Name string
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
	return nil, errors.New("not implemented")
}

// ListJobs returns the Jobs in a namespace, including completed Jobs retained
// by history limits. An empty namespace lists across all namespaces.
func (c *client) ListJobs(ctx context.Context, namespace string) ([]JobInfo, error) {
	return nil, errors.New("not implemented")
}

// NextRun returns the next time a cron schedule fires after the given time.
// timeZone is an IANA name; "" means UTC (the GKE cluster default).
func NextRun(schedule, timeZone string, after time.Time) (time.Time, error) {
	return time.Time{}, errors.New("not implemented")
}
