package gcb

import (
	"testing"

	cloudbuild "google.golang.org/api/cloudbuild/v1"
)

// build is a mainline (`{repo}-build` trigger) build, the kind the Apps tab
// exists to show.
func build(repo, sha, status string) *cloudbuild.Build {
	return &cloudbuild.Build{
		Status: status,
		LogUrl: "https://console.cloud.google.com/build/" + sha,
		Substitutions: map[string]string{
			"REPO_NAME": repo, "SHORT_SHA": sha, "TRIGGER_NAME": repo + "-build",
		},
	}
}

// previewBuild is the same thing from the `{repo}-preview-build` trigger:
// identical REPO_NAME, so only TRIGGER_NAME tells the two apart. Substitution
// names and values match what the Cloud Build API really returns for these
// triggers.
func previewBuild(repo, sha, status string) *cloudbuild.Build {
	b := build(repo, sha, status)
	b.Substitutions["TRIGGER_NAME"] = repo + "-preview-build"
	return b
}

func TestLatestByRepo(t *testing.T) {
	// Newest first, as the API returns them.
	builds := []*cloudbuild.Build{
		build("bifrost", "abc1234", "WORKING"),
		build("bifrost", "0ldbu1ld", "SUCCESS"), // older, must lose to WORKING
		build("identity", "def5678", "FAILURE"),
		{Status: "SUCCESS"}, // manual build, no substitutions → skipped
	}
	got := latestByRepo(builds)
	if len(got) != 2 {
		t.Fatalf("got %d repos, want 2: %v", len(got), got)
	}
	if b := got["bifrost"]; b.Status != "WORKING" || b.SHA != "abc1234" {
		t.Errorf("bifrost = %+v, want newest WORKING abc1234", b)
	}
	if b := got["identity"]; b.Status != "FAILURE" || b.SHA != "def5678" {
		t.Errorf("identity = %+v, want FAILURE def5678", b)
	}
}

// A failed preview build must not become its service's fleet build status:
// the Apps tab would otherwise read "✗ build failed" for a service whose
// main is perfectly healthy.
func TestLatestByRepoExcludesPreviewBuilds(t *testing.T) {
	builds := []*cloudbuild.Build{
		previewBuild("footstrike-api", "bad0000", "FAILURE"), // newest, someone's branch
		build("footstrike-api", "g00d123", "SUCCESS"),        // the real mainline state
		previewBuild("identity", "prev111", "SUCCESS"),       // only ever preview-built
	}
	got := latestByRepo(builds)

	b, ok := got["footstrike-api"]
	if !ok {
		t.Fatalf("footstrike-api missing from %v, want its mainline build", got)
	}
	if b.Status != "SUCCESS" || b.SHA != "g00d123" {
		t.Errorf("footstrike-api = %+v, want the mainline SUCCESS g00d123, not the failed preview build", b)
	}
	if _, ok := got["identity"]; ok {
		t.Errorf("identity = %+v, want no entry at all: its only build is a preview build", got["identity"])
	}
}

func TestStatePredicates(t *testing.T) {
	cases := []struct {
		status     string
		inProgress bool
		failed     bool
	}{
		{"QUEUED", true, false},
		{"PENDING", true, false},
		{"WORKING", true, false},
		{"SUCCESS", false, false},
		{"CANCELLED", false, false}, // deliberate, not a failure
		{"FAILURE", false, true},
		{"INTERNAL_ERROR", false, true},
		{"TIMEOUT", false, true},
		{"EXPIRED", false, true},
		{"", false, false}, // zero value: no recent build
	}
	for _, tc := range cases {
		b := BuildStatus{Status: tc.status}
		if b.InProgress() != tc.inProgress {
			t.Errorf("%q InProgress = %v, want %v", tc.status, b.InProgress(), tc.inProgress)
		}
		if b.Failed() != tc.failed {
			t.Errorf("%q Failed = %v, want %v", tc.status, b.Failed(), tc.failed)
		}
	}
}

func TestBuildIDFromOperation(t *testing.T) {
	cases := []struct {
		name    string
		meta    string
		want    string
		wantErr bool
	}{
		{"normal", `{"build":{"id":"abc-123"}}`, "abc-123", false},
		{"missing build", `{}`, "", true},
		{"empty id", `{"build":{"id":""}}`, "", true},
		{"malformed", `not-json`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildIDFromOperation([]byte(tc.meta))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("id = %q, want %q", got, tc.want)
			}
		})
	}
}
