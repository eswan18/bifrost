# Preview Service Registry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make onboarding a new previewable app (forecasting, asset-manager, …) a declarative config edit plus per-repo build plumbing — no Go changes — by replacing the three hardcoded per-service env functions with an embedded registry file, and put the system's documentation where the system lives (bifrost + `ib`).

**Architecture:** A single `internal/preview/registry.yaml`, `go:embed`-ed into bifrost, declares every previewable service: its Neon reference (if any) and its env wiring as templates. One primitive carries the relationships that made this app-specific in the first place — `{{ url X }}` resolves to X's *preview* URL when X is part of this preview, and otherwise **defers to the value the app's own staging ConfigMap already sets**, falling back to bifrost's `STAGING_URLS` only when there is no such baseline (see Task 2's cascade). That deferral is deliberate: it keeps bifrost from holding a second copy of a fact the app already owns. Plus `{{ internalUrl X }}` for in-cluster DNS and `{{ config KEY }}` for operator-supplied values. `envConfigFor` becomes a template evaluator over that data. The registry is bifrost-side and code-reviewed on purpose: it names which Neon project gets branched, which must never be influenced by the branch under test.

**Tech Stack:** Go 1.26, `sigs.k8s.io/yaml` (already a direct dep — no new modules), `embed`.

**Repo/branch:** `~/Develop/ibormeith/bifrost`, branch `preview-registry`; one PR.

**Prior art that constrains this:** the preview system shipped and passed a live end-to-end smoke on 2026-07-28. Behavior for footstrike-api, footstrike-dashboard, and identity must not change — Task 3's equivalence tests are the gate that proves it.

## Global Constraints

- Gates for every task: `go build ./... && go vet ./... && go test ./... -count=1 && gofmt -l .` (empty) AND `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run ./...` — all green. (Bifrost CI runs golangci-lint, not the Makefile's `go vet`.)
- No new go.mod dependencies.
- The registry declares *what*, never *how*: no conditionals, no per-service special cases in the file format. If a service needs behavior the templates can't express, that's a signal to extend the primitive set deliberately, not to add a `switch`.
- `ENV: staging` for previewable apps stays as-is (apps validate `ENV` against a closed set; previews are staging-flavored — this cost a crash-loop on 2026-07-28).
- The Neon reference is registry-only. Nothing in this plan may let branch-fetched content select a Neon project, database, or role.
- Golden tests must assert the *rendered env map* per service, not just that the file parses.

---

### Task 1: Registry format, embed, and loader

**Files:**
- Create: `internal/preview/registry.yaml`, `internal/preview/registry.go`, `internal/preview/registry_test.go`

**Interfaces (produced):**

```go
// Service is one previewable app's declaration.
type Service struct {
	Neon     *NeonRef          `json:"neon,omitempty"`     // nil = no database
	Env      map[string]string `json:"env,omitempty"`      // key -> template
	Required []string          `json:"required,omitempty"` // env keys that must resolve non-empty
}
type NeonRef struct {
	Project  string `json:"project"`
	Database string `json:"database"`
	Role     string `json:"role"`
}
type Registry map[string]Service

// LoadRegistry parses the embedded registry.yaml.
func LoadRegistry() (Registry, error)
// Names returns the previewable service names, sorted.
func (r Registry) Names() []string
```

The registry file's initial content must reproduce today's behavior exactly (Task 3 proves it):

```yaml
# Previewable services. Adding one here + a cloudbuild-preview.yaml and
# {repo}-preview-build trigger in its repo is the whole onboarding path —
# no Go changes. Templates: {{ url X }} is X's preview URL when X is part of
# this preview; otherwise the app's own staging ConfigMap value for that key
# wins, and only if there is none does bifrost fall back to STAGING_URLS.
# {{ internalUrl X }} is the same cascade over in-cluster DNS;
# {{ config KEY }} is an operator-supplied value from bifrost's config.
# Neon references live here and never come from the branch under test.
footstrike-api:
  neon:
    project: aged-river-81935268
    database: neondb
    role: neondb_owner
  env:
    ENV: staging
    PUBLIC_API_BASE_URL: "{{ url self }}"
    PUBLIC_DASHBOARD_BASE_URL: "{{ url footstrike-dashboard }}"
    IDENTITY_PROVIDER_URL: "{{ internalUrl identity }}"
    JWT_ISSUER: "{{ url identity }}"

footstrike-dashboard:
  env:
    APP_API_URL: "{{ url footstrike-api }}"
    APP_IDENTITY_URL: "{{ url identity }}"
    APP_OAUTH_CLIENT_ID: "{{ config previewOAuthClientID }}"
  required: [APP_API_URL, APP_IDENTITY_URL, APP_OAUTH_CLIENT_ID]

identity:
  neon:
    project: plain-heart-27630935
    database: neondb
    role: app
  env:
    JWT_ISSUER: "{{ url self }}"
```

**Behavioral note:** today footstrike-api and identity override `IDENTITY_PROVIDER_URL`/`JWT_ISSUER` only *when identity is a member*, otherwise the staging ConfigMap's values pass through untouched. Task 2's cascade reproduces that exactly by deferring to the baseline rather than looking up a staging URL — which is why no drift between bifrost's `STAGING_URLS` and the app's own config can affect these keys.

- [ ] **Step 1:** Failing tests — registry parses; `Names()` returns the three sorted; a service with no `neon` yields nil; malformed YAML and an unknown top-level field error; every `required` key also appears in `env`.
- [ ] **Step 2:** Verify failure. **Step 3:** Implement with `//go:embed registry.yaml`.
- [ ] **Step 4:** Gates + commit — "Add the previewable-service registry".

---

### Task 2: Template evaluator

**Files:**
- Create: `internal/preview/template.go`, `internal/preview/template_test.go`

**Interfaces (produced):**

```go
// EvalContext is what templates resolve against.
type EvalContext struct {
	Service string            // the service being rendered ("self")
	Tag     string
	Members []string
	Cfg     *config.Config    // StagingURLs, PreviewOAuthClientID
	// Baseline is the app's staging ConfigMap data. Step 2 of the cascade
	// defers to it, so bifrost never restates a fact the app already owns.
	Baseline map[string]string
	Key      string            // the env key being rendered (for cascade step 2)
}
// Eval renders one template string. Unknown functions/services are errors,
// never silent empties.
func Eval(tmpl string, ctx EvalContext) (string, error)
```

**Resolution cascade (the core design decision — read this before writing code).** A URL template does NOT simply look up a staging URL when the target isn't a member. It resolves in this order, and stops at the first hit:

1. **Target is a preview member** (or is `self`) → its preview URL (`{{ url }}`) or in-cluster preview DNS (`{{ internalUrl }}`).
2. **The key already has a value in the app's staging ConfigMap** (the `stagingData` baseline) → **keep that value untouched**.
3. → `cfg.StagingURLs[X]` for `{{ url }}`, or the `http://X.X-staging.svc.cluster.local` convention for `{{ internalUrl }}`.
4. → error naming the key and the unresolvable service.

Step 2 is the point of the whole design. Today's code doesn't compute a staging URL when identity isn't a member — it leaves the staging ConfigMap's value alone. Resolving to `cfg.StagingURLs[X]` instead would make bifrost hold a second, independently-maintained copy of a fact the app already states, and the two would silently drift (Task 1's report flagged exactly this). Deferring to the baseline means bifrost has no opinion where the app already has one, and drift is structurally impossible.

Step 3 still has to exist: footstrike-dashboard's preview image is environment-agnostic by design (no `VITE_*` baked in, no staging ConfigMap), so `APP_API_URL` has no baseline to inherit and must be computed.

Supported forms (exactly these; anything else is an error naming the offending template):
- `{{ url X }}` / `{{ url self }}` → preview URL `https://X-<tag>.preview.footstrike.run`, else the cascade above.
- `{{ internalUrl X }}` → `http://X.preview-<tag>.svc.cluster.local`, else the cascade above.
- `{{ config KEY }}` → currently only `previewOAuthClientID`; unknown key is an error.
- A literal with no `{{ }}` passes through unchanged (e.g. `ENV: staging`).

Whitespace inside the braces is flexible (`{{url X}}` and `{{ url  X }}` both work). Reuse the existing `previewURL`/`internalPreviewURL` helpers rather than re-deriving hostnames — they're the single source of truth shared with `render.go`.

- [ ] **Step 1:** Failing table tests covering every cascade step explicitly, since the cascade IS the design: (1) member → preview URL, for both `url` and `internalUrl`, including `self`; (2) non-member WITH a baseline value → that exact baseline value returned, byte-for-byte, and specifically NOT `cfg.StagingURLs[X]` — set them to *different* strings in the fixture so a regression to the lookup fails loudly; (3) non-member with NO baseline → `StagingURLs` / DNS convention; (4) non-member, no baseline, no staging URL → error naming key and service. Plus: literal passthrough, unknown function, unknown config key, empty template, malformed braces. Assert error *messages* name the offending template.
- [ ] **Step 2–3:** Verify failure, implement (hand-rolled parsing is fine and probably clearer than `text/template` for four forms — your call, justify it in the report).
- [ ] **Step 4:** Gates + commit — "Add the preview env template evaluator".

---

### Task 3: Migrate `envConfigFor` — equivalence is the gate

**Files:**
- Modify: `internal/preview/envconfig.go` (delete the three per-service functions and the `switch`), `internal/preview/envconfig_test.go`

**Interfaces:** `envConfigFor` keeps its exact signature and semantics: staging data copied, then registry keys rendered over it; `required` keys that render empty produce the same error shape as today's mandatory-`APP_*` check. `Registry` is threaded in (add it to the `Orchestrator` or pass it as a parameter — pick the smaller diff and say why).

**One sanctioned divergence — everything else must be byte-identical.** Today `footstrikeAPIEnvConfig` sets `PUBLIC_DASHBOARD_BASE_URL` unconditionally to the preview dashboard URL, even when footstrike-dashboard is NOT a preview member — pointing the api at a host nothing serves (dead OAuth redirect target, dead CORS origin). Under the cascade it resolves to staging's dashboard, which actually exists. That is a deliberate bug fix, decided 2026-07-28; capture it as an intentionally-updated golden with a comment explaining why. It applies to exactly one key in one member-set combination. Any OTHER divergence is a production bug: STOP and report, do not update the golden.

**Runtime `required` check (carried from Task 2's review).** `parseRegistry`'s load-time check only proves a `required` key has an `env` template; it does NOT prove the rendered value is non-empty. Task 3 must add the runtime check: after rendering a service's env, every key in `Service.Required` that renders empty produces an error with the same shape as today's `dashboardEnvConfig` emits for an unset `PreviewOAuthClientID`. Without this, a preview could deploy with `APP_OAUTH_CLIENT_ID=""`.

- [ ] **Step 1 (the important one): write equivalence tests BEFORE deleting anything.** For each of the three services, for both member sets (with and without identity), capture the *current* `envConfigFor` output as a golden map literal in the test file. These goldens must be generated by running the existing code, not hand-written from the plan — a hand-written golden that matches the plan rather than reality would defeat the whole exercise. Note in your report how you generated them.
- [ ] **Step 2:** Verify the goldens pass against the current implementation.
- [ ] **Step 3:** Replace the switch with registry-driven rendering; keep the goldens unchanged. They must pass byte-for-byte. If any differs, STOP and report — a behavior change here is a production bug, not a test to update.
- [ ] **Step 4:** Delete `footstrikeAPIEnvConfig`, `identityEnvConfig`, `dashboardEnvConfig`, the `svc*` constants if now unused, and `memberOrStagingURL` if the evaluator supersedes it.
- [ ] **Step 5:** Gates + commit — "Render preview env from the registry".

---

### Task 4: Registry becomes the inventory source

**Files:**
- Modify: `internal/config/config.go` (+test), `cmd/bifrost/main.go`, `internal/preview/orchestrator.go` (+test), `k8s/prod/configmap-env.yaml`

Today `PREVIEW_SERVICES` and `NEON_PROJECTS` duplicate what the registry now holds. Collapse them:

- [ ] **Step 1:** Remove `PreviewServices` and `NeonProjects` from `Config` and its parsing (keep `PreviewOAuthClientID` and the three tokens — those are values/secrets, not structure). Registry `Names()` and `Service.Neon` replace them at every consumer: the orchestrator's membership loop and Neon branching, `main.go`'s trigger-ID filtering, and the `Orch`-construction gate (`len(cfg.PreviewServices) > 0` → `len(registry) > 0`).
- [ ] **Step 2:** Drop `PREVIEW_SERVICES` and `NEON_PROJECTS` from `k8s/prod/configmap-env.yaml`. **Deployment-order note for the PR body:** the running pod ignores unknown ConfigMap keys and the new binary ignores their absence, so either order is safe — but say so explicitly, since ArgoCD applies the manifest before the new image rolls.
- [ ] **Step 3:** Update tests (config tests lose two cases; orchestrator fakes gain a registry). Gates + commit — "Drive preview inventory from the registry".

---

### Task 5: Documentation lands where the system lives

**Files:**
- Modify: `README.md` (bifrost), `docs/` (bifrost — new file), and `ib.py`'s docstring in `~/Develop/ibormeith/infra` (separate branch + PR there)

- [ ] **Step 1 — bifrost README:** add a "Preview environments" section after the existing feature description. What they are, the three `ib preview` commands, the lifecycle (membership by branch name → preview builds → Neon branch → render+apply → phase on namespace annotations), and the tailnet-only URL shape. Keep the README's existing terse voice.
- [ ] **Step 2 — bifrost `docs/preview-environments.md`:** the operator/agent-facing runbook. Port the content from `~/Develop/ibormeith/.claude/CLAUDE.md`'s "Preview environments" section (it was fact-checked against the code on 2026-07-28 — read it, don't re-derive), and add: **how to onboard a new app** (registry entry → `cloudbuild-preview.yaml` in its repo → `{repo}-preview-build` Pulumi trigger → Neon project/db/role if it has a database), plus the two gotchas (builds are manual-only; a stuck `creating` recovers by re-running `up`, which re-runs every member's build). Then replace that section in `~/Develop/ibormeith/.claude/CLAUDE.md` with a two-line pointer to this file — the workspace file is untracked, so it must not remain the only copy.
- [ ] **Step 3 — `ib.py` docstring** (infra repo, branch `preview-docs`, its own PR): the Usage block already lists the four invocations; add a one-line pointer to bifrost's `docs/preview-environments.md` for the concepts, and an Examples entry showing the matching-branch-name workflow (`git push origin my-feature` in two repos, then `ib preview up my-feature`).
- [ ] **Step 4:** Gates on both repos (`ruff check` AND `ruff format --check .` for infra — the Makefile's `lint` misses the format check and it has cost a red CI once). Commit; open both PRs.

---

## Self-review notes

- **The migration risk is concentrated in Task 3** and is handled by goldens captured from the *running* code before any deletion. Everything else is additive or mechanical.
- **What this does not do:** onboard forecasting or asset-manager. That's a follow-up per app (registry entry + `cloudbuild-preview.yaml` + Pulumi trigger + Neon ref), and the point of this plan is that it needs no Go changes. Their env wiring may reveal a primitive the evaluator lacks — extend it deliberately then.
- **Deliberately not moved into the registry:** `StagingURLs` (used by the fleet UI too), the three tokens, and `PreviewOAuthClientID` (a value, referenced via `{{ config }}`).
- **Type consistency:** `NeonRef` mirrors today's `config.NeonProjectRef` field-for-field so the orchestrator's Neon call sites change only in where the struct comes from.
