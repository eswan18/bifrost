package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eswan18/bifrost/internal/auth"
	"github.com/eswan18/bifrost/internal/commitlog"
)

// fakeCommits is the CommitReader the tests drive. between answers every
// range; delay makes Between slow (it honours the context, as the real client
// does); markErr fails every MarkRead.
type fakeCommits struct {
	mu      sync.Mutex
	rng     commitlog.Range
	err     error
	delay   time.Duration
	markErr error

	betweenCalls []string // "owner/repo base...head"
	marked       []int
}

func (f *fakeCommits) Between(ctx context.Context, owner, repo, base, head string) (commitlog.Range, error) {
	f.mu.Lock()
	f.betweenCalls = append(f.betweenCalls, fmt.Sprintf("%s/%s %s...%s", owner, repo, base, head))
	rng, err, delay := f.rng, f.err, f.delay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return commitlog.Range{}, ctx.Err()
		}
	}
	return rng, err
}

func (f *fakeCommits) MarkRead(_ context.Context, _, _ string, pr int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markErr != nil {
		return f.markErr
	}
	f.marked = append(f.marked, pr)
	return nil
}

func (f *fakeCommits) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.betweenCalls)
}

func (f *fakeCommits) set(r commitlog.Range) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rng = r
}

// unreadRange is three commits: an unread factory PR, a dependency bump and
// one of the owner's own with no PR.
func unreadRange() commitlog.Range {
	return commitlog.Range{Total: 3, Commits: []commitlog.Commit{
		{SHA: "b6e95f4", Subject: "Implement issue #221 (#277)", PR: 277, Author: "haruspex-factory[bot]", Bot: true, Kind: commitlog.Unread},
		{SHA: "0c1d2e3", Subject: "Bump next from 15.1 to 15.2 (#276)", PR: 276, Author: "dependabot[bot]", Bot: true, Kind: commitlog.Bump},
		{SHA: "9f8e7d6", Subject: "Fix the thing", Author: "eswan18", Kind: commitlog.Yours},
	}}
}

func readRange() commitlog.Range {
	r := unreadRange()
	r.Commits = r.Commits[1:]
	r.Total = 2
	return r
}

// driftKube is the one-service fleet with staging ahead of prod.
func driftKube() *fakeKube {
	return &fakeKube{imgs: map[string][]string{
		"foo-staging": {"reg/foo:abc1234"},
		"foo-prod":    {"reg/foo:def5678"},
	}}
}

func driftHandlers(t *testing.T, fc *fakeCommits) (*Handlers, *fakeKube, *auth.Session) {
	t.Helper()
	k := driftKube()
	h, sess := newTestHandlers(t, k)
	if fc != nil {
		h.Commits = fc
	}
	return h, k, sess
}

func appsBody(t *testing.T, h *Handlers, sess *auth.Session) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Apps(rec, authed(t, "GET", "/apps", "", sess))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	return rec.Body.String()
}

// --- fleet assembly -----------------------------------------------------------

func TestAttachChangesUnread(t *testing.T) {
	fc := &fakeCommits{rng: unreadRange()}
	h, _, _ := driftHandlers(t, fc)
	f := h.assembleFleet(context.Background())

	// Owner and repo come from the registry (foo → foo_repo), the range from
	// the prod and staging tags.
	if want := []string{"eswan18/foo_repo def5678...abc1234"}; !reflect.DeepEqual(fc.betweenCalls, want) {
		t.Errorf("Between calls = %v, want %v", fc.betweenCalls, want)
	}
	c := f.Apps[0].Changes
	if c == nil || c.Unavailable {
		t.Fatalf("Changes = %+v", c)
	}
	if c.Note != "3 to promote" || c.UnreadNote != "1 unread" {
		t.Errorf("note = %q / %q", c.Note, c.UnreadNote)
	}
	if c.UnreadHead != "1 change nobody has read" || c.Confirm != "Promote and mark 1 as read" {
		t.Errorf("head = %q, confirm = %q", c.UnreadHead, c.Confirm)
	}
	if c.Summary != "Also 1 dependency bump and 1 by you" {
		t.Errorf("summary = %q", c.Summary)
	}
	if !reflect.DeepEqual(c.UnreadPRs, []int{277}) || len(c.Unread) != 1 || len(c.Commits) != 3 {
		t.Fatalf("unread = %v, rows = %d, commits = %d", c.UnreadPRs, len(c.Unread), len(c.Commits))
	}
	row := c.Unread[0]
	if row.Title != "Implement issue #221" || row.Author != "haruspex-factory[bot]" ||
		row.URL != "https://github.com/eswan18/foo_repo/pull/277" {
		t.Errorf("unread row = %+v", row)
	}
	// No PR → the commit itself.
	if got := c.Commits[2].URL; got != "https://github.com/eswan18/foo_repo/commit/9f8e7d6" {
		t.Errorf("commit URL = %q", got)
	}
	if c.AllLabel != "All 3 commits" || c.Truncated != "" {
		t.Errorf("all = %q, truncated = %q", c.AllLabel, c.Truncated)
	}
	// The overview's attention item carries the same note.
	if a := f.Overview.Attention[0]; a.Note != "3 to promote" || a.Unread != "1 unread" {
		t.Errorf("attention = %+v", a)
	}
}

func TestAttachChangesNoneUnread(t *testing.T) {
	h, _, _ := driftHandlers(t, &fakeCommits{rng: readRange()})
	c := h.assembleFleet(context.Background()).Apps[0].Changes
	if c == nil {
		t.Fatal("Changes = nil")
	}
	if c.UnreadNote != "" || c.Confirm != "" || len(c.Unread) != 0 || len(c.UnreadPRs) != 0 {
		t.Errorf("unread leaked into %+v", c)
	}
	if c.Note != "2 to promote" || c.Summary != "Nothing unread · 1 dependency bump and 1 by you" {
		t.Errorf("note = %q, summary = %q", c.Note, c.Summary)
	}
}

func TestAttachChangesError(t *testing.T) {
	h, _, _ := driftHandlers(t, &fakeCommits{err: errors.New("github compare returned 500")})
	c := h.assembleFleet(context.Background()).Apps[0].Changes
	if c == nil || !c.Unavailable || c.Note != "" {
		t.Fatalf("Changes = %+v, want only Unavailable", c)
	}
}

func TestAttachChangesTimeout(t *testing.T) {
	fc := &fakeCommits{rng: unreadRange(), delay: 5 * time.Second}
	h, _, _ := driftHandlers(t, fc)
	h.commitBudget = 30 * time.Millisecond

	start := time.Now()
	c := h.assembleFleet(context.Background()).Apps[0].Changes
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("assembleFleet took %s; the budget did not bound it", took)
	}
	if c == nil || !c.Unavailable {
		t.Fatalf("Changes = %+v, want Unavailable", c)
	}
}

func TestAttachChangesTruncated(t *testing.T) {
	r := unreadRange()
	r.Total = 600
	h, _, _ := driftHandlers(t, &fakeCommits{rng: r})
	c := h.assembleFleet(context.Background()).Apps[0].Changes
	if c == nil || c.Note != "600 to promote" || c.AllLabel != "3 of 600 commits" {
		t.Fatalf("Changes = %+v", c)
	}
	if !strings.Contains(c.Truncated, "only 3 of the 600") {
		t.Errorf("truncated = %q", c.Truncated)
	}

	// With nothing unread among what came back, no all-clear is claimed.
	r = readRange()
	r.Total = 600
	h, _, _ = driftHandlers(t, &fakeCommits{rng: r})
	c = h.assembleFleet(context.Background()).Apps[0].Changes
	if strings.Contains(c.Summary, "Nothing unread") || c.Truncated == "" {
		t.Errorf("summary = %q, truncated = %q", c.Summary, c.Truncated)
	}
}

func TestAttachChangesCache(t *testing.T) {
	fc := &fakeCommits{rng: unreadRange()}
	h, _, _ := driftHandlers(t, fc)
	for i := 0; i < 3; i++ {
		if c := h.assembleFleet(context.Background()).Apps[0].Changes; c == nil || c.UnreadNote != "1 unread" {
			t.Fatalf("render %d: Changes = %+v", i, c)
		}
	}
	if n := fc.calls(); n != 1 {
		t.Errorf("Between called %d times across three renders, want 1", n)
	}

	// An entry past its TTL is read again.
	h.commitCache.mu.Lock()
	for k, e := range h.commitCache.m {
		e.at = e.at.Add(-2 * commitTTL)
		h.commitCache.m[k] = e
	}
	h.commitCache.mu.Unlock()
	h.assembleFleet(context.Background())
	if n := fc.calls(); n != 2 {
		t.Errorf("Between called %d times after expiry, want 2", n)
	}
}

// Nothing is asked about a service with nothing to promote, or one whose prod
// tag names no commit.
func TestAttachChangesSkipsUnpromotable(t *testing.T) {
	for name, prodTag := range map[string]string{"in sync": "abc1234", "unpinned prod": "latest"} {
		t.Run(name, func(t *testing.T) {
			fc := &fakeCommits{rng: unreadRange()}
			h, _ := newTestHandlers(t, &fakeKube{imgs: map[string][]string{
				"foo-staging": {"reg/foo:abc1234"},
				"foo-prod":    {"reg/foo:" + prodTag},
			}})
			h.Commits = fc
			f := h.assembleFleet(context.Background())
			if fc.calls() != 0 || f.Apps[0].Changes != nil {
				t.Errorf("calls = %d, Changes = %+v", fc.calls(), f.Apps[0].Changes)
			}
		})
	}
}

func TestAttachChangesEmptyRange(t *testing.T) {
	h, _, sess := driftHandlers(t, &fakeCommits{})
	if c := h.assembleFleet(context.Background()).Apps[0].Changes; c != nil {
		t.Fatalf("Changes = %+v, want nil for an empty range", c)
	}
	if body := appsBody(t, h, sess); strings.Contains(body, "to promote") || strings.Contains(body, `class="changes"`) {
		t.Error("an empty range rendered a note or a changes block")
	}
}

// --- rendering ----------------------------------------------------------------

func TestRenderChangesUnread(t *testing.T) {
	h, _, sess := driftHandlers(t, &fakeCommits{rng: unreadRange()})
	body := appsBody(t, h, sess)

	for _, want := range []string{
		// Card and row note, with the unread half marked up to stand out.
		`<span class="promote-note">3 to promote · <span class="unread-count">1 unread</span></span>`,
		// Dialog: heading, a whole-row link to the PR, title without "(#277)".
		`1 change nobody has read`,
		`<a class="change-row" href="https://github.com/eswan18/foo_repo/pull/277" target="_blank" rel="noopener">`,
		`<span class="mono change-pr">#277</span>`,
		`<span class="change-title">Implement issue #221</span>`,
		`<span class="change-author">haruspex-factory[bot]</span>`,
		`Also 1 dependency bump and 1 by you`,
		`<summary>All 3 commits</summary>`,
		`href="https://github.com/eswan18/foo_repo/pull/276"`,
		`href="https://github.com/eswan18/foo_repo/commit/9f8e7d6"`,
		`<span class="env-badge env-prod">unread</span>`,
		`<span class="env-badge env-stg">bump</span>`,
		// The form carries what was shown, and the button says what it does.
		`<input type="hidden" name="unread_pr" value="277">`,
		`Promote and mark 1 as read</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("apps page missing %q", want)
		}
	}
	if n := strings.Count(body, "promote-note"); n != 2 {
		t.Errorf("promote note rendered %d times, want 2 (desktop row and mobile card)", n)
	}
	if strings.Contains(body, "Promote to prod</button>") {
		t.Error("confirm button kept its plain label despite unread PRs")
	}
	if strings.Contains(body, "could not be read") {
		t.Error("unavailable line rendered for a readable range")
	}

	// The overview's attention item says it too.
	rec := httptest.NewRecorder()
	h.Overview(rec, authed(t, "GET", "/", "", sess))
	if ov := rec.Body.String(); !strings.Contains(ov, "· 3 to promote") || !strings.Contains(ov, `<span class="unread-count">1 unread</span>`) {
		t.Error("overview attention item missing the promote note")
	}
}

func TestRenderChangesPlural(t *testing.T) {
	r := unreadRange()
	r.Total = 4
	r.Commits = append([]commitlog.Commit{
		{SHA: "1111111", Subject: "Implement issue #230 (#281)", PR: 281, Author: "haruspex-factory[bot]", Bot: true, Kind: commitlog.Unread},
	}, r.Commits...)
	h, _, sess := driftHandlers(t, &fakeCommits{rng: r})
	body := appsBody(t, h, sess)
	for _, want := range []string{
		"2 changes nobody has read", "Promote and mark 2 as read</button>", "· <span class=\"unread-count\">2 unread</span>",
		`name="unread_pr" value="281"`, `name="unread_pr" value="277"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestRenderChangesNoneUnread(t *testing.T) {
	h, _, sess := driftHandlers(t, &fakeCommits{rng: readRange()})
	body := appsBody(t, h, sess)
	for _, want := range []string{
		`<span class="promote-note">2 to promote</span>`,
		`Nothing unread · 1 dependency bump and 1 by you`,
		`<summary>All 2 commits</summary>`,
		`Promote to prod</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("apps page missing %q", want)
		}
	}
	for _, not := range []string{"unread-count", "nobody has read", "unread_pr", "mark 0", "change-row"} {
		if strings.Contains(body, not) {
			t.Errorf("apps page has %q with nothing unread", not)
		}
	}
}

func TestRenderChangesUnavailable(t *testing.T) {
	h, _, sess := driftHandlers(t, &fakeCommits{err: errors.New("github compare returned 500")})
	body := appsBody(t, h, sess)
	if n := strings.Count(body, "The changes could not be read from GitHub."); n != 1 {
		t.Errorf("unavailable line rendered %d times, want exactly once (in the dialog)", n)
	}
	for _, not := range []string{"promote-note", "to promote", "all-commits", "unread_pr", "github compare returned"} {
		if strings.Contains(body, not) {
			t.Errorf("apps page has %q after a failed lookup", not)
		}
	}
	if !strings.Contains(body, "Promote to prod</button>") {
		t.Error("confirm button changed after a failed lookup")
	}
}

func TestRenderChangesTruncated(t *testing.T) {
	r := unreadRange()
	r.Total = 600
	h, _, sess := driftHandlers(t, &fakeCommits{rng: r})
	body := appsBody(t, h, sess)
	for _, want := range []string{"GitHub returned only 3 of the 600 commits", "<summary>3 of 600 commits</summary>", "600 to promote"} {
		if !strings.Contains(body, want) {
			t.Errorf("apps page missing %q", want)
		}
	}
}

// A nil client must leave the page exactly as it was: same output as a
// handler that never had the field.
func TestRenderNilClientUnchanged(t *testing.T) {
	h, _, sess := driftHandlers(t, nil)
	body := appsBody(t, h, sess)
	for _, not := range []string{"promote-note", "changes", "unread", "could not be read", "promote-confirm", "all-commits"} {
		if strings.Contains(body, not) {
			t.Errorf("apps page has %q with no commit client", not)
		}
	}
	if !strings.Contains(body, `class="js-mutate" data-service="foo"`) || !strings.Contains(body, "Promote to prod</button>") {
		t.Error("promote form changed with no commit client")
	}
}

// Titles and logins are untrusted text from GitHub.
func TestRenderChangesEscapes(t *testing.T) {
	evil := `<script>alert(1)</script> "onmouseover="x (#277)`
	r := commitlog.Range{Total: 2, Commits: []commitlog.Commit{
		{SHA: "b6e95f4", Subject: evil, PR: 277, Author: `<img src=x onerror=alert(2)>`, Kind: commitlog.Unread},
		// A SHA that is not a SHA must not become part of a URL.
		{SHA: `"><script>`, Subject: "<script>alert(3)</script>", Author: "eswan18", Kind: commitlog.Yours},
	}}
	h, _, sess := driftHandlers(t, &fakeCommits{rng: r})
	body := appsBody(t, h, sess)

	for _, bad := range []string{"<script>alert", "<img src=x", `"onmouseover="x`, `commit/"`} {
		if strings.Contains(body, bad) {
			t.Errorf("unescaped %q in the page", bad)
		}
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("escaped title not found")
	}
	if !strings.Contains(body, "&lt;img src=x onerror=alert(2)&gt;") {
		t.Error("escaped author not found")
	}
	if !strings.Contains(body, `<span class="commit-row">`) {
		t.Error("a commit with an unlinkable SHA should render without a link")
	}
}

// --- promote handler ----------------------------------------------------------

func promoteWith(t *testing.T, h *Handlers, sess *auth.Session, json bool, prs ...string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"csrf": {csrf(h, sess)}, "expected_sha": {"abc1234"}, "unread_pr": prs}
	req := authed(t, "POST", "/services/foo/promote", form.Encode(), sess)
	if json {
		req.Header.Set("Accept", "application/json")
	}
	req.SetPathValue("name", "foo")
	rec := httptest.NewRecorder()
	h.Promote(rec, req)
	return rec
}

func TestPromoteMarksShownAndUnread(t *testing.T) {
	fc := &fakeCommits{rng: unreadRange()}
	h, k, sess := driftHandlers(t, fc)
	// Prime the cache the way a page load would, to see it dropped.
	h.assembleFleet(context.Background())

	rec := promoteWith(t, h, sess, true, "277")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if k.patched["foo-prod"] != "reg/foo:abc1234" {
		t.Fatalf("patched = %q", k.patched["foo-prod"])
	}
	if !reflect.DeepEqual(fc.marked, []int{277}) {
		t.Errorf("marked = %v, want [277]", fc.marked)
	}
	// The range is re-read for the tags promoted, not taken from the cache.
	if want := "eswan18/foo_repo def5678...abc1234"; len(fc.betweenCalls) != 2 || fc.betweenCalls[1] != want {
		t.Errorf("Between calls = %v", fc.betweenCalls)
	}
	got := decodeJSON(t, rec)
	if got["ok"] != true || got["note"] != " · marked #277 as read" {
		t.Errorf("response = %v", got)
	}
	fl := flashFrom(t, rec)
	if fl == nil || fl.Kind != FlashSuccess || fl.Msg != "Promoted foo prod → abc1234 · marked #277 as read" {
		t.Errorf("flash = %+v", fl)
	}
	h.commitCache.mu.Lock()
	n := len(h.commitCache.m)
	h.commitCache.mu.Unlock()
	if n != 0 {
		t.Errorf("%d cached ranges survived a MarkRead", n)
	}
}

func TestPromoteDoesNotMarkWhatIsNoLongerUnread(t *testing.T) {
	fc := &fakeCommits{rng: unreadRange()}
	h, k, sess := driftHandlers(t, fc)
	h.assembleFleet(context.Background())
	// Between page load and confirm, someone removed the label.
	fc.set(readRange())

	rec := promoteWith(t, h, sess, false, "277")
	if rec.Code != http.StatusSeeOther || k.patched["foo-prod"] == "" {
		t.Fatalf("code = %d, patched = %q", rec.Code, k.patched["foo-prod"])
	}
	if len(fc.marked) != 0 {
		t.Errorf("marked = %v, want none", fc.marked)
	}
	if fl := flashFrom(t, rec); fl == nil || fl.Msg != "Promoted foo prod → abc1234" {
		t.Errorf("flash = %+v", fl)
	}
}

func TestPromoteDoesNotMarkWhatWasNotShown(t *testing.T) {
	// Two unread in the promoted range; the user's dialog listed only 277.
	r := unreadRange()
	r.Total = 4
	r.Commits = append([]commitlog.Commit{
		{SHA: "1111111", Subject: "Implement issue #230 (#281)", PR: 281, Author: "haruspex-factory[bot]", Bot: true, Kind: commitlog.Unread},
	}, r.Commits...)
	fc := &fakeCommits{rng: r}
	h, _, sess := driftHandlers(t, fc)

	rec := promoteWith(t, h, sess, false, "277")
	if !reflect.DeepEqual(fc.marked, []int{277}) {
		t.Errorf("marked = %v, want [277] only", fc.marked)
	}
	if fl := flashFrom(t, rec); fl == nil || fl.Msg != "Promoted foo prod → abc1234 · marked #277 as read" {
		t.Errorf("flash = %+v", fl)
	}

	// And with nothing submitted, nothing is marked and GitHub is not asked.
	fc2 := &fakeCommits{rng: r}
	h, _, sess = driftHandlers(t, fc2)
	rec = promoteWith(t, h, sess, false)
	if len(fc2.marked) != 0 || fc2.calls() != 0 {
		t.Errorf("marked = %v, Between calls = %d", fc2.marked, fc2.calls())
	}
	if fl := flashFrom(t, rec); fl == nil || fl.Msg != "Promoted foo prod → abc1234" {
		t.Errorf("flash = %+v", fl)
	}
}

// A submitted number that is in the range only as a READ PR (the bump, #276),
// or not in it at all, is never marked: the form alone decides nothing.
func TestPromoteIgnoresForgedNumbers(t *testing.T) {
	fc := &fakeCommits{rng: unreadRange()}
	h, _, sess := driftHandlers(t, fc)
	rec := promoteWith(t, h, sess, false, "276", "9999")
	if rec.Code != http.StatusSeeOther || len(fc.marked) != 0 {
		t.Errorf("code = %d, marked = %v", rec.Code, fc.marked)
	}
}

func TestPromoteMarkReadFailureStillSucceeds(t *testing.T) {
	fc := &fakeCommits{rng: unreadRange(), markErr: errors.New("github label removal returned 403")}
	h, k, sess := driftHandlers(t, fc)

	rec := promoteWith(t, h, sess, true, "277")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if k.patched["foo-prod"] != "reg/foo:abc1234" {
		t.Fatalf("patched = %q", k.patched["foo-prod"])
	}
	want := " · could not mark #277 as read: github label removal returned 403"
	if got := decodeJSON(t, rec); got["ok"] != true || got["note"] != want || got["error"] != "" {
		t.Errorf("response = %v", got)
	}
	if fl := flashFrom(t, rec); fl == nil || fl.Kind != FlashSuccess || fl.Msg != "Promoted foo prod → abc1234"+want {
		t.Errorf("flash = %+v", fl)
	}
}

func TestPromoteRereadFailureStillSucceeds(t *testing.T) {
	fc := &fakeCommits{err: context.DeadlineExceeded}
	h, k, sess := driftHandlers(t, fc)
	rec := promoteWith(t, h, sess, true, "277")
	if rec.Code != http.StatusOK || k.patched["foo-prod"] == "" {
		t.Fatalf("code = %d, patched = %q", rec.Code, k.patched["foo-prod"])
	}
	if got := decodeJSON(t, rec); got["note"] != " · could not mark #277 as read: timed out" {
		t.Errorf("note = %v", got["note"])
	}
	if len(fc.marked) != 0 {
		t.Errorf("marked = %v", fc.marked)
	}
}

func TestPromoteRejectsBadUnreadPRs(t *testing.T) {
	tooMany := make([]string, maxShownPRs+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprint(i + 1)
	}
	cases := map[string][]string{
		"letters":   {"abc"},
		"negative":  {"-277"},
		"zero":      {"0"},
		"empty":     {""},
		"spaces":    {" 277"},
		"injection": {"277; drop"},
		"huge":      {"99999999999999999999"},
		"mixed":     {"277", "x"},
		"too many":  tooMany,
	}
	for name, prs := range cases {
		t.Run(name, func(t *testing.T) {
			fc := &fakeCommits{rng: unreadRange()}
			h, k, sess := driftHandlers(t, fc)
			rec := promoteWith(t, h, sess, true, prs...)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("code = %d, want 400", rec.Code)
			}
			if _, ok := k.patched["foo-prod"]; ok {
				t.Error("promoted despite a malformed unread_pr list")
			}
			if len(fc.marked) != 0 || fc.calls() != 0 {
				t.Errorf("marked = %v, Between calls = %d", fc.marked, fc.calls())
			}
		})
	}
}

// With no client the endpoint behaves as it always did, whatever the form
// carries.
func TestPromoteNilClientUnchanged(t *testing.T) {
	h, k, sess := driftHandlers(t, nil)
	rec := promoteWith(t, h, sess, true, "277")
	if rec.Code != http.StatusOK || k.patched["foo-prod"] != "reg/foo:abc1234" {
		t.Fatalf("code = %d, patched = %q", rec.Code, k.patched["foo-prod"])
	}
	if got := decodeJSON(t, rec); got["ok"] != true || got["note"] != "" {
		t.Errorf("response = %v", got)
	}
	if fl := flashFrom(t, rec); fl == nil || fl.Msg != "Promoted foo prod → abc1234" {
		t.Errorf("flash = %+v", fl)
	}
}

// A failed promotion marks nothing.
func TestPromotePatchFailureMarksNothing(t *testing.T) {
	fc := &fakeCommits{rng: unreadRange()}
	h, k, sess := driftHandlers(t, fc)
	k.patchErr = errors.New("boom")
	rec := promoteWith(t, h, sess, true, "277")
	if rec.Code != http.StatusBadGateway || len(fc.marked) != 0 || fc.calls() != 0 {
		t.Errorf("code = %d, marked = %v, Between calls = %d", rec.Code, fc.marked, fc.calls())
	}
}

func TestParseShownPRs(t *testing.T) {
	got, err := parseShownPRs([]string{"277", "281", "277"})
	if err != nil || !reflect.DeepEqual(got, []int{277, 281}) {
		t.Errorf("got %v, %v", got, err)
	}
	if got, err := parseShownPRs(nil); err != nil || len(got) != 0 {
		t.Errorf("nil: got %v, %v", got, err)
	}
}

func TestNewGitHubCommits(t *testing.T) {
	if c := NewGitHubCommits("", nil); c != nil {
		t.Errorf("no tokens: got %v, want nil (feature off)", c)
	}
	if c := NewGitHubCommits("", map[string]string{"Footstrike": ""}); c != nil {
		t.Errorf("empty owner token: got %v, want nil", c)
	}
	g, ok := NewGitHubCommits("default", map[string]string{"Footstrike": "fs"}).(*githubCommits)
	if !ok {
		t.Fatal("not a *githubCommits")
	}
	// Per-owner selection is case-insensitive, like internal/github's.
	if g.clientFor("footstrike") != g.clientFor("FOOTSTRIKE") || g.clientFor("footstrike") == g.fallback {
		t.Error("owner token not selected for its owner")
	}
	if g.clientFor("eswan18") != g.fallback {
		t.Error("an owner without a token should use the default")
	}
}
