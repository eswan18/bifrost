// Package gcb reads recent Google Cloud Build history so the UI can show
// per-service build status ("building", "build failed").
package gcb

import (
	"context"

	cloudbuild "google.golang.org/api/cloudbuild/v1"
)

// BuildStatus is the most recent build for one repo.
type BuildStatus struct {
	// Status is Cloud Build's raw status: QUEUED, PENDING, WORKING, SUCCESS,
	// FAILURE, INTERNAL_ERROR, TIMEOUT, CANCELLED, EXPIRED.
	Status string
	SHA    string // short commit SHA the build is building
	LogURL string // console link to the build log
}

// InProgress reports whether the build is still running.
func (b BuildStatus) InProgress() bool {
	switch b.Status {
	case "QUEUED", "PENDING", "WORKING":
		return true
	}
	return false
}

// Failed reports whether the build ended unsuccessfully. CANCELLED is not a
// failure: it was deliberate.
func (b BuildStatus) Failed() bool {
	switch b.Status {
	case "FAILURE", "INTERNAL_ERROR", "TIMEOUT", "EXPIRED":
		return true
	}
	return false
}

// Client provides the latest build per GitHub repo name.
type Client interface {
	LatestBuilds(ctx context.Context) (map[string]BuildStatus, error)
}

type client struct {
	svc     *cloudbuild.Service
	project string
}

// New returns a Client using Application Default Credentials (workload
// identity in-cluster, gcloud ADC locally).
func New(ctx context.Context, project string) (Client, error) {
	svc, err := cloudbuild.NewService(ctx)
	if err != nil {
		return nil, err
	}
	return &client{svc: svc, project: project}, nil
}

// TriggerIDs returns a map of build-trigger name → trigger ID for the project.
// bifrost calls this once at startup to build per-service "view build pipeline"
// links (the console filters build history by trigger ID). All of this
// project's triggers live in the global region, which the non-regional list
// endpoint returns; one page covers far more triggers than there are services.
func TriggerIDs(ctx context.Context, project string) (map[string]string, error) {
	svc, err := cloudbuild.NewService(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := svc.Projects.Triggers.List(project).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(resp.Triggers))
	for _, t := range resp.Triggers {
		if t.Name != "" && t.Id != "" {
			out[t.Name] = t.Id
		}
	}
	return out, nil
}

// LatestBuilds lists recent builds (newest first) and keeps the newest one
// per repo. One page is plenty: the page covers far more builds than there
// are services, and a service whose last build fell off the page simply
// shows no build badge.
func (c *client) LatestBuilds(ctx context.Context) (map[string]BuildStatus, error) {
	resp, err := c.svc.Projects.Builds.List(c.project).PageSize(50).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return latestByRepo(resp.Builds), nil
}

// latestByRepo maps repo name → newest build. Builds must be ordered newest
// first (the API's default ordering); the first build seen per repo wins.
func latestByRepo(builds []*cloudbuild.Build) map[string]BuildStatus {
	out := map[string]BuildStatus{}
	for _, b := range builds {
		repo := b.Substitutions["REPO_NAME"]
		if repo == "" {
			continue // manually-submitted build, not from a trigger
		}
		if _, ok := out[repo]; ok {
			continue
		}
		out[repo] = BuildStatus{
			Status: b.Status,
			SHA:    b.Substitutions["SHORT_SHA"],
			LogURL: b.LogUrl,
		}
	}
	return out
}
