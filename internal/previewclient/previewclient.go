// Package previewclient talks to bifrost's preview API on behalf of `bif
// preview`. It is a port of infra/ib.py's preview_token/preview_api.
//
// It is a separate package from cmd/bif on purpose, and that purpose is a
// property, not tidiness. `bif promote bifrost` is how bifrost is recovered
// when bifrost is down, so status and promote must never reach for the server
// or its bearer token. Everything that CAN do either lives here, so the ban is
// enforceable: cmd/bif/main_test.go's TestNoBifrostServerDependency asserts
// that the files implementing status and promote import neither this package
// nor net/http, and that no package they depend on pulls this one in
// transitively. Only cmd/bif/preview.go may import it.
//
// Preview is the one command that is an HTTP client by design: bifrost owns
// the orchestration, the busy set, the cluster write credentials and the Neon
// and Cloud Build tokens, so there is nothing here for the CLI to do locally.
//
// Most of this package's tests live in cmd/bif/preview_test.go, driving the
// real Client against an httptest server through the commands that use it —
// the headers, the status-code mapping and the token are asserted there, on
// the path an operator actually runs. The on-disk token cache (tokencache.go)
// is tested here instead, in tokencache_test.go: file modes, expiry and the
// rotation retry are properties of this package rather than of any command.
// Nothing in the test suite shells out to gcloud or talks to a live bifrost.
package previewclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/eswan18/bifrost/internal/previewapi"
)

// DefaultBaseURL is where bifrost runs, overridable through BIFROST_URL for a
// local or staging instance — the same env var and default as ib.py's.
const DefaultBaseURL = "https://bifrost.ethanswan.com"

// TokenSecret is the Secret Manager secret holding the preview API bearer
// token, read with `gcloud secrets versions access latest --secret=...`.
const TokenSecret = "bifrost_prod_preview_api_token"

// UserAgent is sent on every request and is NOT optional. Cloudflare's WAF
// bans default client signatures outright (error 1010,
// browser_signature_banned) before the request ever reaches bifrost — it did
// so for "Python-urllib/x.y", and Go's "Go-http-client/1.1" is no more
// welcome. A missing header here breaks the CLI against prod while every test
// against an httptest server keeps passing, which is why TestSendsUserAgent
// exists.
//
// The value deliberately still says "ib" after the binary was renamed to bif.
// Every other ib→bif rename in this port was a string a human reads; this one
// is a token an external, dashboard-managed WAF sees, and nothing in this repo
// can prove no rule keys on it. A cosmetic rename is not worth an
// unverifiable risk to the prod path.
const UserAgent = "ib-preview-cli"

// requestTimeout bounds a single API call, matching ib.py's urlopen timeout.
const requestTimeout = 30 * time.Second

// Client calls bifrost's preview API. The zero value is not usable; use New.
type Client struct {
	// BaseURL is bifrost's origin, with no trailing slash.
	BaseURL string
	// HTTP is the transport. Tests point this at an httptest server.
	HTTP *http.Client
	// Token returns the bearer token. Injected so tests never shell out to
	// gcloud, and so the token source is one named thing rather than a call
	// buried in the request path.
	Token func(ctx context.Context) (string, error)

	// cache is the on-disk token cache (see tokencache.go), shared by every
	// bif invocation on the machine. New sets it; a hand-built Client leaves
	// it nil, which disables caching entirely — that is what every test with
	// an injected Token wants, since a unit test must neither read nor write
	// the developer's real token file.
	cache *tokenCache

	// The token, memoized for the life of the process. ib.py re-runs gcloud
	// on every single request, which is invisible for `list` and `down` (one
	// call each) but is a subprocess every 3 seconds for `up`'s poll loop.
	//
	// fromCache records that the memoized value came off disk rather than
	// from gcloud, and refreshed that the one permitted rotation retry has
	// been spent. Both belong to discardStaleToken; see there.
	//
	// A mutex rather than the sync.Once this used to be, because the value is
	// no longer write-once: a 401 can replace it mid-run.
	mu        sync.Mutex
	haveToken bool
	cached    string
	tokErr    error
	fromCache bool
	refreshed bool
}

// New builds a Client against BIFROST_URL (or DefaultBaseURL), reading the
// bearer token from Secret Manager through gcloud.
func New() *Client {
	base := os.Getenv("BIFROST_URL")
	if base == "" {
		base = DefaultBaseURL
	}
	return &Client{
		BaseURL: strings.TrimSuffix(base, "/"),
		HTTP:    &http.Client{Timeout: requestTimeout},
		Token:   GcloudToken,
		cache:   &tokenCache{},
	}
}

// GcloudToken reads the preview API token by shelling out to gcloud, exactly
// as ib.py does. Shelling out is deliberate and is called out in the plan as
// out of scope to change: the Secret Manager client library would be a new
// dependency and new IAM for a value the operator's own gcloud login already
// grants.
//
// The error is gcloud's own stderr, so a caller printing "Error: %v" reproduces
// ib.py's run() verbatim — an expired login says so in gcloud's words rather
// than in wording invented here.
func GcloudToken(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "gcloud", "secrets", "versions", "access", "latest", "--secret="+TokenSecret)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", errors.New(msg)
		}
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

// token returns the bearer token: the process memo, then the on-disk cache,
// then gcloud — and a gcloud read is written back so the next bif on this
// machine does not pay for it either.
//
// The cache write is best-effort and its error is dropped. A read-only HOME,
// a full disk or a cache directory somebody has chmodded shut costs latency,
// not a command — and there is no stream here to report it on that is not
// somebody's stdout. See tokencache.go.
func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.haveToken {
		return c.cached, c.tokErr
	}
	if c.cache != nil {
		if token, ok := c.cache.read(); ok {
			c.cached, c.tokErr, c.fromCache, c.haveToken = token, nil, true, true
			return c.cached, c.tokErr
		}
	}
	c.cached, c.tokErr = c.Token(ctx)
	c.haveToken = true
	if c.tokErr == nil && c.cache != nil {
		_ = c.cache.write(c.cached)
	}
	return c.cached, c.tokErr
}

// discardStaleToken answers the one failure the cache introduces that shelling
// out every time could not: bifrost_prod_preview_api_token was rotated while a
// copy of the old value sat on this machine's disk. Without this, rotating the
// secret would silently break `bif preview` for everyone until they deleted a
// file they did not know existed — a far worse problem than the half-second
// the cache saves.
//
// It purges the file, reads the secret again, and reports whether the caller
// should retry. Three conditions, each doing real work:
//
//   - there IS a cache. A hand-built Client without one is unchanged.
//   - the rejected token came OFF that cache. A token gcloud handed over
//     moments ago is not stale; a 401 on it means the token is genuinely
//     refused, and re-asking gcloud for the same string to send it again would
//     be a wasted round trip on every 401 the CLI has ever reported.
//   - the retry has not already been spent. One per client, so a bifrost that
//     401s everything answers in two requests rather than forever.
func (c *Client) discardStaleToken(ctx context.Context) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil || !c.fromCache || c.refreshed {
		return false
	}
	c.refreshed = true
	c.cache.purge()

	token, err := c.Token(ctx)
	if err != nil {
		// Keep the original 401 as the error the operator sees: "check the
		// preview API token" is closer to the truth than whatever gcloud says
		// about a login, and the request has already failed either way.
		return false
	}
	c.cached, c.tokErr, c.fromCache = token, nil, false
	_ = c.cache.write(token)
	return true
}

// staleTokenRejected reports whether err is the shape a rotated token takes:
// bifrost refusing the credential. 403 as well as 401 because which of the two
// a rejected bearer token produces is the server's choice, not this client's.
func staleTokenRejected(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) &&
		(apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden)
}

// NotFoundError is a 404 from the preview API, carrying the server's own
// detail message.
//
// It is a distinct error type rather than an exit, because a 404 is not always
// a failure and only the caller knows which it is. bifrost 404s any tag with
// no namespace behind it, and POST /api/previews returns 202 as soon as it
// accepts the request — membership resolution, pre-flight and EnsureNamespace
// all run afterward in a detached goroutine, so there is a window (seconds to
// minutes) where a preview being created legitimately does not exist yet.
// Which of "not created yet", "already gone" and "no such preview" a 404 means
// depends entirely on who asked. `preview up` reads it as absence and keeps
// waiting; `preview down` reads it as the error it really is there. Treating
// it as fatal everywhere is the bug that made `--no-wait` abort with "Preview
// API error 404" on a create that was proceeding perfectly.
//
// Its Error() is what a caller that DOES treat it as fatal should print, which
// is exactly what the generic handler used to say.
type NotFoundError struct {
	// Detail is the server's error message, verbatim.
	Detail string
}

func (e *NotFoundError) Error() string { return fmt.Sprintf("Preview API error 404: %s", e.Detail) }

// APIError is any other non-2xx. Its Error() is the message ib.py prints for
// that status, so callers print the error and nothing else — the wording lives
// in one place, next to the status it explains.
type APIError struct {
	Status int
	// Detail is the server's own "error" field, or the raw body when the
	// response is not JSON (a plain-text 404/405 from a router in front of
	// bifrost, say). Never invented here.
	Detail string
}

func (e *APIError) Error() string {
	switch e.Status {
	case http.StatusServiceUnavailable:
		return "Preview API unavailable — bifrost has no preview config."
	case http.StatusConflict:
		return "That preview is busy — another up/down is in flight. " +
			"`bif preview list` will show it while this is happening."
	case http.StatusUnauthorized:
		return "Unauthorized — check the preview API token."
	default:
		return fmt.Sprintf("Preview API error %d: %s", e.Status, e.Detail)
	}
}

// TransportError is a failure to reach bifrost at all — DNS, connection
// refused, TLS, timeout. Named separately from APIError because it says
// something different: the server did not answer, so nothing about the
// preview's state can be inferred.
type TransportError struct {
	BaseURL string
	Err     error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("Cannot reach bifrost at %s: %v", e.BaseURL, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

// do performs one API call, retrying exactly once if the failure was bifrost
// refusing a token that had been sitting in the on-disk cache since before a
// rotation. See discardStaleToken for why that is the only case retried, and
// why one is the count.
//
// The body is re-encoded by attempt on each try rather than shared, so the
// retry sends a fresh reader rather than one that has already been drained.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	err := c.attempt(ctx, method, path, body, out)
	if staleTokenRejected(err) && c.discardStaleToken(ctx) {
		return c.attempt(ctx, method, path, body, out)
	}
	return err
}

// attempt performs one API call, decoding a 2xx body into out (which may be
// nil to discard it) and mapping every failure onto the error types above.
func (c *Client) attempt(ctx context.Context, method, path string, body, out any) error {
	var payload io.Reader
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			return fmt.Errorf("encoding the request body: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, payload)
	if err != nil {
		return &TransportError{BaseURL: c.BaseURL, Err: err}
	}
	tok, err := c.token(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	if encoded != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &TransportError{BaseURL: c.BaseURL, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return &TransportError{BaseURL: c.BaseURL, Err: err}
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		detail := errorDetail(raw)
		if resp.StatusCode == http.StatusNotFound {
			return &NotFoundError{Detail: detail}
		}
		return &APIError{Status: resp.StatusCode, Detail: detail}
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding the preview API response: %w", err)
	}
	return nil
}

// errorDetail pulls the message out of a non-2xx body. Non-2xx bodies are
// previewapi.ErrorResponse, but not always: a plain-text 404 or 405 from the
// router itself, before it reaches bifrost's handlers, has no JSON in it at
// all. That falls back to the raw text rather than printing an empty detail.
func errorDetail(raw []byte) string {
	var body previewapi.ErrorResponse
	if err := json.Unmarshal(raw, &body); err == nil && body.Error != "" {
		return body.Error
	}
	return strings.TrimSpace(string(raw))
}

// List returns every preview bifrost knows about, busy tags with no namespace
// included.
func (c *Client) List(ctx context.Context) ([]previewapi.Record, error) {
	var resp previewapi.ListResponse
	if err := c.do(ctx, http.MethodGet, "/api/previews", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Previews, nil
}

// Get fetches one preview by tag.
//
// A *NotFoundError here means bifrost has no namespace behind the tag, which
// is NOT the same as "no such preview" — see NotFoundError. `bif preview up`
// reads it as "not created yet" both before the POST and throughout the poll
// loop; nothing else calls this.
func (c *Client) Get(ctx context.Context, tag string) (*previewapi.Record, error) {
	var rec previewapi.Record
	if err := c.do(ctx, http.MethodGet, "/api/previews/"+tag, nil, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Create asks bifrost to create or update the preview for a branch.
//
// The response is a 202 sent as soon as the request is accepted: membership
// resolution, pre-flight and EnsureNamespace all run afterward in a detached
// goroutine. So the record returned here carries only what bifrost can know
// synchronously — the tag it derived and a "creating" phase — and is a partial
// previewapi.Record rather than a description of anything that exists yet.
// Everything else comes from polling Get.
//
// Its Tag is authoritative and the caller must use it in preference to any tag
// it derived itself: bifrost, not the CLI, decides what a branch is called.
func (c *Client) Create(ctx context.Context, req previewapi.CreateRequest) (*previewapi.Record, error) {
	var rec previewapi.Record
	if err := c.do(ctx, http.MethodPost, "/api/previews", req, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Delete tears down one preview. A *NotFoundError here means bifrost has no
// such preview; whether that is an error is the caller's call (for `bif
// preview down` it is).
func (c *Client) Delete(ctx context.Context, tag string) error {
	return c.do(ctx, http.MethodDelete, "/api/previews/"+tag, nil, nil)
}
