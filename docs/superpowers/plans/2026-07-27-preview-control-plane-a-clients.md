# Preview Control Plane 3a: Platform Clients Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give bifrost the tested building blocks the preview control plane needs — preview-related config, a bearer-token auth middleware, a GitHub branch client, a Neon branch client, and Cloud Build trigger-run/build-get methods — with no behavior change to the running app (nothing is wired into `main.go` yet; that's plan 3b).

**Architecture:** Three new/extended packages following the repo's established client pattern (`internal/gcb` exemplar: exported `Client` interface + `New(...)` constructor + pure separately-tested logic). GitHub and Neon are hand-rolled `net/http` clients modeled on `internal/auth/oidc.go`'s `fetchDiscoveryDoc` (context-threaded requests, per-package timeout const, exact-status checks), tested with `httptest.NewServer` — new to this repo but the right tool for faking third-party REST APIs. Config gains five vars, all following the "empty disables" precedent of `GCP_PROJECT`. No retries anywhere (matches the codebase's single-attempt convention).

**Tech Stack:** Go 1.26, stdlib `net/http` + `httptest`, existing `google.golang.org/api/cloudbuild/v1`, module `github.com/eswan18/bifrost`.

**Repo / branch:** `~/Develop/ibormeith/bifrost`, branch `preview-control-plane-a` from up-to-date main; PR at the end.

**Spec:** `docs/superpowers/specs/2026-07-26-preview-environments-design.md` (Control plane section). Sub-plan 3b (state + read API + UI) and 3c (create/teardown orchestration) build on these packages; their needs define the exact surfaces below — resist adding more.

## Global Constraints

- **No `main.go` or `internal/web` wiring changes** — except the mechanical fake-stub additions in `internal/web/handlers_test.go` required by the `gcb.Client` interface growth (Task 5). The app must build and behave identically.
- New config vars follow existing conventions exactly: accumulate missing-required errors, `parsePairs` for map-valued vars, "empty string disables the feature" for tokens/keys.
- HTTP clients: `http.NewRequestWithContext` always; `&http.Client{Timeout: httpTimeout}` with a per-package `const httpTimeout = 10 * time.Second`; `defer func() { _ = resp.Body.Close() }()`; exact-status checks; leaf functions return raw errors, call sites wrap with `fmt.Errorf("action: %w", err)`.
- Secrets (`GITHUB_TOKEN`, `NEON_API_KEY`, `PREVIEW_API_TOKEN`) must never be logged or embedded in error messages.
- Verification for every task: `go build ./... && go vet ./... && go test ./...` from the repo root, all green.
- Token comparison in the bearer middleware must be constant-time (`crypto/subtle`).

---

### Task 1: Preview config vars

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go` (append)

**Interfaces:**
- Produces on `Config`: `PreviewServices []string`, `NeonProjects map[string]NeonProjectRef`, `GitHubToken string`, `NeonAPIKey string`, `PreviewAPIToken string`, and type `NeonProjectRef{ProjectID, Database, Role string}`. Plans 3b/3c consume exactly these names.

- [ ] **Step 1: Write the failing tests** (append to `internal/config/config_test.go`, following the file's `minimalValidEnv()` + one-func-per-case style):

```go
func TestPreviewConfigDefaults(t *testing.T) {
	cfg, err := loadFromMap(minimalValidEnv())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.PreviewServices) != 0 || len(cfg.NeonProjects) != 0 {
		t.Errorf("expected empty preview config, got %v / %v", cfg.PreviewServices, cfg.NeonProjects)
	}
	if cfg.GitHubToken != "" || cfg.NeonAPIKey != "" || cfg.PreviewAPIToken != "" {
		t.Errorf("expected empty tokens by default")
	}
}

func TestPreviewServicesParsed(t *testing.T) {
	m := minimalValidEnv()
	m["PREVIEW_SERVICES"] = "footstrike-api, footstrike-dashboard,identity,"
	cfg, err := loadFromMap(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"footstrike-api", "footstrike-dashboard", "identity"}
	if !reflect.DeepEqual(cfg.PreviewServices, want) {
		t.Errorf("PreviewServices = %v, want %v", cfg.PreviewServices, want)
	}
}

func TestNeonProjectsParsed(t *testing.T) {
	m := minimalValidEnv()
	m["NEON_PROJECTS"] = "footstrike-api=proj-abc/fitnessdb/fitness_owner,identity=proj-def/identitydb/identity_owner"
	cfg, err := loadFromMap(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]NeonProjectRef{
		"footstrike-api": {ProjectID: "proj-abc", Database: "fitnessdb", Role: "fitness_owner"},
		"identity":       {ProjectID: "proj-def", Database: "identitydb", Role: "identity_owner"},
	}
	if !reflect.DeepEqual(cfg.NeonProjects, want) {
		t.Errorf("NeonProjects = %v, want %v", cfg.NeonProjects, want)
	}
}

func TestNeonProjectsMalformedRef(t *testing.T) {
	m := minimalValidEnv()
	m["NEON_PROJECTS"] = "footstrike-api=proj-abc/fitnessdb"
	if _, err := loadFromMap(m); err == nil {
		t.Error("expected error for ref missing role segment")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/config/ -run 'TestPreview|TestNeonProjects' -v`
Expected: compile error — `cfg.PreviewServices` undefined.

- [ ] **Step 3: Implement**

In `internal/config/config.go`:

Add to the struct (after `ProdURLs`):

```go
	// Preview control plane (all optional; empty disables preview features).
	PreviewServices []string                  // services eligible for preview environments
	NeonProjects    map[string]NeonProjectRef // service -> Neon project/db/role for DB branching
	GitHubToken     string                    // PAT for branch lookups on private repos
	NeonAPIKey      string                    // Neon API key for branch create/delete
	PreviewAPIToken string                    // static bearer token for the preview API (CLI use)
```

Add the type near the top of the file:

```go
// NeonProjectRef locates one service's database for preview branching.
type NeonProjectRef struct {
	ProjectID string
	Database  string
	Role      string
}
```

In `Load()`, add to the env map reads: `PREVIEW_SERVICES`, `NEON_PROJECTS`, `GITHUB_TOKEN`, `NEON_API_KEY`, `PREVIEW_API_TOKEN`.

In `loadFromMap`, after the existing map-var parsing:

```go
	for _, s := range strings.Split(m["PREVIEW_SERVICES"], ",") {
		if s = strings.TrimSpace(s); s != "" {
			cfg.PreviewServices = append(cfg.PreviewServices, s)
		}
	}
	neonPairs, err := parsePairs(m["NEON_PROJECTS"], "NEON_PROJECTS")
	if err != nil {
		return nil, err
	}
	if len(neonPairs) > 0 {
		cfg.NeonProjects = make(map[string]NeonProjectRef, len(neonPairs))
		for svc, ref := range neonPairs {
			parts := strings.Split(ref, "/")
			if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
				return nil, fmt.Errorf("NEON_PROJECTS entry %q is not projectID/database/role", svc+"="+ref)
			}
			cfg.NeonProjects[svc] = NeonProjectRef{ProjectID: parts[0], Database: parts[1], Role: parts[2]}
		}
	}
	cfg.GitHubToken = m["GITHUB_TOKEN"]
	cfg.NeonAPIKey = m["NEON_API_KEY"]
	cfg.PreviewAPIToken = m["PREVIEW_API_TOKEN"]
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/config/ -v` — all pass (new and pre-existing).

- [ ] **Step 5: Full gates + commit**

Run: `go build ./... && go vet ./... && go test ./...`

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "Add preview control-plane config vars"
```

---

### Task 2: Bearer-token middleware

**Files:**
- Create: `internal/auth/bearer.go`
- Test: `internal/auth/bearer_test.go`

**Interfaces:**
- Produces: `func RequireBearer(token string) func(http.Handler) http.Handler`. Plan 3b wraps the `/api/previews*` routes with it (alongside, not replacing, session auth — 3b handles the either/or). A bearer-authed request carries no session; handlers behind it must not call `SessionFromContext`.

- [ ] **Step 1: Failing tests** (`internal/auth/bearer_test.go`):

```go
package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireBearer(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := RequireBearer("s3cret")(next)

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"valid token", "Bearer s3cret", http.StatusNoContent},
		{"wrong token", "Bearer nope", http.StatusUnauthorized},
		{"missing header", "", http.StatusUnauthorized},
		{"wrong scheme", "Basic s3cret", http.StatusUnauthorized},
		{"token as prefix", "Bearer s3cret-and-more", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/previews", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("%s: status = %d, want %d", tc.name, rec.Code, tc.want)
			}
		})
	}
}

func TestRequireBearerEmptyTokenRejectsEverything(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := RequireBearer("")(next)
	req := httptest.NewRequest("GET", "/api/previews", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("empty configured token must reject all requests, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Verify failure** — `go test ./internal/auth/ -run TestRequireBearer -v` → compile error.

- [ ] **Step 3: Implement** (`internal/auth/bearer.go`):

```go
package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// RequireBearer gates a route on a static bearer token (the preview API's
// CLI auth). An empty configured token rejects every request — the feature
// is disabled, not open. Constant-time comparison; requests authed this way
// carry no session, so downstream handlers must not use SessionFromContext.
func RequireBearer(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || token == "" ||
				subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
```

- [ ] **Step 4: Verify pass** — `go test ./internal/auth/ -v` (whole package — existing middleware tests must stay green).

- [ ] **Step 5: Gates + commit**

```bash
git add internal/auth/bearer.go internal/auth/bearer_test.go
git commit -m "Add static bearer-token middleware for the preview API"
```

---

### Task 3: GitHub branch client

**Files:**
- Create: `internal/github/github.go`
- Test: `internal/github/github_test.go`

**Interfaces:**
- Produces: `type Client interface { BranchSHA(ctx context.Context, repo, branch string) (string, error) }`, `func New(org, token string) Client`, `func NewWithBaseURL(org, token, baseURL string) Client` (test seam), sentinel `var ErrNoBranch = errors.New("branch not found")`. 3c's membership detection is: `BranchSHA` per preview service; `errors.Is(err, ErrNoBranch)` → service not in the preview.

- [ ] **Step 1: Failing tests** (`internal/github/github_test.go`) — `httptest.NewServer` faking the GitHub API:

```go
package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBranchSHA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-pat" {
			t.Errorf("Authorization = %q, want Bearer test-pat", got)
		}
		switch r.URL.Path {
		case "/repos/eswan18/footstrike-api/branches/hae-cadence":
			w.Write([]byte(`{"name":"hae-cadence","commit":{"sha":"49a402ab12cd"}}`))
		case "/repos/eswan18/footstrike-api/branches/missing":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := NewWithBaseURL("eswan18", "test-pat", srv.URL)

	sha, err := c.BranchSHA(context.Background(), "footstrike-api", "hae-cadence")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sha != "49a402ab12cd" {
		t.Errorf("sha = %q, want 49a402ab12cd", sha)
	}

	_, err = c.BranchSHA(context.Background(), "footstrike-api", "missing")
	if !errors.Is(err, ErrNoBranch) {
		t.Errorf("expected ErrNoBranch, got %v", err)
	}
}

func TestBranchSHAServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := NewWithBaseURL("eswan18", "test-pat", srv.URL)
	_, err := c.BranchSHA(context.Background(), "footstrike-api", "main")
	if err == nil || errors.Is(err, ErrNoBranch) {
		t.Errorf("expected non-ErrNoBranch error, got %v", err)
	}
}
```

- [ ] **Step 2: Verify failure** — `go test ./internal/github/ -v` → compile error.

- [ ] **Step 3: Implement** (`internal/github/github.go`):

```go
// Package github looks up branch heads so the preview control plane can
// detect which repos carry a preview branch. Hand-rolled net/http (matching
// internal/auth/oidc.go's conventions) rather than a SDK dependency — one
// endpoint doesn't justify one.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const httpTimeout = 10 * time.Second

// ErrNoBranch reports that the repo has no branch by that name (GitHub 404).
var ErrNoBranch = errors.New("branch not found")

type Client interface {
	// BranchSHA returns the head commit SHA of repo's branch.
	// A missing branch is ErrNoBranch; other failures are opaque errors.
	BranchSHA(ctx context.Context, repo, branch string) (string, error)
}

type client struct {
	org     string
	token   string
	baseURL string
	http    *http.Client
}

func New(org, token string) Client {
	return NewWithBaseURL(org, token, "https://api.github.com")
}

// NewWithBaseURL exists as a test seam; production code uses New.
func NewWithBaseURL(org, token, baseURL string) Client {
	return &client{org: org, token: token, baseURL: baseURL, http: &http.Client{Timeout: httpTimeout}}
}

func (c *client) BranchSHA(ctx context.Context, repo, branch string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/branches/%s", c.baseURL, c.org, repo, branch)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", ErrNoBranch
	default:
		return "", fmt.Errorf("github branch lookup returned %d", resp.StatusCode)
	}
	var body struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.Commit.SHA == "" {
		return "", errors.New("github response missing commit sha")
	}
	return body.Commit.SHA, nil
}
```

- [ ] **Step 4: Verify pass** — `go test ./internal/github/ -v`.

- [ ] **Step 5: Gates + commit**

```bash
git add internal/github/
git commit -m "Add GitHub branch-lookup client"
```

---

### Task 4: Neon branch client

**Files:**
- Create: `internal/neon/neon.go`
- Test: `internal/neon/neon_test.go`

**Interfaces:**
- Produces:

```go
type Branch struct{ ID, Name string }
type Client interface {
	ListBranches(ctx context.Context, projectID string) ([]Branch, error)
	CreateBranch(ctx context.Context, projectID, name, parentID string) (Branch, error)
	DeleteBranch(ctx context.Context, projectID, branchID string) error
	ConnectionURI(ctx context.Context, projectID, branchID, database, role string) (string, error)
}
func New(apiKey string) Client
func NewWithBaseURL(apiKey, baseURL string) Client
```

3c's orchestration composes these: create-if-missing = `ListBranches` scan then `CreateBranch`; `DeleteBranch` on teardown; `ConnectionURI` feeds the copied secret's `DATABASE_URL` override, using the service's `NeonProjectRef` from Task 1. `parentID` empty string means "branch from the project's default branch" (omit `parent_id` from the request body).

**API-shape caveat (bindingly part of this task):** the endpoint paths and response shapes below follow the Neon API v2 reference as of planning. Before Step 3, fetch https://api-docs.neon.tech/reference/listprojectbranches, .../createprojectbranch, .../deleteprojectbranch, and .../getconnectionuri (WebFetch) and confirm paths, request bodies, and response envelopes. If they differ, adjust the implementation AND the test fixtures to the live shapes, and record the deltas in your report. Do not skip this.

- [ ] **Step 1: Failing tests** (`internal/neon/neon_test.go`):

```go
package neon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer(t *testing.T) (*httptest.Server, *http.ServeMux) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer neon-key" {
			t.Errorf("Authorization = %q, want Bearer neon-key", got)
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, mux
}

func TestListBranches(t *testing.T) {
	srv, mux := newTestServer(t)
	mux.HandleFunc("GET /projects/proj-abc/branches", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"branches":[{"id":"br-main-123","name":"main"},{"id":"br-prev-456","name":"preview-hae-cadence"}]}`))
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	got, err := c.ListBranches(context.Background(), "proj-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[1].ID != "br-prev-456" || got[1].Name != "preview-hae-cadence" {
		t.Errorf("branches = %+v", got)
	}
}

func TestCreateBranch(t *testing.T) {
	srv, mux := newTestServer(t)
	mux.HandleFunc("POST /projects/proj-abc/branches", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("bad body: %v", err)
		}
		branch, _ := body["branch"].(map[string]any)
		if branch["name"] != "preview-hae-cadence" {
			t.Errorf("branch.name = %v", branch["name"])
		}
		if _, hasParent := branch["parent_id"]; hasParent {
			t.Error("empty parentID must omit parent_id")
		}
		if _, hasEndpoints := body["endpoints"]; !hasEndpoints {
			t.Error("expected endpoints in create request (branch needs a compute to connect to)")
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"branch":{"id":"br-new-789","name":"preview-hae-cadence"}}`))
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	got, err := c.CreateBranch(context.Background(), "proj-abc", "preview-hae-cadence", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "br-new-789" {
		t.Errorf("branch = %+v", got)
	}
}

func TestDeleteBranch(t *testing.T) {
	srv, mux := newTestServer(t)
	called := false
	mux.HandleFunc("DELETE /projects/proj-abc/branches/br-prev-456", func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.Write([]byte(`{}`))
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	if err := c.DeleteBranch(context.Background(), "proj-abc", "br-prev-456"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("DELETE endpoint not hit")
	}
}

func TestConnectionURI(t *testing.T) {
	srv, mux := newTestServer(t)
	mux.HandleFunc("GET /projects/proj-abc/connection_uri", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("branch_id") != "br-prev-456" || q.Get("database_name") != "fitnessdb" || q.Get("role_name") != "fitness_owner" {
			t.Errorf("query = %v", q)
		}
		w.Write([]byte(`{"uri":"postgresql://fitness_owner:pw@ep-x.neon.tech/fitnessdb"}`))
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	uri, err := c.ConnectionURI(context.Background(), "proj-abc", "br-prev-456", "fitnessdb", "fitness_owner")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uri != "postgresql://fitness_owner:pw@ep-x.neon.tech/fitnessdb" {
		t.Errorf("uri = %q", uri)
	}
}

func TestErrorStatusSurfaces(t *testing.T) {
	srv, mux := newTestServer(t)
	mux.HandleFunc("GET /projects/proj-abc/branches", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	if _, err := c.ListBranches(context.Background(), "proj-abc"); err == nil {
		t.Error("expected error on 429")
	}
}
```

- [ ] **Step 2: Verify failure** — `go test ./internal/neon/ -v` → compile error.

- [ ] **Step 3: Verify live API shapes (see caveat above), then implement** (`internal/neon/neon.go`):

```go
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

type Branch struct {
	ID   string `json:"id"`
	Name string `json:"name"`
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
// okStatus is the exact status the endpoint returns on success.
func (c *client) do(ctx context.Context, method, path string, body any, okStatus int, out any) error {
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
	if resp.StatusCode != okStatus {
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
	err := c.do(ctx, "GET", "/projects/"+projectID+"/branches", nil, http.StatusOK, &body)
	if err != nil {
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
	err := c.do(ctx, "POST", "/projects/"+projectID+"/branches", req, http.StatusCreated, &body)
	if err != nil {
		return Branch{}, err
	}
	return body.Branch, nil
}

func (c *client) DeleteBranch(ctx context.Context, projectID, branchID string) error {
	return c.do(ctx, "DELETE", "/projects/"+projectID+"/branches/"+branchID, nil, http.StatusOK, nil)
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
	err := c.do(ctx, "GET", "/projects/"+projectID+"/connection_uri?"+q.Encode(), nil, http.StatusOK, &body)
	if err != nil {
		return "", err
	}
	return body.URI, nil
}
```

- [ ] **Step 4: Verify pass** — `go test ./internal/neon/ -v`.

- [ ] **Step 5: Gates + commit**

```bash
git add internal/neon/
git commit -m "Add Neon branch client for preview databases"
```

---

### Task 5: Cloud Build trigger-run and build-get

**Files:**
- Modify: `internal/gcb/gcb.go`
- Test: `internal/gcb/gcb_test.go` (append)
- Modify: `internal/web/handlers_test.go` (mechanical: `fakeBuilds` must satisfy the grown interface)

**Interfaces:**
- Produces, on `gcb.Client`:

```go
	// RunTrigger starts the named trigger against branch; returns the build ID.
	RunTrigger(ctx context.Context, triggerID, branch string) (string, error)
	// GetBuild fetches one build's current status by ID.
	GetBuild(ctx context.Context, buildID string) (BuildStatus, error)
```

3c's flow: resolve `{svc}-preview-build` IDs via the existing package-level `TriggerIDs`, `RunTrigger` per member service, poll `GetBuild` until `!InProgress()`. (Polling lives in 3c, not here.)

- [ ] **Step 1: Failing test for the pure part** (append to `internal/gcb/gcb_test.go`):

The SDK-touching bodies stay untested (package convention — `New`/`LatestBuilds` aren't tested either); the extractable pure logic is build-ID parsing from the long-running Operation metadata:

```go
func TestBuildIDFromOperation(t *testing.T) {
	cases := []struct {
		name    string
		meta    string
		want    string
		wantErr bool
	}{
		{"normal", `{"build":{"id":"abc-123"}}`, "abc-123", false},
		{"missing build", `{}`, "", true},
		{"empty id", `{"build":{"id":""}}`, "", true},
		{"malformed", `not-json`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildIDFromOperation([]byte(tc.meta))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("id = %q, want %q", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Verify failure** — `go test ./internal/gcb/ -run TestBuildIDFromOperation -v` → compile error.

- [ ] **Step 3: Implement** (append to `internal/gcb/gcb.go`; add `"encoding/json"` and `"errors"` to imports):

Add the two methods to the `Client` interface (after `LatestBuilds`), then:

```go
// buildIDFromOperation extracts the created build's ID from a trigger-run
// Operation's metadata (a cloudbuild.BuildOperationMetadata JSON blob).
func buildIDFromOperation(meta []byte) (string, error) {
	var m struct {
		Build struct {
			Id string `json:"id"`
		} `json:"build"`
	}
	if err := json.Unmarshal(meta, &m); err != nil {
		return "", err
	}
	if m.Build.Id == "" {
		return "", errors.New("operation metadata has no build id")
	}
	return m.Build.Id, nil
}

func (c *client) RunTrigger(ctx context.Context, triggerID, branch string) (string, error) {
	op, err := c.svc.Projects.Triggers.Run(c.project, triggerID,
		&cloudbuild.RepoSource{BranchName: branch}).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return buildIDFromOperation(op.Metadata)
}

func (c *client) GetBuild(ctx context.Context, buildID string) (BuildStatus, error) {
	b, err := c.svc.Projects.Builds.Get(c.project, buildID).Context(ctx).Do()
	if err != nil {
		return BuildStatus{}, err
	}
	return BuildStatus{
		Status:     b.Status,
		SHA:        b.Substitutions["SHORT_SHA"],
		LogURL:     b.LogUrl,
		StartTime:  parseTime(b.StartTime),
		FinishTime: parseTime(b.FinishTime),
	}, nil
}
```

Note: `op.Metadata` is `googleapi.RawMessage` (a `[]byte` alias), passed straight to `buildIDFromOperation`. If the compiler disagrees on the exact field type, adapt with a conversion — the pure function keeps taking `[]byte`.

- [ ] **Step 4: Fix the interface fake** (`internal/web/handlers_test.go`, `fakeBuilds`): add stub methods so it still satisfies `gcb.Client`:

```go
func (f *fakeBuilds) RunTrigger(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (f *fakeBuilds) GetBuild(_ context.Context, _ string) (gcb.BuildStatus, error) {
	return gcb.BuildStatus{}, nil
}
```

- [ ] **Step 5: Verify pass** — `go test ./internal/gcb/ ./internal/web/ -v` (gcb new tests green; web package still compiles and passes).

- [ ] **Step 6: Full gates + commit + PR**

Run: `go build ./... && go vet ./... && go test ./...`

```bash
git add internal/gcb/gcb.go internal/gcb/gcb_test.go internal/web/handlers_test.go
git commit -m "Add trigger-run and build-get to the Cloud Build client"
git push -u origin preview-control-plane-a
gh pr create --title "Preview control plane 3a: platform clients" \
  --body "Config vars, bearer-token middleware, GitHub branch client, Neon branch client, and Cloud Build trigger-run/build-get — the tested building blocks for the preview control plane (3b: state + read API; 3c: orchestration). No behavior change: nothing is wired into main.go yet.

🤖 Generated with [Claude Code](https://claude.com/claude-code)"
```

---

## Self-review notes

- **Spec coverage:** provides every client surface the spec's "Creation flow" and "Teardown" steps name (branch detection, trigger runs, build status, Neon branching, connection URIs, CLI bearer auth) — and nothing else; tarball fetch, kustomize rendering, namespace state, endpoints, and UI are deliberately 3b/3c.
- **No-behavior-change claim:** verified by construction — no `main.go` edits, no handler edits, only additive packages + a test-fake stub; the `gcb.Client` interface growth is the one compile-coupling, handled in Task 5.
- **Type consistency:** `NeonProjectRef` (Task 1) feeds `ConnectionURI(projectID, branchID, database, role)` (Task 4); `TriggerIDs` (existing) feeds `RunTrigger` (Task 5); `ErrNoBranch` (Task 3) is the membership sentinel 3c consumes.
- **Known risk:** Neon API shapes written from the v2 reference; Task 4 bindingly requires verifying against live docs before implementing, with deltas recorded.
