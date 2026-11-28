package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eswan18/bifrost/internal/commitlog"
)

// ---- fake GitHub --------------------------------------------------------

// fakeCommits serves commit ranges from an "owner/repo base...head" map and
// records every label removal. It is the commitReader seam's fake, at the same
// altitude as fakeBuilds and fakeDigests: the HTTP underneath is
// internal/commitlog's to test (with httptest); what is under test here is
// what bif does with the answers, and with their absence.
//
// The counters are mutex-guarded because the lookups run concurrently.
type fakeCommits struct {
	ranges   map[string]commitlog.Range
	errs     map[string]error // per-range failure
	dialErr  error
	hang     bool          // never answer; wait for the context instead
	markErrs map[int]error // per-PR label-removal failure

	mu     sync.Mutex
	dials  int
	asked  []string
	marked []string // "owner/repo#n", in the order asked
}

func (f *fakeCommits) dial(context.Context) (commitReader, error) {
	f.mu.Lock()
	f.dials++
	f.mu.Unlock()
	if f.dialErr != nil {
		return nil, f.dialErr
	}
	return f, nil
}

func (f *fakeCommits) Between(ctx context.Context, owner, repo, base, head string) (commitlog.Range, error) {
	key := fmt.Sprintf("%s/%s %s...%s", owner, repo, base, head)
	f.mu.Lock()
	f.asked = append(f.asked, key)
	f.mu.Unlock()
	if f.hang {
		<-ctx.Done()
		return commitlog.Range{}, ctx.Err()
	}
	if err := f.errs[key]; err != nil {
		return commitlog.Range{}, err
	}
	r, ok := f.ranges[key]
	if !ok {
		return commitlog.Range{}, errors.New("github compare returned 404")
	}
	return r, nil
}

func (f *fakeCommits) MarkRead(_ context.Context, owner, repo string, pr int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marked = append(f.marked, fmt.Sprintf("%s/%s#%d", owner, repo, pr))
	return f.markErrs[pr]
}

func (f *fakeCommits) counts() (dials int, asked, marked []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dials, slices.Clone(f.asked), slices.Clone(f.marked)
}

// execCommits is exec with a GitHub to talk to. Cloud Build is a working,
// empty fake rather than the unreachable default, so stderr holds only what
// the commit lookup put there.
func execCommits(t *testing.T, cluster *fakeCluster, commits *fakeCommits, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(context.Background(), args, strings.NewReader(stdin), &out, &errb, cluster.connect, (&fakeBuilds{}).dial, unreachableDigests, commits.dial, noPreview)
	return out.String(), errb.String(), code
}

// rangeOf builds a classified range the way commitlog.Between returns one:
// newest first, Total equal to what is listed.
func rangeOf(commits ...commitlog.Commit) commitlog.Range {
	return commitlog.Range{Commits: commits, Total: len(commits)}
}

func unreadCommit(sha string, pr int, subject string) commitlog.Commit {
	return commitlog.Commit{SHA: sha, PR: pr, Subject: fmt.Sprintf("%s (#%d)", subject, pr), Author: "haruspex-factory[bot]", Bot: true, Kind: commitlog.Unread}
}

func bumpCommit(sha string, pr int, subject string) commitlog.Commit {
	return commitlog.Commit{SHA: sha, PR: pr, Subject: fmt.Sprintf("%s (#%d)", subject, pr), Author: commitlog.BumpBotLogin, Bot: true, Kind: commitlog.Bump}
}

func yourCommit(sha, subject string) commitlog.Commit {
	return commitlog.Commit{SHA: sha, Subject: subject, Author: "eswan18", Kind: commitlog.Yours}
}

// The haruspex promotion every single-service test below is about: staging on
// bbbbbbb, prod on aaaaaaa, and this key is the question bif must ask.
const (
	haruspexStaging = "bbbbbbb"
	haruspexProd    = "aaaaaaa"
	haruspexRange   = "eswan18/haruspex aaaaaaa...bbbbbbb"
)

func haruspexOutOfSync(t *testing.T) *fakeCluster {
	t.Helper()
	return promoteFleet(t, map[string][2]string{"haruspex": {haruspexStaging, haruspexProd}})
}

// mixedRange is one unread PR, one bump and one hand-written commit.
func mixedRange() commitlog.Range {
	return rangeOf(
		yourCommit("3333333", "Tidy the settings page"),
		unreadCommit("b6e95f4", 277, "Implement issue #221"),
		bumpCommit("1111111", 275, "Bump next from 15.1 to 15.2"),
	)
}

const promoteCheckHead = "\nharuspex promotion check:\n" +
	"--------------------------------------------------\n" +
	"  staging: bbbbbbb\n" +
	"  prod:    aaaaaaa\n"

const promotedTail = "\nRunning: kubectl patch application haruspex-prod -n argocd\n" +
	"\n✓ Promoted haruspex prod to bbbbbbb\n" +
	"  (ArgoCD will sync automatically)\n"

// ---- bif promote <app> --------------------------------------------------

// TestPromoteShowsUnreadAndMarksThemOnConfirm is the feature end to end, with
// the whole of stdout compared: the unread PR by name above the verdict, the
// rest as a count, a prompt that says what confirming does, and — after the
// write succeeded — the label removed and reported.
func TestPromoteShowsUnreadAndMarksThemOnConfirm(t *testing.T) {
	c := haruspexOutOfSync(t)
	gh := &fakeCommits{ranges: map[string]commitlog.Range{haruspexRange: mixedRange()}}

	out, stderr, code := execCommits(t, c, gh, "y\n", "promote", "haruspex")

	want := promoteCheckHead +
		"\n  3 commits since prod. 1 has not been read:\n" +
		"\n    ! b6e95f4  #277  Implement issue #221  haruspex-factory[bot]\n" +
		"\n  Also: 1 dependency bump, 1 by you.  (--commits to list them)\n" +
		"\n→ Promote prod to: bbbbbbb\n" +
		"\nProceed? This marks #277 as read. [y/N] " +
		promotedTail +
		"✓ Marked #277 as read\n"
	if out != want {
		t.Errorf("stdout mismatch\n got %q\nwant %q", out, want)
	}
	if stderr != "" || code != 0 {
		t.Errorf("stderr = %q, exit = %d; want empty, 0", stderr, code)
	}
	if got := c.promotedApps(); !slices.Equal(got, []string{"haruspex"}) {
		t.Errorf("promoted %v, want [haruspex]", got)
	}
	dials, asked, marked := gh.counts()
	if dials != 1 || !slices.Equal(asked, []string{haruspexRange}) {
		t.Errorf("dials = %d, asked = %v; want one dial and one range", dials, asked)
	}
	if !slices.Equal(marked, []string{"eswan18/haruspex#277"}) {
		t.Errorf("marked %v, want [eswan18/haruspex#277]", marked)
	}
}

// TestPromoteDeclinedMarksNothing: the label removal is a consequence of the
// promotion, so a declined prompt removes nothing — and writes nothing.
func TestPromoteDeclinedMarksNothing(t *testing.T) {
	c := haruspexOutOfSync(t)
	gh := &fakeCommits{ranges: map[string]commitlog.Range{haruspexRange: mixedRange()}}
	out, _, code := execCommits(t, c, gh, "n\n", "promote", "haruspex")
	if !strings.HasSuffix(out, "Proceed? This marks #277 as read. [y/N] Aborted.\n") || code != 0 {
		t.Errorf("stdout = %q, exit = %d; want the prompt then Aborted., 0", out, code)
	}
	if _, _, marked := gh.counts(); len(marked) != 0 || len(c.patches) != 0 {
		t.Errorf("declined, but marked %v and patched %v", marked, c.patches)
	}
}

// TestPromoteNothingUnreadIsOneLine: with nothing unread the summary is a
// single line, the prompt is the old prompt, and no label is touched.
func TestPromoteNothingUnreadIsOneLine(t *testing.T) {
	c := haruspexOutOfSync(t)
	gh := &fakeCommits{ranges: map[string]commitlog.Range{haruspexRange: rangeOf(
		yourCommit("3333333", "Tidy the settings page"),
		yourCommit("2222222", "Fix the leaderboard sort"),
		bumpCommit("1111111", 275, "Bump next from 15.1 to 15.2"),
	)}}

	out, stderr, code := execCommits(t, c, gh, "y\n", "promote", "haruspex")

	want := promoteCheckHead +
		"\n  3 commits since prod (1 dependency bump, 2 by you).  (--commits to list them)\n" +
		"\n→ Promote prod to: bbbbbbb\n" +
		"\nProceed? [y/N] " +
		promotedTail
	if out != want {
		t.Errorf("stdout mismatch\n got %q\nwant %q", out, want)
	}
	if stderr != "" || code != 0 {
		t.Errorf("stderr = %q, exit = %d; want empty, 0", stderr, code)
	}
	if _, _, marked := gh.counts(); len(marked) != 0 {
		t.Errorf("nothing was unread, but marked %v", marked)
	}
}

// TestPromoteCommitsFlagListsEverything: --commits swaps the summary for every
// commit, newest first, marked, with a legend. Accepted anywhere, like -y.
func TestPromoteCommitsFlagListsEverything(t *testing.T) {
	c := haruspexOutOfSync(t)
	r := mixedRange()
	r.Commits = append(r.Commits, commitlog.Commit{SHA: "0000000", Subject: "Regenerate lockfile", Author: "github-actions[bot]", Bot: true, Kind: commitlog.Automation})
	r.Total = 4
	gh := &fakeCommits{ranges: map[string]commitlog.Range{haruspexRange: r}}

	out, _, _ := execCommits(t, c, gh, "n\n", "promote", "--commits", "haruspex")

	want := promoteCheckHead +
		"\n  4 commits since prod. 1 has not been read:\n" +
		"\n" +
		"      3333333        Tidy the settings page       eswan18\n" +
		"    ! b6e95f4  #277  Implement issue #221         haruspex-factory[bot]\n" +
		"    ~ 1111111  #275  Bump next from 15.1 to 15.2  dependabot[bot]\n" +
		"    ~ 0000000        Regenerate lockfile          github-actions[bot]\n" +
		"\n" +
		"    ! not read   ~ dependency bump or automation\n" +
		"\n→ Promote prod to: bbbbbbb\n" +
		"\nProceed? This marks #277 as read. [y/N] Aborted.\n"
	if out != want {
		t.Errorf("stdout mismatch\n got %q\nwant %q", out, want)
	}

	// And with nothing unread, the header carries the counts instead.
	gh = &fakeCommits{ranges: map[string]commitlog.Range{haruspexRange: rangeOf(yourCommit("3333333", "Tidy"))}}
	out, _, _ = execCommits(t, c, gh, "n\n", "promote", "haruspex", "--commits")
	if want := "\n  1 commit since prod (1 by you):\n\n      3333333  Tidy  eswan18\n\n" + commitLegend + "\n"; !strings.Contains(out, want) {
		t.Errorf("stdout = %q, want it to contain %q", out, want)
	}
}

// TestPromoteYesLeavesUnreadLabelled: -y asks nothing, so nobody agreed to
// call anything read. The summary still prints, the promotion still happens,
// and the labels stay — said in one line.
func TestPromoteYesLeavesUnreadLabelled(t *testing.T) {
	for _, flag := range []string{"-y", "--yes"} {
		t.Run(flag, func(t *testing.T) {
			c := haruspexOutOfSync(t)
			gh := &fakeCommits{ranges: map[string]commitlog.Range{haruspexRange: mixedRange()}}

			out, stderr, code := execCommits(t, c, gh, "", "promote", "haruspex", flag)

			want := promoteCheckHead +
				"\n  3 commits since prod. 1 has not been read:\n" +
				"\n    ! b6e95f4  #277  Implement issue #221  haruspex-factory[bot]\n" +
				"\n  Also: 1 dependency bump, 1 by you.  (--commits to list them)\n" +
				"\n→ Promote prod to: bbbbbbb\n" +
				promotedTail +
				"  #277 stays labelled unreviewed (-y asks nothing, so nothing was marked as read)\n"
			if out != want {
				t.Errorf("stdout mismatch\n got %q\nwant %q", out, want)
			}
			if stderr != "" || code != 0 {
				t.Errorf("stderr = %q, exit = %d; want empty, 0", stderr, code)
			}
			if _, _, marked := gh.counts(); len(marked) != 0 {
				t.Errorf("-y must not remove labels; marked %v", marked)
			}
			if len(c.patches) != 1 {
				t.Errorf("patches = %+v, want the promotion to have happened", c.patches)
			}
		})
	}
}

// TestPromoteLabelRemoval covers what the removal itself can do. In every case
// prod has already moved, so the exit code is the promotion's: 0.
func TestPromoteLabelRemoval(t *testing.T) {
	two := rangeOf(
		unreadCommit("b6e95f4", 277, "Implement issue #221"),
		unreadCommit("a5d84e3", 276, "Implement issue #219"),
	)
	tests := []struct {
		name       string
		markErrs   map[int]error
		wantStdout string // what follows the promotion
		wantStderr string
	}{
		{
			// A 404 is commitlog.MarkRead returning nil (TestMarkRead pins
			// that), so "already done" and "done" look the same from here.
			name:       "success, including a label that was already gone",
			wantStdout: "✓ Marked #277, #276 as read\n",
		},
		{
			name:       "one fails, the other is still marked",
			markErrs:   map[int]error{277: errors.New("github label removal returned 403")},
			wantStdout: "✓ Marked #276 as read\n",
			wantStderr: "Warning: could not mark #277 as read: github label removal returned 403\n" +
				"  Run: gh pr edit 277 --remove-label unreviewed --repo eswan18/haruspex\n",
		},
		{
			name:     "all fail: no confirmation line, a manual command for each",
			markErrs: map[int]error{277: context.DeadlineExceeded, 276: context.DeadlineExceeded},
			wantStderr: "Warning: could not mark #277 as read: timed out\n" +
				"  Run: gh pr edit 277 --remove-label unreviewed --repo eswan18/haruspex\n" +
				"Warning: could not mark #276 as read: timed out\n" +
				"  Run: gh pr edit 276 --remove-label unreviewed --repo eswan18/haruspex\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := haruspexOutOfSync(t)
			gh := &fakeCommits{ranges: map[string]commitlog.Range{haruspexRange: two}, markErrs: tc.markErrs}

			out, stderr, code := execCommits(t, c, gh, "y\n", "promote", "haruspex")

			if !strings.Contains(out, "Proceed? This marks #277, #276 as read. [y/N] ") {
				t.Errorf("stdout = %q, want the prompt to name both PRs", out)
			}
			if want := promotedTail + tc.wantStdout; !strings.HasSuffix(out, want) {
				t.Errorf("stdout = %q, want it to end %q", out, want)
			}
			if stderr != tc.wantStderr {
				t.Errorf("stderr mismatch\n got %q\nwant %q", stderr, tc.wantStderr)
			}
			if code != 0 {
				t.Errorf("exit = %d, want 0: the promotion succeeded", code)
			}
			if _, _, marked := gh.counts(); len(marked) != 2 {
				t.Errorf("attempted %v, want both PRs attempted", marked)
			}
		})
	}
}

// TestPromoteFailedWriteMarksNothing: a promotion that did not happen reviewed
// nothing. The labels stay and the exit code is the failed write's.
func TestPromoteFailedWriteMarksNothing(t *testing.T) {
	c := haruspexOutOfSync(t)
	c.patchErr = errors.New("applications.argoproj.io is forbidden")
	gh := &fakeCommits{ranges: map[string]commitlog.Range{haruspexRange: mixedRange()}}
	out, _, code := execCommits(t, c, gh, "y\n", "promote", "haruspex")
	if code != 1 || !strings.Contains(out, "✗ Promotion failed") {
		t.Errorf("exit = %d, stdout = %q; want a failed promotion", code, out)
	}
	if _, _, marked := gh.counts(); len(marked) != 0 || strings.Contains(out, "as read\n") {
		t.Errorf("marked %v after a failed write; stdout %q", marked, out)
	}
}

// TestPromoteUnreadableCommitsChangesNothingOnStdout is the degradation
// contract, for every way GitHub can fail: stdout, the prompt, the write and
// the exit code are exactly what `bif promote` produced before it ever asked
// GitHub anything, and stderr gains exactly one line.
func TestPromoteUnreadableCommitsChangesNothingOnStdout(t *testing.T) {
	// The reference: the same promotion with the unreachable default.
	baseline, _ := execStdin(t, haruspexOutOfSync(t), "y\n", "promote", "haruspex")
	if want := promoteCheckHead + "\n→ Promote prod to: bbbbbbb\n\nProceed? [y/N] " + promotedTail; baseline != want {
		t.Fatalf("baseline stdout = %q, want %q", baseline, want)
	}

	tests := []struct {
		name       string
		gh         *fakeCommits
		wantStderr string
	}{
		{"no way to dial", &fakeCommits{dialErr: errors.New("no credentials")}, "Warning: could not read commits from GitHub: no credentials\n"},
		{"the API refuses", &fakeCommits{errs: map[string]error{haruspexRange: errors.New("github compare returned 404")}}, "Warning: could not read commits from GitHub: github compare returned 404\n"},
		{"GitHub never answers", &fakeCommits{hang: true}, "Warning: could not read commits from GitHub: timed out\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			restore := promoteCommitTimeout
			promoteCommitTimeout = 20 * time.Millisecond
			t.Cleanup(func() { promoteCommitTimeout = restore })

			c := haruspexOutOfSync(t)
			start := time.Now()
			out, stderr, code := execCommits(t, c, tc.gh, "y\n", "promote", "haruspex")
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Errorf("took %s; the lookup is bounded at %s", elapsed, promoteCommitTimeout)
			}
			if out != baseline {
				t.Errorf("stdout changed\n got %q\nwant %q", out, baseline)
			}
			if stderr != tc.wantStderr {
				t.Errorf("stderr = %q, want exactly %q", stderr, tc.wantStderr)
			}
			if code != 0 || len(c.patches) != 1 {
				t.Errorf("exit = %d, patches = %+v; want the promotion to proceed", code, c.patches)
			}
			if _, _, marked := tc.gh.counts(); len(marked) != 0 {
				t.Errorf("marked %v with no commits read", marked)
			}
		})
	}
}

// TestPromoteAsksGitHubOnlyWhenPromoting: a refusal, an in-sync service and a
// prod tag with no SHA raise no question, so they make no dial — and an empty
// range (prod ahead of staging) prints nothing.
func TestPromoteAsksGitHubOnlyWhenPromoting(t *testing.T) {
	tests := []struct {
		name          string
		staging, prod string
	}{
		{"in sync", "abc1234", "abc1234"},
		{"prod is not pinned to a commit", "bbbbbbb", "latest"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := promoteFleet(t, map[string][2]string{"haruspex": {tc.staging, tc.prod}})
			gh := &fakeCommits{}
			_, stderr, _ := execCommits(t, c, gh, "n\n", "promote", "haruspex")
			if dials, asked, _ := gh.counts(); dials != 0 || len(asked) != 0 || stderr != "" {
				t.Errorf("dials = %d, asked = %v, stderr = %q; want no GitHub contact", dials, asked, stderr)
			}
		})
	}

	t.Run("an empty range says nothing", func(t *testing.T) {
		c := haruspexOutOfSync(t)
		gh := &fakeCommits{ranges: map[string]commitlog.Range{haruspexRange: {}}}
		out, stderr, _ := execCommits(t, c, gh, "n\n", "promote", "haruspex", "--commits")
		if want := promoteCheckHead + "\n→ Promote prod to: bbbbbbb\n\nProceed? [y/N] Aborted.\n"; out != want || stderr != "" {
			t.Errorf("stdout = %q, stderr = %q; want the plain check", out, stderr)
		}
	})
}

// TestPromoteUsesTheRegistryRepoAndOwner: the range is asked of the repo the
// registry names, under the owner it names — footstrike-api lives under
// Footstrike, not the default owner.
func TestPromoteUsesTheRegistryRepoAndOwner(t *testing.T) {
	c := promoteFleet(t, map[string][2]string{"footstrike-api": {"bbbbbbb", "aaaaaaa"}})
	gh := &fakeCommits{}
	_, _, _ = execCommits(t, c, gh, "n\n", "promote", "footstrike-api")
	if _, asked, _ := gh.counts(); !slices.Equal(asked, []string{"Footstrike/footstrike-api aaaaaaa...bbbbbbb"}) {
		t.Errorf("asked %v, want the Footstrike-owned repo", asked)
	}

	repo := withRepoOverride(t, "haruspex")
	c = haruspexOutOfSync(t)
	gh = &fakeCommits{}
	_, _, _ = execCommits(t, c, gh, "n\n", "promote", "haruspex")
	if _, asked, _ := gh.counts(); !slices.Equal(asked, []string{"eswan18/" + repo + " aaaaaaa...bbbbbbb"}) {
		t.Errorf("asked %v, want the overridden repo %s", asked, repo)
	}
}

// TestChangesTruncationIsSaid: when GitHub capped the range, the header counts
// the real total and a line says how much of it was actually read.
func TestChangesTruncationIsSaid(t *testing.T) {
	r := mixedRange()
	r.Total = 312
	var b bytes.Buffer
	writeChanges(&b, r, false, commitsHint)
	want := "  312 commits since prod. 1 has not been read:\n" +
		"\n    ! b6e95f4  #277  Implement issue #221  haruspex-factory[bot]\n" +
		"\n  Also: 1 dependency bump, 1 by you.  (--commits to list them)\n" +
		"  GitHub returned only 3 of the 312; an unread PR among the rest would not show here.\n"
	if got := b.String(); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// TestChangesAllUnreadHasNoAlsoLine: nothing is left over to count.
func TestChangesAllUnreadHasNoAlsoLine(t *testing.T) {
	var b bytes.Buffer
	writeChanges(&b, rangeOf(unreadCommit("b6e95f4", 277, "Implement issue #221"), unreadCommit("a5d84e3", 276, "Implement issue #219")), false, commitsHint)
	want := "  2 commits since prod. 2 have not been read:\n" +
		"\n    ! b6e95f4  #277  Implement issue #221  haruspex-factory[bot]\n" +
		"    ! a5d84e3  #276  Implement issue #219  haruspex-factory[bot]\n"
	if got := b.String(); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestCommitRowsClipLongSubjectsAndTolerateNoAuthor(t *testing.T) {
	var b bytes.Buffer
	writeCommitRows(&b, []commitlog.Commit{
		{SHA: "1111111", Subject: strings.Repeat("x", 80), Author: "eswan18"},
		{SHA: "2222222", Subject: "short"},
	})
	want := "      1111111  " + strings.Repeat("x", 59) + "…  eswan18\n" +
		"      2222222  short\n"
	if got := b.String(); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestPRListNamesThreeThenCounts(t *testing.T) {
	tests := []struct {
		prs  []int
		want string
	}{
		{nil, ""},
		{[]int{277}, " This marks #277 as read."},
		{[]int{277, 276, 270}, " This marks #277, #276, #270 as read."},
		{[]int{277, 276, 270, 269, 268}, " This marks #277, #276, #270 and 2 more as read."},
	}
	for _, tc := range tests {
		if got := markReadClause(prRefs("", tc.prs)); got != tc.want {
			t.Errorf("markReadClause(%v) = %q, want %q", tc.prs, got, tc.want)
		}
	}
}

// ---- bif promote <app> <app> --------------------------------------------

// TestPromoteSeveralServicesCarriesTheCounts: the combined plan gains a suffix
// on each promotable row, the ONE prompt names every unread PR by service, and
// labels come off per service after that service's write.
func TestPromoteSeveralServicesCarriesTheCounts(t *testing.T) {
	c := promoteFleet(t, map[string][2]string{
		"haruspex": {"bbbbbbb", "aaaaaaa"},
		"identity": {"ddddddd", "ccccccc"},
		"comms":    {"fffffff", "eeeeeee"},
	})
	gh := &fakeCommits{ranges: map[string]commitlog.Range{
		haruspexRange:                        mixedRange(),
		"eswan18/identity ccccccc...ddddddd": rangeOf(yourCommit("4444444", "Rotate keys"), yourCommit("5555555", "Fix MFA")),
		// comms is deliberately unreadable: its row and its promotion must be
		// exactly what they were.
	}}

	out, stderr, code := execCommits(t, c, gh, "y\n", "promote", "haruspex", "identity", "comms", "bifrost")

	want := "\n" +
		"haruspex  bbbbbbb -> prod  (prod: aaaaaaa)  (3 commits, 1 unread)\n" +
		"identity  ddddddd -> prod  (prod: ccccccc)  (2 commits)\n" +
		"comms     fffffff -> prod  (prod: eeeeeee)\n" +
		"bifrost   already in sync, skipping\n" +
		"\nProceed with 3 promotions? This marks haruspex#277 as read. [y/N] " +
		promotedTail +
		"✓ Marked #277 as read\n" +
		"\nRunning: kubectl patch application identity-prod -n argocd\n" +
		"\n✓ Promoted identity prod to ddddddd\n  (ArgoCD will sync automatically)\n" +
		"\nRunning: kubectl patch application comms-prod -n argocd\n" +
		"\n✓ Promoted comms prod to fffffff\n  (ArgoCD will sync automatically)\n" +
		"\nSummary: 3 promoted (haruspex, identity, comms), 1 skipped (bifrost)\n"
	if out != want {
		t.Errorf("stdout mismatch\n got %q\nwant %q", out, want)
	}
	if want := "Warning: could not read commits from GitHub: github compare returned 404 (1 of 3 services)\n"; stderr != want {
		t.Errorf("stderr = %q, want exactly %q", stderr, want)
	}
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	dials, asked, marked := gh.counts()
	if dials != 1 || len(asked) != 3 {
		t.Errorf("dials = %d, asked = %v; want one dial, three ranges (bifrost is in sync and asks nothing)", dials, asked)
	}
	if !slices.Equal(marked, []string{"eswan18/haruspex#277"}) {
		t.Errorf("marked %v, want only haruspex's PR", marked)
	}
}

// TestPromoteSeveralServicesYesAndFailure: under -y no label is removed and
// each service with unread PRs says so; and a service whose write FAILED keeps
// its labels even when the prompt was confirmed.
func TestPromoteSeveralServicesYesAndFailure(t *testing.T) {
	ranges := map[string]commitlog.Range{
		haruspexRange:                        mixedRange(),
		"eswan18/identity ccccccc...ddddddd": rangeOf(unreadCommit("4444444", 12, "Implement issue #9")),
	}
	tags := map[string][2]string{"haruspex": {"bbbbbbb", "aaaaaaa"}, "identity": {"ddddddd", "ccccccc"}}

	t.Run("-y", func(t *testing.T) {
		c := promoteFleet(t, tags)
		gh := &fakeCommits{ranges: ranges}
		out, _, code := execCommits(t, c, gh, "", "promote", "haruspex", "identity", "-y")
		for _, want := range []string{
			"  (ArgoCD will sync automatically)\n  #277 stays labelled unreviewed (-y asks nothing, so nothing was marked as read)\n",
			"  (ArgoCD will sync automatically)\n  #12 stays labelled unreviewed (-y asks nothing, so nothing was marked as read)\n",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("stdout = %q, want it to contain %q", out, want)
			}
		}
		if _, _, marked := gh.counts(); len(marked) != 0 || code != 0 {
			t.Errorf("marked %v, exit %d; want nothing marked, 0", marked, code)
		}
	})

	t.Run("a failed write keeps its labels", func(t *testing.T) {
		c := promoteFleet(t, tags)
		c.patchErrs = map[string]error{"haruspex": errors.New("forbidden")}
		gh := &fakeCommits{ranges: ranges}
		out, _, code := execCommits(t, c, gh, "y\n", "promote", "haruspex", "identity")
		if !strings.Contains(out, "Proceed with 2 promotions? This marks haruspex#277, identity#12 as read. [y/N] ") {
			t.Errorf("stdout = %q, want one prompt naming both services' PRs", out)
		}
		if _, _, marked := gh.counts(); !slices.Equal(marked, []string{"eswan18/identity#12"}) {
			t.Errorf("marked %v, want only identity's: haruspex was not promoted", marked)
		}
		if code != 1 {
			t.Errorf("exit = %d, want 1 for the failed write", code)
		}
	})

	t.Run("--commits lists each service's commits under the plan", func(t *testing.T) {
		c := promoteFleet(t, tags)
		gh := &fakeCommits{ranges: ranges}
		out, _, _ := execCommits(t, c, gh, "n\n", "promote", "haruspex", "identity", "--commits")
		want := "\nidentity\n" +
			"  1 commit since prod. 1 has not been read:\n" +
			"\n    ! 4444444  #12  Implement issue #9  haruspex-factory[bot]\n" +
			"\n" + commitLegend + "\n" +
			"\nProceed with 2 promotions?"
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q, want it to contain %q", out, want)
		}
	})
}

// ---- bif status ---------------------------------------------------------

// TestStatusTableNotesWhatWouldBePromoted: -v gains a trailing note on the
// out-of-sync rows GitHub could describe, and nothing else about the table
// moves — no header, no change to any other row.
func TestStatusTableNotesWhatWouldBePromoted(t *testing.T) {
	tags := map[string][2]string{
		"haruspex": {"bbbbbbb", "aaaaaaa"},
		"identity": {"ddddddd", "ccccccc"},
		"comms":    {"fffffff", "eeeeeee"},
	}
	baseline, _, _ := execCommits(t, promoteFleet(t, tags), &fakeCommits{dialErr: errors.New("x")}, "", "status", "-v")

	gh := &fakeCommits{ranges: map[string]commitlog.Range{
		haruspexRange:                        mixedRange(),
		"eswan18/identity ccccccc...ddddddd": rangeOf(yourCommit("4444444", "Rotate keys")),
	}}
	out, stderr, code := execCommits(t, promoteFleet(t, tags), gh, "", "status", "-v")

	if code != 1 {
		t.Errorf("exit = %d, want 1 (out of sync), unchanged by the notes", code)
	}
	if want := "Warning: could not read commits from GitHub: github compare returned 404 (1 of 3 services)\n"; stderr != want {
		t.Errorf("stderr = %q, want exactly one summary line %q", stderr, want)
	}
	wantNotes := map[string]string{
		"haruspex": "  3 to promote, 1 unread",
		"identity": "  1 to promote",
	}
	baseLines, lines := strings.Split(baseline, "\n"), strings.Split(out, "\n")
	if len(baseLines) != len(lines) {
		t.Fatalf("line count changed: %d -> %d\n%s", len(baseLines), len(lines), out)
	}
	for i, line := range lines {
		app, _, _ := strings.Cut(line, " ")
		// Each noted row is the old row plus its note; every other line —
		// header, comms (unreadable), in-sync rows, promote line — is identical.
		if want := baseLines[i] + wantNotes[app]; line != want {
			t.Errorf("line %d\n got %q\nwant %q", i, line, want)
		}
	}
	if !strings.Contains(out, "3 to promote, 1 unread") || !strings.Contains(out, "1 to promote\n") {
		t.Errorf("the notes are missing:\n%s", out)
	}
	if _, asked, _ := gh.counts(); len(asked) != 3 {
		t.Errorf("asked %v, want one range per out-of-sync service and none for main", asked)
	}
}

// TestStatusDetailShowsTheCommitsAndWhatIsStillOnMain: -vv shows the block
// promote shows, in full, and then what main holds beyond staging.
func TestStatusDetailShowsTheCommitsAndWhatIsStillOnMain(t *testing.T) {
	c := haruspexOutOfSync(t)
	gh := &fakeCommits{ranges: map[string]commitlog.Range{
		haruspexRange: mixedRange(),
		"eswan18/haruspex bbbbbbb...main": rangeOf(
			unreadCommit("7777777", 280, "Implement issue #230"),
			yourCommit("6666666", "Fix the footer"),
		),
	}}
	out, stderr, code := execCommits(t, c, gh, "", "status", "haruspex", "-vv")

	want := "  promote: bif promote haruspex (deploys bbbbbbb to prod)\n" +
		"\n" +
		"  3 commits since prod. 1 has not been read:\n" +
		"\n" +
		"      3333333        Tidy the settings page       eswan18\n" +
		"    ! b6e95f4  #277  Implement issue #221         haruspex-factory[bot]\n" +
		"    ~ 1111111  #275  Bump next from 15.1 to 15.2  dependabot[bot]\n" +
		"\n" +
		"    ! not read   ~ dependency bump or automation\n" +
		"\n" +
		"  2 more on main, not yet on staging:\n" +
		"\n" +
		"    ! 7777777  #280  Implement issue #230  haruspex-factory[bot]\n" +
		"      6666666        Fix the footer        eswan18\n"
	if !strings.HasSuffix(out, want) {
		t.Errorf("stdout = %q\nwant it to end %q", out, want)
	}
	if stderr != "" || code != 1 {
		t.Errorf("stderr = %q, exit = %d; want empty, 1", stderr, code)
	}
	if _, _, marked := gh.counts(); len(marked) != 0 {
		t.Errorf("status removed labels: %v", marked)
	}

	// main level with staging: the second half is simply absent.
	gh.ranges["eswan18/haruspex bbbbbbb...main"] = commitlog.Range{}
	out, _, _ = execCommits(t, c, gh, "", "status", "haruspex", "-vv")
	if strings.Contains(out, "more on main") || !strings.HasSuffix(out, commitLegend+"\n") {
		t.Errorf("stdout = %q, want no main section", out)
	}
}

// TestStatusCommitLookupDegradesSilentlyOnStdout: every failure leaves -v and
// -vv byte-identical to a run with no GitHub at all, inside the time bound,
// with at most one stderr line however many services were asked about.
func TestStatusCommitLookupDegradesSilentlyOnStdout(t *testing.T) {
	tags := map[string][2]string{"haruspex": {"bbbbbbb", "aaaaaaa"}, "identity": {"ddddddd", "ccccccc"}}
	for _, flag := range []string{"-v", "-vv"} {
		baseline, _, _ := execBuilds(t, promoteFleet(t, tags), &fakeBuilds{}, "status", flag)
		tests := []struct {
			name       string
			gh         *fakeCommits
			wantStderr string
		}{
			{"dial fails", &fakeCommits{dialErr: errors.New("no credentials")}, "Warning: could not read commits from GitHub: no credentials (2 of 2 services)\n"},
			{"every range fails", &fakeCommits{}, "Warning: could not read commits from GitHub: github compare returned 404 (2 of 2 services)\n"},
			{"GitHub never answers", &fakeCommits{hang: true}, "Warning: could not read commits from GitHub: timed out (2 of 2 services)\n"},
		}
		for _, tc := range tests {
			t.Run(flag+" "+tc.name, func(t *testing.T) {
				restore := statusCommitTimeout
				statusCommitTimeout = 20 * time.Millisecond
				t.Cleanup(func() { statusCommitTimeout = restore })

				start := time.Now()
				out, stderr, code := execCommits(t, promoteFleet(t, tags), tc.gh, "", "status", flag)
				if elapsed := time.Since(start); elapsed > 2*time.Second {
					t.Errorf("took %s; the lookup is bounded at %s", elapsed, statusCommitTimeout)
				}
				if out != baseline {
					t.Errorf("stdout changed\n got %q\nwant %q", out, baseline)
				}
				if stderr != tc.wantStderr {
					t.Errorf("stderr = %q, want exactly %q", stderr, tc.wantStderr)
				}
				if code != 1 {
					t.Errorf("exit = %d, want 1", code)
				}
			})
		}
	}
}

// TestStatusCommitLookupsRunConcurrently: the budget is for the PHASE. Seven
// services each taking most of the budget must still finish inside it, which
// only happens if they overlap.
func TestStatusCommitLookupsRunConcurrently(t *testing.T) {
	restore := statusCommitTimeout
	statusCommitTimeout = 2 * time.Second
	t.Cleanup(func() { statusCommitTimeout = restore })

	c := fleet(t)
	want := 0
	for ns := range c.images {
		if app, ok := strings.CutSuffix(ns, "-prod"); ok {
			c.images[ns] = []string{image(app, "aaaaaaa")}
			c.images[app+"-staging"] = []string{image(app, "bbbbbbb")}
			want++
		}
	}
	gh := &slowCommits{delay: 300 * time.Millisecond}
	var out, errb bytes.Buffer
	start := time.Now()
	run(context.Background(), []string{"status", "-v"}, strings.NewReader(""), &out, &errb, c.connect, (&fakeBuilds{}).dial, unreachableDigests, gh.dial, noPreview)
	elapsed := time.Since(start)

	if got := strings.Count(out.String(), "1 to promote"); got != want {
		t.Errorf("%d rows carry a note, want all %d:\n%s", got, want, out.String())
	}
	if want < 3 {
		t.Fatalf("the fleet has %d services; too few to tell concurrent from serial", want)
	}
	if serial := time.Duration(want) * gh.delay; elapsed > serial*2/3 {
		t.Errorf("took %s for %d lookups of %s each; serial would be %s", elapsed, want, gh.delay, serial)
	}
}

// slowCommits answers every range after a delay, with one commit.
type slowCommits struct{ delay time.Duration }

func (s *slowCommits) dial(context.Context) (commitReader, error) { return s, nil }

func (s *slowCommits) Between(ctx context.Context, _, _, _, _ string) (commitlog.Range, error) {
	select {
	case <-time.After(s.delay):
		return rangeOf(yourCommit("1234567", "x")), nil
	case <-ctx.Done():
		return commitlog.Range{}, ctx.Err()
	}
}

func (s *slowCommits) MarkRead(context.Context, string, string, int) error {
	return errors.New("status must not remove labels")
}

// TestQuietListAndAttentionNeverAskGitHub: the default list, -q and
// --attention are contracts this feature must not touch — not their output,
// and not their cost. A GitHub that would have answered is never dialled.
func TestQuietListAndAttentionNeverAskGitHub(t *testing.T) {
	for _, args := range [][]string{
		{"status"},
		{"status", "-q"},
		{"status", "haruspex"},
		{"status", "-a"},
		{"status", "haruspex", "--attention"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			gh := &fakeCommits{ranges: map[string]commitlog.Range{haruspexRange: mixedRange()}}
			out, _, _ := execCommits(t, haruspexOutOfSync(t), gh, "", args...)
			if dials, asked, _ := gh.counts(); dials != 0 || len(asked) != 0 {
				t.Errorf("dials = %d, asked = %v; want no GitHub contact", dials, asked)
			}
			if strings.Contains(out, "unread") || strings.Contains(out, "commits since prod") {
				t.Errorf("stdout gained commit information:\n%s", out)
			}
		})
	}
}

// TestStatusInSyncFleetNeverDialsGitHub: nothing to promote, nothing to ask —
// so no token discovery and no `gh` process either.
func TestStatusInSyncFleetNeverDialsGitHub(t *testing.T) {
	gh := &fakeCommits{}
	_, stderr, _ := execCommits(t, fleet(t), gh, "", "status", "-vv")
	if dials, _, _ := gh.counts(); dials != 0 || stderr != "" {
		t.Errorf("dials = %d, stderr = %q; want no GitHub contact", dials, stderr)
	}
}

// ---- token discovery ----------------------------------------------------

// TestDiscoverGitHubTokenOrder: GITHUB_TOKEN, then GH_TOKEN, then gh, then
// nothing — and gh is not even run when the environment answered.
func TestDiscoverGitHubTokenOrder(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		gh        string
		ghErr     error
		want      string
		wantGHRun bool
	}{
		{name: "GITHUB_TOKEN wins", env: map[string]string{"GITHUB_TOKEN": "from-github-token", "GH_TOKEN": "from-gh-token"}, gh: "from-gh", want: "from-github-token"},
		{name: "GH_TOKEN is next", env: map[string]string{"GH_TOKEN": "from-gh-token"}, gh: "from-gh", want: "from-gh-token"},
		{name: "an empty GITHUB_TOKEN is skipped", env: map[string]string{"GITHUB_TOKEN": "", "GH_TOKEN": "from-gh-token"}, want: "from-gh-token"},
		{name: "a blank GITHUB_TOKEN is skipped", env: map[string]string{"GITHUB_TOKEN": "  ", "GH_TOKEN": " from-gh-token\n"}, want: "from-gh-token"},
		{name: "gh is the fallback", gh: "from-gh\n", want: "from-gh", wantGHRun: true},
		{name: "gh failing means no token", ghErr: errors.New("gh is not on PATH"), want: "", wantGHRun: true},
		{name: "gh answering nothing means no token", gh: "", want: "", wantGHRun: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ran := false
			gh := func(context.Context) (string, error) {
				ran = true
				return tc.gh, tc.ghErr
			}
			got := discoverGitHubToken(context.Background(), func(k string) string { return tc.env[k] }, gh)
			if got != tc.want {
				t.Errorf("token = %q, want %q", got, tc.want)
			}
			if ran != tc.wantGHRun {
				t.Errorf("gh ran = %v, want %v", ran, tc.wantGHRun)
			}
		})
	}
	// No gh function at all is the same as no gh.
	if got := discoverGitHubToken(context.Background(), func(string) string { return "" }, nil); got != "" {
		t.Errorf("token = %q with no environment and no gh, want empty", got)
	}
}
