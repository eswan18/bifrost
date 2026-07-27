# Preview Environments — Design

**Date:** 2026-07-26
**Status:** Approved (pending final spec review)
**Owner repos touched:** bifrost (primary), infra (`ib.py`, Pulumi), identity, footstrike-api, footstrike-dashboard (small per-repo additions)

## Problem

Today every app deploys to `{app}-staging` and `{app}-prod` namespaces. There is no way to
see coordinated changes across repos (e.g. footstrike-api + footstrike-dashboard on matching
feature branches) working together before merging to main.

## Decision summary

| Fork | Decision |
|------|----------|
| Restructure permanent namespaces? | **No.** `{app}-{env}` stays exactly as-is. Cross-namespace K8s DNS already connects apps (e.g. `identity.identity-staging.svc.cluster.local`); grouping namespaces buys nothing and churns workload-identity bindings, SecretProviderClasses, ArgoCD destinations, and tunnel URLs. |
| Preview scope | **Overlay on staging.** A preview contains only the apps whose branch is being tested; everything else resolves cross-namespace to shared staging. The IDP is just another app a preview may or may not include. |
| Naming | **Dynamic names on shared wildcard infra.** `preview-<tag>` namespaces where `<tag>` is a slug of the branch name. No fixed slots. |
| Who executes | **Bifrost (prod instance) is the single brain**, exposing preview operations over its HTTP API. No logic in ib.py beyond thin HTTP calls (avoids repeating the status/promote dual-implementation problem — see `internal/promote/status.go`'s "Mirrors ib.py" comment). |
| CLI | **`ib preview up/down/list`** added to the existing ib.py as a thin client of bifrost's API. No new CLI binary. |
| Data | **Neon branch per stateful preview app**, branched from that app's staging DB, named `preview-<tag>`. |
| Source of truth for builds | **Pushed branches only.** Bifrost runs in-cluster and has no laptop checkout; builds are triggered via the Cloud Build API and manifests fetched from GitHub at the branch. |

## Preview anatomy

- **Namespace:** `preview-<tag>`, `<tag>` = DNS-safe slug of the branch name. Same branch ⇒ same
  preview; re-running `up` updates in place.
- **Membership:** for each known repo (bifrost config maps each service → its GitHub repo and,
  if stateful, its Neon project — footstrike-api and identity have DBs, the dashboard doesn't),
  the preview includes the app iff the branch exists in that repo. This matches the "name the branch the
  same across repos" workflow.
- **Hostnames:** `{app}-{tag}.preview.footstrike.run` — single-label because the wildcard cert
  `*.preview.footstrike.run` covers only one level. Tailnet-only via the existing shared
  `staging-ingress` Tailscale LB + nginx ingress class.
- **Env wiring (rendered into a per-preview configmap):**
  - `VITE_API_URL` / `PUBLIC_API_BASE_URL` / `PUBLIC_DASHBOARD_BASE_URL` → preview URLs
  - `EXTRA_CORS_ORIGINS` → preview dashboard URL
  - `IDENTITY_PROVIDER_URL` / `JWT_ISSUER` → staging identity, **unless** identity is in the
    preview, in which case they point at the preview's identity
  - `DATABASE_URL` → the app's Neon preview branch
- **CronJobs** deploy with `suspend: true` in previews — no background jobs against throwaway DB
  branches.
- **Secrets:** bifrost copies each app's staging K8s Secret values into a plain K8s Secret in
  the preview namespace, overriding `DATABASE_URL`. No CSI driver, no GCP SA, no
  workload-identity binding in preview namespaces.

## Control plane (bifrost)

**API** (session auth for the UI, plus a static bearer token for CLI use):

- `POST /api/previews {branch}` — starts async creation, returns the preview record immediately
- `GET /api/previews` / `GET /api/previews/{tag}` — list/status
- `DELETE /api/previews/{tag}` — teardown

**State:** no database. The preview namespace itself is the record — labels/annotations hold
branch, member apps, creation time, and phase; live status is derived from GCB build state and
pod readiness, in bifrost's existing "read live state, assume conventions" style. Creation runs
in a goroutine; because `up` is idempotent (server-side apply, create-if-missing Neon branch,
skip build when the image for the branch HEAD SHA already exists), a bifrost restart mid-create
is recovered by re-running `up`.

**Creation flow:**

1. Resolve membership (GitHub API, PAT).
2. For each member app: trigger its `{name}-preview-build` Cloud Build trigger against the branch
   (RunBuildTrigger API, no substitutions). Wait for green. Per-preview URLs (dashboard `APP_*`
   vars) are set later via the preview overlay's env vars, not as build substitutions.
3. Create Neon branches for stateful member apps (Neon REST API).
4. Fetch each member repo's `k8s/base` at the branch (GitHub tarball API), render with embedded
   kustomize + a generated preview overlay, apply via client-go server-side apply.
5. Copy staging secrets (with `DATABASE_URL` override) and the wildcard TLS cert secret into
   the namespace; create the per-preview Ingress.

**Teardown:** delete namespace, delete Neon branches. Best-effort; `GET /api/previews` (and the
UI) surfaces zombies by age.

**New privileges** (deliberate graduation from dashboard to small deployment controller;
single-user OIDC-gated): namespace create/delete; create/update of workloads, configmaps,
secrets, ingresses in `preview-*`; secret read in `*-staging`; Cloud Build trigger run; GitHub
PAT (private repos); Neon API key. Prod bifrost only — staging bifrost never orchestrates
previews.

**UI:** a Previews tab — list with age/phase/links, create-from-branch, tear down.

## Builds

Each previewable repo gets `cloudbuild-preview.yaml` + a `{name}-preview-build` trigger (Pulumi):

- Tags `preview-{SHORT_SHA}` only — env-agnostic and branch-content-addressed (no per-preview
  suffix; the same branch content always produces the same tag). **Never `latest`, never a bare
  SHA** — staging's image-updater (`newest-build` + `allowTags: regexp:^[a-f0-9]{7,}$`) would
  otherwise scoop a branch build straight into staging.
- The dashboard preview image is env-agnostic; per-preview URLs are supplied at deploy time as
  `APP_API_URL` / `APP_IDENTITY_URL` / `APP_OAUTH_CLIENT_ID` env vars on the container,
  materialized as `/config.js` by the nginx entrypoint (runtime config with per-key fallback to
  build-time values).

## Identity / auth

- Identity change: clients flagged as preview-eligible (the staging footstrike clients) also
  accept redirect URIs matching `https://*.preview.footstrike.run` + the registered callback
  path. Exact matching is unchanged for everything else. Risk accepted: scoped to designated
  staging clients, and everything under the wildcard is tailnet-only.
- When identity itself is in a preview, its Neon branch inherits staging's client registrations
  and the same wildcard rule, so preview-issued auth works with zero registration steps; member
  apps' `JWT_ISSUER`/`IDENTITY_PROVIDER_URL` point at the preview identity.

## `ib preview` (infra repo)

`ib preview up <branch>` / `ib preview down <tag>` / `ib preview list` — thin HTTP calls to prod
bifrost with the static bearer token (read from Secret Manager or env). `up` polls status until
ready and prints the preview URLs.

## One-time provisioning

- Wildcard DNS `*.preview.footstrike.run` → staging Tailscale LB address.
- cert-manager wildcard Certificate (DNS01, existing issuer) in a shared `previews` namespace;
  bifrost copies its secret into each preview namespace.
- Identity wildcard-redirect support + flag on the staging clients.
- Pulumi: preview-build triggers per repo; secrets for bifrost's PAT, Neon API key, and API
  bearer token; bifrost RBAC additions.

## Out of scope (deliberate)

- Prod-flavored previews (staging-flavored only)
- Auto-teardown / TTL reaper (manual `down`; `list` shows age)
- ArgoCD involvement in previews (imperative by design; GitOps stays for permanent envs)
- Building from uncommitted local changes (pushed branches only)
- Collapsing footstrike-dashboard's staging/prod `{sha}-{env}` dual builds onto the new runtime
  config (now that preview builds are substitution-free, the same could apply to staging/prod,
  but that's a separate migration — deferred)
- Pub/Sub isolation (preview apps would share staging topics if publishing ever lands)

## Testing

- Unit: slug/naming rules, membership resolution, overlay rendering (golden manifests),
  build-skip logic, status derivation from namespace + build state.
- Existing bifrost handler-test patterns extend to the new endpoints.
- Smoke (manual, first deploy): preview of a trivial two-repo branch — verify hostnames, TLS,
  login via staging identity, api⇄dashboard wiring, Neon branch isolation, teardown leaves
  nothing behind.
