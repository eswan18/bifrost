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
    bif status <app> <app>   # several services, in the order given
    bif status -q            # list out-of-sync services (* = mid-deploy)
    bif status <app> -q      # minimal output; exit 0 in sync, 1 if not
    bif status -a            # what needs attention, and why (--attention)
    bif status <app> -a      # the same four checks, for one service
    bif promote <app>        # compare staging vs prod, then ask before promoting
    bif promote <app> <app>  # several services: one combined plan, one prompt
    bif promote <app> -y     # promote without the prompt
    bif preview list         # table of preview environments
    bif preview up <branch>  # create/update, show progress, print URLs
    bif preview down <tag>   # tear down (confirms unless -y/--yes)
    bif completion zsh|bash  # print the tab-completion shim (see below)

Both `status` and `promote` take **one or more** service names. Names are
deduped and acted on in the order given (with no names, `status` uses registry
order and `promote` is a usage error — there is no form of `promote` that
promotes everything). Every name is validated up front, so a typo in the third
argument fails before the first cluster read, and — for `promote` — before
anything could have been written.

`preview` takes `--ttl <duration>`, `--auto-update` and `--no-wait`; see
`docs/preview-environments.md`.

Exit codes are a contract: `status` exits **1** only when a service is
definitely out of sync, in the default and `-q` forms alike. Mid-deploy, missing
pods and an unreadable staging tag all exit **0** — a script asking "is there
anything to promote?" gets "no", not an error, when the answer isn't knowable
yet. (`--attention` asks a broader question and has its own rules; see below.)
`promote` exits **1** when it refuses (no deployment on either side, a
staging rollout in flight) or when the patch fails; declining the prompt exits
**0**, because nothing went wrong.

### Tab completion

`bif completion` prints a shim for your shell. One line in your shell's rc file
is the whole install — there is no directory to pick and nothing to keep in
sync, so no `make` target would save a step:

    # ~/.zshrc — after compinit, which is what defines compdef
    source <(bif completion zsh)

    # ~/.bashrc
    source <(bif completion bash)

If you would rather not run `bif` at every shell start, drop the shim in the
directory your shell already looks in:

    # zsh: any directory on your fpath, named _bif
    mkdir -p ~/.zsh/completions && bif completion zsh > ~/.zsh/completions/_bif
    # ...and, in ~/.zshrc BEFORE compinit:  fpath=(~/.zsh/completions $fpath)

    # bash (with bash-completion installed)
    mkdir -p ~/.local/share/bash-completion/completions
    bif completion bash > ~/.local/share/bash-completion/completions/bif

What completes: the commands, service names for `status` and `promote` — minus
the ones already on the line, since both commands dedupe what you give them —
`preview`'s subcommands, each command's flags, and the live preview tags for
`bif preview down`.

The shims themselves know nothing: they hand the words you have typed to a
hidden `bif __complete` and print what comes back, so the candidates are always
the installed binary's own answer. A generated script with the fleet baked into
it would keep offering a service that had since been renamed.

`bif preview down <tag>` is the one completion that reaches the network — those
tags are branch-derived and not worth typing from memory. It is bounded at
400ms and fails to **nothing**: a slow bifrost, an expired `gcloud` login or no
network at all give you an empty completion, never an error where a candidate
should be. Since reading the token costs a `gcloud` subprocess, that budget is
genuinely tight and a cold Tab often comes back empty — `bif preview list` is
still the way to see what exists. No other position makes any call at all: the
fleet comes from the registry compiled into the binary.

### `bif promote` with several services

Several names produce **one combined plan and one prompt**, not a prompt each:

    $ bif promote bifrost footstrike-api identity

    bifrost         eb12dfa -> prod  (prod: 5ea4c5a)
    footstrike-api  89aed5a -> prod  (prod: 8ccc788)
    identity        already in sync, skipping

    Proceed with 2 promotions? [y/N]

Nothing is written before the whole plan has been shown, so the answer is given
once, about everything. If nothing needs promoting there is no prompt at all.

The **asymmetry survives**, per service: a *staging* mismatch refuses that
service and a *prod* mismatch warns and promotes it anyway (staging mid-deploy
means the artifact is not settled; prod mid-deploy just means the last rollout
is still landing, and re-pinning it is how a bad one gets corrected). In a
several-service run a refusing service is reported and **skipped**, not allowed
to abort the others — and neither is a failed write, so a failure on the second
service still leaves the third attempted. The run ends with a summary naming
every service in each group:

    Summary: 2 promoted (bifrost, identity), 1 failed (comms)

Exit **0** only if everything attempted worked and nothing was refused. A
service skipped because it is already in sync is not a failure; a service that
refused is, because the operator asked for a promotion that did not happen.

A single name keeps the original per-service output and its `Proceed? [y/N]`
prompt, unchanged.

`bif status` and `bif status <app>` also show each service's most recent Cloud
Build, under its prod tag:

      staging: 0ab11f2
      prod:    abc1234
      build:   ◌ 0ab11f2 building (2m)

`✓ <sha> succeeded 3h ago` / `✗ <sha> failed 12m ago` once it finishes. The
column is **information, never a verdict**: a failed build does not make a
service out of sync and never changes the exit code — what's deployed is what
that word means. It is also best-effort. One API call covers the whole fleet,
it is bounded by its own short timeout, and if Cloud Build is slow, unreachable
or unauthenticated the cell reads `(build status unavailable)`, a note goes to
stderr, and the rest of the output is exactly what it would have been. `bif
status -q` is untouched: its output is a scriptable contract, so it renders no
build text and makes no Cloud Build call at all. The project is `GCP_PROJECT`,
defaulting to `ethans-services`.

### `bif status --attention`

`-a` / `--attention` answers "what needs my attention?" — every service with
something noteworthy, and what. One line per reason, each line naming its own
service so it stands alone under `grep`:

    bifrost   build 0ab11f2 succeeded 3d ago, staging still on abc1234
    bifrost   staging and prod differ: staging abc1234, prod def5678 (bif promote bifrost)
    comms     build 4f2a1b0 is building (3m)
    identity  deploy in progress: staging is running 2 images (abc1234, def5678)

Four conditions qualify: **staging and prod differ** (something to promote);
**two or more distinct images inside one environment** (a deploy in progress —
what `-q` marks with `*`); **a build running right now** (`QUEUED`/`PENDING`/
`WORKING`); and **the newest successful build's SHA is not what staging is
running**.

That last one is the point of the mode. The other three are already visible
somewhere — in the table, in `-q`, on the Apps tab — but a green build that
never reached staging is visible nowhere, and it is the signature of a failure
this fleet has actually hit: when ArgoCD's `github-eswan18-repocreds` PAT
expires, the Applications go to `ComparisonError` and **syncs silently stop**.
Builds keep going green, staging quietly stays behind, and `promote` appears to
do nothing. It is *not* a duplicate of "staging and prod differ": that compares
two environments to each other, this compares CI to staging, and they fail
independently. When both fire, the stalled sync is listed first — it explains
why the promote suggested under it would appear to do nothing.

There is **no grace period** on it. Right after a push there is a brief window
where the build is green and staging has not moved yet, and that state is
worth being able to see rather than something to suppress — so it is reported,
and the elapsed time in the line is what tells the two apart at a glance
(`succeeded just now` versus `succeeded 3d ago`). The reader draws the
conclusion. It does not fire when it cannot tell: no recent build, a build that
failed or is still running, staging with no pods, staging on an unpinned
mutable tag, or any staging image already on the build's commit (a half-finished
rollout is a rollout, and reports itself as one).

Exit **0** only when all four checks ran and nothing qualified — and it says so,
`Nothing needs attention.`, rather than printing nothing. Exit **1** when
something qualified, which makes it usable from cron (`bif status -a || notify`).

**If the Cloud Build read fails, it does not claim an all-clear.** Two of the
four checks are unknowable without build data, so the run says on *stdout* that
they were skipped, prints whatever the two cluster-only checks found, and exits
**1**. `0` is reserved for "I checked all four and the fleet is clean", because
that is the only state in which this command's silence means anything. This is a
deliberate departure from the build column's best-effort degradation below,
which is right for `status` — whose answer comes from the cluster — and wrong
here.

`-a` and `-q` together are refused rather than resolved: they are two different
answers to what stdout should be, and either winner silently takes something
away — `-q`'s offline guarantee, or the mode that was asked for. `bif status
<app> -a` is accepted and scoped to that one service; it costs the same single
Cloud Build call, which answers for the whole fleet either way.

`promote` writes one thing: a kustomize images override on the `<app>-prod`
ArgoCD Application, which is what the retired `ib promote` always did. A staging
image mismatch refuses — the artifact isn't settled, so promoting might ship the
wrong one — while a prod mismatch only warns, since re-pinning prod is how a bad
rollout gets corrected.

`status` and `promote` reach the cluster directly through client-go and never
call bifrost's API. That is deliberate and load-bearing: `bif promote bifrost` is
how bifrost gets recovered when bifrost is down, so it cannot depend on bifrost
being up. The service list is `go:embed`ed, so it needs no network either. The
property is about that one server, not about the network — the Kubernetes API
and Cloud Build are third parties, not the service being managed, and the Cloud
Build read degrades to an unknown column rather than a failure.

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
