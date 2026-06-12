package gcb

import (
	"testing"

	cloudbuild "google.golang.org/api/cloudbuild/v1"
)

func build(repo, sha, status string) *cloudbuild.Build {
	return &cloudbuild.Build{
		Status:        status,
		LogUrl:        "https://console.cloud.google.com/build/" + sha,
		Substitutions: map[string]string{"REPO_NAME": repo, "SHORT_SHA": sha},
	}
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
