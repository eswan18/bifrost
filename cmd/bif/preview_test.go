package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eswan18/bifrost/internal/previewapi"
	"github.com/eswan18/bifrost/internal/previewclient"
)

// ---- the fake bifrost ---------------------------------------------------

// capturedRequest is what the CLI actually put on the wire. The headers are
// recorded because two of them are load-bearing and invisible from above the
// client: without User-Agent, Cloudflare's WAF rejects the request before
// bifrost ever sees it, and without Authorization every call is a 401.
type capturedRequest struct {
	Method        string
	Path          string
	UserAgent     string
	Authorization string
	Accept        string
	// Body is the raw request bytes. `preview up` is the only command that
	// sends one, and what is in it — whether "ttl" and "autoUpdate" are
	// present at all — is the difference between a preview that expires and
	// one that does not.
	Body string
}

// fakeBifrost is an httptest server standing in for the preview API, plus the
// requests it received and the number of times the CLI asked for a token.
// Nothing here shells out to gcloud — the token comes from the injected
// function, which is the whole reason previewclient.Client has one.
type fakeBifrost struct {
	srv *httptest.Server

	mu         sync.Mutex
	got        []capturedRequest
	tokenCalls int
}

func newFakeBifrost(t *testing.T, h http.HandlerFunc) *fakeBifrost {
	t.Helper()
	f := &fakeBifrost{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Put it back: the handler under test is entitled to read it too.
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.mu.Lock()
		f.got = append(f.got, capturedRequest{
			Method:        r.Method,
			Path:          r.URL.Path,
			UserAgent:     r.Header.Get("User-Agent"),
			Authorization: r.Header.Get("Authorization"),
			Accept:        r.Header.Get("Accept"),
			Body:          string(body),
		})
		f.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// client is the REAL previewclient.Client pointed at the fake. The preview
// tests deliberately do not stub previewAPI: the status-code mapping, the
// headers and "a declined teardown sends nothing" are all properties of the
// client and the wire, and a hand-written fake would assert none of them.
func (f *fakeBifrost) client() *previewclient.Client {
	return &previewclient.Client{
		BaseURL: f.srv.URL,
		HTTP:    f.srv.Client(),
		Token: func(context.Context) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.tokenCalls++
			return "test-token", nil
		},
	}
}

func (f *fakeBifrost) dial() func() previewAPI {
	c := f.client()
	return func() previewAPI { return c }
}

func (f *fakeBifrost) requests() []capturedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capturedRequest(nil), f.got...)
}

func (f *fakeBifrost) tokens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls
}

// jsonHandler answers every request with one status and body.
func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// execPreview runs a whole `bif preview ...` invocation against the fake.
func execPreview(f *fakeBifrost, stdin string, args ...string) (stdout, stderr string, code int) {
	var out, errb bytes.Buffer
	code = run(context.Background(), append([]string{"preview"}, args...), strings.NewReader(stdin), &out, &errb, noCluster, unreachableBuilds, unreachableDigests, f.dial())
	return out.String(), errb.String(), code
}

// ---- the table ----------------------------------------------------------

// listFixture is the JSON GET /api/previews returns, written out by hand
// rather than marshalled from previewapi.Record. That is the point: the struct
// is shared with the server now, so a marshal-then-unmarshal test would pass
// no matter what the fields were called. This is literal wire text with the
// server's key names in it, so renaming "expiresAt" or "autoUpdate" on the
// server breaks this test even though it still compiles everywhere.
//
// The four records are the four shapes that render differently: a healthy
// preview with a TTL and auto-update, a synthesized busy record with no
// namespace behind it, a preview mid-create that the orchestrator also holds,
// and one whose TTL has passed but which the hourly sweep has not reaped.
const listFixture = `{"previews":[
  {"tag":"feat-billing","branch":"feat/billing","apps":["footstrike-api","identity"],
   "phase":"ready","health":"healthy","createdAt":"2026-07-30T09:00:00Z",
   "urls":{"footstrike-api":"https://footstrike-api-feat-billing.preview.footstrike.run"},
   "expiresAt":"2026-07-31T12:00:00Z","autoUpdate":true},
  {"tag":"feat-brand-new","branch":"","apps":[],"phase":"busy","health":"unknown",
   "createdAt":"0001-01-01T00:00:00Z","urls":{},"busy":true},
  {"tag":"feat-long-running","branch":"feat/long-running","apps":["forecasting"],
   "phase":"creating","health":"degraded","createdAt":"2026-07-31T03:50:00Z",
   "urls":{"forecasting":"https://forecasting-feat-long-running.preview.footstrike.run"},
   "busy":true},
  {"tag":"feat-stale","branch":"feat/stale","apps":["comms"],"phase":"ready",
   "health":"unhealthy","createdAt":"2026-07-20T09:00:00Z","urls":{},
   "expiresAt":"2026-07-31T03:00:00Z"}
]}`

// wantTable is joined from quoted lines rather than written as one raw string
// because the trailing whitespace is part of the assertion: APPS is the last
// column and is unpadded, so a preview with no apps ends its line at the space
// separating it from AUTO. ib.py's output does exactly that. The explicit
// `+ emptyApps` puts that whitespace somewhere an editor cannot silently trim.
var wantTable = strings.Join([]string{
	"TAG                      BRANCH                   PHASE      HEALTH       EXPIRES    AUTO  APPS",
	"feat-billing             feat/billing             ready      healthy      8h0m       ✓     footstrike-api,identity",
	// "busy*", not a bare "busy": bifrost sends phase "busy" AND busy true for
	// a synthesized record, so the mark applies. See TestPreviewPhaseCell.
	"feat-brand-new           -                        busy*      unknown      -" + emptyApps,
	"feat-long-running        feat/long-running        creating*  degraded     -          -     forecasting",
	"feat-stale               feat/stale               ready      unhealthy    expired    -     comms",
	"",
}, "\n")

// emptyApps is how a row for a preview with no apps ends: the rest of the
// EXPIRES field, the AUTO cell, and the separator before an empty APPS.
const emptyApps = "          -     "

// TestPreviewListRendersTheTable pins the table ib.py prints, character for
// character, from the wire bytes bifrost sends.
func TestPreviewListRendersTheTable(t *testing.T) {
	f := newFakeBifrost(t, jsonHandler(http.StatusOK, listFixture))
	client := f.client()
	records, err := client.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// Fixed clock: feat-billing expires in exactly 8h, feat-stale an hour ago.
	now := time.Date(2026, 7, 31, 4, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	renderPreviewList(&out, records, now)

	if out.String() != wantTable {
		t.Errorf("table mismatch\n got:\n%s\nwant:\n%s", out.String(), wantTable)
	}
}

// TestPreviewListRendersASparseRecordCleanly: a tag the orchestrator holds
// with no namespace behind it has no branch, no apps, no urls, no health and
// no phase to report. Every one of those is a Go zero value, and the row must
// still read as a row — never "<nil>", never "0001-01-01", never "None".
func TestPreviewListRendersASparseRecordCleanly(t *testing.T) {
	// Only "tag" and "busy". Everything else is absent from the JSON, which is
	// how omitempty/omitzero fields arrive.
	f := newFakeBifrost(t, jsonHandler(http.StatusOK, `{"previews":[{"tag":"feat-phantom","busy":true}]}`))
	records, err := f.client().List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var out bytes.Buffer
	renderPreviewList(&out, records, time.Now())
	line := strings.Split(out.String(), "\n")[1]

	want := "feat-phantom             -                        busy       -            -" + emptyApps
	if line != want {
		t.Errorf("sparse row  = %q\nwant        = %q", line, want)
	}
	for _, leak := range []string{"<nil>", "0001-01-01", "None", "false", "%!"} {
		if strings.Contains(out.String(), leak) {
			t.Errorf("rendered table leaks %q:\n%s", leak, out.String())
		}
	}
}

// TestPreviewListEmpty: ib.py prints a sentence, not an empty table.
func TestPreviewListEmpty(t *testing.T) {
	f := newFakeBifrost(t, jsonHandler(http.StatusOK, `{"previews":[]}`))
	stdout, stderr, code := execPreview(f, "", "list")
	if stdout != "No preview environments.\n" || code != 0 {
		t.Errorf("stdout = %q, exit = %d; want the empty-fleet sentence and 0", stdout, code)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

// TestPreviewListEndToEnd drives `bif preview list` all the way through
// dispatch, so the wire decode and the render are joined by the same code the
// operator runs. The EXPIRES values here are clock-independent on purpose (a
// past expiry is "expired" whenever you run it, an absent one is "-"), since
// this path uses the real time.Now.
func TestPreviewListEndToEnd(t *testing.T) {
	body := `{"previews":[
	  {"tag":"feat-gone","branch":"feat/gone","apps":["identity"],"phase":"ready","health":"healthy",
	   "createdAt":"2026-01-01T00:00:00Z","urls":{},"expiresAt":"2026-01-02T00:00:00Z","autoUpdate":true},
	  {"tag":"feat-plain","branch":"feat/plain","apps":["comms"],"phase":"ready","health":"healthy",
	   "createdAt":"2026-01-01T00:00:00Z","urls":{}},
	  {"tag":"feat-phantom","branch":"","apps":[],"phase":"busy","health":"unknown",
	   "createdAt":"0001-01-01T00:00:00Z","urls":{},"busy":true}
	]}`
	want := strings.Join([]string{
		"TAG                      BRANCH                   PHASE      HEALTH       EXPIRES    AUTO  APPS",
		"feat-gone                feat/gone                ready      healthy      expired    ✓     identity",
		"feat-plain               feat/plain               ready      healthy      -          -     comms",
		"feat-phantom             -                        busy*      unknown      -" + emptyApps,
		"",
	}, "\n")
	f := newFakeBifrost(t, jsonHandler(http.StatusOK, body))
	stdout, stderr, code := execPreview(f, "", "list")
	if code != 0 {
		t.Errorf("exit = %d, want 0 (stderr %q)", code, stderr)
	}
	if stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", stdout, want)
	}
	got := f.requests()
	if len(got) != 1 || got[0].Method != http.MethodGet || got[0].Path != "/api/previews" {
		t.Errorf("requests = %+v, want one GET /api/previews", got)
	}
}

// TestPreviewListRejectsLeftoverArguments. ib.py ignores them; this refuses,
// for the reason `--ttl=8h` taught it — an argument silently dropped is a
// request silently not honoured.
func TestPreviewListRejectsLeftoverArguments(t *testing.T) {
	f := newFakeBifrost(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("bif preview list sent a request despite bad arguments")
	})
	stdout, _, code := execPreview(f, "", "list", "feat-billing")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stdout, previewListUsage) {
		t.Errorf("stdout = %q, want %q", stdout, previewListUsage)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("%d requests sent; want none", n)
	}
}

// ---- the client ---------------------------------------------------------

// TestSendsUserAgentAndAuth: the custom User-Agent is not decoration.
// Cloudflare's WAF bans default client signatures outright (error 1010) before
// the request reaches bifrost, so a Go default agent breaks the CLI against
// prod while every httptest-backed test keeps passing — which is exactly why
// this asserts the header rather than trusting the behaviour above it.
func TestSendsUserAgentAndAuth(t *testing.T) {
	f := newFakeBifrost(t, jsonHandler(http.StatusOK, `{"previews":[]}`))
	if _, _, code := execPreview(f, "", "list"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if err := f.client().Delete(context.Background(), "feat-x"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got := f.requests()
	if len(got) != 2 {
		t.Fatalf("requests = %+v, want two", got)
	}
	for _, req := range got {
		if req.UserAgent != previewclient.UserAgent {
			t.Errorf("%s %s User-Agent = %q, want %q (Go's default is banned by Cloudflare's WAF)",
				req.Method, req.Path, req.UserAgent, previewclient.UserAgent)
		}
		if req.Authorization != "Bearer test-token" {
			t.Errorf("%s %s Authorization = %q, want the bearer token", req.Method, req.Path, req.Authorization)
		}
		if req.Accept != "application/json" {
			t.Errorf("%s %s Accept = %q", req.Method, req.Path, req.Accept)
		}
	}
}

// TestTokenIsFetchedOnce. ib.py shells out to gcloud on every single request,
// which is invisible for list and down but is a subprocess every 3 seconds for
// `up`'s poll loop. Memoizing cannot change an outcome and this pins that it
// happens.
func TestTokenIsFetchedOnce(t *testing.T) {
	f := newFakeBifrost(t, jsonHandler(http.StatusOK, `{"previews":[]}`))
	c := f.client()
	for range 3 {
		if _, err := c.List(context.Background()); err != nil {
			t.Fatalf("List: %v", err)
		}
	}
	if n := f.tokens(); n != 1 {
		t.Errorf("token fetched %d times across 3 requests, want 1", n)
	}
}

// TestTokenFailureIsReportedAndNothingIsSent: an expired gcloud login must
// fail with gcloud's own words, and must not produce an unauthenticated
// request.
func TestTokenFailureIsReportedAndNothingIsSent(t *testing.T) {
	f := newFakeBifrost(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a request was sent without a token")
	})
	c := f.client()
	c.Token = func(context.Context) (string, error) {
		return "", errors.New("ERROR: (gcloud.secrets.versions.access) You do not currently have an active account selected.")
	}
	dial := func() previewAPI { return c }

	var stdout, stderr bytes.Buffer
	code := previewCmd(context.Background(), []string{"list"}, strings.NewReader(""), &stdout, &stderr, dial)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "gcloud.secrets.versions.access") {
		t.Errorf("stderr = %q, want gcloud's own error", stderr.String())
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("%d requests sent without a token; want none", n)
	}
}

// TestAPIErrorMessages ports preview_api's status handling exactly. The
// wording is the contract — an operator reads these and nothing else — and
// each one names what to do next.
func TestAPIErrorMessages(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			name:   "503 is a bifrost with no preview config",
			status: http.StatusServiceUnavailable,
			body:   `{"error":"previews are not configured"}`,
			want:   "Preview API unavailable — bifrost has no preview config.",
		},
		{
			name:   "409 points at the listing that explains it",
			status: http.StatusConflict,
			body:   `{"error":"preview feat-x is busy"}`,
			want:   "That preview is busy — another up/down is in flight. `bif preview list` will show it while this is happening.",
		},
		{
			name:   "401 is the token",
			status: http.StatusUnauthorized,
			body:   `{"error":"unauthorized"}`,
			want:   "Unauthorized — check the preview API token.",
		},
		{
			// The server's message verbatim: bifrost is the only thing that
			// knows what went wrong, so the CLI must not paraphrase it.
			name:   "anything else surfaces the server's own detail",
			status: http.StatusInternalServerError,
			body:   `{"error":"list previews failed"}`,
			want:   "Preview API error 500: list previews failed",
		},
		{
			// A plain-text body from a router in front of bifrost, before any
			// handler runs. Falls back to the raw text rather than an empty
			// detail.
			name:   "a non-JSON body falls back to its text",
			status: http.StatusBadGateway,
			body:   "502 Bad Gateway\n",
			want:   "Preview API error 502: 502 Bad Gateway",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeBifrost(t, jsonHandler(tc.status, tc.body))
			stdout, stderr, code := execPreview(f, "", "list")
			if code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			if strings.TrimSpace(stderr) != tc.want {
				t.Errorf("stderr = %q, want %q", strings.TrimSpace(stderr), tc.want)
			}
			if stdout != "" {
				t.Errorf("stdout = %q; a failed list must print no table", stdout)
			}
		})
	}
}

// TestUnreachableBifrostNamesTheURL: nothing about a preview's state can be
// inferred when the server never answered, and the message says which server
// was not reachable so BIFROST_URL typos are self-diagnosing.
func TestUnreachableBifrostNamesTheURL(t *testing.T) {
	// A server that is closed immediately: connecting to it fails at the
	// transport, before any status code exists.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := &previewclient.Client{
		BaseURL: url,
		HTTP:    &http.Client{Timeout: 2 * time.Second},
		Token:   func(context.Context) (string, error) { return "t", nil },
	}
	var stdout, stderr bytes.Buffer
	code := previewCmd(context.Background(), []string{"list"}, strings.NewReader(""), &stdout, &stderr,
		func() previewAPI { return c })

	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Cannot reach bifrost at "+url) {
		t.Errorf("stderr = %q, want it to name %q", stderr.String(), url)
	}
}

// TestPreviewListTreatsA404AsFatal. The opposite of `up`'s reading, and
// deliberately so: the collection endpoint has nothing to be missing about
// (an empty fleet is a 200 with an empty list), so a 404 is the route itself
// not being there. An empty table claiming there are no previews would be a
// lie about the whole fleet.
func TestPreviewListTreatsA404AsFatal(t *testing.T) {
	f := newFakeBifrost(t, jsonHandler(http.StatusNotFound, `{"error":"no such route"}`))
	stdout, stderr, code := execPreview(f, "", "list")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if strings.TrimSpace(stderr) != "Preview API error 404: no such route" {
		t.Errorf("stderr = %q", stderr)
	}
	if strings.Contains(stdout, "No preview environments") {
		t.Errorf("a 404 was rendered as an empty fleet:\n%s", stdout)
	}
}

// ---- down ---------------------------------------------------------------

// TestPreviewDownConfirmed: "y" sends the DELETE and reports it.
func TestPreviewDownConfirmed(t *testing.T) {
	f := newFakeBifrost(t, jsonHandler(http.StatusAccepted, `{"tag":"feat-billing"}`))
	stdout, stderr, code := execPreview(f, "y\n", "down", "feat-billing")

	if code != 0 {
		t.Errorf("exit = %d, want 0 (stderr %q)", code, stderr)
	}
	want := "Tear down preview feat-billing? [y/N] Tearing down feat-billing.\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	got := f.requests()
	if len(got) != 1 || got[0].Method != http.MethodDelete || got[0].Path != "/api/previews/feat-billing" {
		t.Errorf("requests = %+v, want one DELETE /api/previews/feat-billing", got)
	}
}

// TestPreviewDownDeclinedSendsNoRequest. The confirmation gates the request,
// not just the message: anything less would tear down a preview on a "n".
func TestPreviewDownDeclinedSendsNoRequest(t *testing.T) {
	for _, answer := range []string{"n\n", "\n", "yes\n", ""} {
		t.Run("answer "+strings.TrimSpace(answer), func(t *testing.T) {
			f := newFakeBifrost(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("declined teardown still sent %s %s", r.Method, r.URL.Path)
			})
			stdout, _, code := execPreview(f, answer, "down", "feat-billing")

			if code != 0 {
				t.Errorf("exit = %d, want 0: declining is not a failure", code)
			}
			if !strings.HasSuffix(stdout, "Aborted.\n") {
				t.Errorf("stdout = %q, want it to end with Aborted.", stdout)
			}
			if n := len(f.requests()); n != 0 {
				t.Errorf("%d requests sent after declining; want none", n)
			}
			if n := f.tokens(); n != 0 {
				t.Errorf("fetched the API token %d times after declining; want none", n)
			}
		})
	}
}

// TestPreviewDownYesSkipsThePrompt: -y and --yes are for scripts, and must not
// block on a read from a stdin nobody is typing at.
func TestPreviewDownYesSkipsThePrompt(t *testing.T) {
	for _, flag := range []string{"-y", "--yes"} {
		t.Run(flag, func(t *testing.T) {
			f := newFakeBifrost(t, jsonHandler(http.StatusAccepted, `{}`))
			stdout, _, code := execPreview(f, "", "down", "feat-billing", flag)
			if code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}
			if strings.Contains(stdout, "[y/N]") {
				t.Errorf("stdout = %q, want no prompt", stdout)
			}
			if n := len(f.requests()); n != 1 {
				t.Errorf("%d requests sent, want 1", n)
			}
		})
	}
}

// TestPreviewDownTreatsA404AsAnError. The deliberate opposite of the poll
// loop's reading of the same status: nobody types a teardown at a tag they
// don't believe exists, so "no such preview" is a typo or an already-gone
// preview. Reporting a teardown that never happened is worse than a spurious
// failure, so this exits nonzero.
func TestPreviewDownTreatsA404AsAnError(t *testing.T) {
	f := newFakeBifrost(t, jsonHandler(http.StatusNotFound, `{"error":"unknown preview"}`))
	stdout, stderr, code := execPreview(f, "", "down", "feat-typo", "-y")

	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	want := "No preview feat-typo — nothing to tear down. `bif preview list` shows what exists.\n"
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	if strings.Contains(stdout, "Tearing down") {
		t.Errorf("stdout claims a teardown that never happened: %q", stdout)
	}
}

// TestPreviewDownBusyIs409: a teardown landing on a preview another operation
// already holds gets the busy message, not the not-found one.
func TestPreviewDownBusyIs409(t *testing.T) {
	f := newFakeBifrost(t, jsonHandler(http.StatusConflict, `{"error":"preview feat-billing is busy"}`))
	stdout, stderr, code := execPreview(f, "", "down", "feat-billing", "-y")

	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "That preview is busy") || !strings.Contains(stderr, "`bif preview list`") {
		t.Errorf("stderr = %q, want the busy message", stderr)
	}
	if strings.Contains(stdout, "Tearing down") {
		t.Errorf("stdout claims a teardown that never happened: %q", stdout)
	}
}

// TestPreviewDownArguments: exactly one tag, and no request on a bad
// invocation.
func TestPreviewDownArguments(t *testing.T) {
	for _, args := range [][]string{
		{"down"},
		{"down", "-y"},
		{"down", "feat-a", "feat-b"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			f := newFakeBifrost(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("bad invocation still sent %s %s", r.Method, r.URL.Path)
			})
			stdout, _, code := execPreview(f, "y\n", args...)
			if code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			if !strings.Contains(stdout, previewDownUsage) {
				t.Errorf("stdout = %q, want %q", stdout, previewDownUsage)
			}
			if n := len(f.requests()); n != 0 {
				t.Errorf("%d requests sent; want none", n)
			}
		})
	}
}

// ---- the cells ----------------------------------------------------------

func TestFormatTTLRemaining(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		after time.Duration
		want  string
	}{
		{"eight hours", 8 * time.Hour, "8h0m"},
		{"an hour and a half", 90 * time.Minute, "1h30m"},
		{"under an hour", 45 * time.Minute, "45m"},
		{"just over a day", 26 * time.Hour, "1d2h"},
		{"three days", 3*24*time.Hour + 2*time.Hour, "3d2h"},
		{"the 30-day cap", 720 * time.Hour, "30d0h"},
		// Under a minute is its own cell rather than "0m", which would read as
		// already gone.
		{"seconds left", 30 * time.Second, "<1m"},
		{"exactly one minute", time.Minute, "1m"},
		// bifrost's sweep runs hourly, so a preview can sit past-due but alive
		// for up to an hour. That is "expired", not a negative duration.
		{"past due", -30 * time.Minute, "expired"},
		{"exactly now", 0, "expired"},
		{"long past due", -100 * time.Hour, "expired"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatTTLRemaining(now.Add(tc.after), now); got != tc.want {
				t.Errorf("formatTTLRemaining(+%v) = %q, want %q", tc.after, got, tc.want)
			}
		})
	}
}

// TestPadCountsRunesNotBytes: the AUTO column's "✓" is three bytes and one
// character. fmt's "%-5s" pads by bytes, which would shift every column after
// AUTO on exactly the rows an auto-updating preview appears in — a bug that
// only shows up on the rows that matter.
func TestPadCountsRunesNotBytes(t *testing.T) {
	if got := pad("✓", 5); got != "✓    " {
		t.Errorf("pad(\"✓\", 5) = %q (%d bytes), want the glyph plus four spaces", got, len(got))
	}
	if got := pad("-", 5); got != "-    " {
		t.Errorf("pad(\"-\", 5) = %q", got)
	}
	// A value at or over the width is never truncated: a long tag pushes its
	// row out rather than losing characters an operator would have to type.
	if got := pad("a-very-long-preview-tag-indeed", 24); got != "a-very-long-preview-tag-indeed" {
		t.Errorf("pad over width = %q, want the value untouched", got)
	}
}

// TestPreviewPhaseCell pins where busy is marked. A stale, unmarked
// ready-looking table is what made a still-running server-side `up` look
// finished (the training-plans incident), so the mark is the assertion.
func TestPreviewPhaseCell(t *testing.T) {
	tests := []struct {
		name string
		rec  previewapi.Record
		want string
	}{
		{"a settled preview", previewapi.Record{Phase: "ready"}, "ready"},
		{"mid-create", previewapi.Record{Phase: "creating"}, "creating"},
		{"mid-create and held", previewapi.Record{Phase: "creating", Busy: true}, "creating*"},
		{"ready but held", previewapi.Record{Phase: "ready", Busy: true}, "ready*"},
		{
			// What bifrost actually sends for a tag with no namespace:
			// previewapi.Record.Phase has no omitempty, and busyRecord fills
			// it with PhaseBusy. ib.py's docstring claims this renders a bare
			// "busy"; its code — the oracle — renders "busy*", and so does
			// this.
			name: "a synthesized record as bifrost sends it",
			rec:  previewapi.Record{Phase: previewapi.PhaseBusy, Busy: true},
			want: "busy*",
		},
		{
			// The bare word, for a server that reports no phase at all. Dead
			// against bifrost as it stands, alive against an older one.
			name: "held with no phase reported",
			rec:  previewapi.Record{Busy: true},
			want: "busy",
		},
		{"nothing known at all", previewapi.Record{}, "-"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := previewPhaseCell(tc.rec); got != tc.want {
				t.Errorf("previewPhaseCell = %q, want %q", got, tc.want)
			}
		})
	}
}
