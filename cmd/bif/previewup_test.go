package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/eswan18/bifrost/internal/preview"
	"github.com/eswan18/bifrost/internal/previewapi"
	"github.com/eswan18/bifrost/internal/previewclient"
)

// Tests for `bif preview up`. Everything here runs against the httptest-backed
// fakeBifrost from preview_test.go with the clock, the sleep and the TTY
// decision injected — no live bifrost, no gcloud, no terminal, and no real
// waiting. The 30-minute timeout is asserted in microseconds.

// ---- the scripted server ------------------------------------------------

// canned is one scripted HTTP response.
type canned struct {
	status int
	body   string
}

var (
	// notFound is bifrost's answer for a tag with no namespace behind it. It
	// is the ordinary state of a preview being created, not an error.
	notFound = canned{http.StatusNotFound, `{"error":"no preview feat-x"}`}
	// accepted is the 202 POST /api/previews answers with: the tag it derived
	// and a creating phase, sent long before the namespace exists.
	accepted = canned{http.StatusAccepted, `{"tag":"feat-x","phase":"creating"}`}
)

// upScript answers the requests `preview up` makes, in the order it makes
// them: the pre-POST lookup (the first GET), the POST, then the poll GETs.
// Running past the end of `polls` repeats the last one, which is what lets the
// timeout test poll a stuck preview forever without a fixture per poll.
type upScript struct {
	mu    sync.Mutex
	pre   canned
	post  canned
	polls []canned
	gets  int
}

func (s *upScript) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		var c canned
		switch r.Method {
		case http.MethodPost:
			c = s.post
		default:
			n := s.gets
			s.gets++
			switch {
			case n == 0:
				c = s.pre
			case len(s.polls) == 0:
				c = notFound
			case n-1 < len(s.polls):
				c = s.polls[n-1]
			default:
				c = s.polls[len(s.polls)-1]
			}
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(c.status)
		_, _ = w.Write([]byte(c.body))
	}
}

// newUp wires a scripted server up to a fake bifrost.
func newUp(t *testing.T, s *upScript) *fakeBifrost {
	t.Helper()
	if s.post.status == 0 {
		s.post = accepted
	}
	if s.pre.status == 0 {
		s.pre = notFound
	}
	return newFakeBifrost(t, s.handler())
}

// ---- the injected clock -------------------------------------------------

// clockStart is the instant every test's clock begins at, so the elapsed times
// in the expected output are arithmetic rather than luck.
var clockStart = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

// fakeClock advances only when the code under test sleeps, so a poll loop runs
// at full speed while its own view of time moves exactly three seconds per
// poll.
type fakeClock struct {
	t time.Time
	// jump overrides how far a sleep moves the clock. Zero means "as long as
	// was asked for"; the timeout test sets it to fast-forward.
	jump time.Duration
}

func newClock() *fakeClock { return &fakeClock{t: clockStart} }

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) sleep(d time.Duration) {
	if c.jump != 0 {
		d = c.jump
	}
	c.t = c.t.Add(d)
}

// pipedEnv and ttyEnv are the two rendering worlds. A Go test's stdout is never
// a terminal, so without this seam the TTY branch could not run at all outside
// production.
func pipedEnv(c *fakeClock) previewEnv {
	return previewEnv{now: c.now, sleep: c.sleep, isTTY: false}
}

func ttyEnv(c *fakeClock) previewEnv {
	return previewEnv{now: c.now, sleep: c.sleep, isTTY: true}
}

// execUp runs one `bif preview up ...` against the fake.
func execUp(f *fakeBifrost, env previewEnv, args ...string) (stdout, stderr string, code int) {
	var out, errb bytes.Buffer
	code = previewUpCmd(context.Background(), args, &out, &errb, f.dial(), env)
	return out.String(), errb.String(), code
}

// ---- fixtures -----------------------------------------------------------

// Wire text, written by hand rather than marshalled from previewapi.Record,
// for the same reason preview_test.go's listFixture is: the struct is shared
// with the server, so a marshal-then-unmarshal fixture would pass whatever the
// fields were called. These carry the server's key names literally.

const readyRecord = `{"tag":"feat-x","branch":"feat/x","apps":["footstrike-api"],"phase":"ready",
  "health":"healthy","createdAt":"2026-07-31T11:59:00Z",
  "urls":{"identity":"https://identity-feat-x.preview.footstrike.run",
          "footstrike-api":"https://footstrike-api-feat-x.preview.footstrike.run"}}`

// creatingRecord is mid-create with a named step and the server's own
// timestamp for when that step started.
func creatingRecord(step, stepSince string) string {
	return `{"tag":"feat-x","branch":"feat/x","apps":["footstrike-api"],"phase":"creating",
	  "health":"unknown","createdAt":"2026-07-31T11:59:00Z","urls":{},
	  "step":"` + step + `","stepSince":"` + stepSince + `"}`
}

func ok(body string) canned { return canned{http.StatusOK, body} }

// ---- argument parsing ---------------------------------------------------

// TestParseUpArgs pins ib.py's parse_up_args, rejections included. The
// rejections are the reason it exists: a flag that is not consumed is a flag
// that is silently ignored, and `--ttl=8h` once produced a preview that never
// expired from a command that explicitly asked it to.
func TestParseUpArgs(t *testing.T) {
	accepted := []struct {
		name string
		args []string
		want upArgs
	}{
		{"just a branch", []string{"my-branch"}, upArgs{branch: "my-branch"}},
		{"ttl after", []string{"my-branch", "--ttl", "8h"}, upArgs{branch: "my-branch", ttl: "8h", ttlSet: true}},
		{"ttl before", []string{"--ttl", "8h", "my-branch"}, upArgs{branch: "my-branch", ttl: "8h", ttlSet: true}},
		{"no-wait", []string{"my-branch", "--no-wait"}, upArgs{branch: "my-branch", noWait: true}},
		{"auto-update", []string{"my-branch", "--auto-update"}, upArgs{branch: "my-branch", autoUpdate: true}},
		{
			// The docstring's example, and the composition it promises works in
			// any order.
			name: "everything at once",
			args: []string{"my-branch", "--ttl", "8h", "--auto-update", "--no-wait"},
			want: upArgs{branch: "my-branch", ttl: "8h", ttlSet: true, autoUpdate: true, noWait: true},
		},
		{
			name: "everything at once, branch last",
			args: []string{"--no-wait", "--auto-update", "--ttl", "8h", "my-branch"},
			want: upArgs{branch: "my-branch", ttl: "8h", ttlSet: true, autoUpdate: true, noWait: true},
		},
		{
			// The flags are stripped before --ttl's value is read, so an
			// interleaved flag does not become the TTL.
			name: "a flag between --ttl and its value",
			args: []string{"my-branch", "--ttl", "--no-wait", "8h"},
			want: upArgs{branch: "my-branch", ttl: "8h", ttlSet: true, noWait: true},
		},
		{
			// An explicitly empty TTL is a value, not an absence: bifrost owns
			// what a bad duration means, and ttlSet still makes the run warn if
			// no expiry comes back.
			name: "an empty ttl is still a request",
			args: []string{"my-branch", "--ttl", ""},
			want: upArgs{branch: "my-branch", ttl: "", ttlSet: true},
		},
		{
			// A branch may legitimately be named after a flag-ish string as
			// long as it is the one leftover token.
			name: "a branch that looks like a value",
			args: []string{"--ttl", "8h", "8h"},
			want: upArgs{branch: "8h", ttl: "8h", ttlSet: true},
		},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseUpArgs(tc.args)
			if !ok {
				t.Fatalf("parseUpArgs(%q) rejected the invocation", tc.args)
			}
			if got != tc.want {
				t.Errorf("parseUpArgs(%q) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}

	rejected := []struct {
		name string
		args []string
	}{
		{"nothing at all", nil},
		{"flags but no branch", []string{"--no-wait"}},
		{"ttl but no branch", []string{"--ttl", "8h"}},
		{"two branches", []string{"branch-a", "branch-b"}},
		// The one this whole check exists for. This file does not parse the
		// equals form, so before the leftover check it sat unconsumed and the
		// TTL was silently dropped.
		{"the equals form of --ttl", []string{"my-branch", "--ttl=8h"}},
		{"the equals form of --auto-update", []string{"my-branch", "--auto-update=true"}},
		{"a typo'd flag", []string{"my-branch", "--no-wat"}},
		{"an unknown flag", []string{"my-branch", "--force"}},
		{"--ttl with nothing after it", []string{"my-branch", "--ttl"}},
		// Without the missing-value check this one is not caught by the
		// leftover-token check either: it reduces to a single token, and that
		// token would become the branch.
		{"--ttl as the only argument", []string{"--ttl"}},
		// --auto-update is stripped first, leaving --ttl at the end with no
		// value at all.
		{"--ttl whose value was a flag", []string{"my-branch", "--ttl", "--auto-update"}},
		// Only the first --ttl pair is consumed, so the second leaves two
		// tokens behind rather than letting one of two values win silently.
		{"a repeated --ttl", []string{"my-branch", "--ttl", "8h", "--ttl", "90m"}},
	}
	for _, tc := range rejected {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			if got, ok := parseUpArgs(tc.args); ok {
				t.Errorf("parseUpArgs(%q) accepted the invocation as %+v; it must be refused, "+
					"since an argument nobody consumes is a request nobody honours", tc.args, got)
			}
		})
	}
}

// TestPreviewUpRejectsBadArgumentsBeforeTouchingBifrost: the usage message
// goes to stdout and NOTHING goes on the wire. A preview created by a command
// that was then rejected would be the worst of both.
func TestPreviewUpRejectsBadArgumentsBeforeTouchingBifrost(t *testing.T) {
	for _, args := range [][]string{
		{"my-branch", "--ttl=8h"},
		{"my-branch", "--auto-update=true"},
		{"my-branch", "--ttl"},
		{"branch-a", "branch-b"},
		{},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			f := newFakeBifrost(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("a rejected invocation still sent %s %s", r.Method, r.URL.Path)
			})
			stdout, _, code := execUp(f, pipedEnv(newClock()), args...)
			if code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			if !strings.Contains(stdout, previewUpUsage) {
				t.Errorf("stdout = %q, want the usage line", stdout)
			}
			if n := len(f.requests()); n != 0 {
				t.Errorf("%d requests sent; want none", n)
			}
		})
	}
}

// ---- the tag, and the mirror that is gone -------------------------------

// TestPreviewUpDerivesTheTagWithTheServersOwnRule is what replaces ib.py's
// hand-written tag_for_branch mirror. The pre-POST lookup needs a tag before
// the server has supplied one, and ib.py answered that with a second
// implementation of the mapping that decides which namespace a branch lands
// in. cmd/bif calls preview.TagForBranch instead.
//
// The assertion is the URL the pre-POST GET went to, compared against
// preview.TagForBranch's own answer for a branch that exercises the mapping's
// folding and lowercasing — so a reintroduced copy that disagreed anywhere
// would have to disagree here.
func TestPreviewUpDerivesTheTagWithTheServersOwnRule(t *testing.T) {
	const branch = "Feat/Billing_Fix"
	want := "/api/previews/" + preview.TagForBranch(branch)
	if want != "/api/previews/feat-billing-fix" {
		t.Fatalf("preview.TagForBranch(%q) = %q; this test's premise moved", branch, preview.TagForBranch(branch))
	}

	f := newUp(t, &upScript{post: canned{http.StatusAccepted, `{"tag":"feat-billing-fix","phase":"creating"}`}})
	_, _, code := execUp(f, pipedEnv(newClock()), branch, "--no-wait")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	got := f.requests()
	if len(got) == 0 || got[0].Method != http.MethodGet || got[0].Path != want {
		t.Errorf("first request = %+v, want a GET %s", got, want)
	}
}

// TestPreviewUpDropsThePrePostSnapshotOnATagMismatch. The guessed tag is only
// a guess; the POST response is authoritative. When they disagree, the record
// fetched beforehand describes some OTHER preview, so it is thrown away rather
// than used to claim an update or to compute a rebuild summary from another
// preview's images.
//
// This is the safety property that made ib.py's mirror tolerable, and it has
// to survive the mirror's removal: preview.TagForBranch is the same rule the
// server uses today, but nothing forces a future bifrost to keep deriving tags
// client-derivably at all.
func TestPreviewUpDropsThePrePostSnapshotOnATagMismatch(t *testing.T) {
	// A perfectly good existing preview under the guessed tag, with images to
	// compare against...
	existing := `{"tag":"feat-x","branch":"feat/x","phase":"ready","health":"healthy",
	  "urls":{},"builtImages":{"footstrike-api":{"commit":"aaaa1111","shortSha":"aaaa111"}}}`
	// ...and a server that names a different one.
	f := newUp(t, &upScript{
		pre:  ok(existing),
		post: canned{http.StatusAccepted, `{"tag":"feat-x-2","phase":"creating"}`},
		polls: []canned{ok(`{"tag":"feat-x-2","branch":"feat/x","phase":"ready","health":"healthy","urls":{},
		  "builtImages":{"footstrike-api":{"commit":"aaaa1111","shortSha":"aaaa111"}}}`)},
	})
	stdout, stderr, code := execUp(f, pipedEnv(newClock()), "feat/x")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.HasPrefix(stdout, "Creating preview feat-x-2 from feat/x...") {
		t.Errorf("stdout = %q, want it to open with Creating (the snapshot was another preview's)", stdout)
	}
	if strings.Contains(stdout, "nothing rebuilt") || strings.Contains(stdout, "rebuilt:") {
		t.Errorf("stdout = %q, want no rebuild line: the before-images belonged to a different tag", stdout)
	}
}

// ---- the verb -----------------------------------------------------------

// TestPreviewUpVerbIsHonest. `up` said "Creating" for every run until the
// pre-POST lookup existed, including the re-runs that are the documented
// recovery path and what --auto-update does on a timer.
func TestPreviewUpVerbIsHonest(t *testing.T) {
	tests := []struct {
		name string
		pre  canned
		want string
	}{
		{
			// A 404 means nothing is there yet: this run creates it.
			name: "no preview yet",
			pre:  notFound,
			want: "Creating preview feat-x from feat/x...",
		},
		{
			name: "a preview already there",
			pre:  ok(`{"tag":"feat-x","branch":"feat/x","phase":"ready","health":"healthy","urls":{}}`),
			want: "Updating preview feat-x from feat/x...",
		},
		{
			// No branch on the record is "can't tell" for the collision check,
			// but the preview is still there, so this is still an update.
			name: "an older preview with no branch recorded",
			pre:  ok(`{"tag":"feat-x","phase":"ready","health":"healthy","urls":{}}`),
			want: "Updating preview feat-x from feat/x...",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newUp(t, &upScript{pre: tc.pre})
			stdout, stderr, code := execUp(f, pipedEnv(newClock()), "feat/x", "--no-wait")
			if code != 0 {
				t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
			}
			if !strings.HasPrefix(stdout, tc.want) {
				t.Errorf("stdout = %q, want it to open with %q", stdout, tc.want)
			}
		})
	}
}

// TestPreviewUpPrePostLookupFailureIsFatal: a 404 is absence, but a 500 is a
// failure, and it happens before anything has been asked for. Downgrading it
// to "no preview there, so this is a create" would report a create the server
// never heard about.
func TestPreviewUpPrePostLookupFailureIsFatal(t *testing.T) {
	f := newUp(t, &upScript{
		pre:  canned{http.StatusInternalServerError, `{"error":"kube unreachable"}`},
		post: canned{http.StatusAccepted, `{"tag":"feat-x","phase":"creating"}`},
	})
	stdout, stderr, code := execUp(f, pipedEnv(newClock()), "feat/x", "--no-wait")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "Preview API error 500: kube unreachable") {
		t.Errorf("stderr = %q, want the server's own message", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing: no preview was requested", stdout)
	}
	for _, req := range f.requests() {
		if req.Method == http.MethodPost {
			t.Error("a POST was sent after the pre-POST lookup failed")
		}
	}
}

// TestPreviewUpCreate404IsFatal. The tolerant reading of a 404 does not extend
// to the create endpoint: it cannot answer "no such preview", since creating
// one is the point, so a 404 there is the route missing.
func TestPreviewUpCreate404IsFatal(t *testing.T) {
	f := newUp(t, &upScript{post: canned{http.StatusNotFound, `{"error":"404 page not found"}`}})
	stdout, stderr, code := execUp(f, pipedEnv(newClock()), "feat/x")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "Preview API error 404: 404 page not found") {
		t.Errorf("stderr = %q, want the 404 reported as an error", stderr)
	}
	if strings.Contains(stdout, "Creating") {
		t.Errorf("stdout = %q claims a create that was refused", stdout)
	}
}

// ---- the request body ---------------------------------------------------

// TestPreviewUpSendsWhatWasAskedFor pins the POST body key by key. The keys
// are previewapi.CreateRequest's, shared with the handler that decodes them,
// and their presence or absence is the whole contract: an omitted "ttl" means
// a preview that never expires, and an omitted "autoUpdate" means one that
// does not follow its branch.
func TestPreviewUpSendsWhatWasAskedFor(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantBody   string
		wantAbsent []string
	}{
		{
			name:       "a bare up",
			args:       []string{"feat/x", "--no-wait"},
			wantBody:   `{"branch":"feat/x"}`,
			wantAbsent: []string{"ttl", "autoUpdate"},
		},
		{
			name:     "with a ttl",
			args:     []string{"feat/x", "--ttl", "8h", "--no-wait"},
			wantBody: `{"branch":"feat/x","ttl":"8h"}`,
		},
		{
			// Never sent as an explicit false: absence is what "off" looks
			// like, on the request as on the record.
			name:     "with auto-update",
			args:     []string{"feat/x", "--auto-update", "--no-wait"},
			wantBody: `{"branch":"feat/x","autoUpdate":true}`,
		},
		{
			name:     "with both",
			args:     []string{"feat/x", "--ttl", "90m", "--auto-update", "--no-wait"},
			wantBody: `{"branch":"feat/x","ttl":"90m","autoUpdate":true}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newUp(t, &upScript{})
			if _, stderr, code := execUp(f, pipedEnv(newClock()), tc.args...); code != 0 {
				t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
			}
			var body string
			for _, req := range f.requests() {
				if req.Method == http.MethodPost {
					body = strings.TrimSpace(req.Body)
				}
			}
			if body != tc.wantBody {
				t.Errorf("POST body = %s, want %s", body, tc.wantBody)
			}
			for _, key := range tc.wantAbsent {
				if strings.Contains(body, key) {
					t.Errorf("POST body = %s, want no %q key at all", body, key)
				}
			}
		})
	}
}

// ---- --no-wait ----------------------------------------------------------

// TestPreviewUpNoWaitTreatsA404AsNotCreatedYet. This is the bug that made a
// 404 a typed error rather than an exit. --no-wait's follow-up GET goes out
// with ZERO delay after a POST whose work is still queued, so the namespace
// almost never exists yet — and this used to abort with "Preview API error
// 404" on a create that was proceeding perfectly.
func TestPreviewUpNoWaitTreatsA404AsNotCreatedYet(t *testing.T) {
	f := newUp(t, &upScript{polls: []canned{notFound}})
	stdout, stderr, code := execUp(f, pipedEnv(newClock()), "feat/x", "--no-wait")

	if code != 0 {
		t.Errorf("exit = %d, want 0: a preview that does not exist yet is not a failure", code)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want nothing", stderr)
	}
	want := "Creating preview feat-x from feat/x...\nNot waiting. Check with: bif preview list\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	// The follow-up GET is the point of doing one at all: it is where a
	// dropped flag or a tag collision would be caught for a caller who is not
	// going to look again.
	gets := 0
	for _, req := range f.requests() {
		if req.Method == http.MethodGet {
			gets++
		}
	}
	if gets != 2 {
		t.Errorf("%d GETs, want 2 (the pre-POST lookup and the follow-up)", gets)
	}
}

// TestPreviewUpNoWaitSkipsTheChecksOnA404 is the other half of the same
// reading: with no record there is nothing to check, and running the checks
// against an empty one would report every flag as silently dropped on every
// --no-wait run.
func TestPreviewUpNoWaitSkipsTheChecksOnA404(t *testing.T) {
	f := newUp(t, &upScript{polls: []canned{notFound}})
	_, stderr, code := execUp(f, pipedEnv(newClock()), "feat/x", "--ttl", "8h", "--auto-update", "--no-wait")
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want no warnings: nothing has been created to inspect yet", stderr)
	}
}

// ---- the dropped-flag warnings ------------------------------------------

// TestPreviewUpWarnsWhenARequestedFlagWasDropped. Go's json.Decoder ignores
// unknown request fields and bifrost does not set DisallowUnknownFields, so a
// server predating --ttl or --auto-update accepts the field, answers success,
// and creates a preview without it. Nothing errors; the only place left to
// catch it is by checking the result against what was asked for.
//
// Exit 0 is as much of the contract as the message: the preview was created
// and is usable, and failing would throw a working preview away over a missing
// convenience.
func TestPreviewUpWarnsWhenARequestedFlagWasDropped(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		record   string
		wantWarn []string
		wantNone []string
	}{
		{
			name:     "a ttl that did not take",
			args:     []string{"feat/x", "--ttl", "8h", "--no-wait"},
			record:   `{"tag":"feat-x","branch":"feat/x","phase":"creating","urls":{}}`,
			wantWarn: []string{"Warning: --ttl 8h was requested for feat-x", "will NOT expire automatically", "bif preview down feat-x"},
			wantNone: []string{"--auto-update"},
		},
		{
			name:     "an auto-update that did not take",
			args:     []string{"feat/x", "--auto-update", "--no-wait"},
			record:   `{"tag":"feat-x","branch":"feat/x","phase":"creating","urls":{}}`,
			wantWarn: []string{"Warning: --auto-update was requested for feat-x", "will NOT follow the branch", "bif preview up feat/x --auto-update"},
			wantNone: []string{"--ttl"},
		},
		{
			name:     "both dropped",
			args:     []string{"feat/x", "--ttl", "8h", "--auto-update", "--no-wait"},
			record:   `{"tag":"feat-x","branch":"feat/x","phase":"creating","urls":{}}`,
			wantWarn: []string{"Warning: --ttl 8h", "Warning: --auto-update"},
		},
		{
			// Both honoured: silence.
			name:     "both honoured",
			args:     []string{"feat/x", "--ttl", "8h", "--auto-update", "--no-wait"},
			record:   `{"tag":"feat-x","branch":"feat/x","phase":"creating","urls":{},"expiresAt":"2026-07-31T20:00:00Z","autoUpdate":true}`,
			wantNone: []string{"Warning"},
		},
		{
			// Nothing was asked for, so nothing is missing. A preview with no
			// expiry is the default, not a fault.
			name:     "nothing requested",
			args:     []string{"feat/x", "--no-wait"},
			record:   `{"tag":"feat-x","branch":"feat/x","phase":"creating","urls":{}}`,
			wantNone: []string{"Warning"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newUp(t, &upScript{polls: []canned{ok(tc.record)}})
			stdout, stderr, code := execUp(f, pipedEnv(newClock()), tc.args...)
			if code != 0 {
				t.Errorf("exit = %d, want 0: the preview was created and is usable", code)
			}
			for _, want := range tc.wantWarn {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr = %q, want it to contain %q", stderr, want)
				}
			}
			for _, none := range tc.wantNone {
				if strings.Contains(stderr, none) {
					t.Errorf("stderr = %q, want no mention of %q", stderr, none)
				}
			}
			if !strings.Contains(stdout, "Not waiting") {
				t.Errorf("stdout = %q, want the run to have finished normally", stdout)
			}
		})
	}
}

// TestPreviewUpWarnsAfterWaiting: the same two checks run at the end of a full
// wait, not just on the --no-wait path, and they run against the finished
// record rather than the 202.
func TestPreviewUpWarnsAfterWaiting(t *testing.T) {
	f := newUp(t, &upScript{polls: []canned{ok(readyRecord)}})
	stdout, stderr, code := execUp(f, pipedEnv(newClock()), "feat/x", "--ttl", "8h", "--auto-update")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stderr, "Warning: --ttl 8h") || !strings.Contains(stderr, "Warning: --auto-update") {
		t.Errorf("stderr = %q, want both warnings", stderr)
	}
	if !strings.Contains(stdout, "footstrike-api: https://") {
		t.Errorf("stdout = %q, want the URLs: the preview is usable, which is why this exits 0", stdout)
	}
}

// ---- the branch-mismatch backstop ---------------------------------------

// TestPreviewUpFailsOnABranchMismatch. bifrost's tag derivation is many-to-one
// and its server-side collision refusal happens in a detached goroutine after
// the 202, so it never reaches this CLI: polling the tag afterward shows the
// OTHER branch's preview looking perfectly healthy. Without this check `up`
// reports success and prints someone else's URLs.
//
// The message must name both branches — the failure is only actionable if you
// can see what the tag is currently holding.
func TestPreviewUpFailsOnABranchMismatch(t *testing.T) {
	collided := `{"tag":"feat-x","branch":"feat/other","phase":"ready","health":"healthy",
	  "urls":{"identity":"https://identity-feat-x.preview.footstrike.run"}}`

	tests := []struct {
		name   string
		script *upScript
		args   []string
	}{
		{
			// Caught on the POST response, before "Creating preview..." is
			// even printed, so a doomed request never looks like progress.
			name:   "on the create response",
			script: &upScript{post: canned{http.StatusAccepted, `{"tag":"feat-x","phase":"creating","branch":"feat/other"}`}},
			args:   []string{"feat/x"},
		},
		{
			name:   "on the --no-wait follow-up",
			script: &upScript{polls: []canned{ok(collided)}},
			args:   []string{"feat/x", "--no-wait"},
		},
		{
			// Checked on EVERY poll, not just the first: the record backing a
			// tag only exists once bifrost's detached goroutine has run, so
			// the collision may not be visible at the first poll.
			name: "mid-poll",
			script: &upScript{polls: []canned{
				ok(creatingRecord("resolving membership", "2026-07-31T12:00:00Z")),
				ok(collided),
			}},
			args: []string{"feat/x"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newUp(t, tc.script)
			stdout, stderr, code := execUp(f, pipedEnv(newClock()), tc.args...)
			if code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			for _, want := range []string{"feat/other", "feat/x", "bif preview down feat-x"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr = %q, want it to name %q", stderr, want)
				}
			}
			if strings.Contains(stdout, "identity-feat-x.preview") {
				t.Errorf("stdout printed another branch's URLs: %q", stdout)
			}
		})
	}
}

// TestPreviewUpDoesNotFailOnAnUnknownBranch: absent or empty is "can't tell",
// not a mismatch. An older preview predating the annotation, or one from a
// partial run, must not fail a re-run — a false alarm here is worse than the
// silent-success bug the check exists to catch.
func TestPreviewUpDoesNotFailOnAnUnknownBranch(t *testing.T) {
	for _, rec := range []string{
		`{"tag":"feat-x","phase":"ready","health":"healthy","urls":{}}`,
		`{"tag":"feat-x","branch":"","phase":"ready","health":"healthy","urls":{}}`,
	} {
		f := newUp(t, &upScript{polls: []canned{ok(rec)}})
		stdout, stderr, code := execUp(f, pipedEnv(newClock()), "feat/x")
		if code != 0 {
			t.Errorf("exit = %d, want 0 for %s (stderr %q)", code, rec, stderr)
		}
		if strings.Contains(stderr, "belongs to branch") {
			t.Errorf("stderr = %q, want no mismatch claim", stderr)
		}
		if !strings.Contains(stdout, "ready") {
			t.Errorf("stdout = %q, want the run to have completed", stdout)
		}
	}
}

// ---- progress rendering -------------------------------------------------

// pollSequence is the ordinary shape of a create: nothing there yet, then two
// named steps, then ready.
func pollSequence() []canned {
	return []canned{
		notFound,
		ok(creatingRecord("resolving membership", "2026-07-31T11:59:56Z")),
		ok(creatingRecord("building footstrike-api", "2026-07-31T12:00:05Z")),
		ok(readyRecord),
	}
}

// TestPreviewUpPipedProgressHasNoTerminalControlBytes is the CI-log contract.
// A carriage-returned spinner in a log file renders as one unreadable line of
// overwritten fragments, so the piped path emits ZERO '\r' and zero escape
// sequences — asserted on the bytes, not on the intent.
func TestPreviewUpPipedProgressHasNoTerminalControlBytes(t *testing.T) {
	f := newUp(t, &upScript{polls: pollSequence()})
	stdout, stderr, code := execUp(f, pipedEnv(newClock()), "feat/x")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
	}

	if strings.Contains(stdout, "\r") {
		t.Errorf("piped output contains a carriage return: %q", stdout)
	}
	if strings.Contains(stdout, "\x1b") {
		t.Errorf("piped output contains an escape sequence: %q", stdout)
	}
	for _, frame := range spinnerFrames {
		if strings.ContainsRune(stdout, frame) {
			t.Errorf("piped output contains the spinner glyph %q: %q", frame, stdout)
		}
	}

	// One plain line per step change, then the phase change to ready, then the
	// URLs in sorted order.
	want := strings.Join([]string{
		"Creating preview feat-x from feat/x...",
		"  resolving membership",
		"  building footstrike-api",
		"  ready",
		"  footstrike-api: https://footstrike-api-feat-x.preview.footstrike.run",
		"  identity: https://identity-feat-x.preview.footstrike.run",
		"",
	}, "\n")
	if stdout != want {
		t.Errorf("stdout:\n%q\nwant:\n%q", stdout, want)
	}
}

// TestPreviewUpTTYProgressRedrawsOneLine drives the branch a Go test's stdout
// can never reach on its own. Each poll redraws in place with '\r'; a completed
// step is finalised with a ✓ and its elapsed time and left in the scrollback;
// and the live line is erased before the URLs print, so nothing lands beside a
// half-drawn spinner.
//
// The elapsed times are arithmetic on the injected clock: the loop sleeps 3s
// per poll from 12:00:00, and each step's elapsed time is measured from the
// server's own stepSince, recomputed fresh rather than cached.
func TestPreviewUpTTYProgressRedrawsOneLine(t *testing.T) {
	f := newUp(t, &upScript{polls: pollSequence()})
	stdout, stderr, code := execUp(f, ttyEnv(newClock()), "feat/x")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
	}

	// Poll 2 (12:00:06) is the first with a record: step 1 started 11:59:56,
	// so 10s. Poll 3 (12:00:09) finalises it at 13s and starts step 2, whose
	// stepSince is 12:00:05, so 4s. Poll 4 is ready with no step: the live
	// line is cleared and the phase change prints.
	const live1 = "  ⠋ resolving membership — 10s"
	const done1 = "  ✓ resolving membership — 13s"
	const live2 = "  ⠙ building footstrike-api — 4s"
	want := "\r" + live1 +
		"\r" + done1 + "\n" +
		"\r" + live2 +
		"\r" + strings.Repeat(" ", utf8.RuneCountInString(live2)) + "\r" +
		"  ready\n" +
		"  footstrike-api: https://footstrike-api-feat-x.preview.footstrike.run\n" +
		"  identity: https://identity-feat-x.preview.footstrike.run\n"

	got := strings.TrimPrefix(stdout, "Creating preview feat-x from feat/x...\n")
	if got != want {
		t.Errorf("tty output:\n%q\nwant:\n%q", got, want)
	}
	// The plain per-step lines belong to the piped path only; printing both
	// would double every step on a terminal.
	if strings.Contains(got, "\n  resolving membership") {
		t.Errorf("tty output also printed the plain step line: %q", got)
	}
}

// TestProgressPadsOverALongerPreviousLine: a redraw shorter than what is on
// screen must blank the tail, and the width is counted in RUNES on BOTH sides
// of the subtraction. Every non-ASCII glyph on this line — the braille spinner,
// the ✓, the em dash — is three bytes and one column, so a byte count would be
// wrong by two per glyph and leave a drifting trail of spaces behind the
// spinner. The two cases are separate because they catch different halves: a
// non-ASCII line being measured, and a non-ASCII line being drawn over one.
func TestProgressPadsOverALongerPreviousLine(t *testing.T) {
	t.Run("a non-ASCII line is measured in runes", func(t *testing.T) {
		var out bytes.Buffer
		p := &progress{w: &out, tty: true}
		p.write("⠋⠋⠋⠋⠋") // five runes, fifteen bytes
		out.Reset()
		p.write("x")

		want := "\rx" + strings.Repeat(" ", 4)
		if out.String() != want {
			t.Errorf("redraw = %q, want %q (measuring the first line in bytes would pad with 14)", out.String(), want)
		}
	})

	t.Run("a non-ASCII redraw is measured in runes", func(t *testing.T) {
		var out bytes.Buffer
		p := &progress{w: &out, tty: true}
		p.write("0123456789") // ten runes, ten bytes
		out.Reset()
		p.write("⠋⠋") // two runes, six bytes

		want := "\r⠋⠋" + strings.Repeat(" ", 8)
		if out.String() != want {
			t.Errorf("redraw = %q, want %q (measuring the redraw in bytes would pad with 4, leaving two characters behind)", out.String(), want)
		}
	})
}

// TestProgressWritesNothingOffATerminal: the no-ops are what make the
// zero-control-bytes property structural rather than a matter of every call
// site remembering to check.
func TestProgressWritesNothingOffATerminal(t *testing.T) {
	var out bytes.Buffer
	p := &progress{w: &out, tty: false}
	p.write("  ⠋ building — 3s")
	p.finish("  ✓ building — 4s")
	p.clear()
	if out.String() != "" {
		t.Errorf("piped progress wrote %q, want nothing", out.String())
	}
}

// TestPreviewUpFallsBackToPhaseChangesWithoutSteps. An older bifrost reports
// no step at all, and so does a phase that legitimately carries none. Both
// rendering modes degrade to printing only when the phase changes, rather than
// going silent for the whole create or narrating a step with no name.
func TestPreviewUpFallsBackToPhaseChangesWithoutSteps(t *testing.T) {
	stepless := `{"tag":"feat-x","branch":"feat/x","phase":"creating","health":"unknown","urls":{}}`
	for _, tc := range []struct {
		name string
		env  func(*fakeClock) previewEnv
	}{
		{"piped", pipedEnv},
		{"tty", ttyEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUp(t, &upScript{polls: []canned{ok(stepless), ok(stepless), ok(readyRecord)}})
			stdout, stderr, code := execUp(f, tc.env(newClock()), "feat/x")
			if code != 0 {
				t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
			}
			// "creating" is the phase the 202 already reported, so the first
			// two polls change nothing and print nothing.
			if n := strings.Count(stdout, "  creating"); n != 0 {
				t.Errorf("printed %q %d times, want 0 (the 202 already said creating):\n%q", "  creating", n, stdout)
			}
			if n := strings.Count(stdout, "  ready\n"); n != 1 {
				t.Errorf("printed the ready phase %d times, want once:\n%q", n, stdout)
			}
			if strings.Contains(stdout, "—") {
				t.Errorf("stdout = %q, want no step narration: the server reported no steps", stdout)
			}
		})
	}
}

// TestPreviewUpDegradesOnAnUnusableStepSince. ib.py died here: a timezone-naive
// stepSince raised an uncaught TypeError out of the elapsed-time arithmetic and
// killed the poll loop mid-run.
//
// In Go the same hazard sits one layer earlier — StepSince is a time.Time, so
// without previewapi.Record's tolerant UnmarshalJSON one bad character in that
// field fails the WHOLE record, losing the phase, the error and the URLs over a
// cosmetic elapsed time. Both bad shapes here must degrade to the locally
// tracked start and the run must finish normally.
func TestPreviewUpDegradesOnAnUnusableStepSince(t *testing.T) {
	naive := `{"tag":"feat-x","branch":"feat/x","phase":"creating","health":"unknown","urls":{},
	  "step":"building footstrike-api","stepSince":"2026-07-31T12:00:05"}`
	garbage := `{"tag":"feat-x","branch":"feat/x","phase":"creating","health":"unknown","urls":{},
	  "step":"building footstrike-api","stepSince":"not-a-timestamp"}`

	f := newUp(t, &upScript{polls: []canned{ok(naive), ok(garbage), ok(readyRecord)}})
	stdout, stderr, code := execUp(f, ttyEnv(newClock()), "feat/x")

	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q): an unusable timestamp must degrade, not end the run", code, stderr)
	}
	if strings.Contains(stderr, "decoding") {
		t.Errorf("stderr = %q: one bad field failed the whole record", stderr)
	}
	// The step starts at the first poll (12:00:03) and the fallback measures
	// from there: 0s on that poll, 3s on the next.
	if !strings.Contains(stdout, "building footstrike-api — 0s") {
		t.Errorf("stdout = %q, want the first poll's elapsed time from the local fallback", stdout)
	}
	if !strings.Contains(stdout, "building footstrike-api — 3s") {
		t.Errorf("stdout = %q, want the second poll measured from the same fallback start", stdout)
	}
	if !strings.Contains(stdout, "footstrike-api: https://") {
		t.Errorf("stdout = %q, want the run to have reached ready", stdout)
	}
}

// ---- terminal phases ----------------------------------------------------

// TestPreviewUpReportsAFailure. Three shapes, because bifrost may or may not
// have recorded a step and an error, and each says as much as is actually
// known. All exit 1 and all point at the Previews tab, which has the detail
// this CLI does not.
func TestPreviewUpReportsAFailure(t *testing.T) {
	tests := []struct {
		name   string
		record string
		want   string
		// wantPhaseLine is whether a bare "  failed" precedes the message on
		// stdout. It follows from WHERE the phase line is printed, and the
		// asymmetry is the oracle's: the failed branch itself never prints one,
		// because the richer message below is strictly more useful and a
		// generic phase line ahead of it would be noise. But a failed record
		// with no step took the step-less branch first, and that branch prints
		// on every phase change — which is the only phase narration such a
		// record gets at all.
		wantPhaseLine bool
	}{
		{
			name: "with a step and an error",
			record: `{"tag":"feat-x","branch":"feat/x","phase":"failed","urls":{},
			  "step":"building footstrike-api","error":"cloud build 7f3 failed"}`,
			want: "Preview feat-x failed while building footstrike-api: cloud build 7f3 failed",
		},
		{
			name:          "with an error only",
			record:        `{"tag":"feat-x","branch":"feat/x","phase":"failed","urls":{},"error":"namespace quota exceeded"}`,
			want:          "Preview feat-x failed: namespace quota exceeded",
			wantPhaseLine: true,
		},
		{
			name:          "with neither",
			record:        `{"tag":"feat-x","branch":"feat/x","phase":"failed","urls":{}}`,
			want:          "Preview feat-x failed (phase: failed).",
			wantPhaseLine: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newUp(t, &upScript{polls: []canned{ok(tc.record)}})
			stdout, stderr, code := execUp(f, pipedEnv(newClock()), "feat/x")
			if code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, want %q", stderr, tc.want)
			}
			if !strings.Contains(stderr, "Check the Previews tab in bifrost for details.") {
				t.Errorf("stderr = %q, want the pointer at the detail this CLI lacks", stderr)
			}
			if got := strings.Contains(stdout, "  failed"); got != tc.wantPhaseLine {
				t.Errorf("stdout = %q; bare phase line = %v, want %v", stdout, got, tc.wantPhaseLine)
			}
		})
	}
}

// TestPreviewUpRefusesToCreateOverATeardown. A create landing on a namespace
// still terminating cannot proceed, and guessing at what is colliding with it
// is what this refuses to do: it says to retry once the teardown finishes.
func TestPreviewUpRefusesToCreateOverATeardown(t *testing.T) {
	f := newUp(t, &upScript{polls: []canned{ok(`{"tag":"feat-x","branch":"feat/x","phase":"terminating","urls":{}}`)}})
	_, stderr, code := execUp(f, pipedEnv(newClock()), "feat/x")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	for _, want := range []string{"being torn down", "Retry once the teardown finishes", "bif preview list"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr, want)
		}
	}
}

// TestPreviewUpTimesOut: the wait is bounded at 30 minutes, matching
// internal/web's asyncOrchestrationTimeout — the CLI stops waiting where
// bifrost stops working. The clock is injected, so this asserts a half-hour in
// microseconds.
func TestPreviewUpTimesOut(t *testing.T) {
	stuck := ok(creatingRecord("building footstrike-api", "2026-07-31T12:00:00Z"))
	f := newUp(t, &upScript{polls: []canned{stuck}})
	c := newClock()
	c.jump = 20 * time.Minute // two polls and the deadline has passed

	_, stderr, code := execUp(f, pipedEnv(c), "feat/x")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "Timed out waiting for feat-x; check `bif preview list`.") {
		t.Errorf("stderr = %q, want the timeout message", stderr)
	}
	if got := c.now().Sub(clockStart); got > previewWaitTimeout+20*time.Minute {
		t.Errorf("polled for %v, want the loop to stop around %v", got, previewWaitTimeout)
	}
}

// TestPreviewUpNotesAnUnrecognizedPhaseOnce. A phase this CLI does not know is
// a newer bifrost, not a broken one, so polling continues — but saying so every
// three seconds for half an hour would bury everything else.
func TestPreviewUpNotesAnUnrecognizedPhaseOnce(t *testing.T) {
	odd := ok(`{"tag":"feat-x","branch":"feat/x","phase":"reconciling","health":"unknown","urls":{}}`)
	f := newUp(t, &upScript{polls: []canned{odd, odd, odd, ok(readyRecord)}})
	stdout, stderr, code := execUp(f, pipedEnv(newClock()), "feat/x")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
	}
	if n := strings.Count(stdout, "unrecognized phase"); n != 1 {
		t.Errorf("noted the unrecognized phase %d times, want once:\n%q", n, stdout)
	}
	if !strings.Contains(stdout, `(unrecognized phase "reconciling"; continuing to poll)`) {
		t.Errorf("stdout = %q, want the phase named", stdout)
	}
	if !strings.Contains(stdout, "footstrike-api: https://") {
		t.Errorf("stdout = %q, want polling to have continued to ready", stdout)
	}
}

// TestPreviewUpPollFailureIsFatal: a 404 mid-poll is "not created yet", but a
// 500 is a failure and ends the wait rather than being polled through until
// the deadline.
func TestPreviewUpPollFailureIsFatal(t *testing.T) {
	f := newUp(t, &upScript{polls: []canned{{http.StatusInternalServerError, `{"error":"kube unreachable"}`}}})
	_, stderr, code := execUp(f, pipedEnv(newClock()), "feat/x")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "Preview API error 500: kube unreachable") {
		t.Errorf("stderr = %q, want the server's own message", stderr)
	}
}

// ---- what was rebuilt ---------------------------------------------------

// TestRebuildSummary. The line compares builtImages before and after, on
// `commit` — the thing that identifies a build — rather than on `shortSha`,
// which is derived from it. Absence on either side, or a member present on only
// one side, is "can't tell" and says NOTHING: a confident "nothing rebuilt" in
// front of a deploy that changed everything is worse than silence.
func TestRebuildSummary(t *testing.T) {
	rec := func(images map[string]previewapi.BuiltImage) *previewapi.Record {
		return &previewapi.Record{BuiltImages: images}
	}
	built := func(commit, short string) previewapi.BuiltImage {
		return previewapi.BuiltImage{Commit: commit, ShortSHA: short}
	}

	tests := []struct {
		name          string
		before, after *previewapi.Record
		want          string
	}{
		{
			name:   "every image reused",
			before: rec(map[string]previewapi.BuiltImage{"footstrike-api": built("aaaa1111", "aaaa111")}),
			after:  rec(map[string]previewapi.BuiltImage{"footstrike-api": built("aaaa1111", "aaaa111")}),
			want:   "  nothing rebuilt — all images reused",
		},
		{
			name:   "one member rebuilt",
			before: rec(map[string]previewapi.BuiltImage{"footstrike-api": built("aaaa1111", "aaaa111"), "identity": built("cccc3333", "cccc333")}),
			after:  rec(map[string]previewapi.BuiltImage{"footstrike-api": built("bbbb2222", "bbbb222"), "identity": built("cccc3333", "cccc333")}),
			want:   "  rebuilt: footstrike-api",
		},
		{
			// Sorted, so the line is stable across runs rather than following
			// Go's randomized map order.
			name:   "several members, named in order",
			before: rec(map[string]previewapi.BuiltImage{"identity": built("1", "1"), "comms": built("2", "2"), "footstrike-api": built("3", "3")}),
			after:  rec(map[string]previewapi.BuiltImage{"identity": built("9", "9"), "comms": built("8", "8"), "footstrike-api": built("7", "7")}),
			want:   "  rebuilt: comms, footstrike-api, identity",
		},
		{
			// commit is what identifies a build; shortSha is derived from it.
			// A differing shortSha with an identical commit is not a rebuild.
			name:   "the same commit with a different shortSha",
			before: rec(map[string]previewapi.BuiltImage{"footstrike-api": built("aaaa1111", "aaaa111")}),
			after:  rec(map[string]previewapi.BuiltImage{"footstrike-api": built("aaaa1111", "different")}),
			want:   "  nothing rebuilt — all images reused",
		},
		{
			name:   "a different commit with the same shortSha",
			before: rec(map[string]previewapi.BuiltImage{"footstrike-api": built("aaaa1111", "same")}),
			after:  rec(map[string]previewapi.BuiltImage{"footstrike-api": built("bbbb2222", "same")}),
			want:   "  rebuilt: footstrike-api",
		},
		{
			// A brand-new preview: everything was built, which is what
			// creating one means.
			name:   "no before at all",
			before: nil,
			after:  rec(map[string]previewapi.BuiltImage{"footstrike-api": built("aaaa1111", "aaaa111")}),
			want:   "",
		},
		{
			name:   "no builtImages before",
			before: rec(nil),
			after:  rec(map[string]previewapi.BuiltImage{"footstrike-api": built("aaaa1111", "aaaa111")}),
			want:   "",
		},
		{
			name:   "no builtImages after",
			before: rec(map[string]previewapi.BuiltImage{"footstrike-api": built("aaaa1111", "aaaa111")}),
			after:  rec(nil),
			want:   "",
		},
		{
			// bifrost drops malformed entries individually, so the map can be
			// present but incomplete. A member on one side only is "can't
			// tell" for that member, never "changed".
			name:   "a member on only one side is ignored",
			before: rec(map[string]previewapi.BuiltImage{"footstrike-api": built("aaaa1111", "aaaa111")}),
			after:  rec(map[string]previewapi.BuiltImage{"footstrike-api": built("aaaa1111", "aaaa111"), "identity": built("cccc3333", "cccc333")}),
			want:   "  nothing rebuilt — all images reused",
		},
		{
			name:   "nothing in common at all",
			before: rec(map[string]previewapi.BuiltImage{"comms": built("aaaa1111", "aaaa111")}),
			after:  rec(map[string]previewapi.BuiltImage{"identity": built("cccc3333", "cccc333")}),
			want:   "",
		},
		{
			// An entry with no commit is skipped rather than compared as
			// empty-equals-empty, which would read as "reused".
			name:   "an entry with no commit",
			before: rec(map[string]previewapi.BuiltImage{"footstrike-api": built("", "aaaa111")}),
			after:  rec(map[string]previewapi.BuiltImage{"footstrike-api": built("", "bbbb222")}),
			want:   "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := rebuildSummary(tc.before, tc.after); got != tc.want {
				t.Errorf("rebuildSummary = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPreviewUpPrintsTheRebuildLine drives the same comparison end to end,
// from the wire text on both sides. A re-run against an unchanged branch reuses
// every image and finishes in seconds, which otherwise looks exactly like a
// full rebuild.
func TestPreviewUpPrintsTheRebuildLine(t *testing.T) {
	withImages := func(phase, apiCommit string) string {
		return `{"tag":"feat-x","branch":"feat/x","phase":"` + phase + `","health":"healthy","urls":{},
		  "builtImages":{"footstrike-api":{"commit":"` + apiCommit + `","shortSha":"` + apiCommit[:7] + `"},
		                 "identity":{"commit":"cccc3333cccc","shortSha":"cccc333"}}}`
	}

	t.Run("nothing rebuilt", func(t *testing.T) {
		f := newUp(t, &upScript{
			pre:   ok(withImages("ready", "aaaa1111aaaa")),
			polls: []canned{ok(withImages("ready", "aaaa1111aaaa"))},
		})
		stdout, _, code := execUp(f, pipedEnv(newClock()), "feat/x")
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if !strings.Contains(stdout, "Updating preview feat-x") {
			t.Errorf("stdout = %q, want the honest verb", stdout)
		}
		if !strings.Contains(stdout, "  nothing rebuilt — all images reused\n") {
			t.Errorf("stdout = %q, want the nothing-rebuilt line", stdout)
		}
	})

	t.Run("one member rebuilt", func(t *testing.T) {
		f := newUp(t, &upScript{
			pre:   ok(withImages("ready", "aaaa1111aaaa")),
			polls: []canned{ok(withImages("ready", "bbbb2222bbbb"))},
		})
		stdout, _, code := execUp(f, pipedEnv(newClock()), "feat/x")
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if !strings.Contains(stdout, "  rebuilt: footstrike-api\n") {
			t.Errorf("stdout = %q, want the rebuilt line naming only what changed", stdout)
		}
	})

	t.Run("a brand-new preview says nothing", func(t *testing.T) {
		f := newUp(t, &upScript{polls: []canned{ok(withImages("ready", "aaaa1111aaaa"))}})
		stdout, _, code := execUp(f, pipedEnv(newClock()), "feat/x")
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if strings.Contains(stdout, "rebuilt") {
			t.Errorf("stdout = %q, want no rebuild line: everything was built, that is what creating means", stdout)
		}
	})
}

// ---- the small pure pieces ----------------------------------------------

func TestFormatElapsed(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{47 * time.Second, "47s"},
		{59500 * time.Millisecond, "59s"}, // truncated, never rounded up past the minute
		{time.Minute, "1m00s"},
		{123 * time.Second, "2m03s"},
		{2 * time.Hour, "120m00s"},
		// A server clock ahead of this one. "-3s of building" is a worse answer
		// to "how long has this been running" than "0s".
		{-3 * time.Second, "0s"},
	}
	for _, tc := range tests {
		if got := formatElapsed(tc.d); got != tc.want {
			t.Errorf("formatElapsed(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestStepElapsed: the server's stepSince wins when it is usable, because it
// is recomputed fresh against wall-clock time on every call and so does not go
// stale between three-second polls. A zero one — absent, or rejected by
// previewapi's decoder — falls back to the locally tracked start.
func TestStepElapsed(t *testing.T) {
	now := clockStart.Add(time.Minute)
	since := clockStart.Add(30 * time.Second)
	fallback := clockStart

	if got := stepElapsed(since, fallback, now); got != 30*time.Second {
		t.Errorf("with stepSince = %v, want 30s", got)
	}
	if got := stepElapsed(time.Time{}, fallback, now); got != time.Minute {
		t.Errorf("without stepSince = %v, want the fallback's 1m", got)
	}
}

func TestSpinnerFrame(t *testing.T) {
	first := spinnerFrame(0)
	if first != "⠋" {
		t.Errorf("spinnerFrame(0) = %q", first)
	}
	if spinnerFrame(len(spinnerFrames)) != first {
		t.Errorf("the spinner does not cycle: frame %d = %q, want %q", len(spinnerFrames), spinnerFrame(len(spinnerFrames)), first)
	}
	seen := map[string]bool{}
	for i := range spinnerFrames {
		seen[spinnerFrame(i)] = true
	}
	if len(seen) != len(spinnerFrames) {
		t.Errorf("%d distinct frames across a full cycle, want %d", len(seen), len(spinnerFrames))
	}
}

// TestIsTerminalIsFalseForEverythingButATerminal. The one thing a Go test can
// assert about the real detection: a buffer and a redirected file are not
// terminals, so the CI path is what a test, a pipe and a redirect all get.
func TestIsTerminalIsFalseForEverythingButATerminal(t *testing.T) {
	if isTerminal(&bytes.Buffer{}) {
		t.Error("a bytes.Buffer was reported as a terminal")
	}
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	defer func() { _ = f.Close() }()
	if isTerminal(f) {
		t.Error("a redirect to a file was reported as a terminal; CI logs would get the spinner")
	}
	// os.DevNull is a CHARACTER DEVICE, which is why the os.ModeCharDevice
	// heuristic is not what isTerminal uses: a CI job redirecting there would
	// otherwise get escape sequences.
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer func() { _ = devnull.Close() }()
	if isTerminal(devnull) {
		t.Errorf("%s was reported as a terminal", os.DevNull)
	}
}

// TestDefaultPreviewEnvIsWired: the real command builds a usable env — a
// clock, a sleep, and the terminal decision taken against the stream progress
// is drawn on rather than against os.Stdout, so `bif preview up > log` takes
// the plain path even with a terminal on stderr.
func TestDefaultPreviewEnvIsWired(t *testing.T) {
	env := defaultPreviewEnv(&bytes.Buffer{})
	if env.now == nil || env.sleep == nil {
		t.Fatal("defaultPreviewEnv left the clock nil; the poll loop would panic")
	}
	if env.isTTY {
		t.Error("a bytes.Buffer was reported as a terminal")
	}
	if got := env.now(); got.IsZero() {
		t.Error("defaultPreviewEnv's clock returns the zero time")
	}
}

// ---- end to end ---------------------------------------------------------

// TestPreviewUpEndToEndThroughRun drives `bif preview up` through dispatch, so
// argument parsing, the client and the rendering are joined by the same code
// the operator runs. --no-wait keeps it off the poll loop's real clock, which
// dispatch supplies rather than a test.
func TestPreviewUpEndToEndThroughRun(t *testing.T) {
	f := newUp(t, &upScript{polls: []canned{ok(`{"tag":"feat-x","branch":"feat/x","phase":"creating","urls":{}}`)}})
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"preview", "up", "feat/x", "--no-wait"},
		strings.NewReader(""), &stdout, &stderr, noCluster, unreachableBuilds, f.dial())

	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr.String())
	}
	want := "Creating preview feat-x from feat/x...\nNot waiting. Check with: bif preview list\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	got := f.requests()
	if len(got) != 3 {
		t.Fatalf("requests = %+v, want three (pre-POST lookup, create, follow-up)", got)
	}
	if got[0].Method != http.MethodGet || got[0].Path != "/api/previews/feat-x" {
		t.Errorf("first request = %+v, want the pre-POST lookup", got[0])
	}
	if got[1].Method != http.MethodPost || got[1].Path != "/api/previews" {
		t.Errorf("second request = %+v, want the create", got[1])
	}
	if got[2].Method != http.MethodGet || got[2].Path != "/api/previews/feat-x" {
		t.Errorf("third request = %+v, want the follow-up lookup", got[2])
	}
	// Every request carries the WAF-mandated User-Agent, the poll loop's
	// included — a header this whole command would fail on in prod while every
	// httptest-backed test kept passing.
	for _, req := range got {
		if req.UserAgent != previewclient.UserAgent {
			t.Errorf("%s %s User-Agent = %q", req.Method, req.Path, req.UserAgent)
		}
	}
	// One gcloud shell-out for the whole command, not one per request.
	if n := f.tokens(); n != 1 {
		t.Errorf("token fetched %d times across %d requests, want 1", n, len(got))
	}
}
