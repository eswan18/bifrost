package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eswan18/bifrost/internal/auth"
	"github.com/eswan18/bifrost/internal/commitlog"
	"github.com/eswan18/bifrost/internal/promote"
)

// This file is the web half of "attention first" promotion (see
// internal/commitlog): what a promotion would ship, which of it nobody has
// read, and marking it read once the promotion has happened. `bif promote`
// prints the same data as terminal text; every decision about what counts as
// unread stays in commitlog, and this file only fetches, caches and lays out.
//
// Nothing here may matter to whether a page renders or a promotion happens.
// A lookup that fails, times out or has no client behind it is "no
// information", and the only trace of it is one quiet line in the promote
// dialog.

// CommitReader is the slice of internal/commitlog the handlers use. It is an
// interface so tests drive a fake, and so that a nil Handlers.Commits switches
// the whole feature off.
type CommitReader interface {
	Between(ctx context.Context, owner, repo, base, head string) (commitlog.Range, error)
	MarkRead(ctx context.Context, owner, repo string, pr int) error
}

// githubCommits picks a commitlog.Client per repo owner, the way
// internal/github's client picks a token per owner: fine-grained GitHub tokens
// are scoped to one owner, so the fleet's second owner has its own.
type githubCommits struct {
	fallback *commitlog.Client
	byOwner  map[string]*commitlog.Client // lower-cased owner → client
}

// NewGitHubCommits builds the production CommitReader from the server's GitHub
// tokens (config's GITHUB_TOKEN and GITHUB_TOKEN_<OWNER>). It returns nil —
// feature off — when there is no token at all: anonymous reads are limited to
// sixty an hour, which a polling dashboard would spend in minutes and then
// show nothing anyway.
//
// The clients are long-lived. That is safe because commitlog.Client expires
// its own label cache and drops it on MarkRead.
func NewGitHubCommits(defaultToken string, ownerTokens map[string]string) CommitReader {
	g := &githubCommits{byOwner: map[string]*commitlog.Client{}}
	for owner, token := range ownerTokens {
		if token != "" {
			g.byOwner[strings.ToLower(owner)] = commitlog.New(token)
		}
	}
	if defaultToken == "" && len(g.byOwner) == 0 {
		return nil
	}
	g.fallback = commitlog.New(defaultToken)
	return g
}

func (g *githubCommits) clientFor(owner string) *commitlog.Client {
	if c, ok := g.byOwner[strings.ToLower(owner)]; ok {
		return c
	}
	return g.fallback
}

func (g *githubCommits) Between(ctx context.Context, owner, repo, base, head string) (commitlog.Range, error) {
	return g.clientFor(owner).Between(ctx, owner, repo, base, head)
}

func (g *githubCommits) MarkRead(ctx context.Context, owner, repo string, pr int) error {
	return g.clientFor(owner).MarkRead(ctx, owner, repo, pr)
}

// --- reading ranges for the fleet view ---------------------------------------

const (
	// commitBudget bounds ALL of a page's commit lookups together. The page
	// waits this long at most and then renders without whatever is missing.
	commitBudget = 3 * time.Second
	// commitTTL is how long a range that was read is reused. The dashboard
	// re-renders every few seconds while something is deploying; without this
	// each of those would be several GitHub requests per drifted service.
	commitTTL = 60 * time.Second
	// commitErrTTL is how long a FAILED lookup is remembered. Short, so a
	// blip clears quickly, but not zero: with GitHub down every poll would
	// otherwise wait out the whole budget again.
	commitErrTTL = 15 * time.Second
	// markReadTimeout bounds the re-read and the label removals after a
	// promotion. Prod has already moved by then; this only decides how long
	// the response waits on bookkeeping.
	markReadTimeout = 5 * time.Second
	// maxShownPRs caps the hidden-field list a promote form may submit. A page
	// never renders more unread PRs than commitlog will describe.
	maxShownPRs = 50
)

// commitKey identifies one range of one repo. Tags are part of the key, so a
// new staging build is a new entry and never served a stale answer.
type commitKey struct{ owner, repo, base, head string }

type commitEntry struct {
	rng commitlog.Range
	err error
	at  time.Time
}

func (e commitEntry) fresh(now time.Time) bool {
	ttl := commitTTL
	if e.err != nil {
		ttl = commitErrTTL
	}
	return now.Sub(e.at) < ttl
}

// commitCache is the in-memory range cache. The zero value is ready to use.
type commitCache struct {
	mu sync.Mutex
	m  map[commitKey]commitEntry
}

func (c *commitCache) get(k commitKey, now time.Time) (commitEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || !e.fresh(now) {
		return commitEntry{}, false
	}
	return e, true
}

func (c *commitCache) put(k commitKey, e commitEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[commitKey]commitEntry{}
	}
	// Entries are keyed by tag pair, so old ones are never asked for again;
	// sweep them here rather than let the map grow with every deploy.
	for old, oe := range c.m {
		if !oe.fresh(e.at) {
			delete(c.m, old)
		}
	}
	c.m[k] = e
}

// forget drops every cached range of owner/repo. Called after a label is
// removed: the cached range still says that PR is unread.
func (c *commitCache) forget(owner, repo string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.m {
		if strings.EqualFold(k.owner, owner) && strings.EqualFold(k.repo, repo) {
			delete(c.m, k)
		}
	}
}

// commitKeyFor is the range a promotion of a would ship, and whether there is
// one to ask about: the service must be promotable and both tags must name a
// commit. An unpinned prod (`latest`) has no SHA to compare from.
func (h *Handlers) commitKeyFor(a appView) (commitKey, bool) {
	if a.PromoteState != promote.OutOfSync {
		return commitKey{}, false
	}
	return h.commitKey(a.Name, a.PromoteFrom, a.PromoteTo)
}

func (h *Handlers) commitKey(svc, fromTag, toTag string) (commitKey, bool) {
	k := commitKey{
		owner: h.Registry.OwnerFor(svc, h.Cfg.GitHubOrg),
		repo:  h.Registry.RepoFor(svc),
		base:  promote.ExtractSHA(fromTag),
		head:  promote.ExtractSHA(toTag),
	}
	return k, k.owner != "" && k.repo != "" && k.base != "" && k.head != ""
}

// attachChanges fills in appView.Changes for every promotable service: cached
// ranges immediately, the rest read concurrently inside one budget. Services
// whose range could not be read get a changesView that says only that.
func (h *Handlers) attachChanges(ctx context.Context, apps []appView) {
	if h.Commits == nil {
		return
	}
	now := time.Now()
	keys := map[int]commitKey{}
	got := map[commitKey]commitEntry{}
	var missing []commitKey
	for i, a := range apps {
		k, ok := h.commitKeyFor(a)
		if !ok {
			continue
		}
		keys[i] = k
		if _, dup := got[k]; dup {
			continue
		}
		if e, hit := h.commitCache.get(k, now); hit {
			got[k] = e
			continue
		}
		got[k] = commitEntry{err: context.DeadlineExceeded} // until read
		missing = append(missing, k)
	}

	if len(missing) > 0 {
		budget := h.commitBudget
		if budget <= 0 {
			budget = commitBudget
		}
		ctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()

		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, k := range missing {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rng, err := h.Commits.Between(ctx, k.owner, k.repo, k.base, k.head)
				e := commitEntry{rng: rng, err: err, at: time.Now()}
				if err != nil {
					// Logged where it happens, once per attempt: a cached
					// failure is not news again on every poll.
					slog.Warn("read commits failed", "repo", k.owner+"/"+k.repo,
						"base", k.base, "head", k.head, "reason", commitlog.Reason(err))
				}
				// A request the browser abandoned says nothing about GitHub.
				if !errors.Is(err, context.Canceled) {
					h.commitCache.put(k, e)
				}
				mu.Lock()
				got[k] = e
				mu.Unlock()
			}()
		}
		// The page does not depend on the client honouring its context: when
		// the budget ends, render with what has arrived.
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
		}

		mu.Lock()
		defer mu.Unlock()
	}

	for i, k := range keys {
		e := got[k]
		if e.err != nil {
			apps[i].Changes = &changesView{Unavailable: true}
			continue
		}
		apps[i].Changes = buildChanges(k.owner, k.repo, e.rng)
	}
}

// --- view model ---------------------------------------------------------------

// changesView is what a promotion would ship, laid out for the templates.
// nil on an appView means there is nothing to say (feature off, service not
// promotable, or an empty range).
type changesView struct {
	// Unavailable: the range could not be read. Every other field is zero and
	// the page shows one quiet line in the promote dialog and nothing else.
	Unavailable bool

	Note       string // "21 to promote" — the quiet half of the card note
	UnreadNote string // "1 unread"; "" at zero

	UnreadHead string      // "1 change nobody has read"
	Unread     []commitRow // one row per unread PR, newest first
	UnreadPRs  []int       // the same PRs, for the form's hidden fields
	Summary    string      // "Also 1 dependency bump and 19 by you"
	Truncated  string      // set when GitHub returned only part of the range
	AllLabel   string      // "All 21 commits"
	Commits    []commitRow // every returned commit, newest first
	Confirm    string      // confirm-button text when it also marks as read; else ""
}

// commitRow is one line of a list in the promote dialog.
type commitRow struct {
	SHA    string
	PR     int // 0 → none
	Title  string
	Author string
	URL    string // the PR when there is one, else the commit; "" → no link
	Mark   string // "unread" | "bump" | "bot" | ""
	Unread bool
}

// linkableSHA is what a commit SHA must look like before it is put in a URL.
// The SHA is a string GitHub returned; owner, repo and PR number are ours.
var linkableSHA = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// buildChanges lays a range out. It returns nil for an empty one: staging and
// prod can differ with no commits in this direction (prod ahead after a
// rollback), and "0 to promote" would be a puzzle, not information.
func buildChanges(owner, repo string, r commitlog.Range) *changesView {
	if r.Total == 0 {
		return nil
	}
	repoBase := "https://github.com/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)

	v := &changesView{
		Note:      fmt.Sprintf("%d to promote", r.Total),
		UnreadPRs: r.UnreadPRs(),
		AllLabel:  fmt.Sprintf("All %s", plural(len(r.Commits), "commit")),
	}
	seen := map[int]bool{}
	for _, c := range r.Commits {
		row := commitRow{SHA: c.SHA, PR: c.PR, Title: c.Subject, Author: c.Author, Unread: c.Kind == commitlog.Unread}
		if c.PR > 0 {
			// The number has a cell of its own; "(#277)" twice on one line
			// makes the issue number in the middle harder to tell apart.
			row.Title = strings.TrimSuffix(row.Title, fmt.Sprintf(" (#%d)", c.PR))
			row.URL = fmt.Sprintf("%s/pull/%d", repoBase, c.PR)
		} else if linkableSHA.MatchString(c.SHA) {
			row.URL = repoBase + "/commit/" + c.SHA
		}
		switch c.Kind {
		case commitlog.Unread:
			row.Mark = "unread"
		case commitlog.Bump:
			row.Mark = "bump"
		case commitlog.Automation:
			row.Mark = "bot"
		}
		v.Commits = append(v.Commits, row)
		if row.Unread && !seen[c.PR] {
			seen[c.PR] = true
			v.Unread = append(v.Unread, row)
		}
	}

	others := otherCounts(r)
	if n := len(v.UnreadPRs); n > 0 {
		v.UnreadNote = fmt.Sprintf("%d unread", n)
		v.UnreadHead = plural(n, "change") + " nobody has read"
		v.Confirm = fmt.Sprintf("Promote and mark %d as read", n)
		if others != "" {
			v.Summary = "Also " + others
		}
	} else if r.Truncated() {
		// No all-clear about a range that was only partly seen.
		v.Summary = others
	} else {
		v.Summary = "Nothing unread"
		if others != "" {
			v.Summary += " · " + others
		}
	}
	if r.Truncated() {
		v.AllLabel = fmt.Sprintf("%d of %d commits", len(r.Commits), r.Total)
		v.Truncated = fmt.Sprintf("GitHub returned only %d of the %d commits; an unread change among the rest would not show here.", len(r.Commits), r.Total)
	}
	return v
}

// otherCounts summarises the commits that are not unread, in the order a
// reader should discount them — "1 dependency bump and 19 by you". Empty
// groups are omitted.
func otherCounts(r commitlog.Range) string {
	var parts []string
	if n := r.Count(commitlog.Bump); n > 0 {
		parts = append(parts, plural(n, "dependency bump"))
	}
	if n := r.Count(commitlog.Automation); n > 0 {
		parts = append(parts, fmt.Sprintf("%d by automation", n))
	}
	if n := r.Count(commitlog.Yours); n > 0 {
		parts = append(parts, fmt.Sprintf("%d by you", n))
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// --- marking as read after a promotion ---------------------------------------

// parseShownPRs validates the promote form's hidden unread_pr fields: the PR
// numbers the dialog listed as unread. Digits only, positive, a bounded count.
// Anything else is an error — the page never produces it, so it is a request
// somebody built by hand, and the promotion is refused before it starts.
func parseShownPRs(values []string) ([]int, error) {
	if len(values) > maxShownPRs {
		return nil, fmt.Errorf("too many unread_pr values (%d)", len(values))
	}
	out := make([]int, 0, len(values))
	seen := map[int]bool{}
	for _, v := range values {
		if v == "" || len(v) > 9 {
			return nil, errors.New("invalid unread_pr value")
		}
		for _, c := range v {
			if c < '0' || c > '9' {
				return nil, errors.New("invalid unread_pr value")
			}
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, errors.New("invalid unread_pr value")
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out, nil
}

// settleUnread runs after a promotion has SUCCEEDED and returns what to append
// to its success message ("" for nothing). It never fails the request: prod
// has moved, and whether a label came off afterwards is bookkeeping.
//
// shown is the PRs the user's dialog listed — a claim from a form, and so not
// trusted on its own. The range that was actually promoted is read again, and
// a PR is marked only if it is in shown AND unread in that range. A PR that is
// unread in the range but was not shown stays labelled: nobody looked at it.
func (h *Handlers) settleUnread(ctx context.Context, sess *auth.Session, app, fromTag, toTag string, shown []int) string {
	if h.Commits == nil || len(shown) == 0 {
		return ""
	}
	fail := func(prs []int, reason string) string {
		slog.Warn("promote: mark as read failed", "user", sess.Email, "service", app,
			"prs", prs, "reason", reason)
		return fmt.Sprintf(" · could not mark %s as read: %s", prList(prs), reason)
	}
	k, ok := h.commitKey(app, fromTag, toTag)
	if !ok {
		return fail(shown, "no commit range to check them against")
	}

	ctx, cancel := context.WithTimeout(ctx, markReadTimeout)
	defer cancel()
	rng, err := h.Commits.Between(ctx, k.owner, k.repo, k.base, k.head)
	if err != nil {
		return fail(shown, commitlog.Reason(err))
	}

	wasShown := map[int]bool{}
	for _, pr := range shown {
		wasShown[pr] = true
	}
	var marked, failed []int
	var failReason string
	for _, pr := range rng.UnreadPRs() {
		if !wasShown[pr] {
			continue
		}
		delete(wasShown, pr)
		if err := h.Commits.MarkRead(ctx, k.owner, k.repo, pr); err != nil {
			failed = append(failed, pr)
			if failReason == "" {
				failReason = commitlog.Reason(err)
			}
			continue
		}
		marked = append(marked, pr)
	}
	if len(wasShown) > 0 {
		// Shown as unread, not unread in what was promoted: already marked by
		// someone else, or never in this range at all. Either way, not ours.
		left := make([]int, 0, len(wasShown))
		for _, pr := range shown {
			if wasShown[pr] {
				left = append(left, pr)
			}
		}
		slog.Info("promote: submitted PRs not unread in the promoted range, left alone",
			"user", sess.Email, "service", app, "prs", left)
	}

	var msg string
	if len(marked) > 0 {
		// The cached range still says these are unread.
		h.commitCache.forget(k.owner, k.repo)
		slog.Info("promote: marked as read", "user", sess.Email, "service", app,
			"repo", k.owner+"/"+k.repo, "prs", marked)
		msg += fmt.Sprintf(" · marked %s as read", prList(marked))
	}
	if len(failed) > 0 {
		msg += fail(failed, failReason)
	}
	return msg
}

// prList renders PR numbers for the flash message: "#277, #280". The flash
// travels in a cookie, so a long list is cut off with a count; the log line
// beside it always carries every number.
func prList(prs []int) string {
	const show = 8
	refs := make([]string, 0, show)
	for i, pr := range prs {
		if i == show {
			break
		}
		refs = append(refs, fmt.Sprintf("#%d", pr))
	}
	s := strings.Join(refs, ", ")
	if len(prs) > show {
		s += fmt.Sprintf(" and %d more", len(prs)-show)
	}
	return s
}
