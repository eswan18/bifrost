// Package neon drives Neon database branching for preview environments:
// one branch of the service's staging DB per preview, deleted on teardown.
package neon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const httpTimeout = 10 * time.Second

// Branch is the slice of Neon's v2 branch object bifrost reads.
//
// CreatedAt is the API's created_at — a REQUIRED property of that object
// (verified against Neon's published OpenAPI spec, neon.com/api_spec/release/
// v2.json: "created_at" is listed in the Branch schema's required set with
// format date-time, and both listProjectBranches and createProjectBranch
// return that same schema). encoding/json parses an RFC3339 string straight
// into time.Time, so no custom unmarshalling is involved.
//
// A missing created_at decodes to the zero time, which reads as "created in
// year 1" — i.e. arbitrarily old. The preview orphan sweep refuses to delete a
// branch younger than an hour, so a silently absent timestamp would defeat
// that floor rather than trip it. The field is required by the API, so this is
// a note on what to watch for, not a live hazard.
// Default reports whether this is the project's DEFAULT branch — the one
// CreateBranch cuts from when it is passed an empty parent ID, and (in every
// project bifrost touches) production. Like created_at it is a REQUIRED
// property of Neon's v2 Branch schema, so a false here means "not the
// default" rather than "the API didn't say".
type Branch struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Default   bool      `json:"default"`
	CreatedAt time.Time `json:"created_at"`
}

type Client interface {
	ListBranches(ctx context.Context, projectID string) ([]Branch, error)
	// CreateBranch makes a branch with a read-write endpoint (compute).
	// Empty parentID branches from the project's default branch.
	CreateBranch(ctx context.Context, projectID, name, parentID string) (Branch, error)
	DeleteBranch(ctx context.Context, projectID, branchID string) error
	ConnectionURI(ctx context.Context, projectID, branchID, database, role string) (string, error)
	// ListRoles returns the NAMES of the Postgres roles on a branch. A branch
	// inherits its parent's roles, so asking the branch a preview will be cut
	// from answers "will this role exist in the preview?" before the preview
	// branch itself exists — see internal/preview.verifyNeonRefs.
	ListRoles(ctx context.Context, projectID, branchID string) ([]string, error)
}

type client struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

func New(apiKey string) Client {
	return NewWithBaseURL(apiKey, "https://console.neon.tech/api/v2")
}

// NewWithBaseURL exists as a test seam; production code uses New.
func NewWithBaseURL(apiKey, baseURL string) Client {
	return &client{apiKey: apiKey, baseURL: baseURL, http: &http.Client{Timeout: httpTimeout}}
}

// do issues one request and decodes the response into out (skipped when nil).
// okStatuses lists the exact status codes the endpoint returns on success;
// anything else is an error. (Neon's deleteProjectBranch, for one, documents
// both 200 and 204 as success.)
//
// includeBodyInError controls whether a non-2xx response's body is read at
// all for the error message — see errBodyExcerpt. It is false only for
// ConnectionURI's call (see its own doc comment for why), true everywhere
// else.
func (c *client) do(ctx context.Context, method, path string, body any, out any, includeBodyInError bool, okStatuses ...int) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	ok := false
	for _, s := range okStatuses {
		if resp.StatusCode == s {
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("neon %s %s returned %d%s", method, path, resp.StatusCode, errBodyExcerpt(resp.Body, includeBodyInError))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// maxErrBodyRead bounds how much of a non-2xx response is even READ before
// errBodyExcerpt trims it further. Neon errors are normally a small JSON
// object, but a proxy in front of the API can return a multi-kilobyte HTML
// error page instead, and this must never buffer all of that just to throw
// most of it away — hence a limited reader rather than io.ReadAll(resp.Body).
const maxErrBodyRead = 4096

// maxErrBodyExcerpt is the cap actually kept, in runes, after whitespace is
// flattened. This string can end up in the bifrost/error annotation, which is
// served over bifrost's JSON API and rendered in the browser — the same
// exposure internal/preview.sanitizeReason exists for, and this mirrors its
// cap (160) and its "flatten whitespace, truncate with an ellipsis" shape.
// The two aren't shared code: internal/preview already imports this package
// for neon.Client, so importing back here would cycle. Behavior, not the
// function, is what's kept identical.
const maxErrBodyExcerpt = 160

// errBodyExcerpt renders a non-2xx response body as ": <excerpt>" for
// appending to an error, or "" when there's nothing usable to add — so a
// caller never ends up with a dangling ": " when the body is empty, absent,
// or excluded.
//
// includeBody is the do call's includeBodyInError: when false, the body is
// never even read.
func errBodyExcerpt(body io.Reader, includeBody bool) string {
	if !includeBody {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(body, maxErrBodyRead))
	if err != nil {
		return ""
	}
	excerpt := sanitizeErrBody(string(raw))
	if excerpt == "" {
		return ""
	}
	return ": " + excerpt
}

// sanitizeErrBody flattens whitespace and caps length — see maxErrBodyExcerpt
// for why this duplicates internal/preview.sanitizeReason's behavior instead
// of calling it.
func sanitizeErrBody(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxErrBodyExcerpt {
		s = string(r[:maxErrBodyExcerpt]) + "..."
	}
	return s
}

func (c *client) ListBranches(ctx context.Context, projectID string) ([]Branch, error) {
	var body struct {
		Branches []Branch `json:"branches"`
	}
	path := "/projects/" + url.PathEscape(projectID) + "/branches"
	if err := c.do(ctx, http.MethodGet, path, nil, &body, true, http.StatusOK); err != nil {
		return nil, err
	}
	return body.Branches, nil
}

// ListRoles is the second endpoint (after ConnectionURI) whose error path
// passes includeBodyInError=false, and for the same reason: its response
// carries credentials. Neon's published spec marks the whole `roles` array
// x-sensitive and each Role's `password` field with it, so a branch's role
// list is not a body that may be echoed into an error that ends up in the
// bifrost/error annotation, the JSON API, and a browser.
//
// Only names are decoded, and only names are returned. `password` is never
// bound to a field here even on the success path, so nothing downstream can
// accidentally log a role's credentials by printing this function's result.
func (c *client) ListRoles(ctx context.Context, projectID, branchID string) ([]string, error) {
	var body struct {
		Roles []struct {
			Name string `json:"name"`
		} `json:"roles"`
	}
	path := "/projects/" + url.PathEscape(projectID) + "/branches/" + url.PathEscape(branchID) + "/roles"
	if err := c.do(ctx, http.MethodGet, path, nil, &body, false, http.StatusOK); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(body.Roles))
	for _, r := range body.Roles {
		names = append(names, r.Name)
	}
	return names, nil
}

func (c *client) CreateBranch(ctx context.Context, projectID, name, parentID string) (Branch, error) {
	branch := map[string]any{"name": name}
	if parentID != "" {
		branch["parent_id"] = parentID
	}
	req := map[string]any{
		"branch":    branch,
		"endpoints": []map[string]any{{"type": "read_write"}},
	}
	var body struct {
		Branch Branch `json:"branch"`
	}
	path := "/projects/" + url.PathEscape(projectID) + "/branches"
	if err := c.do(ctx, http.MethodPost, path, req, &body, true, http.StatusCreated); err != nil {
		return Branch{}, err
	}
	return body.Branch, nil
}

func (c *client) DeleteBranch(ctx context.Context, projectID, branchID string) error {
	path := "/projects/" + url.PathEscape(projectID) + "/branches/" + url.PathEscape(branchID)
	// Neon returns 200 when the branch was deleted, and 204 when it was
	// already gone; both mean the branch no longer exists, so both count as
	// success for this idempotent teardown operation.
	return c.do(ctx, http.MethodDelete, path, nil, nil, true, http.StatusOK, http.StatusNoContent)
}

// ConnectionURI's success response is a credentialed Postgres connection
// string — the value internal/preview's own doc comments repeatedly warn
// must never be logged or embedded in an error (see branchNeonDatabases and
// fail in orchestrator.go: "it's a secret" / "never a secret value"). Its
// error path therefore passes includeBodyInError=false and never reads the
// response body at all, rather than trusting the general sanitizer to notice
// a URI it wasn't written to look for.
//
// This is deliberately conservative, not a reaction to an observed leak:
// Neon's error responses across the API share one general error shape
// (a small {code, message} object, per its published spec), not the shape of
// whatever endpoint failed, so there is no reason to expect getConnectionURI
// to echo the uri field it failed to produce. But this is the one endpoint
// whose entire job is handing back credentials, so it gets the one exception
// that costs nothing: on failure there is no successful URI to report
// anyway, and every other Neon error already names the operation and status
// code without it.
func (c *client) ConnectionURI(ctx context.Context, projectID, branchID, database, role string) (string, error) {
	q := url.Values{
		"branch_id":     {branchID},
		"database_name": {database},
		"role_name":     {role},
	}
	var body struct {
		URI string `json:"uri"`
	}
	path := "/projects/" + url.PathEscape(projectID) + "/connection_uri?" + q.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, &body, false, http.StatusOK); err != nil {
		return "", err
	}
	return body.URI, nil
}
