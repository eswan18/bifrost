# Preview Control Plane 3c: Orchestration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make previews real: `POST /api/previews {"branch": ...}` detects member repos, builds preview images, branches Neon databases, renders each member's manifests with a generated preview overlay, and applies them into a `preview-<tag>` namespace; `DELETE /api/previews/{tag}` tears everything down. Idempotent re-`POST` updates in place.

**Architecture:** The orchestrator (`internal/preview`) composes the 3a clients and new primitives: a GitHub tarball fetch (`k8s/` subtree at a branch), an embedded-kustomize renderer (in-memory fs: fetched `k8s/base` + a generated overlay), and kube write methods (ensure-namespace, server-side apply, secret copy, namespace delete). Env plumbing is deliberately service-agnostic, based on verified facts: the CSI `/mnt/secrets` mount is never read by app code (it exists only to sync a Secret), so the preview patch strips CSI volumes and SA names, and wires `envFrom: [base configmap, generated preview configmap, copied staging Secret]` — the staging Secret's keys are already env-var-named. Preview-specific values = the staging overlay's `configmap-env.yaml` data (fetched from the same tarball) with URL keys overridden. State transitions live on the namespace annotations 3b reads (`bifrost/phase`: `creating` → `ready`|`failed`), written by an async goroutine; re-running `POST` is the recovery path.

**Tech Stack:** Go 1.26; new deps `sigs.k8s.io/kustomize/api` + `sigs.k8s.io/kustomize/kyaml` (embedded krusty build) — the only new go.mod entries allowed; existing client-go dynamic client + RESTMapper for SSA; archive/tar+gzip stdlib for the tarball.

**Repo/branch:** `~/Develop/ibormeith/bifrost`, branch `preview-control-plane-c`; one PR.

**Spec:** `docs/superpowers/specs/2026-07-26-preview-environments-design.md`. 3b's schema is binding: label `bifrost/preview=true`, annotations `bifrost/branch`, `bifrost/apps` (CLEAN comma list — no trailing/empty segments), `bifrost/phase` ∈ `creating|ready|failed`; namespace `preview-<tag>` (`web.previewNSPrefix`).

## Global Constraints

- Gates for every task: `go build ./... && go vet ./... && go test ./... && gofmt -l .` (empty) AND `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run ./...` — all green.
- Tag = DNS-safe slug of the branch: lowercase, `[a-z0-9-]`, `/`→`-`, other invalid chars dropped, max 30 chars, no leading/trailing `-`. One implementation (`preview.TagForBranch`), used everywhere.
- Preview hostnames: `{app}-{tag}.preview.footstrike.run`. Internal DNS: `{app}.preview-{tag}.svc.cluster.local`.
- Preview deployments: `replicas: 1`; CronJobs `suspend: true`; NO `serviceAccountName`; NO CSI volumes/volumeMounts; images overridden to `preview-{shortSHA}` via kustomize `images:`.
- Copied Secrets: staging Secret data verbatim EXCEPT `DATABASE_URL` replaced with the Neon branch URI (services with a `NeonProjectRef` only). Never log secret data; error messages carry names/keys, never values.
- The generated preview ConfigMap starts from the fetched staging `configmap-env.yaml` data with overrides (per-service map in Task 4); dashboard (no staging configmap) gets a synthesized one (`APP_*` keys). All three `APP_*` vars are MANDATORY for dashboard previews — fail creation if unresolvable (spec, binding).
- Mutating endpoints accept bearer auth, or session auth with a valid `X-CSRF-Token` header; a bearer request never touches session/CSRF code paths.
- Concurrency: one orchestration goroutine per tag at a time (in-process mutex map); a `POST` for a tag mid-creation returns 409.
- Library-API caveat (bindingly part of Tasks 2–3): the krusty/kyaml and tarball code below was written from the kustomize API reference; verify against pkg.go.dev (WebFetch) before implementing, adapt AND record deltas in your report.

---

### Task 1: Branch slug + GitHub tarball fetch

**Files:**
- Create: `internal/preview/tag.go`, `internal/preview/tag_test.go`
- Modify: `internal/github/github.go` (+`FetchK8s`), `internal/github/github_test.go`

**Interfaces (produced):**

```go
// internal/preview
func TagForBranch(branch string) string

// internal/github — added to Client
// FetchK8s downloads repo's tarball at ref and returns the k8s/ subtree as
// path→content, paths relative to k8s/ (e.g. "base/deployment.yaml").
FetchK8s(ctx context.Context, repo, ref string) (map[string][]byte, error)
```

- [ ] **Step 1: Failing tests.** `tag_test.go` table: `("hae-cadence","hae-cadence")`, `("feat/preview API","feat-preview-api")`, `("Feat_X","feat-x")` (underscore dropped or mapped—pick `-`), `("--weird--","weird")`, 40-char branch → 30-char tag with no trailing `-`, `("änderung","nderung")`. For `FetchK8s`: httptest server serving a gzipped tar (build with `archive/tar` in the test) containing `repo-sha1234/k8s/base/deployment.yaml` and `repo-sha1234/README.md`; assert the map has key `base/deployment.yaml`, correct content, no README, and the request path is `/repos/eswan18/{repo}/tarball/{ref}` with auth header; 404 → `ErrNoBranch`.
- [ ] **Step 2–3: Implement.** `TagForBranch`: lowercase, map `/`,`_`,space→`-`, drop other non-`[a-z0-9-]`, collapse `--`, trim `-`, cap 30, trim `-` again. `FetchK8s`: GET `{base}/repos/{org}/{repo}/tarball/{ref}` (follow redirects — default client does), 200→`gzip.NewReader`→`tar.Reader` loop; strip the first path segment (GitHub prepends `{org}-{repo}-{sha}/`); keep entries under `k8s/` (typeflag regular file only); return map keyed by the path after `k8s/`. 404→`ErrNoBranch`; cap total extracted bytes at 5MB (`io.LimitReader`) with a clear error.
- [ ] **Step 4: Gates + commit** — "Add branch slugging and k8s tarball fetch".

---

### Task 2: Renderer — generated overlay + embedded kustomize

**Files:**
- Create: `internal/preview/render.go`, `internal/preview/render_test.go`, `internal/preview/testdata/` (fixture: a copy of footstrike-api's real base files + staging configmap-env.yaml, and dashboard's base)

**Interfaces (produced):**

```go
// RenderInput is one member service's render request.
type RenderInput struct {
	Service   string            // e.g. "footstrike-api"
	Tag       string
	ShortSHA  string            // preview image tag suffix
	K8sFiles  map[string][]byte // from github.FetchK8s
	EnvConfig map[string]string // final preview configmap data (Task 4 computes)
	SecretName string           // "" = service has no secret (dashboard)
}

// Render builds base + generated overlay and returns the objects to apply.
func Render(in RenderInput) ([]*unstructured.Unstructured, error)
```

- [ ] **Step 1: Failing tests** against the real fixture manifests (golden assertions, not string snapshots): rendering footstrike-api with EnvConfig + SecretName must yield objects where — every object's namespace is unset-or-`preview-<tag>` (kustomize `namespace:` sets it; assert it IS set on all); Deployment has replicas 1, no `serviceAccountName`, no volumes/volumeMounts, container image `...footstrike-api:preview-abc1234`, and envFrom exactly [`footstrike-api-config`, `footstrike-api-preview-env`, secretRef `footstrike-api-preview-secrets`]; the staging patch's per-var `env` secretKeyRefs are ABSENT (we do not fetch the staging deployment-patch at all — base has no `env`); CronJob has `suspend: true` and the preview image; the generated ConfigMap `footstrike-api-preview-env` carries EnvConfig verbatim; an Ingress exists with host `footstrike-api-<tag>.preview.footstrike.run`, ingressClassName nginx, tls secretName `preview-footstrike-run-tls`, backend service `footstrike-api:80`. Dashboard render: no Secret envFrom, no cronjob, image `...:preview-abc1234`.
- [ ] **Step 2–3: Implement.** Build an in-memory kustomize fs (`filesys.MakeFsInMemory()`): write fetched `base/` files under `/base/`, then generate `/overlay/`:
  - `kustomization.yaml`: `namespace: preview-<tag>`; `resources: [../base, configmap.yaml, ingress.yaml]`; `images: [{name: us-central1-docker.pkg.dev/ethans-services/containers/<svc>, newTag: preview-<sha>}]`; `patches: [{path: deployment-patch.yaml}]` plus, when the base has a CronJob (detect by scanning fetched files for `kind: CronJob`), `{path: cronjob-patch.yaml}`.
  - `deployment-patch.yaml` (strategic merge): replicas 1; container (name = service name) `envFrom` per the constraint (omit secretRef when `SecretName==""`, omit configMapRef `<svc>-config` when the base has no configmap — detect by scanning for the name); and a JSON6902-style delete for the CSI volume is NOT needed — instead the strategic-merge patch sets `volumes: []`?? — NO: strategic merge replaces lists only with `$patch: replace`. Use `patchesJson6902`/`patches` with a JSON patch for the two deletions: `[{"op":"remove","path":"/spec/template/spec/volumes"},{"op":"remove","path":"/spec/template/spec/containers/0/volumeMounts"}]`, applied only when the base declares volumes (scan). Dashboard has none — skip.
  - `cronjob-patch.yaml`: the `suspend: true` strategic merge from the research.
  - `configmap.yaml` (`<svc>-preview-env`) and `ingress.yaml` (host/tls per constraints, cert secret `preview-footstrike-run-tls`).
  Then `krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(fs, "/overlay")` → `ResMap` → for each resource `.Map()`/YAML → `unstructured.Unstructured`. (Verify exact krusty/resmap method names per the Library-API caveat.)
- [ ] **Step 4: Gates + commit** — "Render preview manifests with a generated kustomize overlay".

---

### Task 3: Kube write primitives

**Files:**
- Create: `internal/kube/apply.go`, `internal/kube/apply_test.go`
- Modify: `internal/kube/client.go` (interface additions), `internal/web/handlers_test.go` (fakeKube stubs)

**Interfaces (produced, on `kube.Client`):**

```go
	// EnsureNamespace creates or updates the namespace with exactly these
	// labels/annotations merged onto whatever exists.
	EnsureNamespace(ctx context.Context, name string, labels, annotations map[string]string) error
	// AnnotateNamespace merges annotations onto an existing namespace.
	AnnotateNamespace(ctx context.Context, name string, annotations map[string]string) error
	// ApplyObjects server-side-applies rendered objects (fieldManager "bifrost-preview").
	ApplyObjects(ctx context.Context, objs []*unstructured.Unstructured) error
	// CopySecret reads srcNS/srcName and creates/updates dstNS/dstName with
	// its data, applying overrides (nil value = keep source).
	CopySecret(ctx context.Context, srcNS, srcName, dstNS, dstName string, overrides map[string][]byte) error
	// DeleteNamespace deletes; absent namespace is not an error.
	DeleteNamespace(ctx context.Context, name string) error
```

- [ ] **Steps: TDD with fakes.** Typed-fake tests for Ensure/Annotate/Copy/Delete (fake.NewSimpleClientset patterns as in the repo). `ApplyObjects` uses the dynamic client + a RESTMapper: implement with `restmapper.GetAPIGroupResources`+`NewDiscoveryRESTMapper` resolved lazily at first apply (cache on client); apply via `dyn.Resource(mapping.Resource).Namespace(ns).Apply(ctx, name, obj, metav1.ApplyOptions{FieldManager: "bifrost-preview", Force: true})`. Test with `dynamicfake.NewSimpleDynamicClient` + a hand-built RESTMapper covering Deployment/Service/ConfigMap/CronJob/Ingress (fake dynamic clients don't support SSA `Apply` — if so, fall back in tests to asserting via a seam: extract `applyOne(mapping, obj)` and test mapping resolution + namespace handling with create/update semantics on the fake; record the delta per the Library-API caveat). fakeKube in web tests gets no-op stubs for all five.
- [ ] **Gates + commit** — "Add kube write primitives for preview orchestration".

---

### Task 4: Orchestrator

**Files:**
- Create: `internal/preview/orchestrator.go`, `internal/preview/orchestrator_test.go`, `internal/preview/envconfig.go`, `internal/preview/envconfig_test.go`
- Modify: `internal/config/config.go` (+`PreviewOAuthClientID string` from `PREVIEW_OAUTH_CLIENT_ID`, optional), `internal/config/config_test.go`

**Interfaces (produced):**

```go
type Orchestrator struct {
	Cfg    *config.Config
	Kube   kube.Client
	GitHub github.Client
	Neon   neon.Client
	Builds gcb.Client
	TriggerIDs map[string]string // {svc}-preview-build → id
}
// Up runs the full creation flow; returns the tag immediately-known errors,
// otherwise runs to completion (caller decides sync vs goroutine).
func (o *Orchestrator) Up(ctx context.Context, branch string) error
func (o *Orchestrator) Down(ctx context.Context, tag string) error
// Busy reports whether an Up/Down for tag is in flight (mutex map).
func (o *Orchestrator) Busy(tag string) bool
```

**Flow of `Up` (each numbered stage updates `bifrost/phase` or fails the whole run to `failed` with `bifrost/error: <msg>` annotation):**
1. `tag := TagForBranch(branch)`; membership: `BranchSHA` per `Cfg.PreviewServices` (`ErrNoBranch` → skip; other error → abort). Zero members → error. Dashboard member without all three APP_* resolvable → error (the mandatory-triple rule; APP_API_URL/IDENTITY_URL derive from membership+URLs, APP_OAUTH_CLIENT_ID from `Cfg.PreviewOAuthClientID` — empty → error).
2. `EnsureNamespace("preview-"+tag, {bifrost/preview: true}, {bifrost/branch, bifrost/apps: clean comma join, bifrost/phase: creating})`.
3. Builds: for each member, skip if image `preview-<shortSHA>` already exists — check via `Builds.GetBuild`? No: simplest skip-check is attempted-and-recorded; v1 ALWAYS runs the trigger (`RunTrigger(TriggerIDs[svc+"-preview-build"], branch)`) and polls `GetBuild` every 10s until terminal (SUCCESS → continue; FAILURE/TIMEOUT etc. → fail run). Record shortSHA per member from the build's `SHA`.
4. Neon: for members with `NeonProjects[svc]`: ensure branch `preview-<tag>` (ListBranches scan → CreateBranch), `ConnectionURI` with the ref's db/role.
5. Secrets: for those members, `CopySecret(svc+"-staging", svc+"-staging-secrets", ns, svc+"-preview-secrets", {"DATABASE_URL": uri})`. Copy the wildcard cert: `CopySecret("previews", "preview-footstrike-run-tls", ns, "preview-footstrike-run-tls", nil)`.
6. Env config (envconfig.go): fetch staging `configmap-env.yaml` from the member's tarball (parse YAML → data map; absent file → empty map), then apply overrides — footstrike-api: `ENV: preview`, `PUBLIC_API_BASE_URL/PUBLIC_DASHBOARD_BASE_URL` → preview URLs, and identity-aware `IDENTITY_PROVIDER_URL`/`JWT_ISSUER` (identity in members → internal preview DNS / preview identity external URL; else staging values pass through). identity: `JWT_ISSUER` → its preview URL when identity is a member. dashboard: synthesized `{APP_API_URL, APP_IDENTITY_URL, APP_OAUTH_CLIENT_ID}`. Pure function per service: `envConfigFor(svc, tag string, members []string, stagingData map[string]string, cfg *config.Config) (map[string]string, error)` — heavily table-tested (both with and without identity in the preview).
7. Render (Task 2) + `ApplyObjects` per member.
8. Phase → `ready`.

`Down`: `DeleteNamespace`, then for every service with a NeonProjectRef, delete Neon branch `preview-<tag>` if present (ListBranches scan). Best-effort: collect errors, return joined.

- [ ] **TDD:** orchestrator tests with hand-fakes for all five dependencies (in-package fake structs implementing the client interfaces), covering: happy path two-member flow (phase transitions asserted via fake kube's recorded annotations), build failure → phase failed + bifrost/error set, dashboard-without-client-id error, no-members error, Down best-effort (neon delete error still deletes namespace, error returned), Busy-mutex behavior. envconfig tests per the table above.
- [ ] **Gates + commit** — "Add the preview orchestrator".

---

### Task 5: Mutating API endpoints + wiring

**Files:**
- Create: `internal/web/previews_mutate.go`, `internal/web/previews_mutate_test.go`
- Modify: `cmd/bifrost/main.go` (construct github/neon clients gated on non-empty tokens per the "empty disables" contract; resolve `{svc}-preview-build` trigger IDs from the existing `TriggerIDs` fetch; build `*preview.Orchestrator`; two routes), `internal/web/handlers.go` (`Orch` field)

**Interfaces (produced):**
- `POST /api/previews` body `{"branch": "..."}` → 202 `{"tag": "...", "phase": "creating"}` (orchestration continues in a goroutine; errors land in namespace annotations); 400 empty branch; 409 `Busy(tag)`; 503 when the orchestrator is nil (preview config absent).
- `DELETE /api/previews/{tag}` → 202 `{"tag": ..., "deleting": true}`; 409 busy; 404 unknown tag.
- Auth: both behind `requirePreviewAuth` PLUS the mutation guard: if a session is present (`auth.SessionFromContext != nil`), require header `X-CSRF-Token` valid per `auth.VerifyCSRF`; bearer requests (nil session) skip CSRF. Never call session code before the nil check.
- Goroutine context: `context.Background()` with 30-min timeout (NOT the request context — the request returns immediately); log completion/failure via slog.

- [ ] **TDD:** handler tests with a fake orchestrator interface (define `orchestration` interface in web with `Up/Down/Busy` so the fake is trivial): 202 shape, 400/409/404/503 paths, CSRF branch (session-context request without header → 403; bearer-style request with nil session → no CSRF demanded). main.go wiring compiles with clients gated (`cfg.GitHubToken != ""` etc.).
- [ ] **Gates + commit** — "Expose preview create and teardown over the API".

---

### Task 6: RBAC write verbs + deferred dashboard hardenings

**Files:**
- Modify: `k8s/prod/rbac.yaml` (bifrost repo — new ClusterRole+Binding `bifrost-prod-preview-orchestrator`: namespaces create/patch/delete; secrets get in `*-staging`/`previews` (cluster-scoped role — namespace scoping via app logic, consistent with existing pattern comments); secrets/configmaps/services/deployments/cronjobs/ingresses create/patch in any namespace (SSA needs patch+create); events none). Staging rbac untouched (staging bifrost never orchestrates).
- Modify (footstrike-dashboard repo, branch `preview-hardenings`, separate small PR): `nginx.conf` (add `location = /config.js { add_header Cache-Control "no-cache, must-revalidate"; }`), `docker-entrypoint.d/90-app-config.sh` (warn to stderr when SOME but not ALL of the three APP_* are set). These are the two spec-deferred hardenings; they ride along in this plan because previews now actually deploy.

- [ ] **Steps:** yaml edits per above; for the dashboard: shell edit + a docker build/run verification like plan 2 Task 2's (config.js no-cache header via curl -I; partial-env warning via docker logs). Commit each repo on its branch; open the dashboard PR; bifrost changes commit to this plan's branch.
- [ ] **Gates + commits** — "Grant the preview orchestrator write RBAC" / "Preview hardenings: config.js no-cache, partial APP_* warning".

---

### Task 7: Config value + prod configmap + PR

**Files:**
- Modify: `k8s/prod/configmap-env.yaml` (add `PREVIEW_OAUTH_CLIENT_ID: "oUYmMxjJaeyvnx_gVBddWYAkC88nhy6xxminHqTxRDc="` — the staging dashboard client, public identifier, wildcard-registered in 4a)
- Final whole-branch gates; push; `gh pr create` — title "Preview control plane 3c: orchestration", body summarizing POST/DELETE, the render pipeline, RBAC, and the CSI-strip design fact, ending with the attribution line.

---

## Self-review notes

- **Spec coverage:** creation flow steps 1–5 and teardown from the spec land here; UI create/teardown buttons deliberately deferred (CLI-first per spec's phasing; the tab is read-only until after 4b).
- **Design facts encoded:** CSI mount strip is safe (verified: nothing reads /mnt/secrets); staging Secret keys are env-named (envFrom replaces per-var refs); staging configmap fetched from tarball is the env baseline; one shared wildcard cert copied per namespace (LE rate limits).
- **3b schema honored:** phases `creating|ready|failed` (+`bifrost/error`), clean comma `bifrost/apps`, `previewNSPrefix` semantics (orchestrator uses its own constant — keep the string identical; exporting web's constant would invert the dependency direction).
- **Known risks named:** krusty API shapes (verify-and-adapt), SSA on fake dynamic clients (test-seam fallback), JSON6902 remove paths when bases change (scan-gated), RESEND_API_KEY copied into preview identity can send real email (accepted; noted for spec).
- **Type consistency:** `RenderInput.EnvConfig` = `envConfigFor` output; `SecretName` `{svc}-preview-secrets` matches the copy in Up stage 5; trigger-ID map keys `{svc}-preview-build` match plan 2's trigger names.
