# Fleet Registry Unification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One entry per app describes that app completely. Collapse bifrost's four remaining parallel service maps (`SERVICES`, `REPO_OVERRIDES`, `STAGING_URLS`, `PROD_URLS`) into the registry that already holds preview config, so adding an app is one YAML block instead of six comma-separated string edits — and a typo becomes a load-time error instead of a broken link nobody notices.

**Architecture:** The preview registry graduates into a fleet registry. `internal/preview/registry.yaml` moves to `internal/registry/` and grows per-app fields (`repo`, `urls.staging`, `urls.prod`), with the existing preview block becoming an optional `preview:` sub-object — absent means "not previewable", which is how forecasting/asset-manager/comms/bifrost will sit until they're onboarded. `internal/preview` imports the new package rather than owning it, which also removes today's oddity where the *web* layer depends on a `preview`-named package for fleet data.

**Why now:** this is the same collapse plan 5 did for preview inventory, applied to the rest. Plan 5 proved the pattern works and left the fleet half visibly inconsistent — `internal/preview/registry.yaml` naming footstrike-api while `k8s/base/configmap.yaml` names it again in four other places.

**Tech Stack:** Go 1.26, `sigs.k8s.io/yaml`, `embed` — no new dependencies.

**Repo/branch:** `~/Develop/ibormeith/bifrost`, branch `fleet-registry`; one PR.

**Prior art that constrains this:** the preview system is deployed and passed a live smoke; plan 5's registry merged as `a261145`. The Apps and Jobs tabs are bifrost's primary function — Task 3's equivalence goldens are the gate that they render identically.

## Global Constraints

- Gates for every task: `go build ./... && go vet ./... && go test ./... -count=1 && gofmt -l .` (empty) AND `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run ./...` — all green. (Bifrost CI runs golangci-lint; the Makefile's `lint` is only `go vet` and will not catch what CI does.)
- No behavior change to the fleet UI. Same seven services, same repo names, same staging/prod links, same ordering.
- The registry is the single source: after this branch, a service name must appear in exactly one place in bifrost.
- Preview behavior must not regress — plan 5's equivalence goldens (`TestEnvConfigForRegistryEquivalence`) must still pass untouched, including its three documented `SANCTIONED DIVERGENCE` entries.
- No new go.mod dependencies.

---

### Task 1: Move the registry to its own package and widen the schema

**Files:**
- Create: `internal/registry/registry.go`, `internal/registry/registry.yaml`, `internal/registry/registry_test.go`
- Delete: `internal/preview/registry.go`, `internal/preview/registry.yaml`, `internal/preview/registry_test.go`
- Modify: every `internal/preview` file referencing the moved symbols (`envconfig.go`, `orchestrator.go`, tests), `cmd/bifrost/main.go`

**Interfaces (produced):**

```go
package registry

type NeonRef struct{ Project, Database, Role string }   // json tags unchanged from plan 5
type Preview struct {
	Neon     *NeonRef          `json:"neon,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Required []string          `json:"required,omitempty"`
}
type URLs struct {
	Staging string `json:"staging,omitempty"`
	Prod    string `json:"prod,omitempty"`
}
// Service is one app in the fleet.
type Service struct {
	Repo    string   `json:"repo,omitempty"`    // defaults to the service name
	URLs    URLs     `json:"urls,omitempty"`
	Preview *Preview `json:"preview,omitempty"` // nil = not previewable
}
type Registry map[string]Service

func Load() (Registry, error)
func (r Registry) Names() []string          // all services, sorted
func (r Registry) PreviewNames() []string   // services with a preview block, sorted
func (r Registry) RepoFor(svc string) string // Repo, or svc when empty
```

The YAML must reproduce today's config exactly. Read `k8s/base/configmap.yaml` for the authoritative current values (`SERVICES`, `REPO_OVERRIDES`, `STAGING_URLS`, `PROD_URLS`) and `internal/preview/registry.yaml` for the preview blocks — do not retype either from this plan. Seven services: asset-manager (repo `asset_manager`), bifrost, comms, footstrike-api, footstrike-dashboard, forecasting, identity. Note `comms` has no URLs today (background worker, no ingress) — represent that as an absent/empty `urls`, and make sure a missing URL stays an empty string at the consumer rather than becoming a literal "missing" link.

- [ ] **Step 1:** Failing tests — all seven parse; `Names()` sorted and complete; `PreviewNames()` returns exactly the three with preview blocks; `RepoFor` returns the override for asset-manager and the identity mapping for everything else; unknown fields error (nested inside `preview:` too); a `required` key absent from `env` errors (carry plan 5's invariant forward).
- [ ] **Step 2–3:** Verify failure; implement. The preview-specific validation and the `//go:embed` move across unchanged in substance — this is a move plus a widening, not a rewrite.
- [ ] **Step 4:** Update `internal/preview` to consume `registry.Registry`/`registry.Preview` (type aliases are acceptable if they keep the diff small — justify either way), and `main.go` to call `registry.Load()`. **Plan 5's `TestEnvConfigForRegistryEquivalence` must pass with its expectations untouched.** If it fails, you've changed preview behavior — STOP and report.
- [ ] **Step 5:** Gates + commit — "Move the registry into its own package and widen it to the fleet".

---

### Task 2: Fleet consumers read the registry

**Files:**
- Modify: `internal/config/config.go` (+test), `internal/web/handlers.go`, `internal/web/fleet.go`, `cmd/bifrost/main.go`, `internal/web/*_test.go` as needed

- [ ] **Step 1: Capture the equivalence goldens FIRST, from the running code.** Before touching a consumer, write a test that pins what the Apps tab renders today: for a fixed fake-kube fleet state, capture `assembleFleet`'s output (or the `appView` slice — whichever is the tightest deterministic seam; justify your choice) as a golden. Include every service, both envs, and specifically the fields sourced from the maps under migration: repo (asset-manager's override is the interesting one), staging URL, prod URL, and comms' empty URLs. Generate it by running the current code — do not hand-write it.
- [ ] **Step 2:** Verify the golden passes against current code.
- [ ] **Step 3:** Repoint consumers: `Handlers` gains a `Registry` field; `fleet.go`'s `for i, svc := range h.Cfg.Services` becomes `h.Registry.Names()`; `h.Cfg.RepoFor(svc)` → `h.Registry.RepoFor(svc)`; `h.Cfg.StagingURLs[svc]`/`ProdURLs[svc]` → the registry's `URLs`. Delete `Config.Services`, `RepoOverrides`, `StagingURLs`, `ProdURLs`, `RepoFor`, and their parsing. `GitHubOrg` stays (it's one value, not a per-app map).
- [ ] **Step 4:** The golden must pass **unchanged**. Any diff is a UI regression — STOP and report rather than updating it.
- [ ] **Step 5:** Gates + commit — "Read fleet inventory from the registry".

**Care point:** the preview cascade's step 3 (`cfg.StagingURLs[X]`) is one of these consumers. It moves to the registry too, and plan 5's cascade tests must still pass. That step is what a service with no staging ConfigMap falls back to (footstrike-dashboard's `APP_API_URL`), so a mistake here silently changes a preview's env — check it explicitly.

---

### Task 3: Retire the ConfigMap keys

**Files:**
- Modify: `k8s/base/configmap.yaml`, `k8s/staging/configmap-env.yaml` and `k8s/prod/configmap-env.yaml` if they carry any of the four keys

- [ ] **Step 1:** Remove `SERVICES`, `REPO_OVERRIDES`, `STAGING_URLS`, `PROD_URLS`. Check all three overlay files; leave everything else (`ARGOCD_NAMESPACE`, `GITHUB_ORG`, OIDC, `DISPLAY_TIMEZONE`, preview keys) alone.
- [ ] **Step 2:** Document the deployment caveat for the PR body — do NOT re-litigate it, the owner ruled on 2026-07-29. Unlike plan 5's optional preview keys, `SERVICES` is REQUIRED by the currently-deployed binary: once ArgoCD applies this ConfigMap, a pod restart before the new image is promoted boots with an empty service list and a blank Apps tab. **Owner's decision: accept it — merge, then promote bifrost promptly; a minute of degraded UI is fine.** The PR body must state this plainly so whoever merges knows to promote, and must NOT present the two-PR alternative as an open question.
- [ ] **Step 3:** Gates + commit — "Drop the fleet inventory ConfigMap keys".

---

### Task 4: Docs and PR

**Files:**
- Modify: `README.md`, `docs/preview-environments.md`, and a new `docs/adding-a-service.md`

- [ ] **Step 1:** `docs/adding-a-service.md` — the recipe this whole plan exists to enable: add a registry entry (fields explained, `preview:` optional), then the per-repo/infra plumbing that is genuinely outside bifrost (Cloud Build trigger, k8s manifests, Pulumi SA/secrets, ArgoCD app). Be explicit about which steps bifrost owns and which it doesn't — the value here is telling a future reader where the boundary is.
- [ ] **Step 2:** Update `docs/preview-environments.md`'s onboarding section to point at the new file for the fleet half and keep only the preview-specific steps, and fix any path references to `internal/preview/registry.yaml` (now `internal/registry/registry.yaml`). Check the README's architecture paragraph for the same stale path.
- [ ] **Step 3:** Full gates, then open the PR: "Fleet registry: one entry per app". Body covers the collapse, the equivalence goldens, the ConfigMap-removal deployment-order caveat from Task 3, and the docs. End after a blank line with: `🤖 Generated with [Claude Code](https://claude.com/claude-code)`.

---

## Self-review notes

- **The risky task is 2**, gated by goldens captured from running code — same discipline that caught three real divergences in plan 5.
- **Task 3 carries a real deployment caveat**, ruled on by the owner (2026-07-29): accept the brief blank-Apps-tab window; merge then promote promptly. The plan states it in the PR body rather than hiding it, and does not re-open the choice.
- **Not in scope:** `ib.py`'s `SERVICES` list stays duplicated on purpose (it must work when bifrost is down — add a comment saying so if touching the file), `infra/__main__.py` stays code, and no new app is onboarded here.
- **Type consistency:** `registry.Preview` is plan 5's `preview.Service` renamed; its json tags and validation carry over unchanged so the preview equivalence tests keep passing.
