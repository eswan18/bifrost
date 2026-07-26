# Stack Environments — Design

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
| Stack scope | **Overlay on staging.** A stack contains only the apps whose branch is being tested; everything else resolves cross-namespace to shared staging. The IDP is just another app a stack may or may not include. |
| Naming | **Dynamic names on shared wildcard infra.** `stack-<tag>` namespaces where `<tag>` is a slug of the branch name. No fixed slots. |
| Who executes | **Bifrost (prod instance) is the single brain**, exposing stack operations over its HTTP API. No logic in ib.py beyond thin HTTP calls (avoids repeating the status/promote dual-implementation problem — see `internal/promote/status.go`'s "Mirrors ib.py" comment). |
| CLI | **`ib stack up/down/list`** added to the existing ib.py as a thin client of bifrost's API. No new CLI binary. |
| Data | **Neon branch per stateful stack app**, branched from that app's staging DB, named `stack-<tag>`. |
| Source of truth for builds | **Pushed branches only.** Bifrost runs in-cluster and has no laptop checkout; builds are triggered via the Cloud Build API and manifests fetched from GitHub at the branch. |

## Stack anatomy

- **Namespace:** `stack-<tag>`, `<tag>` = DNS-safe slug of the branch name. Same branch ⇒ same
  stack; re-running `up` updates in place.
- **Membership:** for each known repo (bifrost config maps each service → its GitHub repo and,
  if stateful, its Neon project — footstrike-api and identity have DBs, the dashboard doesn't),
  the stack includes the app iff the branch exists in that repo. This matches the "name the branch the
  same across repos" workflow.
- **Hostnames:** `{app}-{tag}.stacks.footstrike.run` — single-label because the wildcard cert
  `*.stacks.footstrike.run` covers only one level. Tailnet-only via the existing shared
  `staging-ingress` Tailscale LB + nginx ingress class.
- **Env wiring (rendered into a per-stack configmap):**
  - `VITE_API_URL` / `PUBLIC_API_BASE_URL` / `PUBLIC_DASHBOARD_BASE_URL` → stack URLs
  - `EXTRA_CORS_ORIGINS` → stack dashboard URL
  - `IDENTITY_PROVIDER_URL` / `JWT_ISSUER` → staging identity, **unless** identity is in the
    stack, in which case they point at the stack's identity
  - `DATABASE_URL` → the app's Neon stack branch
- **CronJobs** deploy with `suspend: true` in stacks — no background jobs against throwaway DB
  branches.
- **Secrets:** bifrost copies each app's staging K8s Secret values into a plain K8s Secret in
  the stack namespace, overriding `DATABASE_URL`. No CSI driver, no GCP SA, no
  workload-identity binding in stack namespaces.

## Control plane (bifrost)

**API** (session auth for the UI, plus a static bearer token for CLI use):

- `POST /api/stacks {branch}` — starts async creation, returns the stack record immediately
- `GET /api/stacks` / `GET /api/stacks/{tag}` — list/status
- `DELETE /api/stacks/{tag}` — teardown

**State:** no database. The stack namespace itself is the record — labels/annotations hold
branch, member apps, creation time, and phase; live status is derived from GCB build state and
pod readiness, in bifrost's existing "read live state, assume conventions" style. Creation runs
in a goroutine; because `up` is idempotent (server-side apply, create-if-missing Neon branch,
skip build when the image for the branch HEAD SHA already exists), a bifrost restart mid-create
is recovered by re-running `up`.

**Creation flow:**

1. Resolve membership (GitHub API, PAT).
2. For each member app: trigger its `{name}-stack-build` Cloud Build trigger against the branch
   (substitutions carry stack URLs for the dashboard's `VITE_*` build-time vars). Wait for
   green.
3. Create Neon branches for stateful member apps (Neon REST API).
4. Fetch each member repo's `k8s/base` at the branch (GitHub tarball API), render with embedded
   kustomize + a generated stack overlay, apply via client-go server-side apply.
5. Copy staging secrets (with `DATABASE_URL` override) and the wildcard TLS cert secret into
   the namespace; create the per-stack Ingress.

**Teardown:** delete namespace, delete Neon branches. Best-effort; `GET /api/stacks` (and the
UI) surfaces zombies by age.

**New privileges** (deliberate graduation from dashboard to small deployment controller;
single-user OIDC-gated): namespace create/delete; create/update of workloads, configmaps,
secrets, ingresses in `stack-*`; secret read in `*-staging`; Cloud Build trigger run; GitHub
PAT (private repos); Neon API key. Prod bifrost only — staging bifrost never orchestrates
stacks.

**UI:** a Stacks tab — list with age/phase/links, create-from-branch, tear down.

## Builds

Each stackable repo gets `cloudbuild-stack.yaml` + a `{name}-stack-build` trigger (Pulumi):

- Tags `stack-<tag>-<shortsha>` only. **Never `latest`, never a bare SHA** — staging's
  image-updater (`newest-build` + `allowTags: regexp:^[a-f0-9]{7,}$`) would otherwise scoop a
  branch build straight into staging.
- Dashboard variant takes stack URLs as substitutions (Vite bakes env at build time).
  Per-stack dashboard builds are accepted; runtime `config.js` injection stays a possible
  future improvement, not part of this work.

## Identity / auth

- Identity change: clients flagged as stack-eligible (the staging footstrike clients) also
  accept redirect URIs matching `https://*.stacks.footstrike.run` + the registered callback
  path. Exact matching is unchanged for everything else. Risk accepted: scoped to designated
  staging clients, and everything under the wildcard is tailnet-only.
- When identity itself is in a stack, its Neon branch inherits staging's client registrations
  and the same wildcard rule, so stack-issued auth works with zero registration steps; member
  apps' `JWT_ISSUER`/`IDENTITY_PROVIDER_URL` point at the stack identity.

## `ib stack` (infra repo)

`ib stack up <branch>` / `ib stack down <tag>` / `ib stack list` — thin HTTP calls to prod
bifrost with the static bearer token (read from Secret Manager or env). `up` polls status until
ready and prints the stack URLs.

## One-time provisioning

- Wildcard DNS `*.stacks.footstrike.run` → staging Tailscale LB address.
- cert-manager wildcard Certificate (DNS01, existing issuer) in a shared `stacks` namespace;
  bifrost copies its secret into each stack namespace.
- Identity wildcard-redirect support + flag on the staging clients.
- Pulumi: stack-build triggers per repo; secrets for bifrost's PAT, Neon API key, and API
  bearer token; bifrost RBAC additions.

## Out of scope (deliberate)

- Prod-flavored stacks (staging-flavored only)
- Auto-teardown / TTL reaper (manual `down`; `list` shows age)
- ArgoCD involvement in stacks (imperative by design; GitOps stays for permanent envs)
- Building from uncommitted local changes (pushed branches only)
- Dashboard runtime config injection
- Pub/Sub isolation (stack apps would share staging topics if publishing ever lands)

## Testing

- Unit: slug/naming rules, membership resolution, overlay rendering (golden manifests),
  build-skip logic, status derivation from namespace + build state.
- Existing bifrost handler-test patterns extend to the new endpoints.
- Smoke (manual, first deploy): stack of a trivial two-repo branch — verify hostnames, TLS,
  login via staging identity, api⇄dashboard wiring, Neon branch isolation, teardown leaves
  nothing behind.
