# Bifrost

A web UI for the `ib` CLI tool — lets me check deployment status and promote
staging→prod on the ethans-services GKE cluster from a phone.

**Mythology aside:** Bifrost is the rainbow bridge between Midgard (mortals →
staging) and Asgard (gods → prod). Naming chose itself.

## What it does

- Lists every service with its currently-deployed staging and prod image tags.
- Flags out-of-sync services and exposes a "Promote" button for each.
- Promote = patches the prod ArgoCD `Application`, same operation as `ib promote`.

## Architecture

A single Go binary in-cluster. Reads pod images across `<app>-{staging,prod}`
namespaces via `client-go`. Patches `argoproj.io/v1alpha1 Application` objects
in the `argocd` namespace via the dynamic client. Authenticates with OIDC
against `identity`; one allowed email gates access.

Self-promotion is supported. If a bad version of bifrost ever lands in prod
and bricks the UI, `ib promote bifrost` from a laptop is the fallback.

## Development

    make css-watch   # one terminal
    make run         # another

Requires these env vars to run (see `internal/config/config.go`):

    BASE_URL=http://localhost:8080
    ENV=local
    SERVICES=fitness-api,identity
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

## Out-of-repo setup

This repo doesn't manage:
- GCP service accounts, workload identity bindings, Secret Manager secrets, or
  the Cloud Build trigger — those live in `ethans-services-infra` (Pulumi).
- OAuth client registration in `identity`'s database.
- Cloudflare Tunnel hostname (`bifrost.ethanswan.com`).
- The actual `kubectl apply -f k8s/argocd/...` to bootstrap ArgoCD.
