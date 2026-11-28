package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/eswan18/bifrost/internal/commitlog"
	"github.com/eswan18/bifrost/internal/registry"
)

// This file is the terminal half of "attention first" promotion: it wires
// internal/commitlog into `bif promote` and `bif status` and renders what that
// package returns. Everything that DECIDES — which commits are in a range,
// which are unread, what kind each is, how many of each — is commitlog's, so
// the web app can show the same facts without re-deriving them. What is here
// is seams, timeouts and text.

// commitLister is the slice of commitlog.Client `bif status` needs: describe a
// range, and nothing else. Narrow for the reason podLister and buildLister
// are — commitlog.Client can also REMOVE a label, and a status command has no
// more business writing to GitHub than it has patching an Application.
type commitLister interface {
	Between(ctx context.Context, owner, repo, base, head string) (commitlog.Range, error)
}

// commitReader is what `bif promote` needs on top of that: the one GitHub
// write this CLI makes, removing the unreviewed label from a PR after a
// promotion the operator confirmed at a prompt that said it would.
type commitReader interface {
	commitLister
	MarkRead(ctx context.Context, owner, repo string, pr int) error
}

// commitlog.Client has to satisfy commitReader, or the fake the tests drive
// would be shaped like an interface the real client doesn't implement.
var _ commitReader = (*commitlog.Client)(nil)

// readOnlyCommits narrows the GitHub connection down to the reads `bif status`
// makes, exactly as readOnly does for the cluster: nothing on the status path
// can grow a label removal without changing this line first.
func readOnlyCommits(dial func(context.Context) (commitReader, error)) func(context.Context) (commitLister, error) {
	return func(ctx context.Context) (commitLister, error) { return dial(ctx) }
}

// defaultGitHubOwner is the owner of every fleet repo whose registry entry
// does not name another (registry.OwnerFor's fallback). The server reads the
// same default from GITHUB_ORG through internal/config; the CLI carries it as
// a constant, like defaultGCPProject, because it has no config to load and
// must not need one — one fleet, one default owner.
const defaultGitHubOwner = "eswan18"

// The whole GitHub phase of each command is bounded, dial included (the dial
// may run `gh auth token`). These bound the PHASE rather than each request,
// because the requests run concurrently — the same shape as
// digestLookupTimeout.
//
// promote gets a little longer than status: it is about to ask a question
// whose answer depends on this, so a slow answer is worth more there than in a
// table. Neither is long, because neither may make `bif` feel broken when
// GitHub is: the fallback is exactly the output this CLI printed before any of
// this existed. Vars so the tests can prove the bounds are applied without
// waiting for them.
var (
	promoteCommitTimeout = 5 * time.Second
	statusCommitTimeout  = 3 * time.Second
	// markReadTimeout bounds the label removals after a promotion. Prod has
	// already moved by then; this only decides how long the prompt takes to
	// come back if GitHub has stopped answering.
	markReadTimeout = 5 * time.Second
)

// mainBranch is the branch `bif status -vv` compares staging against to say
// what has merged and not yet deployed. Every fleet repo builds from main (the
// Cloud Build triggers watch it), so this is not configurable.
const mainBranch = "main"

// repoRef names one service's GitHub repo.
type repoRef struct{ owner, repo string }

func (r repoRef) String() string { return r.owner + "/" + r.repo }

// repoOf resolves a service to its repo through the registry — the same
// RepoFor/OwnerFor the server uses, so a service whose repo or owner differs
// from the default (the footstrike pair) is looked up where it actually lives.
func repoOf(reg registry.Registry, app string) repoRef {
	return repoRef{owner: reg.OwnerFor(app, defaultGitHubOwner), repo: reg.RepoFor(app)}
}

// commitQuery is one service's question: what is in base...head, and — for
// `bif status -vv` only — what is on main beyond head.
type commitQuery struct {
	app        string
	repo       repoRef
	base, head string
	withMain   bool
}

// commitInfo is the answer. Either pointer is nil when that range could not be
// read (or was not asked for), and nil means one thing to every renderer:
// print nothing about it.
type commitInfo struct {
	changes *commitlog.Range // base...head: what a promote would ship
	ahead   *commitlog.Range // head...main: merged, not yet on staging
}

// readCommits answers every query concurrently inside one timeout, and returns
// the client it dialled (promote needs it again to mark PRs as read).
//
// Three properties are deliberate, and they are resolveSameContent's:
//
//   - NOTHING happens when there are no queries. No dial, no `gh auth token`,
//     no request: a fleet with nothing to promote costs what it always did.
//   - One dial, therefore one token discovery, for the whole invocation.
//   - It never fails the command. The first error is RETURNED, not printed —
//     the caller prints one warning line for the whole phase, however many
//     services it covered — and a query that failed is simply absent from the
//     map, which every renderer reads as "say nothing extra".
//
// failed counts the queries with at least one unreadable range, for that
// warning.
func readCommits[C commitLister](ctx context.Context, timeout time.Duration, dial func(context.Context) (C, error), queries []commitQuery) (client C, infos map[string]commitInfo, failed int, firstErr error) {
	if len(queries) == 0 {
		return client, nil, 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client, err := dial(ctx)
	if err != nil {
		return client, nil, len(queries), err
	}

	var mu sync.Mutex
	infos = make(map[string]commitInfo, len(queries))
	var wg sync.WaitGroup
	for _, q := range queries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var info commitInfo
			var errs []error
			if r, err := client.Between(ctx, q.repo.owner, q.repo.repo, q.base, q.head); err != nil {
				errs = append(errs, err)
			} else {
				info.changes = &r
			}
			// Sequential with the first on purpose: both ranges are in one
			// repo, and commitlog caches a repo's unreviewed labels, so the
			// second range reuses the first's label read instead of racing it.
			if q.withMain {
				if r, err := client.Between(ctx, q.repo.owner, q.repo.repo, q.head, mainBranch); err != nil {
					errs = append(errs, err)
				} else {
					info.ahead = &r
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if info.changes != nil || info.ahead != nil {
				infos[q.app] = info
			}
			if len(errs) > 0 {
				failed++
				if firstErr == nil {
					firstErr = errs[0]
				}
			}
		}()
	}
	wg.Wait()
	return client, infos, failed, firstErr
}

// warnCommits prints the ONE line a failed GitHub read is allowed, on stderr
// so stdout stays what it was. One line per invocation, not per service: a
// GitHub outage during a whole-fleet status is one fact, and seven copies of
// it would bury the table it is a footnote to.
func warnCommits(stderr io.Writer, err error, failed, asked int) {
	if err == nil {
		return
	}
	reason := commitlog.Reason(err)
	if asked > 1 {
		reason += fmt.Sprintf(" (%d of %d services)", failed, asked)
	}
	outf(stderr, "Warning: could not read commits from GitHub: %s\n", reason)
}

// ---- rendering ----------------------------------------------------------

// commitsHint is appended where a summary stands in for a list, in `bif
// promote`. Two leading spaces: it trails a sentence.
const commitsHint = "  (--commits to list them)"

// commitLegend explains the two marks of a full commit list. One line, and
// only printed with the full list — the short form shows only "!" rows under a
// header that already says what they are.
const commitLegend = "    ! not read   ~ dependency bump or automation"

// subjectWidth caps a commit subject in a list. A subject is free text and one
// long one would push every author off the right edge of the terminal; the SHA
// and PR number beside it are the way to the full message.
const subjectWidth = 60

// writeChanges renders what a promotion would ship: one screen's worth by
// default, ordered by how much of the reader's attention each part needs.
//
//	21 commits since prod. 1 has not been read:
//
//	  ! b6e95f4  #277  Implement issue #221  haruspex-factory[bot]
//
//	Also: 1 dependency bump, 19 by you.  (--commits to list them)
//
// "Attention first": the unread PRs are listed by name because they are the
// ones nobody has looked at, and the rest are a count because the reader wrote
// or approved them. With nothing unread the whole block is one line. listAll
// (`--commits`, and always in `bif status -vv`) lists every commit instead,
// newest first, marked.
//
// It prints NOTHING for an empty range. Staging and prod can differ with no
// commits between them in this direction — prod ahead of staging after a
// rollback — and "0 commits since prod" would be a puzzle, not information.
// Callers print the blank line that separates this from what is above it.
func writeChanges(w io.Writer, r commitlog.Range, listAll bool, hint string) {
	if r.Total == 0 {
		return
	}
	unread := r.Count(commitlog.Unread)
	head := "  " + count(r.Total, "commit") + " since prod"
	others := otherCounts(r)

	switch {
	case unread > 0:
		outf(w, "%s. %d %s not been read:\n", head, unread, hasHave(unread))
	case listAll:
		outf(w, "%s%s:\n", head, parens(others))
	default:
		outf(w, "%s%s.%s\n", head, parens(others), hint)
	}

	switch {
	case listAll:
		outln(w)
		writeCommitRows(w, r.Commits)
		outln(w)
		outln(w, commitLegend)
	case unread > 0:
		outln(w)
		var rows []commitlog.Commit
		for _, c := range r.Commits {
			if c.Kind == commitlog.Unread {
				rows = append(rows, c)
			}
		}
		writeCommitRows(w, rows)
		// No "Also:" when everything is unread: there is no rest to count,
		// and nothing more for --commits to show.
		if others != "" {
			outln(w)
			outf(w, "  Also: %s.%s\n", others, hint)
		}
	}

	if r.Truncated() {
		// Said out loud, because the counts above cover only what came back:
		// an unread PR among the commits GitHub did not return is invisible
		// here, and silence about that would be an unearned all-clear.
		outf(w, "  GitHub returned only %d of the %d; an unread PR among the rest would not show here.\n", len(r.Commits), r.Total)
	}
}

// otherCounts summarises the commits that are NOT unread, in the order a
// reader should discount them: bumps, other automation, then their own. Empty
// groups are omitted, for writeSummary's reason — "0 by automation" invites a
// second look at whether it says 0.
func otherCounts(r commitlog.Range) string {
	var parts []string
	if n := r.Count(commitlog.Bump); n > 0 {
		parts = append(parts, count(n, "dependency bump"))
	}
	if n := r.Count(commitlog.Automation); n > 0 {
		parts = append(parts, fmt.Sprintf("%d by automation", n))
	}
	if n := r.Count(commitlog.Yours); n > 0 {
		parts = append(parts, fmt.Sprintf("%d by you", n))
	}
	return strings.Join(parts, ", ")
}

// parens wraps a non-empty summary for the one-line form.
func parens(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

func hasHave(n int) string {
	if n == 1 {
		return "has"
	}
	return "have"
}

// writeCommitRows lists commits one per line, in the order given (newest
// first, as commitlog returns them):
//
//	! b6e95f4  #277  Implement issue #221        haruspex-factory[bot]
//	~ 0c1d2e3  #276  Bump next from 15.1 to 15.2 dependabot[bot]
//	  9f8e7d6        Fix the thing               eswan18
//
// The mark shares a cell with the SHA so an unmarked row still starts in the
// same column. A commit with no PR leaves that cell blank rather than
// borrowing a dash: absent is the common case for a hand-pushed commit and
// should look like nothing.
func writeCommitRows(w io.Writer, commits []commitlog.Commit) {
	// The PR column exists only if some row fills it. Most of the fleet's
	// repos are pushed to by hand and have no PR numbers at all, and a column
	// of nothing would read as a rendering mistake.
	anyPR := false
	for _, c := range commits {
		anyPR = anyPR || c.PR != 0
	}
	rows := make([][]string, 0, len(commits))
	for _, c := range commits {
		pr, subject := "", c.Subject
		if c.PR != 0 {
			pr = fmt.Sprintf("#%d", c.PR)
			// The squash suffix the number was parsed from is dropped from the
			// subject: it now has a column of its own, and "(#277)" twice on
			// one line makes the issue number in the middle harder to tell
			// from the PR number at the end.
			subject = strings.TrimSuffix(subject, fmt.Sprintf(" (#%d)", c.PR))
		}
		row := []string{commitMark(c.Kind) + " " + c.SHA}
		if anyPR {
			row = append(row, pr)
		}
		row = append(row, clip(subject, subjectWidth))
		// An unknown author drops its cell instead of padding the subject out
		// to a column of trailing spaces.
		if c.Author != "" {
			row = append(row, c.Author)
		}
		rows = append(rows, row)
	}
	writeColumns(w, "    ", rows)
}

// commitMark is the one-character flag in front of a listed commit. Bumps and
// other automation share "~": both mean "a machine did this and it is not
// flagged unread", and a third glyph would need a longer legend than the
// distinction is worth in a list that already prints the author.
func commitMark(k commitlog.Kind) string {
	switch k {
	case commitlog.Unread:
		return "!"
	case commitlog.Bump, commitlog.Automation:
		return "~"
	default:
		return " "
	}
}

// clip shortens s to at most width runes, ending in an ellipsis when it cut.
func clip(s string, width int) string {
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	return string([]rune(s)[:width-1]) + "…"
}

// changesNote is the compact form for one table cell or plan line: the
// caller's lead ("21 commits", "21 to promote") plus ", 1 unread". The unread
// half is omitted at zero — in a column scanned for the exception, ", 0
// unread" on every row is noise that hides the one row that says otherwise.
// "" for an empty range, so there is nothing to append and the line it would
// have joined is unchanged.
func changesNote(lead string, r commitlog.Range) string {
	if r.Total == 0 {
		return ""
	}
	if n := r.Count(commitlog.Unread); n > 0 {
		lead += fmt.Sprintf(", %d unread", n)
	}
	return lead
}

// prList renders PR references for a prompt or a confirmation: up to three by
// name, then a count. The prompt is one line read immediately before an
// irreversible write, so it names enough to recognise and no more.
func prList(refs []string) string {
	const show = 3
	if len(refs) <= show {
		return strings.Join(refs, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(refs[:show], ", "), len(refs)-show)
}

// prRefs renders PR numbers as "#277", prefixed with the service ("haruspex#277")
// when several services share one prompt and a bare number would be ambiguous.
func prRefs(prefix string, prs []int) []string {
	out := make([]string, 0, len(prs))
	for _, n := range prs {
		out = append(out, fmt.Sprintf("%s#%d", prefix, n))
	}
	return out
}

// markReadClause is the sentence a prompt gains when confirming it will remove
// labels. It is part of the QUESTION on purpose: the label removal is a second
// consequence of the same keystroke, and a consequence the operator was not
// told about before pressing y is not one they agreed to. Empty when there is
// nothing unread, which leaves the prompt byte-for-byte what it always was.
func markReadClause(refs []string) string {
	if len(refs) == 0 {
		return ""
	}
	return fmt.Sprintf(" This marks %s as read.", prList(refs))
}

// settleUnread is what happens to a service's unread PRs once its promotion
// has SUCCEEDED. It never changes the exit code: prod has moved, and whether a
// label came off afterwards is bookkeeping about that fact, not part of it.
//
// confirmed is whether the operator answered a prompt that carried
// markReadClause. Only then are labels removed. With -y there was no prompt,
// so nobody was shown the PRs and nobody agreed to call them read: they stay
// labelled, and one line says so — a script promoting unattended must not
// quietly clear the very flag that exists to make a human look.
//
// A removal that fails is reported with the command that does it by hand,
// because the label is now wrong in the direction that nags (still unread,
// already promoted) and the operator should not have to go and work out how to
// fix it.
func settleUnread(ctx context.Context, gh commitReader, p promotePlan, confirmed bool, stdout, stderr io.Writer) {
	prs := p.unreadPRs()
	if len(prs) == 0 {
		return
	}
	if !confirmed {
		verb := "stays"
		if len(prs) > 1 {
			verb = "stay"
		}
		outf(stdout, "  %s %s labelled %s (-y asks nothing, so nothing was marked as read)\n",
			prList(prRefs("", prs)), verb, commitlog.UnreviewedLabel)
		return
	}

	ctx, cancel := context.WithTimeout(ctx, markReadTimeout)
	defer cancel()
	var marked []int
	for _, pr := range prs {
		if err := gh.MarkRead(ctx, p.repo.owner, p.repo.repo, pr); err != nil {
			outf(stderr, "Warning: could not mark #%d as read: %s\n", pr, commitlog.Reason(err))
			outf(stderr, "  Run: gh pr edit %d --remove-label %s --repo %s\n", pr, commitlog.UnreviewedLabel, p.repo)
			continue
		}
		marked = append(marked, pr)
	}
	if len(marked) > 0 {
		// Every number is named here, with no "and N more": this is the record
		// of a write, and a record that summarises is one you have to go and
		// verify.
		outf(stdout, "✓ Marked %s as read\n", strings.Join(prRefs("", marked), ", "))
	}
}
