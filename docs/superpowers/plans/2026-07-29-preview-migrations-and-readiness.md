# Preview Migrations and Readiness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A preview branch carrying a new database migration should work — and if it doesn't, `ib preview up` should say so instead of reporting success.

**The bug this fixes.** `footstrike-api`'s `check_migrations()` compares `alembic_version` against head and raises `RuntimeError` on mismatch — it verifies migrations, it does not run them. A preview branches staging's database (at staging's revision), so a branch with a new migration produces a pod that crash-loops on startup. Worse, `Up` marks the preview `ready` as soon as manifests are applied, without checking that anything came up: the operator gets a green CLI over a dead environment. Identity has the same shape (golang-migrate files under `db/migrations`, no migration call at startup) and will inherit the bug when it becomes previewable.

**Architecture:** Two independent halves. (1) A `migrate:` command declared in the registry beside `neon:` — deliberately adjacent, so the branch-then-migrate relationship is visible in one place — rendered as an initContainer sharing the app's image and `envFrom` (so it inherits the `DATABASE_URL` pointing at the fresh Neon branch). Kubernetes enforces the ordering; `upgrade head` is idempotent across restarts. (2) A bounded readiness wait at the end of `Up`, reported through plan 7's step mechanism, so an unready member fails the preview with the pod's reason instead of passing silently.

**Tech Stack:** Go 1.26, existing kustomize renderer, client-go.

**Repo/branch:** `~/Develop/ibormeith/bifrost`, branch `preview-migrations`; one PR.

**Dependency:** Task 2 uses `Orchestrator.step()` from the preview-progress branch (plan 7). Task 1 has no such dependency and can run immediately.

## Global Constraints

- Gates: `go build ./... && go vet ./... && go test ./... -count=1 && gofmt -l .` (empty) AND `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run ./...`.
- Plan 5/6 equivalence goldens (`TestEnvConfigForRegistryEquivalence`, the fleet golden) must pass untouched.
- `migrate:` is optional. A service without it renders exactly as today — verify by golden, not by assertion.
- No new go.mod dependencies, no new RBAC (Deployments are already created; an initContainer is part of the Deployment).

---

### Task 1: `migrate:` registry field → initContainer

**Files:**
- Modify: `internal/registry/registry.go` (+test), `internal/registry/registry.yaml`, `internal/preview/render.go` (+test)

**Interfaces (produced):** `registry.Preview` gains

```go
	// Migrate is run as an initContainer before the app starts, with the
	// same image and env as the app container — so it sees DATABASE_URL
	// pointing at this preview's fresh Neon branch. Empty = no migration
	// step. Kept beside Neon deliberately: branch the database, then bring
	// it up to the branch's schema.
	Migrate []string `json:"migrate,omitempty"`
```

Registry entry for footstrike-api: `migrate: ["uv", "run", "alembic", "upgrade", "head"]` — **verify this is the correct invocation for the image** before committing it (read the repo's `Dockerfile` and `CLAUDE.md`; the image runs as a non-root user with `uv` available, but confirm rather than assume; if the entrypoint layout means a bare `alembic upgrade head` is right, use that). Do NOT add one for identity — it isn't previewable yet, and inventing a migrate command for it would be untested speculation.

- [ ] **Step 1: Failing tests.** Registry: `migrate` parses; absent = nil; unknown fields still rejected. Renderer: with `Migrate` set, the rendered Deployment has exactly one initContainer, named clearly (e.g. `migrate`), with the same image as the app container (including the `preview-<sha>` tag override — assert this, since a wrong-image migration would run the WRONG schema version), the same `envFrom` list, `restartPolicy` untouched, and no volumes (the CSI machinery is stripped for previews). With `Migrate` nil, the Deployment has NO initContainers — assert explicitly.
- [ ] **Step 2–3:** Verify failure; implement. The image tag comes from the same kustomize `images:` transformer that rewrites the app container — confirm it rewrites initContainers too (kustomize's image transformer does handle initContainers, but VERIFY with a rendered-output assertion rather than trusting it; if it doesn't, the initContainer must be given the resolved tag explicitly).
- [ ] **Step 4:** Confirm the plan 5/6 goldens pass untouched. Gates + commit — "Run branch migrations in a preview initContainer".

---

### Task 2: `Up` waits for pods

**Depends on:** plan 7's `Orchestrator.step()` being present on this branch (rebase onto main after plan 7 merges, or cherry-pick). If it isn't available when you start, STOP and report — do not reimplement it.

**Files:**
- Modify: `internal/preview/orchestrator.go` (+test), `internal/kube/*.go` if a readiness helper is needed (+test)

After `renderAndApply` and before marking `ready`:

- Report the step (`waiting for pods`).
- Poll each member's Deployment until its pods are ready, using the existing `kube` client (`ListPods` + `SummarizeHealth` already exist and are the natural tools — prefer them over adding a new API surface; justify if you add one).
- **Bound it at 5 minutes** — migrations plus image pull on a cold node can be slow, and the caller (the CLI and the API goroutine) already has its own 30-minute ceiling.
- On timeout or a crash-looping pod: mark the preview `failed` with a sanitized `bifrost/error` naming the member and the pod's reason (e.g. `footstrike-api not ready: CrashLoopBackOff`). Never include env values or secrets — pod reasons are safe, pod *logs* are not; do not fetch logs.
- On success: clear the step and mark `ready` as today.

**Care points:**
- A preview whose member has no Deployment (nothing to wait for) must not hang — treat "no pods found" as a timeout with a clear message rather than looping forever.
- `Down` must still work on a preview that failed this way; the namespace exists and teardown is unchanged. Add a test.
- This changes what `ready` means (now: pods actually ready). Note it in the commit message and PR body — it's the point of the task, but it's a semantic change to the API contract the CLI and UI read.

- [ ] **Step 1: Failing tests** — happy path reaches `ready` only after pods report ready; a never-ready member yields `failed` with the member named in `bifrost/error`; the bound is respected (inject the timeout/poll interval so the test doesn't take 5 real minutes); teardown after such a failure still works.
- [ ] **Step 2–3:** Verify failure; implement.
- [ ] **Step 4:** Gates + commit — "Wait for member pods before marking a preview ready".

---

### Task 3: Docs and PR

- [ ] **Step 1:** `docs/preview-environments.md` — document the migration step in the lifecycle (branch → migrate → apply → wait for pods) and add `migrate:` to the registry-field reference. `docs/adding-a-service.md` — add `migrate:` to the onboarding recipe for a service with a database, and say plainly that a service whose app *verifies* migrations (rather than running them) needs this or its previews will crash-loop. Verify every claim against the code on this branch.
- [ ] **Step 2:** Open the PR: "Run branch migrations and verify previews come up". Body covers the bug (verify-don't-run + ready-means-applied → green CLI over a dead API), both halves of the fix, and the `ready` semantic change. End after a blank line with: `🤖 Generated with [Claude Code](https://claude.com/claude-code)`.

---

## Self-review notes

- **Why an initContainer over a Job:** no new RBAC, no polling/timeout code in the orchestrator, ordering enforced by Kubernetes, and idempotent on restart. A Job would give marginally better failure attribution, which Task 2 provides anyway.
- **Why not have the apps auto-migrate:** it would push preview-awareness into app repos, which the registry design deliberately avoids, and prod's verify-don't-run behavior is a deliberate safety property worth keeping.
- **Not in scope:** identity's migrate command (not previewable yet), down-migrations (a preview's Neon branch is disposable — tear it down), and any change to staging/prod migration handling.
