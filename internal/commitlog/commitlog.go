// Package commitlog describes the commits between two SHAs of a service's
// repo, and says which of them nobody has read.
//
// It exists for "attention first" promotion. In some fleet repos an automation
// (a "factory" — haruspex has the first) merges pull requests with no human
// reading them, and labels each one `unreviewed`. The owner reviews at promotion time — so the
// moment `bif promote` asks its question is the moment to put exactly those
// PRs in front of them, and confirming is what marks them read. Before this
// package that was a GitHub search to remember and a label to remove by hand.
//
// Two halves, kept apart on purpose:
//
//   - pure logic: ParsePR, Classify, and the Range summary methods. No I/O, so
//     the decision about what counts as unread is testable without a server.
//   - a thin HTTP layer (Client): the compare endpoint, the labelled-issues
//     listing, and the one label removal. Hand-rolled net/http, like
//     internal/github and internal/oci — three endpoints do not justify an SDK.
//
// It is its own package rather than more methods on internal/github.Client for
// two reasons. That interface is the SERVER's (preview detection, with the
// server's per-owner tokens), and widening it would drag every fake of it
// along. And cmd/bif's status and promote files are forbidden a raw net/http
// import (see cmd/bif/main_test.go): a third-party read arrives as a package
// that owns its protocol, the way internal/gcb did.
//
// Everything that is not presentation lives here — fetching, classifying,
// counting, removing the label — as exported functions that take a context and
// return data. Nothing in this package prints, reads the environment or runs a
// process: cmd/bif renders it as terminal text today and the web app will
// render the same data next, and neither should have to re-derive a decision.
// Even the token is an argument (see New); finding one is the caller's job.
//
// Nothing here is allowed to matter to whether a promotion happens. Every
// caller treats an error from this package as "say nothing extra".
package commitlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The two names below are a FLEET-WIDE convention, defined here and nowhere
// else. They are not specific to haruspex, which merely happens to be the
// first repo with a factory: any repo's merge automation can opt in by putting
// UnreviewedLabel on a pull request it merged with no human reading it, and
// from then on `bif` (and the web app, which shows the same information)
// surfaces that PR at promotion time with no per-repo configuration.
const (
	// UnreviewedLabel is the label a factory puts on a PR it merged unread.
	// Removing it is what "marking as read" means.
	UnreviewedLabel = "unreviewed"

	// BumpBotLogin is the author of a dependency bump. Dependabot is a Bot
	// like any other automation, but its commits are their own category: "3
	// dependency bumps" and "3 commits by some automation" ask for different
	// amounts of a reader's attention.
	BumpBotLogin = "dependabot[bot]"
)

// CompareCap is the most commits GitHub's compare endpoint returns in one
// unpaginated response. A range longer than this comes back truncated, with
// total_commits still telling the truth — see Range.Truncated.
const CompareCap = 250

// httpTimeout is a backstop only. Every caller passes a context with a much
// shorter deadline (a few seconds for the whole lookup, not per request); this
// just guarantees a request made with a bare context still ends.
const httpTimeout = 10 * time.Second

// Kind is what a commit asks of the person promoting it. Every commit is
// exactly one.
type Kind int

const (
	// Yours is everything that is not one of the others: a commit a person
	// wrote or merged knowingly. The zero value, so an unclassified commit
	// reads as the unremarkable case rather than as an alarm.
	Yours Kind = iota
	// Unread: the commit's PR carries the unreviewed label. This wins over
	// every other kind — a factory PR is authored by a Bot, and "automation"
	// would bury the one thing this package exists to surface.
	Unread
	// Bump: authored by dependabot.
	Bump
	// Automation: authored by any other Bot account.
	Automation
)

// Commit is one commit in a range, reduced to what a promote prompt shows.
type Commit struct {
	SHA     string // abbreviated to seven characters
	Subject string // first line of the message
	// PR is the pull request the commit came from. Zero when there is none.
	//
	// For an Unread commit it is the labelled PR whose merge commit this IS,
	// as GitHub records it. For every other commit it is only what the subject
	// claims in its trailing " (#123)": good enough to label a row, and never
	// used to decide anything, because whoever writes a commit writes its
	// subject.
	PR     int
	Author string // GitHub login; the git author name when no account is linked
	Bot    bool   // the author account's type is "Bot"
	Kind   Kind
}

// Range is the commits reachable from head and not from base.
type Range struct {
	// Commits is NEWEST FIRST. GitHub returns them oldest first; they are
	// reversed once, here, because every reader of this wants the top of the
	// list to be the most recent change.
	Commits []Commit
	// Total is GitHub's total_commits: how many commits the range really
	// holds, which exceeds len(Commits) when the response was capped.
	Total int
	// full holds each commit's whole SHA, parallel to Commits. Kept out of
	// Commit because nothing that renders one wants forty characters; Between
	// needs it to match a commit against a PR's recorded merge commit.
	full []string
}

// Truncated reports whether GitHub returned fewer commits than the range
// holds. Callers must say so: an unread PR among the commits that were not
// returned is invisible here, and a count that silently covered only part of
// the range would be an all-clear nobody checked.
func (r Range) Truncated() bool { return r.Total > len(r.Commits) }

// Count returns how many of the returned commits are of kind k.
func (r Range) Count(k Kind) int {
	n := 0
	for _, c := range r.Commits {
		if c.Kind == k {
			n++
		}
	}
	return n
}

// UnreadPRs returns the distinct PR numbers behind the range's unread commits,
// in the order the commits appear (newest first). Distinct because a PR merged
// without squashing contributes several commits and is still one thing to mark
// as read.
func (r Range) UnreadPRs() []int {
	var out []int
	seen := map[int]bool{}
	for _, c := range r.Commits {
		if c.Kind != Unread || seen[c.PR] {
			continue
		}
		seen[c.PR] = true
		out = append(out, c.PR)
	}
	return out
}

// trailingPR matches the squash-merge convention: GitHub appends " (#123)" to
// the subject. Anchored at the end on purpose — "Implement issue #221 (#277)"
// is PR 277, and the 221 in the middle is somebody's prose.
var trailingPR = regexp.MustCompile(` \(#(\d+)\)$`)

// ParsePR returns the PR number at the end of a commit subject, or 0 when
// there is none. A merge commit, a direct push and a rebase-merge all have
// none, and that is fine: they cannot be unread, because unread is a property
// of a labelled PR.
func ParsePR(subject string) int {
	m := trailingPR.FindStringSubmatch(strings.TrimRight(subject, " \t\r"))
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// Classify decides a commit's kind. unread says whether this commit is the
// recorded merge commit of a PR that carries the unreviewed label — a fact
// about the commit's SHA, established by Between, and deliberately not
// something read off the subject line: a subject that says "(#12)" is a claim
// by whoever wrote the commit, and one that could otherwise hide an unread
// change or get somebody else's PR marked as read. The order of the cases is
// the priority order; see the Kind constants.
func Classify(c Commit, unread bool) Kind {
	switch {
	case unread:
		return Unread
	case c.Author == BumpBotLogin:
		return Bump
	case c.Bot:
		return Automation
	default:
		return Yours
	}
}

// ---- the HTTP layer -----------------------------------------------------

// Client reads commit ranges and unreviewed labels from GitHub's REST API.
type Client struct {
	token   string
	baseURL string
	http    *http.Client

	// labels and merges cache the two label answers per owner/repo, so that
	// `bif status -vv`, which asks about two ranges of the same repo, asks
	// GitHub once. They are dropped after cacheTTL: a CLI run never lives that
	// long, but a server holding one Client for days must not go on showing
	// a PR as unread after its label has been removed, or miss a new one.
	mu       sync.Mutex
	cachedAt map[string]time.Time
	labels   map[string]map[int]bool
	merges   map[string]map[string]int
}

// cacheTTL is how long a repo's label answers are reused.
const cacheTTL = 30 * time.Second

// fresh reports whether key's cached answers may still be used, and forgets
// them if not. Callers hold c.mu.
func (c *Client) fresh(key string) bool {
	at, ok := c.cachedAt[key]
	if ok && time.Since(at) < cacheTTL {
		return true
	}
	delete(c.labels, key)
	delete(c.merges, key)
	delete(c.cachedAt, key)
	return false
}

// Forget drops what is cached about owner/repo. MarkRead calls it, so that a
// reader sees the label gone at once and not cacheTTL later.
func (c *Client) Forget(owner, repo string) {
	key := strings.ToLower(owner + "/" + repo)
	c.mu.Lock()
	delete(c.labels, key)
	delete(c.merges, key)
	delete(c.cachedAt, key)
	c.mu.Unlock()
}

// New builds a client. token may be empty: unauthenticated requests still read
// public repos, at a lower rate limit, and a private repo simply reads as an
// error the caller already knows how to ignore.
func New(token string) *Client {
	return NewWithBaseURL(token, "https://api.github.com")
}

// NewWithBaseURL exists as a test seam; production code uses New.
func NewWithBaseURL(token, baseURL string) *Client {
	return &Client{
		token:    token,
		baseURL:  strings.TrimRight(baseURL, "/"),
		http:     &http.Client{Timeout: httpTimeout},
		labels:   map[string]map[int]bool{},
		merges:   map[string]map[string]int{},
		cachedAt: map[string]time.Time{},
	}
}

// do sends one request and returns the response for the statuses in ok. Any
// other status is an error that names the status and never the token — the
// URL carries no credential, and the Authorization header is not echoed.
func (c *Client) do(ctx context.Context, method, reqURL, what string, ok ...int) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, reqURL, nil)
	if err != nil {
		return nil, err
	}
	// The header is omitted entirely without a token: "Bearer " with nothing
	// after it is a 401, where no header at all is an anonymous read.
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	for _, code := range ok {
		if resp.StatusCode == code {
			return resp, nil
		}
	}
	// Drained and closed here so every error path releases the connection.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	// An exhausted rate limit is a 403 (or 429) like any other refusal, and it
	// is the one an anonymous client will actually meet — sixty requests an
	// hour — so it is worth its own words.
	if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) && resp.Header.Get("X-RateLimit-Remaining") == "0" {
		return nil, fmt.Errorf("github %s: rate limit exceeded", what)
	}
	return nil, fmt.Errorf("github %s returned %d", what, resp.StatusCode)
}

// Compare returns the commits in base...head — reachable from head, not from
// base — UNCLASSIFIED: every Kind is Yours until Between has the labels.
//
// Short SHAs work for both ends, and so does a branch name. The response is
// GitHub's unpaginated one, which stops at CompareCap commits; Total carries
// the real size so the caller can say what it did not see.
func (c *Client) Compare(ctx context.Context, owner, repo, base, head string) (Range, error) {
	// The "..." is part of the path, not a separator to escape; each ref is
	// escaped on its own.
	reqURL := fmt.Sprintf("%s/repos/%s/%s/compare/%s...%s", c.baseURL,
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(base), url.PathEscape(head))
	resp, err := c.do(ctx, http.MethodGet, reqURL, "compare", http.StatusOK)
	if err != nil {
		return Range{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	var body struct {
		TotalCommits int `json:"total_commits"`
		Commits      []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
				Author  struct {
					Name string `json:"name"`
				} `json:"author"`
			} `json:"commit"`
			// Author is the GitHub ACCOUNT the commit is linked to, and is
			// null when the commit's email matches no account.
			Author *struct {
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"author"`
		} `json:"commits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Range{}, fmt.Errorf("github compare: decoding: %w", err)
	}

	out := Range{Total: body.TotalCommits, Commits: make([]Commit, 0, len(body.Commits))}
	for i := len(body.Commits) - 1; i >= 0; i-- {
		raw := body.Commits[i]
		subject, _, _ := strings.Cut(raw.Commit.Message, "\n")
		subject = strings.TrimSpace(subject)
		commit := Commit{SHA: shortSHA(raw.SHA), Subject: subject, PR: ParsePR(subject), Author: raw.Commit.Author.Name}
		if raw.Author != nil && raw.Author.Login != "" {
			commit.Author = raw.Author.Login
			commit.Bot = raw.Author.Type == "Bot"
		}
		out.Commits = append(out.Commits, commit)
		out.full = append(out.full, raw.SHA)
	}
	// total_commits is trusted for one thing — being LARGER than what came
	// back. A response that omits it must not make a full list look empty.
	if out.Total < len(out.Commits) {
		out.Total = len(out.Commits)
	}
	return out, nil
}

// shortSHA abbreviates to the seven characters the image tags use, so a commit
// in this list can be matched by eye against the tag columns above it.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// labelPageSize is GitHub's maximum, and maxLabelPages bounds the walk: a
// thousand closed-and-still-unreviewed PRs is far past the point where the
// answer matters more than the latency, and an API that never returned a short
// page must not be able to hold `bif promote` in a loop.
const (
	labelPageSize = 100
	maxLabelPages = 10
)

// UnreviewedPRs returns the numbers of the closed PULL REQUESTS that carry the
// unreviewed label.
//
// The issues endpoint is used because it is the one that filters by label; it
// returns issues and PRs alike, and a PR is the entry with a `pull_request`
// key. An unreviewed-labelled plain issue is not something a commit can have
// been merged from, so it is dropped rather than risk its number colliding
// with a "(#123)" in a subject.
func (c *Client) UnreviewedPRs(ctx context.Context, owner, repo string) (map[int]bool, error) {
	key := strings.ToLower(owner + "/" + repo)
	c.mu.Lock()
	cached, ok := c.labels[key]
	ok = ok && c.fresh(key)
	c.mu.Unlock()
	if ok {
		return cached, nil
	}

	out := map[int]bool{}
	for page := 1; page <= maxLabelPages; page++ {
		q := url.Values{}
		q.Set("labels", UnreviewedLabel)
		q.Set("state", "closed")
		q.Set("per_page", strconv.Itoa(labelPageSize))
		q.Set("page", strconv.Itoa(page))
		reqURL := fmt.Sprintf("%s/repos/%s/%s/issues?%s", c.baseURL, url.PathEscape(owner), url.PathEscape(repo), q.Encode())
		n, err := c.labelPage(ctx, reqURL, out)
		if err != nil {
			return nil, err
		}
		// A short page is the last one. Counted before the pull_request
		// filter: the page size is about entries returned, not entries kept.
		if n < labelPageSize {
			break
		}
	}

	c.mu.Lock()
	c.labels[key] = out
	c.cachedAt[key] = time.Now()
	c.mu.Unlock()
	return out, nil
}

// labelPage reads one page of the listing into out and returns how many
// entries the page held.
func (c *Client) labelPage(ctx context.Context, reqURL string, out map[int]bool) (int, error) {
	resp, err := c.do(ctx, http.MethodGet, reqURL, "label lookup", http.StatusOK)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	var issues []struct {
		Number      int              `json:"number"`
		PullRequest *json.RawMessage `json:"pull_request"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&issues); err != nil {
		return 0, fmt.Errorf("github label lookup: decoding: %w", err)
	}
	for _, issue := range issues {
		if issue.PullRequest != nil {
			out[issue.Number] = true
		}
	}
	return len(issues), nil
}

// maxUnreadPRs bounds how many labelled PRs are looked up one by one. The
// label is removed at every promotion, so more than this many means something
// has gone wrong, and the honest answer then is "could not tell".
const maxUnreadPRs = 50

// unreadMerges maps the merge commit of every merged PR that carries the
// unreviewed label to that PR's number. This, and not a number in a subject
// line, is what makes a commit unread: GitHub records which commit a PR
// became, and nobody who writes commits can change that record.
func (c *Client) unreadMerges(ctx context.Context, owner, repo string) (map[string]int, error) {
	key := strings.ToLower(owner + "/" + repo)
	c.mu.Lock()
	cached, ok := c.merges[key]
	ok = ok && c.fresh(key)
	c.mu.Unlock()
	if ok {
		return cached, nil
	}

	labelled, err := c.UnreviewedPRs(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	if len(labelled) > maxUnreadPRs {
		return nil, fmt.Errorf("github: %d pull requests are labelled %s, too many to check", len(labelled), UnreviewedLabel)
	}
	out := make(map[string]int, len(labelled))
	for pr := range labelled {
		sha, err := c.mergeCommit(ctx, owner, repo, pr)
		if err != nil {
			return nil, err
		}
		// A labelled PR that was closed without merging has no commit on the
		// branch, so there is nothing of it to ship.
		if sha != "" {
			out[sha] = pr
		}
	}

	c.mu.Lock()
	c.merges[key] = out
	c.mu.Unlock()
	return out, nil
}

// mergeCommit returns the full SHA of the commit a PR was merged as, or ""
// if it was not merged.
func (c *Client) mergeCommit(ctx context.Context, owner, repo string, pr int) (string, error) {
	reqURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, url.PathEscape(owner), url.PathEscape(repo), pr)
	resp, err := c.do(ctx, http.MethodGet, reqURL, "pull request lookup", http.StatusOK)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Merged         bool   `json:"merged"`
		MergeCommitSHA string `json:"merge_commit_sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("github pull request lookup: decoding: %w", err)
	}
	if !body.Merged {
		return "", nil
	}
	return body.MergeCommitSHA, nil
}

// Between is Compare with every commit classified: the one call the CLI makes.
//
// A commit is unread when it is the merge commit of a labelled PR. The labels
// are asked for on every range, whatever the subjects say: skipping the
// question when no subject names a PR would let a subject decide the answer.
func (c *Client) Between(ctx context.Context, owner, repo, base, head string) (Range, error) {
	r, err := c.Compare(ctx, owner, repo, base, head)
	if err != nil {
		return Range{}, err
	}
	// Not degraded to "none are unread" on failure: a range whose labels could
	// not be read is a range this package cannot describe, and saying
	// "0 unread" about it would be a false all-clear.
	merges, err := c.unreadMerges(ctx, owner, repo)
	if err != nil {
		return Range{}, err
	}
	for i := range r.Commits {
		pr, unread := 0, false
		if i < len(r.full) {
			pr, unread = merges[r.full[i]]
		}
		if unread {
			r.Commits[i].PR = pr
		}
		r.Commits[i].Kind = Classify(r.Commits[i], unread)
	}
	return r, nil
}

// MarkRead removes the unreviewed label from one PR. It is the only write in
// this package, and the caller makes it only after a promotion the operator
// confirmed at a prompt that said this would happen.
//
// A 404 is success: the label (or the PR's claim to it) is already gone, which
// is the state being asked for — somebody removed it by hand, or a second
// promote raced this one.
func (c *Client) MarkRead(ctx context.Context, owner, repo string, pr int) error {
	reqURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d/labels/%s", c.baseURL,
		url.PathEscape(owner), url.PathEscape(repo), pr, url.PathEscape(UnreviewedLabel))
	resp, err := c.do(ctx, http.MethodDelete, reqURL, "label removal", http.StatusOK, http.StatusNoContent, http.StatusNotFound)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	c.Forget(owner, repo)
	return nil
}

// Reason renders an error from this package as the few words a one-line
// warning has room for. A deadline is named as one — the callers' timeouts are
// the expected way for this to fail, and Go's wrapped `Get "https://...":
// context deadline exceeded` buries that under a URL.
func Reason(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	// net/http wraps transport failures in *url.Error, whose text leads with
	// the method and the full URL. The cause is the part worth a line.
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err.Error()
	}
	return err.Error()
}
