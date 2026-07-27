# Preview Control Plane 3b: State + Read Path Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make previews *visible*: define the namespace-as-record schema, add namespace listing to the kube client, expose `GET /api/previews` + `GET /api/previews/{tag}` as JSON (session- or bearer-authed), add a Previews tab to the UI, and wire the prod deployment's config/secrets. Creation/teardown is 3c.

**Architecture:** A preview IS its namespace (spec: no database). 3b defines the record schema — label `bifrost/preview=true` selects preview namespaces; annotations `bifrost/branch`, `bifrost/apps` (comma-separated member services), `bifrost/phase` (`creating|ready|failed`, written by 3c's orchestrator) carry what the cluster can't derive; everything else (age, pod health, URLs) is derived live in bifrost's established "read live state" style, reusing `kube.SummarizeHealth`. The API returns JSON (the repo's `json.NewEncoder` pattern), gated by a new either/or middleware: a Bearer header takes the bearer path (no session, no CSRF), anything else falls through to the existing session auth. The UI mirrors the Apps tab exactly (full handler + `tab-body` fragment + poll endpoint).

**Tech Stack:** Go 1.26, client-go (typed clientset + fake), html/template, existing 3a packages (`config.PreviewAPIToken` etc.).

**Repo/branch:** `~/Develop/ibormeith/bifrost`, branch `preview-control-plane-b` from up-to-date main; one PR.

**Spec:** `docs/superpowers/specs/2026-07-26-preview-environments-design.md` (Control plane, Preview anatomy). Prior art: plan 3a (merged) — its Interfaces blocks name what this plan consumes.

## Global Constraints

- Record schema exactly: namespace name `preview-<tag>`; label `bifrost/preview: "true"`; annotations `bifrost/branch`, `bifrost/apps`, `bifrost/phase`. 3c writes them; 3b only reads. Absent/empty phase renders as `unknown`.
- Preview URLs are `https://{app}-{tag}.preview.footstrike.run` — derived, never stored.
- Bearer-authed requests carry no session: no `SessionFromContext` deref, no CSRF on the API read path. The either/or middleware must route ANY request bearing an `Authorization: Bearer` header to bearer validation (a bad bearer token gets 401, never a login redirect).
- `handlers_test.go`'s `fakeKube` must grow the new kube methods (interface growth breaks it otherwise).
- Verification for every task: `go build ./... && go vet ./... && go test ./...` AND `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run ./...` (bifrost CI runs golangci-lint — errcheck bit us in 3a; check ALL error returns including `w.Write` in test fixtures: `_, _ =` them).
- No behavior change for anyone not using previews: with `PreviewAPIToken`/`PreviewServices` empty, the API routes 401/return-empty respectively and the tab shows an empty list.
- Opportunistic 3a-review cleanups allowed ONLY in files a task already touches: rename the shadowed `url` var if touching `internal/github` (not planned), add `WWW-Authenticate: Bearer` header on the new middleware's 401s (Task 2 — do this).

---

### Task 1: kube — namespace listing and lookup

**Files:**
- Create: `internal/kube/namespaces.go`
- Test: `internal/kube/namespaces_test.go`
- Modify: `internal/kube/client.go` (interface: two methods after `ListReplicaSets`)
- Modify: `internal/web/handlers_test.go` (mechanical `fakeKube` stubs)

**Interfaces:**
- Produces on `kube.Client`:

```go
	// ListNamespaces returns namespaces matching labelSelector ("" = all).
	ListNamespaces(ctx context.Context, labelSelector string) ([]NamespaceInfo, error)
	// GetNamespace fetches one namespace; found=false (no error) when absent.
	GetNamespace(ctx context.Context, name string) (NamespaceInfo, bool, error)
```

- And type (in `namespaces.go`):

```go
// NamespaceInfo is the slice of namespace state the preview control plane
// reads: a preview's record lives in its namespace's labels/annotations.
type NamespaceInfo struct {
	Name        string
	Labels      map[string]string
	Annotations map[string]string
	CreatedAt   time.Time
	Phase       string // "Active" | "Terminating"
}
```

- [ ] **Step 1: Failing tests** (`internal/kube/namespaces_test.go`, following `pods_test.go`'s fake-clientset pattern):

```go
package kube

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func previewNS(name, branch string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      map[string]string{"bifrost/preview": "true"},
			Annotations: map[string]string{"bifrost/branch": branch, "bifrost/apps": "footstrike-api,footstrike-dashboard", "bifrost/phase": "ready"},
		},
		Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	}
}

func TestListNamespacesFiltersByLabel(t *testing.T) {
	cs := fake.NewSimpleClientset(
		previewNS("preview-hae-cadence", "hae-cadence"),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "footstrike-api-staging"}},
	)
	c := &client{typed: cs}
	got, err := c.ListNamespaces(context.Background(), "bifrost/preview=true")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Name != "preview-hae-cadence" {
		t.Fatalf("namespaces = %+v, want just preview-hae-cadence", got)
	}
	ns := got[0]
	if ns.Annotations["bifrost/branch"] != "hae-cadence" || ns.Phase != "Active" || ns.CreatedAt.IsZero() && false {
		t.Errorf("record fields not carried through: %+v", ns)
	}
}

func TestGetNamespace(t *testing.T) {
	cs := fake.NewSimpleClientset(previewNS("preview-x", "x"))
	c := &client{typed: cs}

	ns, found, err := c.GetNamespace(context.Background(), "preview-x")
	if err != nil || !found {
		t.Fatalf("expected found, got found=%v err=%v", found, err)
	}
	if ns.Annotations["bifrost/phase"] != "ready" {
		t.Errorf("annotations = %v", ns.Annotations)
	}

	_, found, err = c.GetNamespace(context.Background(), "preview-missing")
	if err != nil {
		t.Fatalf("absent namespace must be (zero, false, nil), got err=%v", err)
	}
	if found {
		t.Error("expected found=false for absent namespace")
	}
}
```

(Note: `CreatedAt` from the fake clientset is the zero time — the `&& false` disables that sub-check; keep the expression exactly as written or drop the CreatedAt clause entirely, your choice, but do not assert non-zero CreatedAt against the fake.)

- [ ] **Step 2: Verify failure** — `go test ./internal/kube/ -run 'TestListNamespaces|TestGetNamespace' -v` → compile error.

- [ ] **Step 3: Implement** (`internal/kube/namespaces.go`):

```go
package kube

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
)

// NamespaceInfo is the slice of namespace state the preview control plane
// reads: a preview's record lives in its namespace's labels/annotations.
type NamespaceInfo struct {
	Name        string
	Labels      map[string]string
	Annotations map[string]string
	CreatedAt   time.Time
	Phase       string // "Active" | "Terminating"
}

func (c *client) ListNamespaces(ctx context.Context, labelSelector string) ([]NamespaceInfo, error) {
	list, err := c.typed.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}
	out := make([]NamespaceInfo, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, namespaceInfo(&list.Items[i]))
	}
	return out, nil
}

func (c *client) GetNamespace(ctx context.Context, name string) (NamespaceInfo, bool, error) {
	ns, err := c.typed.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return NamespaceInfo{}, false, nil
	}
	if err != nil {
		return NamespaceInfo{}, false, err
	}
	return namespaceInfo(ns), true, nil
}

func namespaceInfo(ns *corev1.Namespace) NamespaceInfo {
	return NamespaceInfo{
		Name:        ns.Name,
		Labels:      ns.Labels,
		Annotations: ns.Annotations,
		CreatedAt:   ns.CreationTimestamp.Time,
		Phase:       string(ns.Status.Phase),
	}
}
```

Add the two method signatures (with the doc comments from the Interfaces block) to the `Client` interface in `client.go` after `ListReplicaSets`.

- [ ] **Step 4: Grow `fakeKube`** (`internal/web/handlers_test.go` — mechanical, next to its other methods):

```go
func (f *fakeKube) ListNamespaces(_ context.Context, _ string) ([]kube.NamespaceInfo, error) {
	return f.namespaces, f.namespacesErr
}
func (f *fakeKube) GetNamespace(_ context.Context, name string) (kube.NamespaceInfo, bool, error) {
	for _, ns := range f.namespaces {
		if ns.Name == name {
			return ns, true, nil
		}
	}
	return kube.NamespaceInfo{}, false, f.namespacesErr
}
```

Add `namespaces []kube.NamespaceInfo` and `namespacesErr error` fields to the `fakeKube` struct.

- [ ] **Step 5: Verify pass + gates + commit**

`go test ./internal/kube/ ./internal/web/ -v` then the full gates (incl. golangci-lint).

```bash
git add internal/kube/ internal/web/handlers_test.go
git commit -m "Add namespace listing and lookup to the kube client"
```

---

### Task 2: auth — session-or-bearer middleware

**Files:**
- Create: `internal/auth/either.go`
- Test: `internal/auth/either_test.go`

**Interfaces:**
- Produces: `func RequireSessionOrBearer(sm *SessionManager, allowedEmail, loginPath, token string) func(http.Handler) http.Handler`. Routing rule: a request WITH an `Authorization: Bearer ...` header is judged solely as a bearer request (valid → through with no session in context; invalid/empty-configured-token → 401 + `WWW-Authenticate: Bearer`); a request WITHOUT one falls through to `RequireAuth(sm, allowedEmail, loginPath)` exactly as today.

- [ ] **Step 1: Failing tests** (`internal/auth/either_test.go`):

```go
package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequireSessionOrBearer(t *testing.T) {
	sm := NewSessionManager([]byte(strings.Repeat("k", 32)), 0)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if SessionFromContext(r.Context()) != nil {
			w.Header().Set("X-Had-Session", "yes")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler := RequireSessionOrBearer(sm, "me@example.com", "/auth/login", "s3cret")(next)

	t.Run("valid bearer passes with no session", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/previews", nil)
		req.Header.Set("Authorization", "Bearer s3cret")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
		if rec.Header().Get("X-Had-Session") != "" {
			t.Error("bearer request must carry no session in context")
		}
	})

	t.Run("bad bearer 401s and never redirects", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/previews", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("WWW-Authenticate = %q, want Bearer", got)
		}
	})

	t.Run("no auth header falls through to session redirect", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/previews", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303 login redirect", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/auth/login" {
			t.Errorf("Location = %q", loc)
		}
	})

	t.Run("empty configured token rejects bearer attempts", func(t *testing.T) {
		h := RequireSessionOrBearer(sm, "me@example.com", "/auth/login", "")(next)
		req := httptest.NewRequest("GET", "/api/previews", nil)
		req.Header.Set("Authorization", "Bearer anything")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
}
```

(Check `NewSessionManager`'s actual signature in `session.go` — if the second arg is a `time.Duration` TTL, pass `12*time.Hour` instead of `0`; adapt only that line.)

- [ ] **Step 2: Verify failure**, then **Step 3: Implement** (`internal/auth/either.go`):

```go
package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// RequireSessionOrBearer gates a route for both browsers (session cookie)
// and CLIs (static bearer token). Any request presenting an Authorization
// Bearer header is judged solely as a bearer request — a bad token gets a
// 401, never a login redirect — so misconfigured CLIs fail loudly. Bearer
// requests carry no session in context; handlers must tolerate a nil
// SessionFromContext. An empty configured token disables the bearer path.
func RequireSessionOrBearer(sm *SessionManager, allowedEmail, loginPath, token string) func(http.Handler) http.Handler {
	sessionAuth := RequireAuth(sm, allowedEmail, loginPath)
	return func(next http.Handler) http.Handler {
		sessionHandler := sessionAuth(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented, isBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !isBearer {
				sessionHandler.ServeHTTP(w, r)
				return
			}
			if token == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
```

- [ ] **Step 4: Verify pass + gates + commit**

```bash
git add internal/auth/either.go internal/auth/either_test.go
git commit -m "Add session-or-bearer middleware for the preview API"
```

---

### Task 3: web — preview record assembly

**Files:**
- Create: `internal/web/previews.go`
- Test: `internal/web/previews_test.go`

**Interfaces:**
- Produces (package-private, consumed by Tasks 4–5):

```go
const previewLabelSelector = "bifrost/preview=true"

type previewRecord struct {
	Tag       string            `json:"tag"`
	Branch    string            `json:"branch"`
	Apps      []string          `json:"apps"`
	Phase     string            `json:"phase"`  // creating|ready|failed|terminating|unknown (annotation, or Terminating from ns phase)
	Health    string            `json:"health"` // from kube.SummarizeHealth over the namespace's pods: healthy|degraded|crashlooping|unknown
	CreatedAt time.Time         `json:"createdAt"`
	URLs      map[string]string `json:"urls"` // app -> https://{app}-{tag}.preview.footstrike.run
}

func (h *Handlers) assemblePreviews(ctx context.Context) ([]previewRecord, error)   // list + per-ns pods health
func (h *Handlers) previewByTag(ctx context.Context, tag string) (previewRecord, bool, error)
func recordFromNamespace(ns kube.NamespaceInfo) previewRecord                        // pure: no health
```

- [ ] **Step 1: Failing tests** (`internal/web/previews_test.go`) — pure derivation via `recordFromNamespace`, and assembly via `fakeKube`:

```go
package web

import (
	"context"
	"testing"
	"time"

	"github.com/eswan18/bifrost/internal/kube"
)

func nsInfo(name, branch, apps, phase string) kube.NamespaceInfo {
	ann := map[string]string{}
	if branch != "" {
		ann["bifrost/branch"] = branch
	}
	if apps != "" {
		ann["bifrost/apps"] = apps
	}
	if phase != "" {
		ann["bifrost/phase"] = phase
	}
	return kube.NamespaceInfo{
		Name: name, Labels: map[string]string{"bifrost/preview": "true"},
		Annotations: ann, CreatedAt: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC), Phase: "Active",
	}
}

func TestRecordFromNamespace(t *testing.T) {
	r := recordFromNamespace(nsInfo("preview-hae-cadence", "hae-cadence", "footstrike-api,footstrike-dashboard", "ready"))
	if r.Tag != "hae-cadence" || r.Branch != "hae-cadence" || r.Phase != "ready" {
		t.Errorf("record = %+v", r)
	}
	if len(r.Apps) != 2 || r.Apps[0] != "footstrike-api" {
		t.Errorf("apps = %v", r.Apps)
	}
	if r.URLs["footstrike-api"] != "https://footstrike-api-hae-cadence.preview.footstrike.run" {
		t.Errorf("urls = %v", r.URLs)
	}
}

func TestRecordFromNamespaceDefaults(t *testing.T) {
	r := recordFromNamespace(nsInfo("preview-x", "", "", ""))
	if r.Phase != "unknown" {
		t.Errorf("absent phase must read unknown, got %q", r.Phase)
	}
	if len(r.Apps) != 0 {
		t.Errorf("absent apps must be empty, got %v", r.Apps)
	}
}

func TestRecordFromNamespaceTerminating(t *testing.T) {
	ns := nsInfo("preview-x", "x", "", "ready")
	ns.Phase = "Terminating"
	if r := recordFromNamespace(ns); r.Phase != "terminating" {
		t.Errorf("terminating namespace must override phase, got %q", r.Phase)
	}
}

func TestAssemblePreviews(t *testing.T) {
	fk := &fakeKube{namespaces: []kube.NamespaceInfo{
		nsInfo("preview-b", "b", "footstrike-api", "ready"),
		nsInfo("preview-a", "a", "footstrike-api", "creating"),
	}}
	h := &Handlers{Kube: fk}
	got, err := h.assemblePreviews(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0].Tag != "a" || got[1].Tag != "b" {
		t.Fatalf("want sorted by tag [a b], got %+v", got)
	}
}

func TestPreviewByTagMissing(t *testing.T) {
	h := &Handlers{Kube: &fakeKube{}}
	_, found, err := h.previewByTag(context.Background(), "nope")
	if err != nil || found {
		t.Fatalf("want (zero,false,nil), got found=%v err=%v", found, err)
	}
}
```

(If `fakeKube`'s zero value doesn't satisfy other required fields for construction, follow how other tests build it. `ListPods` on the fake returns its configured pods — with none configured, health derives to `unknown`; assert nothing stronger about Health in these tests.)

- [ ] **Step 2: Verify failure**, then **Step 3: Implement** (`internal/web/previews.go`):

```go
package web

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/eswan18/bifrost/internal/kube"
)

// previewLabelSelector selects preview namespaces — the namespace itself is
// the preview's record (labels/annotations written by the 3c orchestrator).
const previewLabelSelector = "bifrost/preview=true"

type previewRecord struct {
	Tag       string            `json:"tag"`
	Branch    string            `json:"branch"`
	Apps      []string          `json:"apps"`
	Phase     string            `json:"phase"`
	Health    string            `json:"health"`
	CreatedAt time.Time         `json:"createdAt"`
	URLs      map[string]string `json:"urls"`
}

// recordFromNamespace derives everything derivable without extra cluster
// calls; Health is filled separately by the assemblers.
func recordFromNamespace(ns kube.NamespaceInfo) previewRecord {
	tag := strings.TrimPrefix(ns.Name, "preview-")
	rec := previewRecord{
		Tag:       tag,
		Branch:    ns.Annotations["bifrost/branch"],
		Phase:     ns.Annotations["bifrost/phase"],
		CreatedAt: ns.CreatedAt,
		URLs:      map[string]string{},
	}
	if apps := ns.Annotations["bifrost/apps"]; apps != "" {
		rec.Apps = strings.Split(apps, ",")
	}
	if rec.Phase == "" {
		rec.Phase = "unknown"
	}
	if ns.Phase == "Terminating" {
		rec.Phase = "terminating"
	}
	for _, app := range rec.Apps {
		rec.URLs[app] = fmt.Sprintf("https://%s-%s.preview.footstrike.run", app, tag)
	}
	return rec
}

func (h *Handlers) assemblePreviews(ctx context.Context) ([]previewRecord, error) {
	namespaces, err := h.Kube.ListNamespaces(ctx, previewLabelSelector)
	if err != nil {
		return nil, err
	}
	records := make([]previewRecord, 0, len(namespaces))
	for _, ns := range namespaces {
		rec := recordFromNamespace(ns)
		rec.Health = h.previewHealth(ctx, ns.Name)
		records = append(records, rec)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Tag < records[j].Tag })
	return records, nil
}

func (h *Handlers) previewByTag(ctx context.Context, tag string) (previewRecord, bool, error) {
	ns, found, err := h.Kube.GetNamespace(ctx, "preview-"+tag)
	if err != nil || !found {
		return previewRecord{}, false, err
	}
	rec := recordFromNamespace(ns)
	rec.Health = h.previewHealth(ctx, ns.Name)
	return rec, true, nil
}

// previewHealth summarizes pod health namespace-wide; errors degrade to
// "unknown" rather than failing the read (consistent with how the fleet
// view tolerates partial reads).
func (h *Handlers) previewHealth(ctx context.Context, namespace string) string {
	pods, err := h.Kube.ListPods(ctx, namespace)
	if err != nil {
		return "unknown"
	}
	return strings.ToLower(string(kube.SummarizeHealth(pods).State))
}
```

(Adapt the last line to `SummarizeHealth`'s real return shape — read `internal/kube/health.go` first; if `HealthState` is already a string-like type with values `Healthy` etc., lowercase it as shown; if the summary struct differs, extract the state equivalently.)

- [ ] **Step 4: Verify pass + gates + commit**

```bash
git add internal/web/previews.go internal/web/previews_test.go
git commit -m "Assemble preview records from namespace state"
```

---

### Task 4: API endpoints + wiring

**Files:**
- Modify: `internal/web/previews.go` (two handlers appended)
- Test: `internal/web/previews_test.go` (append handler tests)
- Modify: `cmd/bifrost/main.go` (middleware + two routes)

**Interfaces:**
- Produces routes: `GET /api/previews` → `{"previews":[...]}`; `GET /api/previews/{tag}` → single record or 404 `{"error":"unknown preview"}`. Both behind `RequireSessionOrBearer`. 3c adds POST/DELETE beside them.

- [ ] **Step 1: Failing handler tests** (append to `previews_test.go`):

```go
func TestPreviewsListJSON(t *testing.T) {
	fk := &fakeKube{namespaces: []kube.NamespaceInfo{nsInfo("preview-a", "a", "footstrike-api", "ready")}}
	h := &Handlers{Kube: fk}
	req := httptest.NewRequest("GET", "/api/previews", nil)
	rec := httptest.NewRecorder()
	h.PreviewsListJSON(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var body struct {
		Previews []previewRecord `json:"previews"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Previews) != 1 || body.Previews[0].Tag != "a" {
		t.Errorf("body = %+v", body)
	}
}

func TestPreviewJSONNotFound(t *testing.T) {
	h := &Handlers{Kube: &fakeKube{}}
	req := httptest.NewRequest("GET", "/api/previews/nope", nil)
	req.SetPathValue("tag", "nope")
	rec := httptest.NewRecorder()
	h.PreviewJSON(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestPreviewsListJSONKubeError(t *testing.T) {
	h := &Handlers{Kube: &fakeKube{namespacesErr: errors.New("boom")}}
	req := httptest.NewRequest("GET", "/api/previews", nil)
	rec := httptest.NewRecorder()
	h.PreviewsListJSON(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
```

(Add `encoding/json`, `errors`, `net/http`, `net/http/httptest` to the test imports as needed.)

- [ ] **Step 2: Verify failure**, then **Step 3: Implement** (append to `previews.go`; add `"encoding/json"`, `"log/slog"`, `"net/http"` imports):

```go
// PreviewsListJSON serves GET /api/previews for both the UI's consumers and
// the ib CLI (bearer-authed — may carry no session; no SessionFromContext).
func (h *Handlers) PreviewsListJSON(w http.ResponseWriter, r *http.Request) {
	records, err := h.assemblePreviews(r.Context())
	if err != nil {
		slog.Error("list previews failed", "err", err)
		http.Error(w, "list previews failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"previews": records})
}

// PreviewJSON serves GET /api/previews/{tag}.
func (h *Handlers) PreviewJSON(w http.ResponseWriter, r *http.Request) {
	rec, found, err := h.previewByTag(r.Context(), r.PathValue("tag"))
	if err != nil {
		slog.Error("get preview failed", "err", err)
		http.Error(w, "get preview failed", http.StatusInternalServerError)
		return
	}
	if !found {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "unknown preview"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}
```

- [ ] **Step 4: Wire routes** (`cmd/bifrost/main.go`): after the `requireAuth :=` line add:

```go
	requirePreviewAuth := auth.RequireSessionOrBearer(sm, cfg.AllowedEmail, "/auth/login", cfg.PreviewAPIToken)
```

and after the `/services/{name}/rollback` route:

```go
	mux.Handle("GET /api/previews", requirePreviewAuth(http.HandlerFunc(webH.PreviewsListJSON)))
	mux.Handle("GET /api/previews/{tag}", requirePreviewAuth(http.HandlerFunc(webH.PreviewJSON)))
```

- [ ] **Step 5: Verify pass + gates + commit**

```bash
git add internal/web/previews.go internal/web/previews_test.go cmd/bifrost/main.go
git commit -m "Serve preview records over /api/previews"
```

---

### Task 5: Previews UI tab

**Files:**
- Create: `templates/previews.html`
- Modify: `templates/base.html` (tabs block: one line)
- Modify: `internal/web/handlers.go` (`pageVM` fields, `dashboardVM` tab handling if needed, two handlers)
- Modify: `cmd/bifrost/main.go` (two routes)
- Test: `internal/web/handlers_test.go` or `render_smoke_test.go` pattern (render smoke for the new page)

**Interfaces:**
- Produces routes `GET /previews` + `GET /partial/previews` (session-authed like the other tabs) and `pageVM` fields `Previews []previewRecord`, `PreviewCount int`.

- [ ] **Step 1: Read the exemplars** — `templates/apps.html` (structure), `render_smoke_test.go` (how pages are smoke-rendered), `handlers.go`'s `Apps`/`AppsFragment` + `dashboardVM`.

- [ ] **Step 2: Template** (`templates/previews.html`) — match the repo's existing table/card idiom from `apps.html` (reuse its CSS classes; introduce no new CSS):

```html
{{define "tab-body"}}
<div class="previews">
  {{if not .Previews}}
  <p class="empty-state">No preview environments. Create one with <code>ib preview up &lt;branch&gt;</code>.</p>
  {{else}}
  <table class="fleet">
    <thead>
      <tr><th>Preview</th><th>Branch</th><th>Apps</th><th>Phase</th><th>Health</th><th>Age</th></tr>
    </thead>
    <tbody>
      {{range .Previews}}
      <tr>
        <td>{{.Tag}}</td>
        <td><code>{{.Branch}}</code></td>
        <td>{{range $app, $url := .URLs}}<a href="{{$url}}" target="_blank">{{$app}}</a> {{end}}</td>
        <td><span class="badge">{{.Phase}}</span></td>
        <td><span class="badge">{{.Health}}</span></td>
        <td>{{reltime .CreatedAt}}</td>
      </tr>
      {{end}}
    </tbody>
  </table>
  {{end}}
</div>
{{end}}
```

(Adapt table/badge class names to what `apps.html` actually uses — the structure above is the requirement, the classes must be the repo's. `reltime` is a registered template func.)

- [ ] **Step 3: Nav + VM + handlers**

`base.html` tabs block, after the Jobs link:

```html
  <a href="/previews" class="tab{{if eq .Tab "previews"}} active{{end}}">Previews<span class="tab-count">{{.PreviewCount}}</span></a>
```

`handlers.go`: add to `pageVM`: `Previews []previewRecord` and `PreviewCount int`. Add handlers following the Apps pair exactly:

```go
func (h *Handlers) Previews(w http.ResponseWriter, r *http.Request) {
	h.previewsPage(w, r, true)
}

func (h *Handlers) PreviewsFragment(w http.ResponseWriter, r *http.Request) {
	h.previewsPage(w, r, false)
}

func (h *Handlers) previewsPage(w http.ResponseWriter, r *http.Request, full bool) {
	records, err := h.assemblePreviews(r.Context())
	if err != nil {
		slog.Error("assemble previews failed", "err", err)
		records = nil
	}
	f := h.assembleFleet(r.Context())
	vm := h.dashboardVM(r, "previews", "/partial/previews", f)
	vm.Previews = records
	vm.PreviewCount = len(records)
	if full {
		vm.Flash = TakeFlash(w, r)
		h.render(w, "previews", vm)
		return
	}
	h.renderNamed(w, "previews", "tab-body", vm)
}
```

**Important adaptation note:** `dashboardVM` computes tab counts from the fleet — check whether `PreviewCount` must be set on EVERY page's VM for the nav badge to render on other tabs. If `dashboardVM` is where `AppCount`/`JobCount` get filled, add the preview count there (one `assemblePreviews` call... which costs a namespace list on every page render — acceptable; alternatively render the Previews tab without a count on other pages by making the count span conditional: `{{if .PreviewCount}}`. Choose the conditional-span approach — cheaper and honest — and note it in your report.)

`main.go`: add beside the other tab routes:

```go
	mux.Handle("GET /previews", requireAuth(http.HandlerFunc(webH.Previews)))
	mux.Handle("GET /partial/previews", requireAuth(http.HandlerFunc(webH.PreviewsFragment)))
```

- [ ] **Step 4: Render smoke test** — follow `render_smoke_test.go`'s existing pattern to render the `previews` page with a non-empty `[]previewRecord` and assert no template error (and that the output contains the tag string).

- [ ] **Step 5: Verify + gates + commit**

```bash
git add templates/ internal/web/ cmd/bifrost/main.go
git commit -m "Add Previews tab"
```

---

### Task 6: prod deployment wiring + PR

**Files:**
- Modify: `k8s/prod/secret-provider-class.yaml`
- Modify: `k8s/prod/configmap-env.yaml`

**Interfaces:**
- Consumes: the `bifrost_prod_*` Secret Manager containers (created by the parallel provisioning track — may not exist yet; that's fine, this is manifest-only and deploys after both merge).

- [ ] **Step 1: SecretProviderClass** — add to `k8s/prod/secret-provider-class.yaml`, following the file's exact existing structure (three new `secrets:` entries + three new `secretObjects.data` entries):

```yaml
      - resourceName: "projects/ethans-services/secrets/bifrost_prod_github_token/versions/latest"
        path: "github_token"
      - resourceName: "projects/ethans-services/secrets/bifrost_prod_neon_api_key/versions/latest"
        path: "neon_api_key"
      - resourceName: "projects/ethans-services/secrets/bifrost_prod_preview_api_token/versions/latest"
        path: "preview_api_token"
```

```yaml
        - objectName: github_token
          key: GITHUB_TOKEN
        - objectName: neon_api_key
          key: NEON_API_KEY
        - objectName: preview_api_token
          key: PREVIEW_API_TOKEN
```

- [ ] **Step 2: ConfigMap** (`k8s/prod/configmap-env.yaml`) — add:

```yaml
  PREVIEW_SERVICES: "footstrike-api,footstrike-dashboard,identity"
  NEON_PROJECTS: "footstrike-api=<API-NEON-PROJECT-ID>/<db>/<role>,identity=<IDENTITY-NEON-PROJECT-ID>/<db>/<role>"
```

**The Neon project IDs/db/role values are not in any repo** — report this step as DONE_WITH_CONCERNS with placeholder values exactly as above and flag that the controller/user must fill real values before the prod deploy (they're visible in the Neon console; db/role are the ones in each service's DATABASE_URL). Do NOT invent values.

- [ ] **Step 3: Gates, commit, push, PR**

Full gates one more time on the whole branch, then:

```bash
git add k8s/prod/
git commit -m "Wire preview config and secrets into the prod deployment"
git push -u origin preview-control-plane-b
gh pr create --title "Preview control plane 3b: state model, read API, Previews tab" \
  --body "Namespace-as-record schema (bifrost/preview label + branch/apps/phase annotations), kube namespace listing, GET /api/previews[/{tag}] JSON behind session-or-bearer auth, a Previews UI tab, and prod deployment wiring for the preview secrets/config. Creation/teardown is 3c.

🤖 Generated with [Claude Code](https://claude.com/claude-code)"
```

---

## Self-review notes

- **Spec coverage:** the spec's read path (`GET /api/previews`, `GET /api/previews/{tag}`), state model (namespace labels/annotations), UI ("a Previews tab — list with age/phase/links"), and bearer auth land here; POST/DELETE, orchestration, RBAC, and the deferred `/config.js` no-cache hardening are 3c.
- **Schema is 3b's to define, 3c's to write** — recorded in `previewLabelSelector`/`recordFromNamespace` doc comments; 3c must use exactly these keys.
- **Adaptation points are named, not silent:** `SummarizeHealth`'s shape, `NewSessionManager`'s signature, `dashboardVM` count plumbing, apps.html CSS classes, Neon project IDs (DONE_WITH_CONCERNS path).
- **Type consistency:** `previewRecord` JSON tags are the API contract `ib preview list` (plan 4b) will parse; `RequireSessionOrBearer` signature matches Task 4's wiring; fakeKube growth in Task 1 precedes every consumer.
