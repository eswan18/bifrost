package commitlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- pure logic ---------------------------------------------------------

func TestParsePR(t *testing.T) {
	tests := []struct {
		subject string
		want    int
	}{
		{"Implement issue #221 (#277)", 277},
		{"Fix the thing (#5)", 5},
		// Trailing whitespace is not the author's intent.
		{"Fix the thing (#5)  ", 5},
		// Only the squash suffix counts: a number in the prose is an issue
		// reference, and reading it as the PR would flag the wrong thing.
		{"Implement issue #221", 0},
		{"Revert (#12) because of the outage", 0},
		{"Merge pull request #88 from eswan18/branch", 0},
		// The space before the parenthesis is part of the convention.
		{"Fix(#5)", 0},
		{"Fix (#abc)", 0},
		{"Fix (#)", 0},
		{"", 0},
	}
	for _, tc := range tests {
		if got := ParsePR(tc.subject); got != tc.want {
			t.Errorf("ParsePR(%q) = %d, want %d", tc.subject, got, tc.want)
		}
	}
}

// TestClassify pins the priority order as well as the four outcomes: every
// commit is exactly one kind, and Unread beats the author-based ones.
func TestClassify(t *testing.T) {
	unreviewed := map[int]bool{277: true, 300: true}
	tests := []struct {
		name string
		c    Commit
		want Kind
	}{
		{"labelled PR by the factory bot is unread, not automation", Commit{PR: 277, Author: "haruspex-factory[bot]", Bot: true}, Unread},
		{"labelled PR by a person is still unread", Commit{PR: 300, Author: "eswan18"}, Unread},
		{"labelled PR by dependabot is unread, not a bump", Commit{PR: 277, Author: BumpBotLogin, Bot: true}, Unread},
		{"dependabot is a bump", Commit{PR: 12, Author: BumpBotLogin, Bot: true}, Bump},
		// The login decides, not the type: the category is "dependency bump".
		{"dependabot is a bump even if the type is missing", Commit{Author: BumpBotLogin}, Bump},
		{"another bot is automation", Commit{PR: 13, Author: "haruspex-factory[bot]", Bot: true}, Automation},
		{"a person is yours", Commit{PR: 14, Author: "eswan18"}, Yours},
		{"no PR and no account is yours", Commit{Author: "Ethan Swan"}, Yours},
		// PR 0 means "no PR"; it must never match a map entry.
		{"no PR is never unread", Commit{PR: 0, Author: "eswan18"}, Yours},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.c, unreviewed); got != tc.want {
				t.Errorf("Classify = %v, want %v", got, tc.want)
			}
		})
	}
	// A nil label set is what Between passes when no commit names a PR.
	if got := Classify(Commit{PR: 277, Bot: true}, nil); got != Automation {
		t.Errorf("Classify with no labels = %v, want Automation", got)
	}
}

func TestRangeSummaries(t *testing.T) {
	r := Range{Total: 5, Commits: []Commit{
		{PR: 9, Kind: Unread},
		{PR: 8, Kind: Bump},
		{PR: 7, Kind: Unread},
		// A PR merged without squashing: two commits, one PR to mark.
		{PR: 9, Kind: Unread},
		{Kind: Yours},
	}}
	if got := r.Count(Unread); got != 3 {
		t.Errorf("Count(Unread) = %d, want 3", got)
	}
	if got := r.Count(Automation); got != 0 {
		t.Errorf("Count(Automation) = %d, want 0", got)
	}
	if got, want := r.UnreadPRs(), []int{9, 7}; !slices.Equal(got, want) {
		t.Errorf("UnreadPRs = %v, want %v (distinct, in commit order)", got, want)
	}
	if r.Truncated() {
		t.Error("Truncated = true for a complete range")
	}
	if !(Range{Total: 300, Commits: make([]Commit, CompareCap)}).Truncated() {
		t.Error("Truncated = false for a capped range")
	}
}

// ---- the HTTP layer -----------------------------------------------------

// fakeGitHub is an httptest GitHub: a compare response per "base...head", a
// set of labelled issues served in pages, and a status per label removal. It
// records every request, because "the label lookup was skipped" and "the
// listing was paginated" are properties no return value can show.
type fakeGitHub struct {
	t        *testing.T
	compare  map[string]string // "base...head" -> JSON body
	issues   []string          // JSON objects, served labelPageSize per page
	deletes  map[string]int    // path -> status
	wantAuth string            // exact Authorization header expected ("" = none)

	mu       sync.Mutex
	requests []string // "METHOD path?query"
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	f.mu.Unlock()
	if got := r.Header.Get("Authorization"); got != f.wantAuth {
		f.t.Errorf("Authorization = %q, want %q", got, f.wantAuth)
	}

	const repoPrefix = "/repos/eswan18/haruspex/"
	rest, ok := strings.CutPrefix(r.URL.Path, repoPrefix)
	if !ok {
		f.t.Errorf("unexpected path %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "compare/"):
		body, ok := f.compare[strings.TrimPrefix(rest, "compare/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	case r.Method == http.MethodGet && rest == "issues":
		q := r.URL.Query()
		if q.Get("labels") != UnreviewedLabel || q.Get("state") != "closed" || q.Get("per_page") != "100" {
			f.t.Errorf("label lookup query = %q, want labels=%s&state=closed&per_page=100", r.URL.RawQuery, UnreviewedLabel)
		}
		page := 1
		if _, err := fmt.Sscanf(q.Get("page"), "%d", &page); err != nil {
			f.t.Errorf("page = %q: %v", q.Get("page"), err)
		}
		start := min((page-1)*labelPageSize, len(f.issues))
		end := min(start+labelPageSize, len(f.issues))
		_, _ = w.Write([]byte("[" + strings.Join(f.issues[start:end], ",") + "]"))
	case r.Method == http.MethodDelete:
		status, ok := f.deletes[rest]
		if !ok {
			f.t.Errorf("unexpected DELETE %s", r.URL.Path)
			status = http.StatusInternalServerError
		}
		w.WriteHeader(status)
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func (f *fakeGitHub) seen(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.Contains(r, substr) {
			n++
		}
	}
	return n
}

func serve(t *testing.T, f *fakeGitHub, token string) *Client {
	t.Helper()
	f.t = t
	if token != "" {
		f.wantAuth = "Bearer " + token
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return NewWithBaseURL(token, srv.URL)
}

// rawCommit is one entry of a compare response.
func rawCommit(sha, message, login, typ string) string {
	author := "null"
	if login != "" {
		author = fmt.Sprintf(`{"login":%q,"type":%q}`, login, typ)
	}
	return fmt.Sprintf(`{"sha":%q,"commit":{"message":%q,"author":{"name":"Git Name"}},"author":%s}`, sha, message, author)
}

func compareBody(total int, commits ...string) string {
	return fmt.Sprintf(`{"total_commits":%d,"commits":[%s]}`, total, strings.Join(commits, ","))
}

func prIssue(n int) string { return fmt.Sprintf(`{"number":%d,"pull_request":{"url":"x"}}`, n) }

// TestBetween is the whole read path against one realistic range: order,
// abbreviation, subject, PR parsing, author fallback and all four kinds.
func TestBetween(t *testing.T) {
	gh := &fakeGitHub{
		compare: map[string]string{"aaaaaaa...bbbbbbb": compareBody(5,
			// GitHub's order: oldest first.
			rawCommit("1111111aaaaaaaaaaaaa", "Hand-written change (#270)\n\nBody text (#999)", "eswan18", "User"),
			rawCommit("2222222aaaaaaaaaaaaa", "Bump next from 15.1 to 15.2 (#275)", "dependabot[bot]", "Bot"),
			rawCommit("3333333aaaaaaaaaaaaa", "Regenerate lockfile", "github-actions[bot]", "Bot"),
			rawCommit("4444444aaaaaaaaaaaaa", "Implement issue #221 (#277)", "haruspex-factory[bot]", "Bot"),
			rawCommit("5555555aaaaaaaaaaaaa", "Direct push", "", ""),
		)},
		// 221 is a labelled ISSUE, not a PR, and 900 is a labelled PR that is
		// not in this range. Neither may make anything unread.
		issues: []string{prIssue(277), `{"number":221}`, prIssue(900)},
	}
	c := serve(t, gh, "test-token")

	got, err := c.Between(context.Background(), "eswan18", "haruspex", "aaaaaaa", "bbbbbbb")
	if err != nil {
		t.Fatalf("Between: %v", err)
	}
	want := []Commit{
		{SHA: "5555555", Subject: "Direct push", Author: "Git Name", Kind: Yours},
		{SHA: "4444444", Subject: "Implement issue #221 (#277)", PR: 277, Author: "haruspex-factory[bot]", Bot: true, Kind: Unread},
		{SHA: "3333333", Subject: "Regenerate lockfile", Author: "github-actions[bot]", Bot: true, Kind: Automation},
		{SHA: "2222222", Subject: "Bump next from 15.1 to 15.2 (#275)", PR: 275, Author: "dependabot[bot]", Bot: true, Kind: Bump},
		{SHA: "1111111", Subject: "Hand-written change (#270)", PR: 270, Author: "eswan18", Kind: Yours},
	}
	if !slices.Equal(got.Commits, want) {
		t.Errorf("commits (newest first) =\n %+v\nwant\n %+v", got.Commits, want)
	}
	if got.Total != 5 || got.Truncated() {
		t.Errorf("Total = %d, Truncated = %v; want 5, false", got.Total, got.Truncated())
	}
	if prs := got.UnreadPRs(); !slices.Equal(prs, []int{277}) {
		t.Errorf("UnreadPRs = %v, want [277]", prs)
	}
}

// TestCompareCap: GitHub stops at 250 commits and total_commits keeps telling
// the truth. The range must carry both numbers so the caller can say so.
func TestCompareCap(t *testing.T) {
	commits := make([]string, 0, CompareCap)
	for i := range CompareCap {
		commits = append(commits, rawCommit(fmt.Sprintf("%07d", i), "change", "eswan18", "User"))
	}
	gh := &fakeGitHub{compare: map[string]string{"a...b": compareBody(312, commits...)}}
	c := serve(t, gh, "")

	got, err := c.Compare(context.Background(), "eswan18", "haruspex", "a", "b")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(got.Commits) != CompareCap || got.Total != 312 || !got.Truncated() {
		t.Errorf("returned %d of Total %d, Truncated = %v; want 250 of 312, true", len(got.Commits), got.Total, got.Truncated())
	}
	// Newest first: the last one GitHub sent is the first one here.
	if got.Commits[0].SHA != fmt.Sprintf("%07d", CompareCap-1) {
		t.Errorf("first commit = %s, want the newest", got.Commits[0].SHA)
	}
}

// TestCompareWithoutTotal: a response missing total_commits must not make a
// full list look like an empty range.
func TestCompareWithoutTotal(t *testing.T) {
	gh := &fakeGitHub{compare: map[string]string{"a...b": `{"commits":[` + rawCommit("1234567", "x", "eswan18", "User") + `]}`}}
	c := serve(t, gh, "")
	got, err := c.Compare(context.Background(), "eswan18", "haruspex", "a", "b")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if got.Total != 1 || got.Truncated() {
		t.Errorf("Total = %d, Truncated = %v; want 1, false", got.Total, got.Truncated())
	}
}

// TestUnreviewedPRsPaginates: 100 per page until a short one, PRs only.
func TestUnreviewedPRsPaginates(t *testing.T) {
	var issues []string
	for n := 1; n <= 230; n++ {
		if n%10 == 0 {
			issues = append(issues, fmt.Sprintf(`{"number":%d}`, n)) // a plain issue
			continue
		}
		issues = append(issues, prIssue(n))
	}
	gh := &fakeGitHub{issues: issues}
	c := serve(t, gh, "tok")

	got, err := c.UnreviewedPRs(context.Background(), "eswan18", "haruspex")
	if err != nil {
		t.Fatalf("UnreviewedPRs: %v", err)
	}
	if len(got) != 207 {
		t.Errorf("got %d PRs, want 207 (230 entries less 23 plain issues)", len(got))
	}
	if !got[1] || !got[229] || got[10] || got[230] {
		t.Errorf("membership wrong: 1=%v 229=%v 10=%v 230=%v", got[1], got[229], got[10], got[230])
	}
	if n := gh.seen("GET /repos/eswan18/haruspex/issues"); n != 3 {
		t.Errorf("made %d page requests, want 3 (100, 100, 30)", n)
	}

	// The answer is cached for the life of the client: a second range of the
	// same repo must not list the labels again.
	if _, err := c.UnreviewedPRs(context.Background(), "eswan18", "haruspex"); err != nil {
		t.Fatalf("second UnreviewedPRs: %v", err)
	}
	if n := gh.seen("/issues"); n != 3 {
		t.Errorf("made %d page requests after a repeat call, want still 3", n)
	}
}

// TestUnreviewedPRsExactPage: a listing that is exactly one full page needs a
// second request to learn that it ended, and must stop there.
func TestUnreviewedPRsExactPage(t *testing.T) {
	var issues []string
	for n := 1; n <= labelPageSize; n++ {
		issues = append(issues, prIssue(n))
	}
	gh := &fakeGitHub{issues: issues}
	c := serve(t, gh, "")
	got, err := c.UnreviewedPRs(context.Background(), "eswan18", "haruspex")
	if err != nil {
		t.Fatalf("UnreviewedPRs: %v", err)
	}
	if len(got) != labelPageSize || gh.seen("/issues") != 2 {
		t.Errorf("got %d PRs in %d requests, want 100 in 2", len(got), gh.seen("/issues"))
	}
}

// TestBetweenSkipsLabelsWhenNoCommitNamesAPR: most of the fleet has no factory
// and no squash suffixes, and should cost one request, not two.
func TestBetweenSkipsLabelsWhenNoCommitNamesAPR(t *testing.T) {
	gh := &fakeGitHub{compare: map[string]string{"a...b": compareBody(1, rawCommit("1234567", "Direct push", "eswan18", "User"))}}
	c := serve(t, gh, "")
	if _, err := c.Between(context.Background(), "eswan18", "haruspex", "a", "b"); err != nil {
		t.Fatalf("Between: %v", err)
	}
	if n := gh.seen("/issues"); n != 0 {
		t.Errorf("made %d label requests for a range with no PRs, want 0", n)
	}
}

// TestBetweenFailsWhenLabelsCannotBeRead: "0 unread" about a range whose
// labels were never read would be a false all-clear, so it is an error.
func TestBetweenFailsWhenLabelsCannotBeRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/compare/") {
			_, _ = w.Write([]byte(compareBody(1, rawCommit("1234567", "Change (#5)", "eswan18", "User"))))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	_, err := NewWithBaseURL("", srv.URL).Between(context.Background(), "eswan18", "haruspex", "a", "b")
	if err == nil || !strings.Contains(err.Error(), "label lookup returned 502") {
		t.Errorf("err = %v, want the label lookup's 502", err)
	}
}

func TestMarkRead(t *testing.T) {
	const path = "issues/277/labels/unreviewed"
	tests := []struct {
		name    string
		status  int
		wantErr string
	}{
		{"200 with the remaining labels is success", http.StatusOK, ""},
		{"204 is success", http.StatusNoContent, ""},
		// Already gone is the state being asked for.
		{"404 counts as already done", http.StatusNotFound, ""},
		{"403 is a failure", http.StatusForbidden, "github label removal returned 403"},
		{"500 is a failure", http.StatusInternalServerError, "github label removal returned 500"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gh := &fakeGitHub{deletes: map[string]int{path: tc.status}}
			c := serve(t, gh, "tok")
			err := c.MarkRead(context.Background(), "eswan18", "haruspex", 277)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("MarkRead: %v, want success", err)
			case tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr):
				t.Errorf("MarkRead err = %v, want %q", err, tc.wantErr)
			}
			if n := gh.seen("DELETE /repos/eswan18/haruspex/" + path); n != 1 {
				t.Errorf("made %d DELETEs, want 1", n)
			}
		})
	}
}

// TestErrorsNeverCarryTheToken: the failure text ends up on a terminal.
func TestErrorsNeverCarryTheToken(t *testing.T) {
	const token = "ghp_supersecretvalue"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := NewWithBaseURL(token, srv.URL)
	for _, err := range []error{
		func() error { _, err := c.Compare(context.Background(), "o", "r", "a", "b"); return err }(),
		func() error { _, err := c.UnreviewedPRs(context.Background(), "o", "r"); return err }(),
		c.MarkRead(context.Background(), "o", "r", 1),
	} {
		if err == nil {
			t.Fatal("expected an error from a 401")
		}
		if strings.Contains(err.Error(), token) || strings.Contains(Reason(err), token) {
			t.Errorf("error %q carries the token", err)
		}
	}
}

func TestRateLimitIsNamed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "API rate limit exceeded"})
	}))
	defer srv.Close()
	_, err := NewWithBaseURL("", srv.URL).Compare(context.Background(), "o", "r", "a", "b")
	if err == nil || err.Error() != "github compare: rate limit exceeded" {
		t.Errorf("err = %v, want the rate limit named", err)
	}
}

// TestCompareHonoursTheContext: the callers' few-second budget is a context
// deadline, and a GitHub that never answers must be cut off by it.
func TestCompareHonoursTheContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := NewWithBaseURL("", srv.URL).Compare(ctx, "o", "r", "a", "b")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %s to give up on a 20ms deadline", time.Since(start))
	}
	if got := Reason(err); got != "timed out" {
		t.Errorf("Reason = %q, want %q", got, "timed out")
	}
}

func TestReason(t *testing.T) {
	if got := Reason(nil); got != "" {
		t.Errorf("Reason(nil) = %q", got)
	}
	if got := Reason(errors.New("github compare returned 404")); got != "github compare returned 404" {
		t.Errorf("Reason = %q", got)
	}
	// A transport failure is reported by its cause, without the URL.
	_, err := NewWithBaseURL("", "http://127.0.0.1:1").Compare(context.Background(), "o", "r", "a", "b")
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if got := Reason(err); strings.Contains(got, "http://") || got == "" {
		t.Errorf("Reason = %q, want the cause without the URL", got)
	}
}
