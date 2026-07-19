# Bifrost

A web UI for the `ib` CLI tool — lets me check deployment status and promote
staging→prod on the ethans-services GKE cluster from a phone.

**Mythology aside:** Bifrost is the rainbow bridge between Midgard (mortals →
staging) and Asgard (gods → prod). Naming chose itself.

## What it does

Three tabs (real routes; each has a polling fragment endpoint that swaps its
body in place):

- **Overview** (`/`) — an attention queue (crashlooping apps, failed jobs,
  drift ready to promote), fleet counts, and running / recently-failed /
  next-scheduled jobs.
- **Apps** (`/apps`) — every service across staging and prod: image hash +
  health per env, last CI build, and job count. Drift exposes **Promote**; a
  crash exposes **Roll back**; in-sync rows offer a ghost roll-back.
- **Jobs** (`/jobs`, `?app=` filter) — every CronJob across both environments,
  each tagged `stg`/`prod`, with last-run state (incl. exit code), duration,
  and next run.

Promote and roll back are confirmed in a hash-diff modal. Both re-validate live
cluster state server-side before patching (a stale hash is refused) and patch
the ArgoCD `Application`, the same operation as `ib promote` / a manual image
override. The UI is server-rendered `html/template` with a single inline
vanilla-JS block: plain form POSTs work without JS; JS upgrades to `fetch()`,
background polling, a light/dark theme toggle, and a "refreshed Ns ago" ticker.

The "Blueprint" theme (`static/style.css`) is hand-written from design tokens —
there is **no CSS build step** and no Node toolchain.

## Architecture

A single Go binary in-cluster. Reads pod images across `<app>-{staging,prod}`
namespaces via `client-go`. Patches `argoproj.io/v1alpha1 Application` objects
in the `argocd` namespace via the dynamic client. Authenticates with OIDC
against `identity`; one allowed email gates access.

Self-promotion is supported. If a bad version of bifrost ever lands in prod
and bricks the UI, `ib promote bifrost` from a laptop is the fallback.

## Development

    make run

`static/style.css` is committed and edited by hand — no build step, no Node.

Requires these env vars to run (see `internal/config/config.go`):

    BASE_URL=http://localhost:8080
    ENV=local
    SERVICES=footstrike-api,identity
    ALLOWED_EMAIL=you@example.com
    OIDC_ISSUER_EXTERNAL=...
    OIDC_ISSUER_INTERNAL=...
    OIDC_CLIENT_ID=...
    OIDC_CLIENT_SECRET=...
    SESSION_SECRET=$(openssl rand -base64 32)

For local dev, `kube.New` falls back to `~/.kube/config`.

## Tests

    make test

## Deployment

Push to `main`. Cloud Build → Artifact Registry. ArgoCD Image Updater bumps
the staging Application. To roll out prod, use the app itself
(`https://bifrost.ethanswan.com`) — or `ib promote bifrost` if the app is
the thing that's broken.

### First deployment

`bifrost-prod` initially runs `:latest` — no `kustomize.images` override
exists on the Application until the first promotion writes one. Bifrost
refuses to promote a service whose prod tag is unparseable, so the FIRST
promotion of bifrost itself must be done manually:

    kubectl patch application bifrost-prod -n argocd --type merge \
      -p '{"spec":{"source":{"kustomize":{"images":["us-central1-docker.pkg.dev/ethans-services/containers/bifrost=us-central1-docker.pkg.dev/ethans-services/containers/bifrost:<sha>"]}}}}'

Also add `bifrost` to the `SERVICES` list in `infra/ib.py` so the laptop CLI
stays a working fallback (that change lives in the infra repo, not here).

## Out-of-repo setup

This repo doesn't manage:
- GCP service accounts, workload identity bindings, Secret Manager secrets, or
  the Cloud Build trigger — those live in `ethans-services-infra` (Pulumi).
- OAuth client registration in `identity`'s database.
- Cloudflare Tunnel hostname (`bifrost.ethanswan.com`).
- The actual `kubectl apply -f k8s/argocd/...` to bootstrap ArgoCD.
