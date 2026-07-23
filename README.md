# Bifrost

An opinionated application state dashboard that supports promotion (staging -> prod) and rollback.

Built atop Argo CD and Kubernetes.

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
the ArgoCD `Application`, the same operation as setting a manual image
override. The UI is server-rendered `html/template` with a single inline
vanilla-JS block: plain form POSTs work without JS; JS upgrades to `fetch()`,
background polling, a light/dark theme toggle, and a "refreshed Ns ago" ticker.

The "Blueprint" theme (`static/style.css`) is hand-written from design tokens —
there is **no CSS build step** and no Node toolchain.

## Architecture

A single Go binary in-cluster. Reads pod images across `<app>-{staging,prod}`
namespaces via `client-go`. Patches `argoproj.io/v1alpha1 Application` objects
in the `argocd` namespace via the dynamic client. Authenticates with OIDC
(code flow + PKCE); one allowed email gates access.

Bifrost assumes conventions rather than configuring around them: each service
deploys to `<app>-staging` and `<app>-prod` namespaces, its ArgoCD
`Application`s are named `<app>-staging` / `<app>-prod` and pin images via
`spec.source.kustomize.images`, images are tagged by commit SHA, and staging
is auto-updated by ArgoCD Image Updater. The CI-build column is Google Cloud
Build-specific and optional — leave `GCP_PROJECT` unset to disable it.

Self-promotion is supported. If a bad version of bifrost ever lands in prod
and bricks the UI, the fallback is patching its `Application` by hand — the
same `kubectl patch` shown under "Manual fallback" below.

## Development

    make run

`static/style.css` is committed and edited by hand — no build step, no Node.

Requires these env vars to run (see `internal/config/config.go`):

    BASE_URL=http://localhost:8080
    ENV=local
    SERVICES=api,dashboard
    ALLOWED_EMAIL=you@example.com
    OIDC_ISSUER_EXTERNAL=...
    OIDC_ISSUER_INTERNAL=...
    OIDC_CLIENT_ID=...
    OIDC_CLIENT_SECRET=...
    SESSION_SECRET=$(openssl rand -base64 32)

The two `OIDC_ISSUER_*` vars exist because in-cluster pods can't resolve the
issuer's public hostname: discovery is fetched from the internal URL and the
token/userinfo/JWKS endpoints are rewritten to it, while browser redirects use
the external one. If that's not your problem, set both to the same URL.

For local dev, `kube.New` falls back to `~/.kube/config`.

## Tests

    make test

## Deployment

Push to `main`. Cloud Build → Artifact Registry. ArgoCD Image Updater bumps
the staging Application. To roll out prod, use the app itself — or patch the
Application by hand if the app is the thing that's broken.

### Unpinned prod

`bifrost-prod` initially runs `:latest` — no `kustomize.images` override
exists on the Application until the first promotion writes one. An
unparseable prod tag (`latest`, `prod`) reads as drift, so the first
promotion works from the UI and writes the pinning override. The same
applies to any service whose Application is recreated (a rename, a cluster
rebuild): the pin lives only on the live Application object, so prod falls
back to the repo manifests' mutable tag until a promotion re-pins it.

### Manual fallback

If the UI itself is what's broken, the equivalent by hand:

    kubectl patch application bifrost-prod -n argocd --type merge \
      -p '{"spec":{"source":{"kustomize":{"images":["us-central1-docker.pkg.dev/ethans-services/containers/bifrost=us-central1-docker.pkg.dev/ethans-services/containers/bifrost:<sha>"]}}}}'

## Out-of-repo setup

This repo doesn't manage:
- GCP service accounts, workload identity bindings, Secret Manager secrets, or
  the Cloud Build trigger — those live in a separate infra repo (Pulumi).
- OAuth client registration with the identity provider.
- The public hostname (a Cloudflare Tunnel, in this deployment).
- The actual `kubectl apply -f k8s/argocd/...` to bootstrap ArgoCD.
