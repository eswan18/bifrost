# Preview environments

Ephemeral, per-branch environments overlaid on staging: `ib preview up <branch>`
stands up namespace `preview-<tag>` (`<tag>` = a slug of the branch name — see
"Branch → tag" below, including what happens when two branches slug to the
same one),
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
GET    /api/previews/{tag}    # one preview's record — ib.py polls this from `up`
POST   /api/previews          # {"branch": "...", "ttl": "8h", "autoUpdate": true}
                              #   -> create/update, returns {tag, phase}
DELETE /api/previews/{tag}    # tear down
```

`ttl`, on the create request, is an optional Go duration string (`"8h"`,
`"90m"`) — absent or empty means the preview never expires, which is the
default: **there is no implicit default TTL.** A caller-supplied `ttl` is
validated synchronously, before anything else happens, so a mistake comes
back as a 400 instead of a preview that vanishes (or doesn't) hours later:

- fails to parse as a Go duration → 400 `ttl must be a Go duration like 8h or 90m`
- zero or negative → 400 `ttl must be positive`
- over 720h (30 days) → 400 `ttl must be at most 720h0m0s`

That 720h cap is a typo guard (a fat-fingered `8760h` — a year — instead of
`8h`), not a policy limit: nothing enforces a maximum preview lifetime, and
omitting `ttl` entirely still means "never expires."

`autoUpdate`, also on the create request, is an optional boolean (default
`false`) that opts the preview into re-deploying itself when new commits land
on its branch — see "Following a branch" below. Like `ttl` it describes the
whole preview rather than this one run, and like `ttl` it is **not** sticky: a
re-POST without it turns auto-update back off, for the same merge reason
described in step 3 of the lifecycle.

A record carries `tag`, `branch`, `apps`, `phase`, `health`, `createdAt`,
`urls`, plus the progress trio: `step` (what `Up` is doing right now, e.g.
`building footstrike-api, footstrike-dashboard`), `stepSince` (RFC3339 — a timestamp, not a
duration, so a poller computes elapsed time locally instead of watching it go
stale between polls), and `error` (the failure cause, same string as the
`bifrost/error` annotation). Those three are omitted from the JSON when empty,
so a consumer must treat a missing key as "". `step` without `stepSince` is a
legal combination: a `bifrost/step-since` annotation that's absent or doesn't
parse as RFC3339 is dropped rather than failing the read.

`expiresAt` (RFC3339) is the preview's reclaim time, set only when it was
created with a `ttl`; it's omitted from the JSON entirely (`omitzero`) for
the common case of a preview with no expiry. See "Automatic expiry" below
for how and when it's actually enforced.

`autoUpdate` (bool) reports whether the preview follows its branch. It's
`omitempty`, so it appears only as `"autoUpdate": true` and is absent
entirely for every preview that didn't opt in — a consumer must read a
missing key as `false`.

A tag mid-`Up`/`Down` is claimed by an in-memory busy set; a concurrent call
for the same tag gets `409` (`ib.py`: "That preview is busy").

`busy` (bool) reports that claim on the record itself. It's `omitempty`, so
like `autoUpdate` it appears only as `"busy": true` and a missing key reads as
`false`. It is the one field not derived from the namespace — the busy set is
bifrost's own in-process state, not cluster state — and that is exactly why
it's reported: it's the reason a concurrent `up`/`down` gets a 409, and
without it nothing in the API said so.

### Busy with no namespace

A tag can be claimed while **no namespace exists for it**, in both directions:

- `Down` holds the tag after `DeleteNamespace` has returned, while it works
  through each service's Neon branches.
- `Up` holds it from the moment it starts, through membership resolution and
  the required-key pre-flight, before it creates the namespace at all.

Because `list` and the Previews tab read namespaces, that state used to be
completely invisible: `ib preview list` said "No preview environments" while
`ib preview up` said "That preview is busy", and neither was the whole truth.
So `GET /api/previews` **synthesizes** a record for every claimed tag that has
no namespace behind it. It carries `"phase": "busy"` and `"busy": true`;
`branch`, `apps` and `urls` are empty and `createdAt` is the zero time,
because a synthesized record has no namespace to learn any of that from and
does not invent it.

`busy` is a phase of its own on purpose. The busy set is a bare claim on a tag
and does not record which of `Up` or `Down` took it, so `creating` and
`terminating` would both be guesses — and `terminating` would be wrong twice
over, since it's reserved for a namespace whose Kubernetes phase really is
`Terminating` and here there is no namespace at all.

Only the list synthesizes. `GET /api/previews/{tag}` still 404s for a tag with
no namespace, because `DELETE /api/previews/{tag}` uses that same lookup to
answer 404 for a preview that doesn't exist — synthesizing there would turn
every in-flight teardown into a 200 and let a `DELETE` proceed against a
preview that is already gone.

Preview apps are reachable at `{app}-<tag>.preview.footstrike.run`,
tailnet-only via the same shared `staging-ingress` Tailscale LB + `nginx`
ingress class staging uses.

## Branch → tag

`<tag>` is `TagForBranch(branch)` (`internal/preview/tag.go`): the branch
lowercased, with `/`, `_` and spaces turned into `-`, every other character
outside `[a-z0-9-]` dropped, runs of `-` collapsed to one, leading and
trailing `-` trimmed, and the result capped at **30 characters**
(`maxTagLen` — a trailing `-` the cut leaves behind is trimmed too). So
`Feat_X` → `feat-x`, `feat/preview API` → `feat-preview-api`, `änderung` →
`nderung`. A branch whose name has no usable characters at all (`!!!???`)
derives an empty tag and is rejected synchronously with a 400 (`branch does
not produce a usable preview tag`).

The tag names everything downstream: the namespace `preview-<tag>`, the Neon
branch `preview-<tag>`, the hostnames `{app}-<tag>.preview.footstrike.run`,
and the argument to `ib preview down <tag>`.

### Two branches can derive the same tag

**The mapping is many-to-one**, by two independent paths:

- **truncation** — `feature/add-user-profile-avatars-v1` and `…-v2` share
  their first 30 characters, so both derive `feature-add-user-profile-avata`.
- **character folding**, with no truncation involved at all — `feat/foo`,
  `feat_foo` and `feat-foo` all derive `feat-foo`.

An `up` for a branch whose tag another branch's preview already holds is
**refused** (step 3 of the lifecycle below), with `preview.ErrTagCollision`
and a message naming both branches. Nothing is built, branched or applied, and
the incumbent preview is left exactly as it was — including its
`bifrost/branch` annotation.

Before that refusal existed, the second `up` silently *adopted* the first
preview: `EnsureNamespace` merges, so `bifrost/branch` was overwritten with
the newcomer; the Neon branch is found by name, so the second branch inherited
the first's database **and whatever migrations it had applied**; and one
`ib preview down <tag>` destroyed both. No layer warned.

Two ways out, and which one is right is yours to decide — bifrost can't:

- **rename your branch** so it derives a different tag, then re-run `up`.
  For a truncation collision the change has to land inside the first 30
  characters; appending to the end of an already-too-long name changes
  nothing. For a folding collision, change more than the separator.
- **tear the other preview down** — `ib preview down <tag>` — if it's finished
  with, then re-run `up`.

The tag is deliberately **not** disambiguated with a hash of the branch name.
That would close only the truncation path, leave folding (which needs no long
branch name at all) wide open, and change the tag of every existing
long-branch preview — orphaning its namespace and its Neon branch from the
`down` that should have reclaimed them. The refusal is the general fix; see
the note on `TagForBranch` in `internal/preview/tag.go`.

**Note where the refusal surfaces.** `POST /api/previews` still answers
**202**: `Up` runs in a background goroutine and the response has already
been sent by the time the check runs, exactly as for `ErrTerminating`. The
refusal is a `preview create failed` line in bifrost's logs, and the tag's own
record is untouched — so `GET /api/previews/<tag>`, `ib preview list` and the
Previews tab all go on describing **the other branch's** preview, at whatever
phase it was already in. A tag showing a `BRANCH` that isn't the one you just
ran `up` for is the symptom.

## Lifecycle (`Up`)

1. **Resolve membership**: for every service in the registry
   (`Registry.Names()`, sorted), check whether `branch` exists in that
   service's GitHub repo. A missing branch means "not a member," not an
   error; any other GitHub error aborts the whole run (membership can't be
   determined, so it's not safe to guess). At least one member is required.
   The same call returns each member's **full** commit SHA, which step 3
   records.
2. **Required-key pre-flight**: any member whose registry entry declares
   `required` env keys (today, only `footstrike-dashboard`'s
   `APP_API_URL`/`APP_IDENTITY_URL`/`APP_OAUTH_CLIENT_ID`) has those keys
   resolved against an empty staging baseline, before the namespace or
   anything else is touched. This fails fast, cleanly, if — for
   example — `PREVIEW_OAUTH_CLIENT_ID` isn't configured.
3. **Refuse an unusable namespace**, then **ensure the namespace**. The
   refusals are a single `GetNamespace` immediately before the write below
   (`refuseUnusableNamespace` in `orchestrator.go` — one API call, two
   verdicts, each with its own error value so the caller can tell them apart),
   and either one aborts the run right there having triggered nothing:

   - `preview-<tag>` exists in Kubernetes phase `Terminating` →
     `preview.ErrTerminating` ("the previous teardown of this preview is still
     in progress; retry once it finishes"). Checked first: a namespace on its
     way out is about to stop existing, so whose it was is the less useful
     thing to say about it.
   - `preview-<tag>` exists carrying a `bifrost/branch` annotation naming a
     **different** branch → `preview.ErrTagCollision`, in an error naming both
     branches and the tag. See "Two branches can derive the same tag" above.

   A namespace for the **same** branch is the ordinary case and passes both:
   re-running `up` is the documented recovery path (see below and Gotchas), and
   the auto-update watcher re-runs `Up` on the recorded branch every time a
   member's SHA moves — a check that fired on the mere presence of a namespace
   would break both, permanently.

   A namespace carrying **no** `bifrost/branch` annotation at all also passes:
   absence isn't evidence of a collision (a preview predating the annotation,
   or one left by a partial or out-of-band run, looks exactly like this), and
   refusing would strand it — nothing but a manual `ib preview down` could get
   past the check again, including the re-run that would repair it. The
   `EnsureNamespace` immediately below writes the annotation, so the state
   heals itself.

   The terminating half is needed because the API server accepts a metadata
   update on a terminating namespace, so `EnsureNamespace` **succeeds**
   against one and
   there is no error to catch. Without the check, tearing a preview down and
   immediately recreating it ran both Cloud Builds and branched Neon against a
   namespace that was disappearing the whole time, dying two and a half
   minutes later at step 5 with a bare
   `namespaces "preview-<tag>" not found`.

   It is a look-then-act and therefore **not** airtight: the namespace can
   enter `Terminating` in the instant after the read, or at any point during
   the minutes that follow (an `ib preview down`, or a bare `kubectl delete`,
   landing mid-create). Kubernetes offers no "create unless terminating"
   primitive to close that with, and the busy set only serializes bifrost's
   own `Up`/`Down`. What the check buys is the common case — recreate
   immediately after teardown — failing in one API call instead of after two
   builds. It refuses rather than waiting: teardown is unbounded in principle,
   and blocking would hold the tag's busy claim for the whole time. Once the
   delete has actually finished, the namespace is simply absent and `up`
   creates it fresh and proceeds normally.

   **Ensure the namespace**: `preview-<tag>`, annotated
   `bifrost/branch`, `bifrost/apps` (comma-joined members),
   `bifrost/source-shas`, and `bifrost/phase: creating` — and, in the same
   write, `bifrost/error`,
   `bifrost/step` and `bifrost/step-since` explicitly cleared to `""`. That
   clearing matters because this write *merges* onto whatever the namespace
   already has: re-running `up` over a previously failed preview (the recovery
   path below) would otherwise show the old run's error and last step for the
   whole retry.

   `bifrost/source-shas` is what step 1 resolved — comma-joined
   `service=sha` pairs in the same style as `bifrost/apps`, e.g.
   `footstrike-api=abc123...,footstrike-dashboard=def456...`. These are
   **full** commit SHAs, not the `preview-<short sha>` image tags the builds
   produce, because they exist to be compared against a later `BranchSHA`
   call by the auto-update watcher. Note *when* it's written: here, at the
   top of the run, from what was resolved — **not** at the end from what
   deployed. A run that fails therefore still records the commit it
   attempted, which is exactly what stops the watcher from retrying a
   doomed build every two minutes forever (see "Following a branch").

   The same write also sets `bifrost/expires-at`: an absolute RFC3339 instant
   (`now + ttl`, where `now` is when the API handler parsed the request — so
   the clock starts when `up` begins, not when the preview becomes usable,
   and a `--ttl 90m` preview
   whose builds take 25 minutes has 65 minutes of ready life) if `ttl` was
   given, or `""` if it wasn't — written
   unconditionally either way, for the identical merge reason the error/step
   fields above are cleared unconditionally. **This means re-running `up`
   (a fresh `POST /api/previews` for the same tag) *without* a `ttl` clears
   any expiry a previous run set for that tag** — there is no way to "leave
   it alone." That's deliberate: the alternative (omit the key when there's
   no `ttl`) would let a stale expiry silently survive a retry, which is a
   worse failure mode than a caller needing to resend the same `ttl` to keep
   it.

   `bifrost/auto-update` (`"true"` or `""`) is written in that same call, and
   is unconditional for that same merge reason: an `up` that doesn't ask for
   auto-update **turns it off** rather than inheriting it.

   The expiry reaching `Up` as an absolute instant rather than a duration is
   what lets the auto-update watcher re-run a preview without extending its
   deadline; `internal/preview`'s `UpOptions` carries both this and the
   auto-update flag, and says so at length.
4. **Build**: get every member an image for the commit step 1 resolved. A
   member whose commit **already has an image** is not rebuilt at all; whatever
   is left is built **concurrently**. Step: one write for the whole stage —
   `building footstrike-api, footstrike-dashboard`, or
   `building footstrike-dashboard (reusing footstrike-api)` when only some
   members need building, or `reusing images for footstrike-api,
   footstrike-dashboard` when none do.

   **The skip is decided off `bifrost/built-images`, never off
   `bifrost/source-shas`**, and the distinction is the whole reason that second
   annotation exists. `source-shas` is written at the *top* of the run, before
   the builds it names have run (step 3 — deliberately, so a failing build
   can't hot-loop the auto-updater). So "the SHA didn't change" does **not**
   mean "an image exists": after a *failed* build the commit is recorded and
   nothing was ever pushed. `built-images` is written only *after* the build
   stage succeeds, so an entry in it means exactly one thing — a
   `preview-<short sha>` image for that commit is in Artifact Registry.

   Its format is comma-joined `service=<full commit>:<short sha>` in
   `bifrost/apps` order, e.g.
   `footstrike-api=c58881f6…:c58881f,footstrike-dashboard=e07e33ab…:e07e33a`.
   Both halves are needed and neither is derived from the other: the **full
   commit** is the only thing comparable against a later `BranchSHA` call
   (that's the skip decision), and the **short SHA** is what becomes the
   image's `preview-<sha>` tag when the build is skipped. `$SHORT_SHA` is in
   fact the first 7 characters of `$COMMIT_SHA` (checked against 200 real
   builds in this project — 200/200), but it is recorded rather than
   recomputed, because the commit a build builds is resolved by *Cloud Build*
   from the branch name at trigger-run time, not from the SHA bifrost got from
   GitHub moments earlier. A commit landing in that window would make a derived
   tag name an image no build ever pushed. Recording what the build reported
   makes the tag exact; pairing it with the commit bifrost resolved leaves that
   same window fail-safe (the next run sees a difference and rebuilds).

   A member is skipped **iff** its currently-resolved commit equals the commit
   in its `built-images` entry, and then the recorded short SHA is what step 7
   renders — a real reuse of the existing image, not a rebuild in disguise. A
   first-ever create has nothing recorded and therefore builds everything, as
   it always did.

   Each build still runs its `{service}-preview-build` trigger and is polled to
   a terminal status (`buildPollInterval`, 10s). What changed is that
   `RunTrigger` returns as soon as Cloud Build accepts the build, so **all** the
   triggers are started before **any** of them is waited on: an n-member preview
   costs one build's wall time rather than n. Builds are bounded by member count
   (rarely more than two or three), so there is no worker pool. Nothing is ever
   cancelled — a build left running in Cloud Build after the run has failed is
   harmless, and cheaper than the machinery to stop it.

   A build that ends in anything but `SUCCESS` fails the preview with the
   build's **console log URL** in `bifrost/error`: `build for footstrike-api:
   build ended with status FAILURE: https://console.cloud.google.com/cloud-build/builds/…`.
   The link is the point — a bare status names a symptom and leaves you
   hand-navigating the Cloud Build console guessing which run was yours. If
   Cloud Build returned no log URL, the message carries the build ID instead
   (`… status FAILURE (build <id>)`, enough for `gcloud builds log <id>`)
   rather than a link to nowhere. The URL only: build *logs* are never
   fetched, for the same reason pod logs aren't (step 8).

   With the builds concurrent, the failure names **which member** failed, and
   it is always the first in `bifrost/apps` order among those that failed —
   deterministic, rather than whichever goroutine lost first. Only one failure
   can be the `bifrost/error` annotation, which has to stay a single readable
   line; if a second member's build failed too, it is logged rather than
   dropped. Nothing is recorded in `bifrost/built-images` for a run whose build
   stage failed — not even for a sibling whose own build succeeded, since a run
   that failed is not evidence about which images survived it. The cost is one
   redundant rebuild on the retry.
5. **Neon branch** (step: `branching databases`): for every member with a
   registry `neon:` reference,
   find-or-create a `preview-<tag>` branch off that project's default branch
   (empty parent ID) and fetch its connection URI for the registry's
   `database`/`role`. A service with no `neon:` entry (e.g.
   `footstrike-dashboard`) is skipped.
6. **Copy secrets** (step: `copying secrets`): each Neon-backed member's
   `{svc}-staging-secrets` Secret
   Store CSI secret is copied into the preview namespace with `DATABASE_URL`
   overridden to the new branch's URI, plus the shared wildcard TLS
   secret (`preview-footstrike-run-tls`, provisioned once in the `previews`
   namespace — copied rather than reissued per preview, since Let's
   Encrypt's duplicate-certificate limit is 5/week for an identical
   `dnsNames` set).
7. **Render and apply** (step: `applying manifests`): for each member, fetch
   its `k8s/` tree, parse its
   `staging/configmap-env.yaml` (empty map if it has none), compute the final
   env via the registry's templates (see "The registry" below), build a
   generated kustomize overlay atop the fetched `k8s/base`, and
   server-side-apply the result into the preview namespace.
8. **Wait for pods** (step: `waiting for pods`): poll the namespace's pods (`podPollInterval`, 5s) until
   every member has at least one Deployment-managed pod and all of them
   report ready, bounded at `podReadyTimeout` (5 minutes — a cold-node image
   pull plus a branch's migrations, well inside the API layer's 30-minute
   per-run budget). A crash-looping container fails the preview immediately
   rather than waiting out the bound; anything else that hasn't converged
   (including a member with **no** pods at all) fails at the bound. Either
   way the `bifrost/error` is sanitized down to the member's name and the
   pod's own reason — e.g. `footstrike-api not ready: migrate initContainer
   CrashLoopBackOff`. Pod *reasons* are safe to surface; pod *logs* are not,
   and are never fetched.

   Only pods running the image this run just applied count. A rolling update
   keeps the previous generation's pods alive until the new ones are ready,
   so a re-`Up` sent to *fix* a crash-looping preview still has the broken
   pod in the namespace the whole time — judging readiness on it would fail
   every recovery attempt. (A re-run that rebuilds an unchanged commit
   produces the same image, and those pods are judged: they're running
   exactly what was applied.)
9. **Mark ready**: `bifrost/phase: ready`, `bifrost/error: ""`,
   `bifrost/step: ""`, `bifrost/step-since: ""` — one write, so a finished
   preview never goes on displaying the last step it ran.

`ready` therefore means "every member has running, ready pods", not merely
"the manifests were accepted" — API consumers (`ib preview up`, the Previews
tab) can treat it as "usable". This matters most for the `migrate`
initContainer (see the registry's `migrate:` key below): a failed migration
leaves a pod in `Init:CrashLoopBackOff` with the app container never
starting, which an apply-then-declare-ready flow would have reported as a
perfectly healthy preview.

Any failure after step 3 sets `bifrost/phase: failed` with a sanitized
`bifrost/error` annotation (never a secret value) naming the cause, and
**deliberately leaves `bifrost/step`/`bifrost/step-since` in place** — the last
step is half the diagnostic (which stage it died in, and when), so it's kept
alongside the error rather than cleared; see Gotchas for how that reads. A
failure in steps 1–2, and either of step 3's two refusals, returns before the
namespace is written to at all, so none of them leaves a zombie namespace —
or, in the collision case, scribbles anything onto the preview that already
owns the tag.

`Up` is safe to re-run: `EnsureNamespace`/`CopySecret`/apply are idempotent,
and Neon branch creation is scan-then-create. This is also the recovery path
for a stuck `creating` (see "Gotchas"), and it is the path step 4 is tuned
for — a re-run rebuilds only the members whose commits actually moved, so
re-running `up` on a branch nobody has pushed to costs no builds at all.

### What a skipped build does not get you

Two consequences are accepted rather than solved, and both are worth knowing
before you go looking for them:

- **A skipped member keeps its old base image.** Same commit and same
  Dockerfile means the app layer is byte-identical, but the
  `python:3.13-slim` / `alpine:latest` underneath it is whatever was current
  when the image was first built. A preview that has been re-run for a week
  without that member changing is a week behind on base-image patches. Force a
  rebuild by pushing a commit to that repo, or tear the preview down and
  recreate it (a fresh namespace has no `bifrost/built-images` to reuse).
- **A skipped build trusts that the image is still in Artifact Registry.**
  There is no cleanup policy today, so nothing deletes preview images; if one
  were deleted by hand, the skip would apply a Deployment naming a tag that
  isn't there. The failure is clean — `ImagePullBackOff`, caught by the
  readiness wait (step 8), which fails the preview with that as the reason —
  but note the interaction: **a future image-cleanup policy would need a
  retention window longer than previews live**, or it would start breaking
  re-runs of previews that are still in use.

### Progress annotations

Steps 4–7 each announce themselves first, by writing `bifrost/step` (operator-
facing prose — never a secret; it lands in an annotation any cluster reader can
see) and `bifrost/step-since` (RFC3339). One write per stage, not per poll
tick: a two-minute build costs one annotation, and elapsed time is computed
from `step-since` by whoever reads it. Step writes are best-effort — a failed
annotation is logged and swallowed, never a reason `Up` fails — and bounded by
a tighter timeout than the failure annotation, since they sit inline on the
creation path (`stepAnnotateTimeout` vs `failAnnotateTimeout` in
`orchestrator.go`).

The build stage is one step for the whole stage, not one per member. It used
to read `building footstrike-api (1/2)`, rewritten as each build started; both
halves of that stopped being true once the builds became concurrent (there is
no first-then-second) and skippable (n is not the member count). It now names
what is actually happening — see step 4 for the three shapes it takes.

`bifrost/built-images` is written in its own call, after the build stage
succeeds. Unlike a step it is a fact rather than prose, and unlike
`bifrost/phase` a failure to write it is logged and swallowed: it can only ever
cost one redundant rebuild (the next run finds no record and builds), which is
strictly better than failing a preview whose images are all built and pushed
over a bookkeeping write.

Membership resolution (step 1) is not narrated: it finishes before the
namespace exists, so there's nothing to annotate yet.

Both the API (`step`/`stepSince`/`error`) and the Previews tab surface these;
the tab polls on its fast cadence whenever any preview is `creating`.

## Lifecycle (`Down`)

Deletes the `preview-<tag>` namespace, then — for **every** registry service
with a `neon:` reference, not just the tag's current members (a re-created
preview may have changed membership since its branch was created, and this is
the only record `Down` has) — best-effort deletes a `preview-<tag>` Neon
branch if one exists. Every step runs regardless of earlier failures; all
errors are joined and returned together.

The namespace-then-Neon order is kept deliberately: the reverse would leave a
window where a preview's pods run against a database that's already gone. The
cost of keeping it — a teardown interrupted between the two halves leaves a
branch nothing can name — is covered from the other side, by the orphan sweep
below.

## Automatic expiry

A preview created with a `ttl` carries `bifrost/expires-at`; a goroutine
inside prod bifrost (`Orchestrator.RunReaper`, started in `cmd/bifrost/main.go`
next to the signal-handling setup, gated on preview config being present)
wakes every `previewReapInterval` (one hour) and calls `PurgeExpired`
(`internal/preview/reaper.go`), which reclaims each past-due preview through
the same `Orchestrator.Down` a manual `ib preview down` would use — namespace
and any Neon branch both go, and Down's own idempotency means a preview
double-swept (e.g. by two `bifrost` replicas) is harmless.

The sweep runs a full interval after bifrost starts, never immediately: a
purge firing on every restart (spot-node preemptions are routine in this
cluster) would be surprising and isn't needed for correctness.

`PurgeExpired` reclaims only on unambiguous evidence a preview is past due.
Every other case is skipped — silently, and never logged as an error:

- `bifrost/expires-at` is absent, empty, or doesn't parse as RFC3339 — a
  value bifrost can't read means "no expiry," never "expired."
- the expiry is still in the future.
- the namespace is already `Terminating` — its teardown is already underway.
- `bifrost/phase` is `creating` — an `Up` is still running; deleting the
  namespace out from under it would race a create that may be minutes from
  finishing. See the first Gotcha below for what this means for a preview
  stuck in `creating`.
- the tag is currently `Busy` — an `Up` or `Down` already holds it.
- the namespace is labelled `bifrost/preview=true` but its name doesn't have
  the `preview-` prefix — off-convention, so there's no tag to safely derive
  (logged as a warning, but still never acted on).

Those rules are judged **twice**: once against the namespace list the sweep
starts from, and again against a fresh `GetNamespace` taken immediately before
each teardown. A sweep isn't instantaneous, and an `up` that starts *and
finishes* while it works through earlier namespaces would otherwise be
reclaimed on the strength of the expiry it had when the sweep began — the busy
set covers an `up` still running, not one that has already completed. A preview
whose fresh copy has a renewed (or cleared) expiry, or is back in `creating`,
is left alone; one whose namespace has vanished by then is skipped silently
(something else tore it down). A namespace that can't be re-read at all is
skipped too, but that failure *is* reported — an unreadable namespace is no
evidence of anything, least of all a reason to delete it.

One preview's teardown failing doesn't abort the sweep — errors accumulate
across the whole pass and are joined, the same way `Down` itself accumulates
Neon errors — and every reclaimed tag is logged with how long it had been
overdue; a sweep that reclaims nothing logs at debug level only.

## Following a branch (auto-update)

A preview created with `"autoUpdate": true` re-deploys itself when new commits
land on its branch. It is **opt-in and off by default**: every other preview
is the snapshot it always was, and pushing to its branch does nothing.

A second goroutine inside prod bifrost (`Orchestrator.RunAutoUpdates`, started
in `cmd/bifrost/main.go` next to the reaper and gated on the same
"orchestrator is fully wired" condition) wakes every `autoUpdateInterval` —
**two minutes** — and calls `PollAutoUpdates` (`internal/preview/autoupdate.go`).
Like the reaper, the first poll happens a full interval after startup, never
immediately.

For each preview namespace annotated `bifrost/auto-update: "true"`, one poll:

1. reads its members from `bifrost/apps` and the commit each was deployed
   from out of `bifrost/source-shas`;
2. calls `BranchSHA` once per member;
3. if **any** member's branch SHA differs from the recorded one, re-runs the
   ordinary `Orchestrator.Up` — the same one `ib preview up` runs, so every
   member is re-applied and the Neon branch (with its data) is found rather
   than re-created. There is no partial update at the *deploy* level: `Up` is
   all-or-nothing.

   It is partial at the *build* level, and that is the point. A push to one of
   a preview's repos used to rebuild every member of that preview, every two
   minutes' worth of pushes; step 4's skip means only the repo that moved is
   rebuilt, and the rest are re-applied from the images they already have.

It's polling rather than a GitHub webhook, deliberately. A tick only ever sees
where the branch points *now*, so five pushes in a minute coalesce into one
redeploy for free (a webhook would fire five times and need a debounce queue);
it adds no new unauthenticated public endpoint to an internet-facing service;
and it needs no per-repo webhook configuration. The cost is one `BranchSHA`
call per member of each opted-in preview per tick — nothing at all for the
previews that didn't opt in.

**A re-run preserves, never extends.** The watcher parses the namespace's
existing `bifrost/expires-at` and passes that same absolute instant back
through, so an auto-updating preview expires exactly when it was always going
to. (This is why `Up` takes an instant rather than a TTL: a duration would be
recomputed from "now" on every refresh, and an actively-developed preview —
the only kind that auto-updates — would keep renewing itself and never expire
at all.) It also passes `autoUpdate` back as true, so a preview keeps
following its branch after the first refresh. Nothing else the user set is
touched.

Every skip is silent, and none is an error:

- no `bifrost/auto-update: "true"` — the default, and almost every preview.
  Only the literal `"true"` counts; `""` (what an `up` that didn't ask for it
  writes) and anything else read as off.
- the namespace is `Terminating` — it's being torn down.
- `bifrost/phase` is `creating` — an `up` is already running.
- the tag is `Busy` — an `up` or `down` holds it.
- the namespace is labelled `bifrost/preview=true` but isn't named
  `preview-<tag>` — no tag to derive. Unlike the reaper's equivalent case this
  isn't even logged: at one pass every two minutes, a warning nothing can act
  on would be 720 log lines a day.
- `bifrost/apps` lists no members — nothing to compare.

One further skip **is** logged, at info: a member whose branch has been
**deleted** from its repo (`ErrNoBranch`). Deleting the branch after a merge
is the normal end of a preview's life rather than a failure, so it isn't
reported as an error — but the preview then sits at whatever it last deployed
until someone runs `ib preview down`, and the log line is the only notice of
that.

**Every refresh is logged**, at info, with the tag, the branch, and which
member's SHA moved (`footstrike-api <old> -> <new>`) — written *before* the
re-run starts, so a run that dies mid-flight is still accounted for. This
redeploys an environment someone may be using; it is never silent.

One preview's failure doesn't stop the others: errors accumulate across the
pass and come back joined, exactly as the reaper's do. A failure to *list*
namespaces aborts the pass — with nothing listed there is nothing to compare.

On the Previews tab, an opted-in preview's BRANCH cell reads
`my-branch · auto` (and the same marker appears on the mobile card's meta
line). It's folded into that cell rather than given a column of its own —
`jobs-grid` is a fixed six-column layout shared with the Jobs tab — the same
way the remaining-time text is folded into AGE. A preview that didn't opt in
renders exactly as it did before.

### What auto-update does not do

- **It never retries the same commit.** Because `Up` records
  `bifrost/source-shas` at the top of the run (step 3 above), a run whose
  build fails has still recorded the SHA it attempted, so the next tick sees
  no difference and leaves the preview alone. A *newer* push produces a new
  SHA and is picked up normally. Without that ordering, a broken commit would
  rebuild every two minutes indefinitely.
- **A failed auto-update marks a previously-working preview `failed`.** The
  re-run is an ordinary `Up`, so any failure in it sets `bifrost/phase:
  failed` and a `bifrost/error` on a preview that was `ready` a moment ago —
  nobody asked for that redeploy, and nobody is watching `ib preview up`
  output when it happens. Nothing is torn down (no namespace delete, no Neon
  branch delete), and if the run failed before the apply stage the previous
  generation's pods are still running and still serving. Re-running
  `ib preview up <branch>` is the recovery path, exactly as for any other
  failed preview.
- **It does not notice membership *changes*.** The comparison only asks about
  the members already recorded on the namespace, so a repo that *newly* gained
  the branch won't be added to a running preview (nor will one that lost it be
  dropped — that member is instead reported as "branch is gone" and the whole
  preview stops updating). Changing a preview's membership needs a manual
  `ib preview up <branch>` re-run, which re-resolves it from scratch.
- **It doesn't extend an expiry**, so an auto-updating preview created with
  `--ttl 8h` is still reclaimed 8 hours after it was *created*, however many
  commits it has absorbed since.

## Orphaned Neon branches

Each tick of that same hourly loop runs a second pass —
`Orchestrator.PurgeOrphanedBranches`, immediately after `PurgeExpired`, on the
same detached `sweepBudget`-bounded context — that deletes `preview-<tag>` Neon
branches which no longer have a preview namespace to belong to.

It exists for the one failure `Down` can't recover from itself. `Down` deletes
the namespace first and the Neon branches second, so an interruption in between
(the process exiting on a spot-node preemption; `sweepBudget` firing
mid-teardown; a Neon API error) leaves a branch **nothing can ever find again**
— the preview is gone from `ListNamespaces`, so no later expiry sweep and no
repeat `ib preview down` can name it. What's left is billed and invisible. The
fix reclaims from the Neon side rather than reversing `Down`'s order, which
would trade this for a window where a preview's pods run against a database
that has already been deleted.

It's cheap: the work is O(distinct Neon projects), not O(previews).
`ListBranches` returns a whole project in one call, and `registry.yaml` names
two distinct projects today (`footstrike-api`'s and `identity`'s), so a full
orphan check is **two extra HTTP calls per sweep** however many previews exist.
Projects are de-duplicated by ID, so two services sharing one Neon project
still cost one call.

A branch is deleted only when every one of these holds. Each miss is skipped
silently and none is an error, matching `PurgeExpired`:

- its name starts with `preview-` and has something after it. A branch named
  `main`, `dev`, or exactly `preview-` is never touched.
- the derived tag has **no** preview namespace. `Terminating` namespaces count
  as live — the namespace still exists, so the branch still belongs to the
  teardown that's removing it.
- the tag isn't `Busy` — an `Up` or `Down` holding it means a teardown in
  flight, not one that died.
- the branch is at least **one hour old** (`minOrphanAge`).

Every deletion is logged at info with its project, branch name and age; this
deletes a database branch, so it is never silent. A project whose
`ListBranches` fails is reported and skipped, and the remaining projects are
still swept. A failure to list *namespaces*, by contrast, aborts the whole pass
without deleting anything — an empty live set alongside a full branch list is
indistinguishable from "every preview is an orphan."

Each project's total branch count — everything that same `ListBranches` call
returned, not just the `preview-*` ones the rules above go on to judge — is
also logged, at debug, on every sweep. Nothing else in bifrost counts
branches, storage, compute, or build minutes against Neon's own quotas, so
this is the only visibility into where a project stands *before* a limit is
hit, rather than only after (as an opaque Neon error once a create fails
against one). It's debug rather than info for the same reason "sweep
reclaimed nothing" is: it's the routine, every-project-every-hour case, not
something that happened.

**The ordering invariant this rests on.** "A branch with no namespace is an
orphan" is only true because `Up` calls `EnsureNamespace` **before**
`branchNeonDatabases`: a preview gets its namespace before it gets its Neon
branch, so there is no window in which the branch exists alone and an in-flight
create can never look like an orphan. **Reordering those two stages of `Up`
would turn this sweep into a data-destroying bug**, and the one-hour age floor
is the only thing that would slow it down. That floor is insurance for exactly
that, not a fix for any race that exists today — there's a comment saying so at
the detection site in `reaper.go`.

**It will collect a branch you made by hand.** The sweep has no way to tell a
branch bifrost created from one a human created: any branch named `preview-*`
in `footstrike-api`'s or `identity`'s Neon project, older than an hour, with no
matching `preview-<tag>` namespace in the cluster, is deleted on the next tick.
If you need a scratch branch in one of those projects to survive, **do not name
it `preview-something`** — any other prefix is ignored entirely.

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
    migrate: ["alembic", "upgrade", "head"]  # omit for apps with no migrations
    env:
      ENV: staging
      PUBLIC_API_BASE_URL: "{{ url self }}"
      PUBLIC_DASHBOARD_BASE_URL: "{{ url footstrike-dashboard }}"
      IDENTITY_PROVIDER_URL: "{{ internalUrl identity }}"
      JWT_ISSUER: "{{ url identity }}"
    required: [...]                       # keys that must render non-empty
```

`migrate:`, when present, is run as a **`migrate` initContainer** before the
app container starts — same image (including the `preview-<sha>` tag
override) and same `envFrom`, so it sees `DATABASE_URL` pointing at this
preview's fresh Neon branch. A preview branches staging's database at
staging's revision, so a branch carrying a new migration would otherwise come
up against an out-of-date schema. Omitting `migrate:` renders no
initContainers at all. Because the whole pod is blocked on it, a failed
migration is what step 8's readiness wait is most often reporting.

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
3. **A `{registry key}-preview-build` manual-invocation Cloud Build trigger**
   in infra's Pulumi (`__main__.py`'s `for preview_key, preview_repo in
   [...]` loop, pinned to `cloudbuild-preview.yaml` on the repo's `main`) —
   add a `(key, repo)` pair to that list and `pulumi up`. The trigger's
   *name* is always the registry key — matching what
   `Orchestrator.TriggerIDs` looks up (`buildMembers` does
   `o.TriggerIDs[svc+"-preview-build"]` with `svc` a registry key, never a
   repo) — while the GitHub *repo* it builds from is sourced independently,
   from the pair's second element. For every previewable service today the
   two happen to be the same string, but that's a coincidence, not a rule:
   `asset-manager` is already a fleet member whose registry entry sets
   `repo: asset_manager` (`internal/registry/registry.yaml`), so onboarding
   *it* to previews means adding `("asset-manager", "asset_manager")` to the
   loop — the key first, the repo second, never the repo in both slots. Get
   this backwards and the trigger still builds fine (Cloud Build doesn't
   care what it's named), but bifrost's lookup by registry key finds no
   `asset-manager-preview-build` trigger, and the build fails — after the
   preview's namespace already exists, leaving a zombie `failed` preview
   behind it. The GCP IAM prod bifrost needs to run any preview-build
   trigger (`cloudbuild.builds.editor` + `actAs` on the Cloud Build SA) is
   already granted once, not per app.

That's it — no Go code, no new bifrost endpoint, no orchestrator change.

## Gotchas

- **Preview build triggers are manual-invocation only** — nothing in Cloud
  Build or GitHub fires on push or PR. A branch with no completed
  `{service}-preview-build` run has no image for `up` to deploy. The one
  thing that runs a preview build without a human asking is bifrost's own
  auto-update watcher (see "Following a branch"), and only for a preview that
  explicitly opted in — it invokes the same manual trigger.
- **Two branches can want the same preview, and the second one loses.** Tags
  are a lossy slug of the branch name (30-character cap, and `/`/`_`/space all
  fold to `-`), so `feat/foo` and `feat_foo` — or two long branches differing
  only past character 30 — are the same preview as far as bifrost is
  concerned. The second `up` is refused rather than merged, but only in
  bifrost's logs: the POST already returned `202`, so what you see is your tag
  sitting there against somebody else's branch. Rename your branch or tear
  their preview down; see "Two branches can derive the same tag" above.
- **A stuck `creating` phase** (e.g. bifrost restarted mid-create) recovers
  by re-running `ib preview up <branch>` — safe, since every stage is
  idempotent, and cheap, since a member whose commit already has an image
  isn't rebuilt (see step 4). A run that died *before* its builds finished has
  nothing recorded and does pay for them. The expiry sweep does
  not help here either: `PurgeExpired` treats `phase: creating` as "still in
  flight" and skips it unconditionally — not merely defers it — so a preview
  whose `Up` died with the process (a spot-node preemption is the routine
  cause) sits at `creating` forever, however far past its `expiresAt`, until
  a human intervenes. It's visible as such in the UI and `ib preview list`;
  `ib preview down` (which doesn't consult phase at all) clears it either
  way.
- **An auto-updating preview can go from `ready` to `failed` on its own.**
  Nobody ran a command; a teammate pushed a commit that doesn't build, and two
  minutes later the preview is `failed` with that build's error. See "What
  auto-update does not do" above for what survives (everything — nothing is
  torn down) and how to recover (re-run `ib preview up`).
- **Expiry is strictly opt-in.** A preview created without a `ttl` never
  expires — there's no implicit default — so most previews, including every
  one that predates this feature, simply carry no `bifrost/expires-at` and
  sit until someone runs `ib preview down`.
- **A preview can outlive its recorded expiry by up to an hour.** The sweep
  (`PurgeExpired`) runs once per `previewReapInterval` (an hour), and the
  first sweep after any bifrost start or restart is delayed a full interval
  rather than firing immediately (see "Automatic expiry" above) — so a
  preview reading "expired" in the UI or past its `expiresAt` in the API
  hasn't necessarily been torn down yet.
- **A teardown that dies after the namespace delete orphans the Neon branch,
  and only the hourly orphan sweep will find it.** `Down` deletes the namespace
  first and the `preview-<tag>` Neon branches second. Once the namespace is gone
  the preview is no longer in `ListNamespaces`, so no expiry sweep can see it
  and no `ib preview down` can name it — whatever the second half didn't finish
  stays unfinished as far as *that* path is concerned. A Neon list/delete API
  error does it; so does the process exiting mid-teardown (`RunReaper` runs each
  sweep on a context detached from shutdown to narrow that window, but nothing
  waits on the goroutine, so a fast exit can still cut one short). What's left
  behind is a live Neon branch: billed, and invisible to the UI, `ib preview
  list` and bifrost generally, since all three list *namespaces*. Nor is there
  much of a signal — teardown runs in a background goroutine and `DELETE
  /api/previews/{tag}` answers `202` before it starts, so `ib preview down`
  reports success either way. What closes the loop is `PurgeOrphanedBranches`
  (see "Orphaned Neon branches" above): it reclaims the branch from the Neon
  side on the next hourly tick, up to an hour later, and logs it. It is not
  instant, and it is deliberately conservative — a branch younger than an hour
  is left for the next pass. To check for strays yourself, list the branches of
  every Neon project in `internal/registry/registry.yaml` (the
  `preview.neon.project` keys — today `footstrike-api`'s and `identity`'s) and
  look for `preview-*` branches with no matching `preview-<tag>` namespace.
- **The orphan sweep will delete a hand-made `preview-*` Neon branch.** It
  can't distinguish one bifrost created from one you created; the name and the
  absent namespace are all it goes on. Name scratch branches in those two
  projects anything that doesn't start with `preview-`.
- **A `failed` preview still expires, on schedule.** Marking a preview failed
  writes only `bifrost/phase` and `bifrost/error` — `bifrost/expires-at`
  survives untouched — and the only phase the sweep skips is `creating`, so a
  preview created with a `ttl` that then failed to build is reclaimed exactly
  like a healthy one, Neon branch included. Usually that's the best case (a broken
  preview is the one least worth keeping), but if you're debugging a failed
  build, note that the namespace you're reading `kubectl` output from will
  disappear at its `expiresAt`. Re-running `up` without a `ttl` clears the
  expiry (see below); since the build that failed recorded no image, that
  member is rebuilt, but any sibling whose build had succeeded on an earlier
  run is reused.
- **Re-running `up` without a `ttl` clears any expiry the tag already
  had** — it does not leave a previous run's `bifrost/expires-at` in place.
  See step 3 of the `Up` lifecycle above for why. Recovering a stuck or
  failed preview that you want to keep expiring on schedule means resending
  the same `ttl`, not omitting it.
- **A `failed` preview keeps showing its last step, on purpose.** `bifrost/step`
  is cleared on `ready` but retained on `failed`, so the row reads "failed ·
  building footstrike-api, footstrike-dashboard — build for footstrike-api: build ended with
  status FAILURE: https://console.cloud.google.com/cloud-build/builds/…" for
  as long as that namespace exists. It describes the run that failed, not something in
  flight. Re-running `up` clears both at the start of the retry (step 3), and a
  preview whose namespace is `Terminating` reports neither — the annotations
  are still on the namespace, but the API record and the UI row suppress them,
  so a teardown never reads as a build in progress.
- **A `failed` phase from step 5 (Neon branch) carries a sanitized excerpt of
  Neon's own error body**, not just the bare HTTP status — e.g.
  `neon POST /projects/aged-river-81935268/branches returned 422:
  {"code":"branch_limit_exceeded","message":"..."}`. Neon puts the actual
  explanation (a free-tier branch limit, a storage limit, or any other quota)
  in the response body, and nothing in bifrost otherwise counts branches,
  storage, compute, or build minutes against those quotas — so that excerpt is
  the only signal such a bound was ever reached. It's whitespace-flattened and
  capped (`internal/neon`'s own copy of the `sanitizeReason` convention, since
  importing `internal/preview` back into `internal/neon` would cycle) before
  it can reach `bifrost/error`, which is served over the API and rendered in
  the browser. The one exception is the connection-URI call: its success
  response is a credentialed connection string, so its error path never reads
  the response body at all, regardless of what it might contain.
- **A `failed` phase from the readiness wait means the pods didn't come up**,
  not that anything bifrost did went wrong — the namespace, secrets, Neon
  branch and manifests are all in place, so `kubectl -n preview-<tag>
  describe pod` (and, for a `migrate initContainer` failure, `kubectl logs
  ... -c migrate`) is where the actual cause lives. bifrost deliberately
  never fetches pod logs itself: unlike a pod *reason*, a log line can carry
  anything, including secrets, and `bifrost/error` is served over the API.
  Fix the branch and re-run `ib preview up`; teardown (`ib preview down`) is
  unaffected either way.
