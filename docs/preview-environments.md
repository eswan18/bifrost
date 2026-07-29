# Preview environments

Ephemeral, per-branch environments overlaid on staging: `ib preview up <branch>`
stands up namespace `preview-<tag>` (`<tag>` = a slug of the branch name),
containing only the apps whose repo has that branch pushed — everything else
a preview app talks to resolves cross-namespace to shared staging.

Which apps exist in the fleet at all lives in `internal/registry/registry.yaml`
— see [`docs/adding-a-service.md`](adding-a-service.md) for that half.
Previewability is an optional `preview:` sub-block on a fleet entry: a Neon
reference (if any) and the app's preview env wiring. Adding that sub-block
to an existing entry (plus a build trigger — see "Onboarding a new
previewable app" below) is the whole path to making an already-onboarded app
previewable; nothing else in bifrost's Go code changes.

```
ib preview list                     # table of preview environments
ib preview up <branch> [--no-wait]  # create/update, poll to ready, print URLs
ib preview down <tag> [-y/--yes]    # tear down (confirms unless -y)
```

## Where it runs

Prod bifrost is the single orchestrator (`internal/preview/orchestrator.go`'s
`Orchestrator.Up`/`Down`) — `ib.py` is a thin HTTP client of bifrost's API, no
preview logic lives in `ib.py` itself. It authenticates with a static bearer
token (Secret Manager secret `bifrost_prod_preview_api_token`) against:

```
GET    /api/previews          # list
GET    /api/previews/{tag}    # one preview's record (phase, health, urls) — ib.py polls this from `up`
POST   /api/previews          # {"branch": "..."} -> create/update, returns {tag, phase}
DELETE /api/previews/{tag}    # tear down
```

A tag mid-`Up`/`Down` is claimed by an in-memory busy set; a concurrent call
for the same tag gets `409` (`ib.py`: "That preview is busy").

Preview apps are reachable at `{app}-<tag>.preview.footstrike.run`,
tailnet-only via the same shared `staging-ingress` Tailscale LB + `nginx`
ingress class staging uses.

## Lifecycle (`Up`)

1. **Resolve membership**: for every service in the registry
   (`Registry.Names()`, sorted), check whether `branch` exists in that
   service's GitHub repo. A missing branch means "not a member," not an
   error; any other GitHub error aborts the whole run (membership can't be
   determined, so it's not safe to guess). At least one member is required.
2. **Required-key pre-flight**: any member whose registry entry declares
   `required` env keys (today, only `footstrike-dashboard`'s
   `APP_API_URL`/`APP_IDENTITY_URL`/`APP_OAUTH_CLIENT_ID`) has those keys
   resolved against an empty staging baseline, before the namespace or
   anything else is touched. This fails fast, cleanly, if — for
   example — `PREVIEW_OAUTH_CLIENT_ID` isn't configured.
3. **Ensure the namespace**: `preview-<tag>`, annotated
   `bifrost/branch`, `bifrost/apps` (comma-joined members), and
   `bifrost/phase: creating`.
4. **Build**: run each member's `{service}-preview-build` Cloud Build trigger and
   wait for it (`buildPollInterval`, 10s), collecting the resulting short SHA.
   Every member's build runs, every time — there is no check for "this SHA
   already has a preview image."
5. **Neon branch**: for every member with a registry `neon:` reference,
   find-or-create a `preview-<tag>` branch off that project's default branch
   (empty parent ID) and fetch its connection URI for the registry's
   `database`/`role`. A service with no `neon:` entry (e.g.
   `footstrike-dashboard`) is skipped.
6. **Copy secrets**: each Neon-backed member's `{svc}-staging-secrets` Secret
   Store CSI secret is copied into the preview namespace with `DATABASE_URL`
   overridden to the new branch's URI, plus the shared wildcard TLS
   secret (`preview-footstrike-run-tls`, provisioned once in the `previews`
   namespace — copied rather than reissued per preview, since Let's
   Encrypt's duplicate-certificate limit is 5/week for an identical
   `dnsNames` set).
7. **Render and apply**: for each member, fetch its `k8s/` tree, parse its
   `staging/configmap-env.yaml` (empty map if it has none), compute the final
   env via the registry's templates (see "The registry" below), build a
   generated kustomize overlay atop the fetched `k8s/base`, and
   server-side-apply the result into the preview namespace.
8. **Mark ready**: `bifrost/phase: ready`, `bifrost/error: ""`.

Any failure after step 3 sets `bifrost/phase: failed` with a sanitized
`bifrost/error` annotation (never a secret value) naming the cause. A failure
in steps 1–2 returns before the namespace exists, so it never leaves a zombie
namespace.

`Up` is safe to re-run: `EnsureNamespace`/`CopySecret`/apply are idempotent,
and Neon branch creation is scan-then-create. This is also the recovery path
for a stuck `creating` (see "Gotchas").

## Lifecycle (`Down`)

Deletes the `preview-<tag>` namespace, then — for **every** registry service
with a `neon:` reference, not just the tag's current members (a re-created
preview may have changed membership since its branch was created, and this is
the only record `Down` has) — best-effort deletes a `preview-<tag>` Neon
branch if one exists. Every step runs regardless of earlier failures; all
errors are joined and returned together.

## The registry (`internal/registry/registry.yaml`)

Preview wiring is the `preview:` sub-block of a fleet entry (`registry.Preview`,
aliased in this package as `Service`):

```yaml
footstrike-api:
  urls:
    staging: https://api.staging.footstrike.run
    prod: https://api.footstrike.run
  preview:
    neon:                                 # omit for apps with no database
      project: aged-river-81935268
      database: neondb
      role: neondb_owner
    env:
      ENV: staging
      PUBLIC_API_BASE_URL: "{{ url self }}"
      PUBLIC_DASHBOARD_BASE_URL: "{{ url footstrike-dashboard }}"
      IDENTITY_PROVIDER_URL: "{{ internalUrl identity }}"
      JWT_ISSUER: "{{ url identity }}"
    required: [...]                       # keys that must render non-empty
```

Three template forms (`internal/preview/template.go`'s `Eval`), each either a
literal (no `{{`/`}}`, passed through unchanged) or exactly one
`{{ func arg }}`:

- **`{{ url X }}`** / **`{{ url self }}`** — X's (or, for `self`, the
  rendering service's) externally-reachable URL, through the resolution
  cascade below.
- **`{{ internalUrl X }}`** — the same cascade, but the in-cluster DNS form.
- **`{{ config KEY }}`** — an operator-supplied value from bifrost's own
  config. Only `previewOAuthClientID` (-> `PREVIEW_OAUTH_CLIENT_ID`) is
  recognized today.

A malformed template (unbalanced braces, wrong arg count, unknown function or
config key) is a load-/render-time error naming the offending string — a
typo in `registry.yaml` fails loudly instead of silently rendering an empty
env var.

### The resolution cascade

`{{ url X }}` / `{{ internalUrl X }}` resolve in this order, stopping at the
first hit:

1. **Member**: if X is this preview's own service or one of its members, its
   preview URL/DNS name (`https://{X}-{tag}.preview.footstrike.run`, or
   `http://{X}.preview-{tag}.svc.cluster.local`).
2. **The app's own staging baseline**: if the *target* app's own
   `staging/configmap-env.yaml` already has a value for this exact env key,
   that value wins, untouched.
3. **Fleet registry / DNS convention**: the fleet registry's `urls.staging`
   for X (`internal/registry/registry.yaml`, `registry.Registry[X].URLs.Staging`
   — for `url`) or the `http://{svc}.{svc}-staging.svc.cluster.local`
   convention (for `internalUrl`, which always succeeds — it's a pure string
   formula).
4. **Error** (only reachable for `url`): names the key and the unresolvable
   service. This is deliberate — it fails at preview-creation time, before
   the cluster is touched, rather than deploying a pod that crash-loops or
   silently misbehaves on a broken URL.

Step 2 is deliberately checked *before* step 3. Note whose ConfigMap this is:
the baseline belongs to **the app being rendered**, not to X. `JWT_ISSUER` for
a footstrike-api preview resolves from *footstrike-api's* own
`staging/configmap-env.yaml`, even though the template argument is `identity`.

Both the fleet registry's `urls.staging` and that app's own staging
ConfigMap describe the same fact (X's staging URL), maintained independently
in two repos, and **nothing enforces they agree**. Deferring to the app's own
value means bifrost never restates a fact the app already owns — if the two
ever drift, previews still resolve to what the app itself says, so drift in
the registry's `urls.staging` can't leak into a preview's env. Keep this in
mind if you're ever tempted to "fix" a preview URL by editing
`internal/registry/registry.yaml` — check the `staging/configmap-env.yaml`
of the app whose env you're changing first.

## Onboarding a new previewable app

The app must already be a fleet member — a plain entry in
`internal/registry/registry.yaml` with no `preview:` block — before it can
be made previewable; see [`docs/adding-a-service.md`](adding-a-service.md)
for that half. From there, making it previewable is three pieces, no Go
changes:

1. **A `preview:` sub-block on its `internal/registry/registry.yaml`
   entry** — a `neon:` block if the app has a database, an `env:` map of
   templates (`{{ url X }}` / `{{ internalUrl X }}` / `{{ config KEY }}`, or
   plain literals), and a `required:` list for any keys that must render
   non-empty (a required key with no matching `env` entry is rejected at
   load time).
2. **A `cloudbuild-preview.yaml` in the app's own repo** that builds and
   pushes `{image}:preview-$SHORT_SHA` (see
   `footstrike-api/cloudbuild-preview.yaml` for the working pattern — no
   push trigger, no `--all-tags`, and the `preview-` prefix deliberately
   fails the staging ImageUpdater's `allowTags` regexp so a preview build can
   never accidentally auto-deploy to staging).
3. **A `{service}-preview-build` manual-invocation Cloud Build trigger** in
   infra's Pulumi (`__main__.py`'s `for preview_repo in [...]` loop, pinned to
   `cloudbuild-preview.yaml` on the repo's `main`) — add the repo name to
   that list and `pulumi up`. The trigger is named after the registry key
   (matching what `Orchestrator.TriggerIDs` looks up), not the repo — the two
   coincide for every previewable service today, but aren't guaranteed to.
   The GCP IAM prod bifrost needs to run any
   preview-build trigger (`cloudbuild.builds.editor` + `actAs` on the Cloud
   Build SA) is already granted once, not per app.

That's it — no Go code, no new bifrost endpoint, no orchestrator change.

## Gotchas

- **Preview builds are manual-only** — nothing fires on push or PR. A branch
  with no completed `{service}-preview-build` run has no image for `up` to
  deploy.
- **A stuck `creating` phase** (e.g. bifrost restarted mid-create) recovers
  by re-running `ib preview up <branch>` — safe, since every stage is
  idempotent — but it **always re-runs every member's preview build** (no
  skip-if-image-exists check), so recovery costs a full rebuild of every app
  in the preview, not just the one that was mid-flight.
