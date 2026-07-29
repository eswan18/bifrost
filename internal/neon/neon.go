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
type Branch struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type Client interface {
	ListBranches(ctx context.Context, projectID string) ([]Branch, error)
	// CreateBranch makes a branch with a read-write endpoint (compute).
	// Empty parentID branches from the project's default branch.
	CreateBranch(ctx context.Context, projectID, name, parentID string) (Branch, error)
	DeleteBranch(ctx context.Context, projectID, branchID string) error
	ConnectionURI(ctx context.Context, projectID, branchID, database, role string) (string, error)
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
func (c *client) do(ctx context.Context, method, path string, body any, out any, okStatuses ...int) error {
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
		return fmt.Errorf("neon %s %s returned %d", method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *client) ListBranches(ctx context.Context, projectID string) ([]Branch, error) {
	var body struct {
		Branches []Branch `json:"branches"`
	}
	path := "/projects/" + url.PathEscape(projectID) + "/branches"
	if err := c.do(ctx, http.MethodGet, path, nil, &body, http.StatusOK); err != nil {
		return nil, err
	}
	return body.Branches, nil
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
	if err := c.do(ctx, http.MethodPost, path, req, &body, http.StatusCreated); err != nil {
		return Branch{}, err
	}
	return body.Branch, nil
}

func (c *client) DeleteBranch(ctx context.Context, projectID, branchID string) error {
	path := "/projects/" + url.PathEscape(projectID) + "/branches/" + url.PathEscape(branchID)
	// Neon returns 200 when the branch was deleted, and 204 when it was
	// already gone; both mean the branch no longer exists, so both count as
	// success for this idempotent teardown operation.
	return c.do(ctx, http.MethodDelete, path, nil, nil, http.StatusOK, http.StatusNoContent)
}

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
	if err := c.do(ctx, http.MethodGet, path, nil, &body, http.StatusOK); err != nil {
		return "", err
	}
	return body.URI, nil
}
