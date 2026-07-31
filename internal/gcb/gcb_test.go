package gcb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	cloudbuild "google.golang.org/api/cloudbuild/v1"
	"google.golang.org/api/option"
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

// ---- FindSuccessfulBuild ----------------------------------------------------

// commitBuild is a preview build as the API really returns one: COMMIT_SHA
// (the full SHA, what the caller matches on) and SHORT_SHA (what it wants
// back) are separate substitutions, and are deliberately unrelated strings
// here so an implementation that derived one from the other cannot pass.
func commitBuild(commit, shortSHA, status string) *cloudbuild.Build {
	return &cloudbuild.Build{
		Status: status,
		Substitutions: map[string]string{
			"COMMIT_SHA": commit, "SHORT_SHA": shortSHA,
			"TRIGGER_NAME": "footstrike-api-preview-build",
		},
	}
}

// TestPriorBuildFilterPushesOnlyTriggerAndStatus pins which predicates go to
// the server, and — the load-bearing half — which one deliberately does not.
//
// trigger_id and status are honored by Cloud Build's filter grammar; the
// substitutions.* namespace is accepted but silently mis-evaluates `!=` as
// `=`, so the commit match is done client-side instead (see priorBuildFilter).
// A filter that quietly grew a substitutions clause would look like it worked
// — the server would accept it — while resting on an expression the API is not
// contractually evaluating.
func TestPriorBuildFilterPushesOnlyTriggerAndStatus(t *testing.T) {
	got := priorBuildFilter("2648d672-e8c9-490a-81e3-6831a1c24c56")
	want := `trigger_id="2648d672-e8c9-490a-81e3-6831a1c24c56" AND status="SUCCESS"`
	if got != want {
		t.Errorf("priorBuildFilter = %q, want %q", got, want)
	}
	if strings.Contains(got, "substitutions") {
		t.Errorf("priorBuildFilter = %q, want no substitutions clause: Cloud Build accepts those but does not reliably evaluate them", got)
	}
}

func TestSuccessfulBuildFor(t *testing.T) {
	const commit = "c58881f6458cc5fd4ada8fd82bf1c8eaa8a7d9e2"
	cases := []struct {
		name   string
		builds []*cloudbuild.Build
		want   string
		found  bool
	}{
		{
			name:   "match",
			builds: []*cloudbuild.Build{commitBuild(commit, "c58881f", "SUCCESS")},
			want:   "c58881f", found: true,
		},
		{
			name:   "no build for this commit",
			builds: []*cloudbuild.Build{commitBuild("0000000000000000000000000000000000000000", "0000000", "SUCCESS")},
			want:   "", found: false,
		},
		{
			// The whole point of the SUCCESS test: a commit whose only build
			// FAILED has no image, and reusing its tag would apply a
			// Deployment naming something that was never pushed.
			name:   "only a failed build at this commit",
			builds: []*cloudbuild.Build{commitBuild(commit, "c58881f", "FAILURE")},
			want:   "", found: false,
		},
		{
			name: "a failed build does not shadow a successful one",
			builds: []*cloudbuild.Build{
				commitBuild(commit, "c58881f", "FAILURE"),
				commitBuild(commit, "c58881f", "SUCCESS"),
			},
			want: "c58881f", found: true,
		},
		{
			// In progress is not built yet either — no image has been pushed.
			name:   "still building",
			builds: []*cloudbuild.Build{commitBuild(commit, "c58881f", "WORKING")},
			want:   "", found: false,
		},
		{
			// Newest first is the API's ordering, so the first match wins.
			name: "newest match wins",
			builds: []*cloudbuild.Build{
				commitBuild(commit, "newest", "SUCCESS"),
				commitBuild(commit, "older", "SUCCESS"),
			},
			want: "newest", found: true,
		},
		{
			// The caller turns this straight into a `preview-<sha>` tag, so an
			// empty SHORT_SHA must read as not-found rather than as a match
			// naming an image that cannot exist.
			name:   "successful build with no short sha",
			builds: []*cloudbuild.Build{commitBuild(commit, "", "SUCCESS")},
			want:   "", found: false,
		},
		{name: "no builds at all", builds: nil, want: "", found: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := successfulBuildFor(tc.builds, commit)
			if got != tc.want || found != tc.found {
				t.Errorf("successfulBuildFor = (%q, %v), want (%q, %v)", got, found, tc.want, tc.found)
			}
		})
	}
}

// buildListServer stands in for the Cloud Build API's builds.list endpoint,
// recording every request it receives so a test can assert what really went
// over the wire, and returning builds regardless of the filter — the server
// side is what's being observed here, not re-implemented.
type buildListServer struct {
	queries []url.Values
	builds  []*cloudbuild.Build
	// nextPageToken, if set, is returned on every response: a caller that
	// paginates would come back for more, which is exactly what must not
	// happen.
	nextPageToken string
}

func (s *buildListServer) client(t *testing.T) *client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.queries = append(s.queries, r.URL.Query())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cloudbuild.ListBuildsResponse{
			Builds: s.builds, NextPageToken: s.nextPageToken,
		})
	}))
	t.Cleanup(srv.Close)
	svc, err := cloudbuild.NewService(context.Background(),
		option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("cloudbuild.NewService: %v", err)
	}
	return &client{svc: svc, project: "ethans-services"}
}

// TestFindSuccessfulBuildQueriesAndMatches is the end-to-end check on the real
// client: what it sends (the filter, a bounded page size) and what it does
// with the answer (match the commit locally). The wire-level assertions are
// the reason this goes through a real service rather than calling
// successfulBuildFor directly — nothing else can see that the filter was
// actually attached to the request, or that the page is bounded.
func TestFindSuccessfulBuildQueriesAndMatches(t *testing.T) {
	const commit = "c58881f6458cc5fd4ada8fd82bf1c8eaa8a7d9e2"
	s := &buildListServer{builds: []*cloudbuild.Build{
		commitBuild("0000000000000000000000000000000000000000", "0000000", "SUCCESS"),
		commitBuild(commit, "c58881f", "SUCCESS"),
	}}
	c := s.client(t)

	got, found, err := c.FindSuccessfulBuild(context.Background(), "trig-api", commit)
	if err != nil {
		t.Fatalf("FindSuccessfulBuild: %v", err)
	}
	if !found || got != "c58881f" {
		t.Errorf("FindSuccessfulBuild = (%q, %v), want (%q, true)", got, found, "c58881f")
	}
	if len(s.queries) != 1 {
		t.Fatalf("builds.list called %d times, want exactly 1", len(s.queries))
	}
	// Spelled out literally rather than via priorBuildFilter: a test that
	// compared the wire against the same function that built it would agree
	// with any filter at all, including one that stopped narrowing.
	q := s.queries[0]
	if want := `trigger_id="trig-api" AND status="SUCCESS"`; q.Get("filter") != want {
		t.Errorf("filter sent = %q, want %q — the server-side narrowing is what keeps the page small", q.Get("filter"), want)
	}
	if q.Get("pageSize") == "" {
		t.Error("no pageSize sent: the lookup must bound how much history it pulls")
	}
}

// TestFindSuccessfulBuildNotFoundIsNotAnError pins the distinction the whole
// return signature exists for: a commit nobody has built is the ORDINARY
// answer, and must be reported as (found=false, err=nil) rather than as a
// failure — the caller treats an error as "could not tell" and logs it, which
// would turn every first-ever build into a warning.
func TestFindSuccessfulBuildNotFoundIsNotAnError(t *testing.T) {
	s := &buildListServer{builds: []*cloudbuild.Build{
		commitBuild("0000000000000000000000000000000000000000", "0000000", "SUCCESS"),
	}}
	got, found, err := s.client(t).FindSuccessfulBuild(context.Background(), "trig-api", "beef99887766554433")
	if err != nil {
		t.Errorf("err = %v, want nil: a commit with no build is not a failure", err)
	}
	if found || got != "" {
		t.Errorf("FindSuccessfulBuild = (%q, %v), want (\"\", false)", got, found)
	}
}

// TestFindSuccessfulBuildDoesNotPaginate holds the work bounded. The server
// hands back a nextPageToken on every response, so a caller that followed it
// would loop forever; one page is the deliberate limit, since a commit whose
// build has fallen off it merely reads as "not built" and costs one rebuild.
func TestFindSuccessfulBuildDoesNotPaginate(t *testing.T) {
	s := &buildListServer{
		builds:        []*cloudbuild.Build{commitBuild("0000", "0000", "SUCCESS")},
		nextPageToken: "there-is-always-more",
	}
	if _, _, err := s.client(t).FindSuccessfulBuild(context.Background(), "trig-api", "nope"); err != nil {
		t.Fatalf("FindSuccessfulBuild: %v", err)
	}
	if len(s.queries) != 1 {
		t.Errorf("builds.list called %d times, want exactly 1 — the lookup must not follow nextPageToken", len(s.queries))
	}
}

// TestFindSuccessfulBuildSkipsTheCallWhenItCannotAnswer: with no trigger or no
// commit there is nothing to ask about, and the API call would be wasted.
func TestFindSuccessfulBuildSkipsTheCallWhenItCannotAnswer(t *testing.T) {
	for _, tc := range []struct{ name, triggerID, commit string }{
		{"no trigger", "", "c58881f6458cc5fd4ada8fd82bf1c8eaa8a7d9e2"},
		{"no commit", "trig-api", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &buildListServer{}
			got, found, err := s.client(t).FindSuccessfulBuild(context.Background(), tc.triggerID, tc.commit)
			if err != nil || found || got != "" {
				t.Errorf("FindSuccessfulBuild = (%q, %v, %v), want (\"\", false, nil)", got, found, err)
			}
			if len(s.queries) != 0 {
				t.Errorf("builds.list called %d times, want 0", len(s.queries))
			}
		})
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
