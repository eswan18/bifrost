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

## Preview environments

Ephemeral, per-branch environments layered onto staging. `bif preview up
<branch>` stands up namespace `preview-<tag>` (`<tag>` = a slug of the
branch), containing only the services whose `internal/registry/registry.yaml`
entry has a `preview:` block and whose repo has that branch pushed —
everything else a preview app talks to resolves cross-namespace to shared
staging.

    bif preview list                     # table of preview environments
    bif preview up <branch> [--no-wait]  # create/update, poll to ready, print URLs
    bif preview down <tag> [-y/--yes]    # tear down (confirms unless -y)

Lifecycle: membership by branch name -> each member's `{registry key}-preview-build`
Cloud Build trigger runs (manual-only, no push trigger; named after the
registry key, not the repo, when the two differ — see "Onboarding a new
previewable app" in `docs/preview-environments.md`) -> a `preview-<tag>`
Neon branch for any member the registry gives a database reference -> its
manifests are rendered from the registry's env templates and applied -> the
namespace's `bifrost/phase` annotation moves `creating` -> `ready` (or
`failed`, with `bifrost/error` set to the cause).

Preview apps are reachable at `https://{app}-<tag>.preview.footstrike.run`,
tailnet-only via the same shared Tailscale LB + nginx ingress class staging
uses. Full runbook — including how to onboard a new previewable app — is in
`docs/preview-environments.md`.

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
Build-specific and optional — leave `GCP_PROJECT` unset to disable it. Every
app bifrost knows about — its repo, public URLs, and whether it's
previewable — is one entry in `internal/registry/registry.yaml`; see
`docs/adding-a-service.md` for what's bifrost's to configure versus what
lives in the app's own repo and infra.

Self-promotion is supported. If a bad version of bifrost ever lands in prod
and bricks the UI, the fallback is patching its `Application` by hand — the
same `kubectl patch` shown under "Manual fallback" below.

## Development

    make run

`static/style.css` is committed and edited by hand — no build step, no Node.

Requires these env vars to run (see `internal/config/config.go`):

    BASE_URL=http://localhost:8080
    ENV=local
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

## The `bif` CLI

`cmd/bif` is bifrost's command-line half — the same decision logic (`internal/promote`),
the same fleet list (`internal/registry`), driven from a terminal instead of a browser.

    make install      # go install ./cmd/bif  →  $(go env GOBIN), else $(go env GOPATH)/bin

That directory has to be on your `PATH`; `make install` says so if it isn't.
`make build-bif` drops a binary in the working directory instead.

**`bif` is the only deploy CLI.** It replaces the Python `ib` (`infra/ib.py`),
which is retired and no longer in the infra repo. The two names differ because
they had to coexist during the port: a Go binary called `ib` would have shadowed
the Python one on `PATH` and silently taken whatever wasn't ported yet away —
the kind of thing you discover mid-incident. The name outlived the reason, and
renaming it now would cost every muscle-memory and script that has since learned
it. An `ib` still on your `PATH` is a leftover install of a script that no
longer exists in any repo — remove it.

    bif status               # every service
    bif status <app>         # one service's staging and prod images
    bif status -q            # list out-of-sync services (* = mid-deploy)
    bif status <app> -q      # minimal output; exit 0 in sync, 1 if not
    bif promote <app>        # compare staging vs prod, then ask before promoting
    bif promote <app> -y     # promote without the prompt
    bif preview list         # table of preview environments
    bif preview up <branch>  # create/update, show progress, print URLs
    bif preview down <tag>   # tear down (confirms unless -y/--yes)

`preview` takes `--ttl <duration>`, `--auto-update` and `--no-wait`; see
`docs/preview-environments.md`.

Exit codes are a contract: `status` exits **1** only when a service is
definitely out of sync, in every form, with or without `-q`. Mid-deploy, missing
pods and an unreadable staging tag all exit **0** — a script asking "is there
anything to promote?" gets "no", not an error, when the answer isn't knowable
yet. `promote` exits **1** when it refuses (no deployment on either side, a
staging rollout in flight) or when the patch fails; declining the prompt exits
**0**, because nothing went wrong.

`promote` writes one thing: a kustomize images override on the `<app>-prod`
ArgoCD Application, which is what the retired `ib promote` always did. A staging
image mismatch refuses — the artifact isn't settled, so promoting might ship the
wrong one — while a prod mismatch only warns, since re-pinning prod is how a bad
rollout gets corrected.

`status` and `promote` reach the cluster directly through client-go and never
call bifrost's API. That is deliberate and load-bearing: `bif promote bifrost` is
how bifrost gets recovered when bifrost is down, so it cannot depend on bifrost
being up. The service list is `go:embed`ed, so it needs no network either.

`preview` is the one exception, and is an HTTP client of bifrost's API by
design — the server owns preview orchestration, the cluster write credentials
and the Neon/Cloud Build tokens, so there is nothing for the CLI to do locally.
The split is enforced by `cmd/bif/main_test.go`'s `TestNoBifrostServerDependency`
file by file, so no future `status` or `promote` change can quietly acquire the
dependency.

### Two deliberate differences from the retired `ib.py`

`ib.py` is gone from the infra repo; both of these are behaviour changes the port
made on purpose, recorded here because the old behaviour is what a long-time user
expects.

An **unparseable prod tag** (`latest`, `prod` — see "Unpinned prod" below) reads
as *out of sync* here, where `ib.py`'s `status` called it indeterminate. Go is
right: `ib.py`'s own `promote` promoted from that state, and calling it unknown
is the bug behind bifrost#30. The visible cost is that `bif status -q` prints
the service and exits **1** where the Python printed nothing and exited **0**.

The **kustomize override key** comes from the image that's running, not from the
service's name. `ib.py` built it as `<registry>/<app>`, so for a service whose
image repository is named something else it wrote an override matching nothing
— kustomize ignores it, the promote reports success, and prod doesn't move.
`footstrike-api` is exactly that service: its image path is still `fitness-api`.

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
