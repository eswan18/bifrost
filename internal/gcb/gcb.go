// Package gcb reads recent Google Cloud Build history so the UI can show
// per-service build status ("building", "build failed").
package gcb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	cloudbuild "google.golang.org/api/cloudbuild/v1"
)

// BuildStatus is the most recent build for one repo.
type BuildStatus struct {
	// Status is Cloud Build's raw status: QUEUED, PENDING, WORKING, SUCCESS,
	// FAILURE, INTERNAL_ERROR, TIMEOUT, CANCELLED, EXPIRED.
	Status string
	SHA    string // short commit SHA the build is building
	LogURL string // console link to the build log
	// StartTime is when the build began executing (zero while queued);
	// FinishTime is when it completed (zero while in progress).
	StartTime  time.Time
	FinishTime time.Time
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
	// RunTrigger starts the named trigger against branch; returns the build ID.
	RunTrigger(ctx context.Context, triggerID, branch string) (string, error)
	// GetBuild fetches one build's current status by ID.
	GetBuild(ctx context.Context, buildID string) (BuildStatus, error)
	// FindSuccessfulBuild reports whether triggerID has already built commit
	// successfully, and what short SHA that build produced.
	FindSuccessfulBuild(ctx context.Context, triggerID, commit string) (shortSHA string, found bool, err error)
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

// parseTime parses Cloud Build's RFC3339 timestamps; zero time on empty or
// malformed input.
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// previewTriggerSuffix is what every preview build's trigger is named with
// ("{service}-preview-build"). The convention is not incidental: the preview
// orchestrator looks its trigger IDs up by exactly this name (see
// preview.Orchestrator.TriggerIDs and main.go's previewTriggerIDs), and
// docs/preview-environments.md's onboarding steps require it, so a preview
// trigger that didn't match this would already be broken for other reasons.
const previewTriggerSuffix = "-preview-build"

// isPreviewBuild reports whether b came from a service's preview-build
// trigger rather than its mainline one.
//
// TRIGGER_NAME is a default substitution Cloud Build sets on every
// trigger-invoked build and returns in the Build resource, so this needs no
// extra API call and no trigger-ID plumbing. A build without it (a
// manually-submitted one) is not a preview build — but those carry no
// REPO_NAME either, so latestByRepo has already dropped them.
func isPreviewBuild(b *cloudbuild.Build) bool {
	return strings.HasSuffix(b.Substitutions["TRIGGER_NAME"], previewTriggerSuffix)
}

// latestByRepo maps repo name → newest build. Builds must be ordered newest
// first (the API's default ordering); the first build seen per repo wins.
//
// Preview builds are excluded outright, not merely ranked below mainline
// ones. They share their service's REPO_NAME with the mainline builds, so
// without this a failed preview build — someone's branch that doesn't
// compile — would become the newest build for that repo and light the
// service's Apps-tab BUILD cell up as "✗ build failed" while `main` was
// perfectly healthy: a preview failure masquerading as a fleet failure. The
// Apps tab is a view of the fleet, and a preview build is not a fleet fact;
// where it does belong is the Previews tab, which reads the build through the
// orchestrator (bifrost/error, log URL and all) rather than through here.
func latestByRepo(builds []*cloudbuild.Build) map[string]BuildStatus {
	out := map[string]BuildStatus{}
	for _, b := range builds {
		repo := b.Substitutions["REPO_NAME"]
		if repo == "" {
			continue // manually-submitted build, not from a trigger
		}
		if isPreviewBuild(b) {
			continue
		}
		if _, ok := out[repo]; ok {
			continue
		}
		out[repo] = BuildStatus{
			Status:     b.Status,
			SHA:        b.Substitutions["SHORT_SHA"],
			LogURL:     b.LogUrl,
			StartTime:  parseTime(b.StartTime),
			FinishTime: parseTime(b.FinishTime),
		}
	}
	return out
}

// buildIDFromOperation extracts the created build's ID from a trigger-run
// Operation's metadata (a cloudbuild.BuildOperationMetadata JSON blob).
func buildIDFromOperation(meta []byte) (string, error) {
	var m struct {
		Build struct {
			Id string `json:"id"`
		} `json:"build"`
	}
	if err := json.Unmarshal(meta, &m); err != nil {
		return "", err
	}
	if m.Build.Id == "" {
		return "", errors.New("operation metadata has no build id")
	}
	return m.Build.Id, nil
}

func (c *client) RunTrigger(ctx context.Context, triggerID, branch string) (string, error) {
	op, err := c.svc.Projects.Triggers.Run(c.project, triggerID,
		&cloudbuild.RepoSource{BranchName: branch}).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return buildIDFromOperation(op.Metadata)
}

// priorBuildPageSize bounds FindSuccessfulBuild's single List call. There is
// deliberately NO pagination: this is an optimization, and a commit whose
// successful build has fallen off the first page of its own trigger's history
// simply reads as "not built", which costs one rebuild and never a wrong
// answer. 100 is far more than the handful of preview builds a trigger
// accumulates between a teardown and the recreate this exists to serve —
// every preview trigger in this project has 13 successful builds in its
// entire history — while staying one page of one API call.
const priorBuildPageSize = 100

// priorBuildFilter is the server-side filter FindSuccessfulBuild sends: the
// build's trigger, and a terminal SUCCESS.
//
// Only these two fields are pushed to the server, and the omission of the
// commit is the interesting part. Cloud Build's filter grammar was probed
// against this project's real build history, and it splits into three tiers:
//
//   - trigger_id and status are honored, individually and ANDed together.
//     (`trigger_id=<api trigger> AND substitutions.COMMIT_SHA=<a bifrost
//     commit>` correctly returns nothing, so both halves really are applied.)
//   - the substitutions.* namespace is accepted and, for `=`, appears to work
//     — but it is NOT trustworthy. `substitutions.COMMIT_SHA!="<sha>"` returns
//     exactly the one build WITH that SHA: the API silently evaluates `!=` as
//     `=` rather than rejecting it. A grammar that answers a supported-looking
//     expression with the opposite of the truth is not one to push a
//     correctness-relevant predicate into.
//   - other shapes are rejected outright (`substitutions["KEY"]="v"`, `status
//     IN (...)`) or silently match nothing (`substitutions.COMMIT_SHA:"70c8"`).
//
// So the commit match happens CLIENT-SIDE, over the bounded page this filter
// returns (see successfulBuildFor). The server-side half is what keeps that
// page small and relevant; the client-side half is what makes the answer
// correct.
func priorBuildFilter(triggerID string) string {
	return fmt.Sprintf("trigger_id=%q AND status=%q", triggerID, "SUCCESS")
}

// FindSuccessfulBuild answers "has triggerID already built commit
// successfully, and what image tag did that build produce?".
//
// found=false with a nil error is the ORDINARY answer, not a failure: most
// commits have never been built. The three states callers must tell apart are
// therefore (shortSHA, true, nil) — reuse this tag; ("", false, nil) — no such
// build, go build one; and ("", false, err) — could not tell, which callers
// should also treat as "go build one" rather than as fatal.
//
// This answers "did a build for this commit succeed?", which is a PROXY for
// "does the image exist?". The two diverge only if the image is deleted after
// being built, and asking Artifact Registry directly would cost a new client,
// a new IAM grant and an infra deploy — where this reuses the Cloud Build
// service and viewer role bifrost already has. The exposure is identical to
// the one bifrost's namespace-annotation reuse path already carries and
// already documents: there is no Artifact Registry cleanup policy today, and
// if one is added, either path fails the same clean way (ImagePullBackOff,
// caught by the preview's readiness wait).
func (c *client) FindSuccessfulBuild(ctx context.Context, triggerID, commit string) (string, bool, error) {
	if triggerID == "" || commit == "" {
		return "", false, nil
	}
	resp, err := c.svc.Projects.Builds.List(c.project).
		Filter(priorBuildFilter(triggerID)).
		PageSize(priorBuildPageSize).
		Context(ctx).Do()
	if err != nil {
		return "", false, err
	}
	shortSHA, found := successfulBuildFor(resp.Builds, commit)
	return shortSHA, found, nil
}

// successfulBuildFor returns the short SHA of the first build in builds that
// succeeded at commit. Builds arrive newest first (the API's default
// ordering), so the first match is the most recent one — which matters only
// for picking a tag among identical candidates, since every successful build
// of the same commit produces the same $SHORT_SHA.
//
// Status is re-checked here rather than trusted from the server-side filter,
// and a build with no SHORT_SHA is passed over rather than returned empty: the
// caller turns this string straight into a `preview-<sha>` image tag, so
// "" would name an image that cannot exist. Every rejection lands on the same
// side — not found, therefore build it — which is the safe direction.
func successfulBuildFor(builds []*cloudbuild.Build, commit string) (string, bool) {
	for _, b := range builds {
		if b.Status != "SUCCESS" || b.Substitutions["COMMIT_SHA"] != commit {
			continue
		}
		if shortSHA := b.Substitutions["SHORT_SHA"]; shortSHA != "" {
			return shortSHA, true
		}
	}
	return "", false
}

func (c *client) GetBuild(ctx context.Context, buildID string) (BuildStatus, error) {
	b, err := c.svc.Projects.Builds.Get(c.project, buildID).Context(ctx).Do()
	if err != nil {
		return BuildStatus{}, err
	}
	return BuildStatus{
		Status:     b.Status,
		SHA:        b.Substitutions["SHORT_SHA"],
		LogURL:     b.LogUrl,
		StartTime:  parseTime(b.StartTime),
		FinishTime: parseTime(b.FinishTime),
	}, nil
}
