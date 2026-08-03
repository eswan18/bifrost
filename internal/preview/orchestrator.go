package preview

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eswan18/bifrost/internal/config"
	"github.com/eswan18/bifrost/internal/gcb"
	"github.com/eswan18/bifrost/internal/github"
	"github.com/eswan18/bifrost/internal/kube"
	"github.com/eswan18/bifrost/internal/neon"
	"github.com/eswan18/bifrost/internal/registry"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ErrBusy reports that an Up or Down for this tag is already in flight. The
// API layer maps this to 409.
var ErrBusy = errors.New("preview: orchestration already in progress for this tag")

// ErrTerminating reports that the tag's namespace still exists but Kubernetes
// is in the middle of deleting it, so this Up cannot be started. Distinguished
// from ErrBusy — and from every other Up failure — so the API layer and the
// CLI can say "still tearing down, retry shortly" rather than surfacing a
// generic failure: it is a wait-and-retry condition, not a broken preview.
var ErrTerminating = errors.New("preview: the previous teardown of this preview is still in progress; retry once it finishes")

// ErrTagCollision reports that the tag's namespace already exists and was
// created for a DIFFERENT branch. TagForBranch is many-to-one (see tag.go), so
// two branch names can derive the same tag; without this the second Up would
// silently adopt the first's environment — its namespace, and its Neon branch
// with whatever migrations the first branch applied — and a later Down on that
// tag would destroy both.
//
// Distinguished from ErrBusy and ErrTerminating — and from every other Up
// failure — because it is neither a wait-and-retry condition nor a broken
// preview: nothing will clear it on its own, and it needs a human decision
// (rename the branch, or tear the other preview down). The error wrapping it
// names both branches so that decision can be made from the message alone.
var ErrTagCollision = errors.New("preview: this preview tag already belongs to a different branch; rename the branch, or tear the existing preview down")

// buildPollInterval is how often Up polls Cloud Build for a preview build's
// status. A var (not a const) so tests can shrink it instead of waiting on
// the real interval.
var buildPollInterval = 10 * time.Second

// podReadyTimeout bounds Up's final wait for the applied manifests' pods to
// actually come up. Five minutes covers a cold-node image pull plus a
// branch's alembic migrations (the "migrate" initContainer) with room to
// spare, while staying far inside the API layer's 30-minute per-run
// goroutine budget, so a wedged preview surfaces as a failed one long before
// the caller's own ceiling fires.
//
// podPollInterval is how often that wait re-lists the namespace's pods.
// Deliberately tighter than buildPollInterval's 10s: a build takes minutes
// and a poll costs a Cloud Build API call, whereas pods flip to ready in
// seconds and this poll is what stands between a finished preview and the
// user being told about it. 60 List calls over the full bound is nothing
// against the client's 50 QPS budget.
//
// Both are vars (not consts) so tests can shrink them instead of waiting on
// the real values.
var (
	podReadyTimeout = 5 * time.Minute
	podPollInterval = 5 * time.Second
)

const (
	// crashLoopBackOff is the one container waiting reason the readiness
	// wait treats as terminal: the kubelet only reports it after the
	// container has already failed and been restarted, so waiting out the
	// rest of the bound would just delay a verdict that's already in.
	crashLoopBackOff = "CrashLoopBackOff"
	// podInitializing is what the kubelet reports for an app container
	// while the pod's init containers are still running. It says nothing
	// about the app itself, so it's never used as a diagnosis when an init
	// container has a real reason of its own to report.
	podInitializing = "PodInitializing"
)

// Orchestrator composes the preview control plane's clients into the full
// creation (Up) and teardown (Down) flows. It has no constructor: every
// field is a plain dependency the caller wires up (main.go), so a zero-value
// Orchestrator{Cfg: ..., Kube: ..., ...} literal is exactly how it's built —
// the busy-mutex map lazily initializes itself on first use.
type Orchestrator struct {
	Cfg        *config.Config
	Kube       kube.Client
	GitHub     github.Client
	Neon       neon.Client
	Builds     gcb.Client
	TriggerIDs map[string]string // {svc}-preview-build → Cloud Build trigger ID
	// Registry is every previewable service's Neon reference and env
	// wiring (see registry.yaml); Up and renderAndApply both thread it
	// into envConfigFor. Normally populated once at startup via
	// LoadRegistry() (main.go) rather than reloaded per-request.
	Registry Registry
	// Fleet is the fleet-wide registry (internal/registry) -- every
	// service's repo name and public URLs, not just the previewable
	// subset Registry narrows to. resolveMembers/renderAndApply use it for
	// RepoFor; envConfigFor threads it through to the preview cascade's
	// step 3 (a non-member service's staging URL fallback). Normally the
	// same value main.go loaded via registry.Load() before narrowing it
	// into Registry via FromFleet.
	Fleet registry.Registry

	mu   sync.Mutex
	busy map[string]bool
}

// acquire claims tag for the caller, returning false if it's already
// claimed. Up and Down share the same claim space: a tag mid-creation can't
// be torn down (or re-created) concurrently.
func (o *Orchestrator) acquire(tag string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.busy == nil {
		o.busy = map[string]bool{}
	}
	if o.busy[tag] {
		return false
	}
	o.busy[tag] = true
	return true
}

func (o *Orchestrator) release(tag string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.busy, tag)
}

// Busy reports whether an Up or Down for tag is currently in flight.
func (o *Orchestrator) Busy(tag string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.busy[tag]
}

// BusyTags returns every tag currently claimed, sorted so callers (and tests)
// get a stable order.
//
// The enumerating counterpart to Busy, and it exists because a claim can
// outlive — or precede — the namespace it belongs to, which makes it
// invisible to anything that reads the cluster. Down holds the tag after
// DeleteNamespace has returned while it works through the Neon branches, and
// Up holds it during membership resolution before the namespace exists at
// all. In both windows `bif preview list` and the Previews tab, which see
// namespaces and nothing else, report the preview as simply absent while a
// concurrent up/down is told the tag is busy. The read path uses this to
// surface those tags instead (see internal/web's assemblePreviews).
func (o *Orchestrator) BusyTags() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	tags := make([]string, 0, len(o.busy))
	for tag := range o.busy {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags
}

// UpOptions is everything about a preview run that isn't the branch itself.
// A struct rather than positional parameters because both fields are
// optional, and their zero values are the defaults every caller that doesn't
// care about them wants: no expiry, no auto-update.
//
// ExpiresAt is an absolute instant, NOT a TTL duration, and that distinction
// is load-bearing rather than stylistic. The auto-update watcher
// (PollAutoUpdates) re-runs Up for a preview that already has an expiry, and
// it must preserve that expiry exactly. If this were a duration, every
// re-run would recompute now+ttl and push the deadline forward, so an
// actively-developed preview — precisely the kind that auto-updates — would
// keep renewing itself and never expire at all. Passing the instant the
// preview is already due at lets a re-run carry it through untouched. The
// now+ttl computation therefore lives in the one place a *user-supplied* TTL
// enters the system: the API handler (internal/web/previews_mutate.go),
// which already parses and validates the ttl string.
//
// Zero ExpiresAt means "no expiry" and, because the namespace write merges,
// actively CLEARS any expiry a previous run recorded for this tag (see
// expiresAtAnnotation). AutoUpdate false clears bifrost/auto-update the same
// way, for the same reason.
type UpOptions struct {
	// ExpiresAt is when the reaper may reclaim this preview. Zero means it
	// never expires, which is the default.
	ExpiresAt time.Time
	// AutoUpdate opts the preview into following its branch: the watcher
	// re-runs Up whenever a member's branch SHA moves. False is the default.
	AutoUpdate bool
}

// Up runs the full preview creation flow for branch to completion: resolve
// membership, stand up (or update) the namespace, build+branch+secret+render
// each member, wait for the resulting pods to actually report ready, and
// only then mark the namespace ready.
//
// That wait is what bifrost/phase=ready means: not "the manifests were
// accepted" but "every member has running, ready pods". Applying a
// Deployment is not evidence its pods can start — a branch whose migrations
// fail leaves the "migrate" initContainer in Init:CrashLoopBackOff and the
// app container never runs at all, which the old apply-then-declare-ready
// flow reported as a perfectly healthy preview. Consumers of the API
// (`bif preview up`, the Previews tab) rely on this: ready means usable.
//
// Every stage past EnsureNamespace that fails — the readiness wait
// included — marks the namespace bifrost/phase=failed with a sanitized
// bifrost/error annotation before returning; stage-1 validation failures
// (bad membership, an unresolvable dashboard triple, a registry-declared Neon
// parent branch that doesn't exist), and both pre-flight refusals that
// immediately precede EnsureNamespace (a still-Terminating namespace, and one
// another branch already owns — see refuseUnusableNamespace), all return
// before the namespace is ever touched, so they never leave a zombie behind.
//
// Idempotent re-`Up` (e.g. after a bifrost restart mid-creation, or a
// deliberate re-POST once the branch has new commits) is the recovery path:
// EnsureNamespace, CopySecret, and ApplyObjects all merge/replace rather than
// erroring on "already exists", and Neon branch creation is scan-then-create.
// The refusals above are scoped so as not to touch it: re-running the SAME
// branch is exactly what they let through.
//
// It is also the path this flow is tuned for, since the auto-update watcher
// walks it every time a member's branch moves: a member whose commit already
// has an image from a previous run is not rebuilt at all, and whatever is left
// builds concurrently rather than one after another (see buildMembers). A
// first-ever create, having nothing recorded to reuse, builds everything.
//
// opts carries the run's optional state (expiry, auto-update); see
// UpOptions, whose zero value — no expiry, no auto-update — is the default.
func (o *Orchestrator) Up(ctx context.Context, branch string, opts UpOptions) error {
	if strings.TrimSpace(branch) == "" {
		return errors.New("preview: Up: branch is required")
	}
	tag := TagForBranch(branch)
	if tag == "" {
		return fmt.Errorf("preview: Up: branch %q slugs to an empty tag", branch)
	}
	if !o.acquire(tag) {
		return ErrBusy
	}
	defer o.release(tag)

	members, sourceSHAs, err := o.resolveMembers(ctx, branch)
	if err != nil {
		return fmt.Errorf("preview: Up: %w", err)
	}
	if len(members) == 0 {
		return fmt.Errorf("preview: Up: branch %q matches no preview-eligible service", branch)
	}
	// Mandatory required-key pre-flight: any member whose registry entry
	// declares Required env keys (today, only footstrike-dashboard's
	// mandatory APP_API_URL/APP_IDENTITY_URL/APP_OAUTH_CLIENT_ID triple) must
	// have every one of them resolve to a non-empty value before we touch
	// the cluster. envConfigFor is the same function stage 6 uses to build
	// that service's actual ConfigMap data, so this pre-check and the real
	// computation can never disagree AS LONG AS the Required-bearing service
	// also has no staging baseline of its own -- true of every one today
	// (footstrike-dashboard ships no staging/configmap-env.yaml, so passing
	// an empty stagingData here, before it's even been fetched, matches what
	// the real render step will see too). A future Required-key service that
	// DOES ship a staging baseline could disagree between this pre-check
	// (which never sees it) and the real computation (which would) -- but
	// only in the fail-closed direction: this pre-flight would reject a
	// preview that the real render step, with the actual baseline available,
	// would have gone on to render successfully.
	for _, svc := range members {
		if len(o.Registry[svc].Required) == 0 {
			continue
		}
		// envConfigFor's own error already names both the service and that
		// it's an env-config failure ("preview: env config for %s: ..."), so
		// this only adds the missing "which stage of Up" context.
		if _, err := envConfigFor(svc, tag, members, map[string]string{}, o.Cfg, o.Registry, o.Fleet); err != nil {
			return fmt.Errorf("preview: Up: pre-flight: %w", err)
		}
	}
	// Second pre-flight, same rationale and same "before the cluster is
	// touched" placement: a registry-declared Neon parent branch that doesn't
	// exist is a static config error, and finding it at stage 5 would cost a
	// zombie failed namespace and a full round of builds. See
	// verifyNeonParents.
	if err := o.verifyNeonParents(ctx, members); err != nil {
		return fmt.Errorf("preview: Up: pre-flight: %w", err)
	}

	ns := previewNamespace(tag)
	priorAnnotations, err := o.refuseUnusableNamespace(ctx, ns, tag, branch)
	if err != nil {
		return err
	}
	// What a PREVIOUS run of this preview last built successfully, read off
	// the same pre-flight GetNamespace the refusals were decided from rather
	// than costing a second call. It has to be captured HERE, before the
	// EnsureNamespace below: that write is what stamps this run's own
	// bifrost/source-shas on, and buildMembers compares against what the last
	// successful build recorded, not against anything this run just wrote.
	//
	// Note also which annotation the write below does NOT clear:
	// bifrost/built-images is the one piece of a previous run this one must
	// inherit rather than reset. Clearing it (as the error/step trio and the
	// expiry/auto-update pair are cleared, each for its own merge reason)
	// would make every re-run rebuild everything — which is the entire cost
	// this reuse exists to remove.
	prior := parseBuiltImages(priorAnnotations[builtImagesAnnotationKey])
	if err := o.Kube.EnsureNamespace(ctx, ns,
		map[string]string{"bifrost/preview": "true"},
		map[string]string{
			branchAnnotationKey: branch,
			appsAnnotationKey:   strings.Join(members, ","),
			phaseAnnotationKey:  "creating",
			// What this run resolved each member's branch to, recorded
			// BEFORE the builds that consume it — see sourceSHAsAnnotation
			// for why writing it up front (rather than on success) is what
			// keeps the auto-update watcher from re-running a failing build
			// every two minutes forever.
			sourceSHAsAnnotationKey: sourceSHAsAnnotation(members, sourceSHAs),
			// Cleared, not merely left alone: EnsureNamespace MERGES
			// annotations onto whatever the namespace already carries, and
			// re-running Up over a previously failed preview is the
			// documented recovery path. Without these three, that retry
			// would display the PREVIOUS run's error and last step for its
			// entire duration ("creating · building X — build ended with
			// status FAILURE") in both the UI and `bif preview up`. This
			// write is Up's entry point, so it clears exactly what the ready
			// write below clears on exit; fail() rewrites bifrost/error (and
			// leaves this run's own step in place) if the retry fails too,
			// so nothing diagnostic is lost.
			"bifrost/error":      "",
			"bifrost/step":       "",
			"bifrost/step-since": "",
			// Both written unconditionally, "" and all, for the same merge
			// reason the three above are cleared: an Up that doesn't ask for
			// an expiry (or for auto-update) must take away one a previous
			// run set, not silently inherit it. See expiresAtAnnotation and
			// autoUpdateAnnotation.
			expiresAtAnnotationKey:  expiresAtAnnotation(opts.ExpiresAt),
			autoUpdateAnnotationKey: autoUpdateAnnotation(opts.AutoUpdate),
		},
	); err != nil {
		return fmt.Errorf("preview: Up: ensure namespace: %w", err)
	}

	built, err := o.buildMembers(ctx, ns, branch, members, sourceSHAs, prior)
	if err != nil {
		return o.fail(ctx, ns, err)
	}
	o.recordBuiltImages(ctx, ns, members, built)
	o.step(ctx, ns, "branching databases")
	dbURIs, err := o.branchNeonDatabases(ctx, tag, members)
	if err != nil {
		return o.fail(ctx, ns, err)
	}
	o.step(ctx, ns, "copying secrets")
	if err := o.copySecrets(ctx, ns, members, dbURIs); err != nil {
		return o.fail(ctx, ns, err)
	}
	o.step(ctx, ns, "applying manifests")
	appImages, err := o.renderAndApply(ctx, ns, tag, branch, members, built)
	if err != nil {
		return o.fail(ctx, ns, err)
	}
	o.step(ctx, ns, "waiting for pods")
	if err := o.waitForPods(ctx, ns, members, appImages); err != nil {
		return o.fail(ctx, ns, err)
	}

	// Reaching ready clears bifrost/step (and its timestamp) in the same
	// write as the phase flip: a finished preview shouldn't go on
	// displaying whatever step last ran. A failed preview (the fail() path
	// above) deliberately never does this — its last step is the
	// diagnostic ("failed while building footstrike-api").
	if err := o.Kube.AnnotateNamespace(ctx, ns, map[string]string{
		phaseAnnotationKey:   "ready",
		"bifrost/error":      "",
		"bifrost/step":       "",
		"bifrost/step-since": "",
	}); err != nil {
		return fmt.Errorf("preview: Up: mark ready: %w", err)
	}
	return nil
}

// refuseUnusableNamespace fails the run, before it has done anything, when ns
// already exists in a state this Up must not proceed into. There are two, both
// decided off the single GetNamespace below — one API call, taken immediately
// before EnsureNamespace — and each carrying its own sentinel so the API layer
// and the CLI can tell them apart:
//
//   - Kubernetes is deleting the namespace (ErrTerminating).
//   - the namespace was created for a DIFFERENT branch that derives the same
//     tag (ErrTagCollision).
//
// An existing namespace for the SAME branch is the ordinary case and is
// untouched by either: re-running `bif preview up` on a branch is the
// documented recovery path, and the auto-update watcher re-runs Up on the
// recorded branch every time a member's SHA moves.
//
// The Terminating check exists because EnsureNamespace cannot report it. The
// API server happily accepts a metadata update on a namespace in phase
// Terminating, so EnsureNamespace SUCCEEDS against one — there is no error
// for Up to catch,
// and Up used to walk straight on into a run that could not possibly finish:
// both Cloud Builds run, the Neon branches get created, and the first call
// that tries to CREATE something inside the doomed namespace (copySecrets) is
// where it finally dies, minutes later, with a bare `namespaces
// "preview-<tag>" not found`. That is the exact production failure this
// guards: tear a preview down, recreate it immediately, and burn two builds
// and a database branch on a namespace that was disappearing the whole time.
//
// Placement is deliberate: immediately before EnsureNamespace, after the
// stages that have no cluster side effects (membership resolution, the
// required-key pre-flight). Later is impossible — EnsureNamespace is the
// point of no return. Earlier (say at the top of Up) would fail a doomed run
// marginally sooner but would WIDEN the window between looking and acting by
// however long membership resolution's GitHub round trips take, and the
// window is the only thing this check is really trading in.
//
// Because it is a look-then-act, it is NOT airtight, and nothing here can
// make it so. The namespace can enter Terminating in the instant between this
// read and EnsureNamespace, or at any point during the minutes that follow —
// a `bif preview down`, or a bare kubectl delete, landing mid-create does
// exactly that. Kubernetes offers no "create unless terminating" primitive to
// close it with, and the busy set only serializes bifrost's own Up and Down,
// not anyone else's delete. What this buys is the COMMON case: the
// recreate-immediately-after-teardown that produced the two-and-a-half-minute
// doomed run now fails in one API call, having triggered nothing. A run that
// loses the race still fails the old way, just as it always did.
//
// It refuses rather than waiting, polling, or retrying, deliberately.
// Namespace teardown is unbounded in principle (finalizers, stuck pods) and
// routinely takes tens of seconds; blocking here would hold the tag's busy
// claim for the whole time, so a caller who would rather give up couldn't,
// and `bif preview list` would show nothing at all meanwhile. The caller gets
// a named error and decides.
//
// The different-branch check exists because TagForBranch is many-to-one: by
// truncation (feature/add-user-profile-avatars-v1 and ...-v2 share their first
// maxTagLen characters) and, independently, by character folding with no
// truncation at all (feat/foo, feat_foo and feat-foo all fold to feat-foo).
// Every layer underneath quietly merged the two branches together and not one
// of them warned. EnsureNamespace merges, overwriting bifrost/branch with the
// newcomer's name; ensureNeonBranch finds the existing preview-<tag> branch by
// name and REUSES it, inheriting whatever migrations the first branch's
// migrate initContainer applied; and a later `bif preview down` on that tag
// destroys both previews at once. The second developer gets the first
// developer's database and schema, and neither is told.
//
// It is caught here rather than made impossible in TagForBranch on purpose —
// see the note in tag.go: appending a hash to truncated tags closes only the
// truncation path, leaves folding wide open, and would change the tag of every
// existing long-branch preview, orphaning its namespace and its Neon branch.
// The refusal is the general fix and costs no extra API call, since the
// annotations are already in hand from the read above.
//
// A namespace carrying NO bifrost/branch annotation is deliberately NOT
// refused. Absence is not evidence of a collision — a preview created before
// bifrost recorded the branch, or a namespace left behind by a partial or
// out-of-band run, looks exactly like this — and refusing would strand it:
// nothing but a manual `bif preview down` could ever get past the guard again,
// including the very re-run that would repair it. Proceeding also fixes it,
// because the EnsureNamespace immediately below writes the annotation this
// read went looking for.
//
// A GetNamespace that fails aborts the run too. A namespace bifrost cannot
// read is not evidence that it is safe to build into — the same reading the
// expiry sweep applies to its own re-read — and proceeding on an unknown is
// precisely the bug being fixed here.
//
// On success it hands back the annotations it read (nil when the namespace
// doesn't exist yet), because this read is also the run's only look at what a
// PREVIOUS run left behind, and Up needs one of those annotations —
// bifrost/built-images, the build-skip record — before EnsureNamespace
// overwrites the rest of them. Returning the snapshot keeps that a property of
// the same single API call rather than a second GetNamespace, and keeps the
// look-then-act window this function documents exactly as narrow as it was.
func (o *Orchestrator) refuseUnusableNamespace(ctx context.Context, ns, tag, branch string) (map[string]string, error) {
	existing, found, err := o.Kube.GetNamespace(ctx, ns)
	if err != nil {
		return nil, fmt.Errorf("preview: Up: check namespace %s: %w", ns, err)
	}
	// Absent is the ordinary recreate-after-teardown path once the delete has
	// actually finished: nothing to refuse, and EnsureNamespace below creates
	// it fresh.
	if !found {
		return nil, nil
	}
	// Terminating first: a namespace on its way out is about to stop existing,
	// so whichever branch it belonged to is the less useful thing to say about
	// it, and "retry once the teardown finishes" is the actionable verdict.
	if existing.Phase == "Terminating" {
		return nil, fmt.Errorf("preview: Up: namespace %s is still Terminating: %w", ns, ErrTerminating)
	}
	if owner := existing.Annotations[branchAnnotationKey]; owner != "" && owner != branch {
		return nil, fmt.Errorf(
			"preview: Up: preview tag %q (namespace %s) already belongs to branch %q, so branch %q cannot use it — both branch names derive the same tag: %w",
			tag, ns, owner, branch, ErrTagCollision)
	}
	return existing.Annotations, nil
}

// The namespace-annotation keys that are both written here and read back
// elsewhere in this package (by the expiry sweep and the auto-update
// watcher) are named, so the writers and the readers cannot drift apart. The
// rest of the bifrost/* family (error/step/step-since) is written here and
// read only outside this package, so those stay literals at their single
// point of use.
const (
	phaseAnnotationKey     = "bifrost/phase"
	expiresAtAnnotationKey = "bifrost/expires-at"
	branchAnnotationKey    = "bifrost/branch"
	appsAnnotationKey      = "bifrost/apps"
	// sourceSHAsAnnotationKey records the commit each member was deployed
	// from; autoUpdateAnnotationKey ("true" | "") opts the preview into
	// following its branch. Both are read by the auto-update watcher — see
	// autoupdate.go.
	sourceSHAsAnnotationKey = "bifrost/source-shas"
	autoUpdateAnnotationKey = "bifrost/auto-update"
	// builtImagesAnnotationKey records what each member's last SUCCESSFUL
	// build produced, and is read back by the next run's build stage to skip
	// rebuilding a commit that already has an image. Deliberately separate
	// from sourceSHAsAnnotationKey — see builtImagesAnnotation for why the
	// two cannot be collapsed.
	builtImagesAnnotationKey = "bifrost/built-images"
)

// builtImage pairs the commit a member's preview image was built from with
// the image tag suffix that build actually produced.
//
// Two fields rather than one because neither answers the other's question.
// Commit is a FULL commit SHA, the only thing comparable against what
// resolveMembers gets from GitHub, so it's what decides "has this member
// moved since we last built it". ShortSHA is what render.go turns into the
// image's `preview-<sha>` tag, and it is NOT recomputed from Commit — see
// builtImagesAnnotation.
type builtImage struct {
	Commit   string
	ShortSHA string
}

// builtImagesAnnotation renders "<svc>=<commit>:<short sha>" entries,
// comma-joined in members order (sorted, so the value is stable), in the same
// style as bifrost/apps and bifrost/source-shas. Members with nothing recorded
// are omitted rather than emitted empty, so a half-populated annotation is
// only ever missing entries, never carrying meaningless ones.
//
// It exists because bifrost/source-shas CANNOT be used to decide whether a
// member needs rebuilding, however similar the two look. source-shas is
// written at the TOP of the run, before the builds it describes have run at
// all — deliberately, so a preview whose build keeps failing records the
// commit it attempted and the auto-update watcher stops re-running it every
// two minutes (see sourceSHAsAnnotation). "The SHA didn't change" therefore
// does not imply "an image exists": after a FAILED build the SHA is recorded
// and no image was ever pushed. Skipping on that evidence would apply a
// Deployment pointing at a tag that does not exist.
//
// This one is written only AFTER the build stage succeeds (see
// Orchestrator.recordBuiltImages), so its presence for a member means exactly
// one thing: a `preview-<short sha>` image for that commit was pushed to
// Artifact Registry.
//
// The short SHA is recorded rather than derived from the commit, even though
// Cloud Build's $SHORT_SHA is in practice the first 7 characters of
// $COMMIT_SHA (verified against 200 real builds in this project, preview
// builds included: 200/200 a 7-character prefix). Two reasons, and the second
// is the load-bearing one:
//
//   - the value is Cloud Build's substitution, not bifrost's convention.
//     awaitBuild reads it back off the build (gcb.BuildStatus.SHA) and this
//     package never parses it; a derivation would be bifrost silently
//     asserting a contract it does not own.
//   - the commit a build builds is resolved by CLOUD BUILD, from the branch
//     name RunTrigger passes, at trigger-run time — not from the SHA
//     resolveMembers got from GitHub moments earlier. A commit landing in
//     that window makes the two differ, and a derived tag would then name an
//     image no build ever pushed.
//
// Recording what the build reported makes the tag exact by construction.
// Pairing it with the commit bifrost RESOLVED (rather than the one Cloud
// Build built, which GetBuild doesn't report) leaves that same window
// fail-SAFE: the next run resolves the newer commit, sees a difference, and
// rebuilds.
func builtImagesAnnotation(members []string, built map[string]builtImage) string {
	pairs := make([]string, 0, len(members))
	for _, svc := range members {
		b := built[svc]
		if b.Commit == "" || b.ShortSHA == "" {
			continue
		}
		pairs = append(pairs, svc+"="+b.Commit+":"+b.ShortSHA)
	}
	return strings.Join(pairs, ",")
}

// parseBuiltImages reads bifrost/built-images back into service -> builtImage.
// It is the exact inverse of builtImagesAnnotation, which is the only thing
// that writes that annotation.
//
// Every malformed or half-empty entry is dropped rather than failing the read,
// and every drop lands on the same side: the member reads as having nothing
// built, which buildMembers treats as "must build". An annotation bifrost
// can't parse therefore costs one redundant build, never a Deployment pointing
// at an image that was never produced. Same reading parseSourceSHAs applies,
// for the same fail-safe reason.
func parseBuiltImages(raw string) map[string]builtImage {
	built := map[string]builtImage{}
	for _, pair := range strings.Split(raw, ",") {
		svc, rest, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		commit, shortSHA, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		svc, commit, shortSHA = strings.TrimSpace(svc), strings.TrimSpace(commit), strings.TrimSpace(shortSHA)
		if svc == "" || commit == "" || shortSHA == "" {
			continue
		}
		built[svc] = builtImage{Commit: commit, ShortSHA: shortSHA}
	}
	return built
}

// expiresAtAnnotation renders expiresAt as RFC3339, or "" for the zero time
// (no expiry). Absolute rather than a duration so the reaper never has to
// know when the preview was created — and so a re-run can carry an existing
// expiry through unchanged instead of extending it (see UpOptions). "" rather
// than an omitted key because EnsureNamespace merges: a re-run without a ttl
// must drop a previous expiry, exactly as it drops the previous run's error
// and step above.
func expiresAtAnnotation(expiresAt time.Time) string {
	if expiresAt.IsZero() {
		return ""
	}
	return expiresAt.UTC().Format(time.RFC3339)
}

// autoUpdateAnnotation renders the opt-in as "true" or "" — never an omitted
// key, and never "false", for the same merge reason expiresAtAnnotation
// returns "": EnsureNamespace merges onto whatever the namespace already
// carries, so an Up that doesn't ask for auto-update has to overwrite a
// previous run's "true" rather than leave it standing. "" (not "false")
// because the watcher's test is `== "true"`, so absent and cleared are one
// case, and because it matches how bifrost/error, step and expires-at are
// cleared here.
func autoUpdateAnnotation(on bool) string {
	if !on {
		return ""
	}
	return "true"
}

// sourceSHAsAnnotation renders "<svc>=<sha>" pairs, comma-joined in members
// order (which is sorted, so the value is stable), in the same style as
// bifrost/apps. These are FULL commit SHAs as GitHub reports them — not the
// short SHAs the builds tag images with — because this value's whole purpose
// is to be compared against a later BranchSHA call by the auto-update
// watcher, and the two must be the same kind of string.
//
// Up writes it in the EnsureNamespace call at the TOP of the run, from what
// resolveMembers just resolved, rather than at the end from what actually
// deployed. That ordering is what stops the watcher from hot-looping: a run
// whose build fails still records the SHA it attempted, so the next tick sees
// no difference and leaves it alone until someone pushes a new commit. See
// PollAutoUpdates.
//
// That same ordering is precisely why this annotation must NOT be used to
// decide whether a member needs rebuilding — a SHA recorded here can belong to
// a build that never produced an image. bifrost/built-images is the annotation
// for that question; see builtImagesAnnotation.
func sourceSHAsAnnotation(members []string, shas map[string]string) string {
	pairs := make([]string, 0, len(members))
	for _, svc := range members {
		pairs = append(pairs, svc+"="+shas[svc])
	}
	return strings.Join(pairs, ",")
}

// failAnnotateTimeout and stepAnnotateTimeout bound fail's and step's
// detached writes. They differ because the two writes have opposite
// priorities, not because one of them was tuned carelessly:
//
//   - fail's write MUST land — it's the only record that a preview failed and
//     why — so it gets the generous bound, long enough to ride out a slow API
//     server. Nothing is waiting on it: Up is already returning an error.
//   - step's write is cosmetic and its error is swallowed, but it sits
//     *inline* on the happy path, between the stages of the very flow this
//     feature exists to make feel faster. A slow API server here adds latency
//     to preview creation itself, so it gets the tight bound: better to lose
//     one step annotation than to spend ten seconds per stage boundary
//     narrating.
const (
	failAnnotateTimeout = 10 * time.Second
	stepAnnotateTimeout = 3 * time.Second
)

// fail marks ns bifrost/phase=failed with a sanitized bifrost/error
// annotation (cause's message only — never a secret value; callers are
// responsible for never constructing cause with one embedded, see the Neon
// helpers below) and returns cause. If the annotate call itself fails, that
// failure is joined in rather than swallowed, so an operator sees both.
//
// The annotate call deliberately runs on a context detached from ctx's
// cancellation (context.WithoutCancel), with its own short timeout: cause
// itself is very often ctx dying (a build-poll wait hitting the caller's
// deadline, e.g. the API layer's 30-minute goroutine budget), and every
// other stage's failure races the same deadline. Annotating on the
// already-dead ctx would make this compensating write fail right along
// with it, leaving the namespace stuck at bifrost/phase=creating with no
// bifrost/error — silently violating the "every post-namespace failure
// lands on failed" contract this whole function exists to uphold.
func (o *Orchestrator) fail(ctx context.Context, ns string, cause error) error {
	annotateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failAnnotateTimeout)
	defer cancel()
	if annErr := o.Kube.AnnotateNamespace(annotateCtx, ns, map[string]string{
		phaseAnnotationKey: "failed",
		"bifrost/error":    cause.Error(),
	}); annErr != nil {
		return fmt.Errorf("%w (additionally failed to annotate failure: %v)", cause, annErr)
	}
	return cause
}

// step records what Up is doing right now, for operators watching `ib
// preview up` go quiet for minutes at a time while a build or a Neon branch
// creation runs. It writes bifrost/step (operator-facing prose — never a
// Neon URI, token, or any other secret; this lands in a namespace
// annotation any cluster reader can see) plus bifrost/step-since (an
// RFC3339 timestamp, not a duration, so a caller re-reading the namespace
// minutes later can still compute accurate elapsed time locally — this is
// also why callers write it once per step rather than once per poll tick:
// a two-minute build costs one annotation write, not twelve).
//
// Best-effort: like fail's compensating write, this runs on a context
// detached from ctx's cancellation (context.WithoutCancel) with its own
// short timeout (stepAnnotateTimeout — deliberately tighter than fail's, see
// there), since ctx dying is exactly when a long-running stage's step text is
// most useful to have already landed. Unlike fail, an annotation failure here
// is logged and swallowed, never returned or otherwise surfaced to the
// caller — narrating progress must never itself become a reason Up fails.
func (o *Orchestrator) step(ctx context.Context, ns, text string) {
	annotateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stepAnnotateTimeout)
	defer cancel()
	if err := o.Kube.AnnotateNamespace(annotateCtx, ns, map[string]string{
		"bifrost/step":       text,
		"bifrost/step-since": time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		slog.Warn("preview: step annotation failed", "ns", ns, "step", text, "err", err)
	}
}

// resolveMembers determines which of the registry's previewable services
// have branch pushed to their repo: ErrNoBranch means "not a member", any
// other error aborts the whole run (we can't tell membership, so we can't
// safely proceed).
//
// It returns each member's full commit SHA alongside the membership itself —
// the same BranchSHA call answers both questions, and the SHA is what Up
// records as bifrost/source-shas for the auto-update watcher to compare
// against. Keyed by service (not repo), since that's how every other
// per-member map in this package is keyed and what the annotation names.
func (o *Orchestrator) resolveMembers(ctx context.Context, branch string) ([]string, map[string]string, error) {
	var members []string
	shas := map[string]string{}
	for _, svc := range o.Registry.Names() {
		sha, err := o.GitHub.BranchSHA(ctx, o.Fleet.RepoFor(svc), branch)
		switch {
		case errors.Is(err, github.ErrNoBranch):
			continue
		case err != nil:
			return nil, nil, fmt.Errorf("checking branch for %s: %w", svc, err)
		default:
			members = append(members, svc)
			shas[svc] = sha
		}
	}
	return members, shas, nil
}

// buildMembers gets every member an image for this run's commit, and returns
// what each one ended up with. A member whose commit already has an image from
// a previous run is not rebuilt at all; the rest are built CONCURRENTLY. ns is
// only for step()'s progress narration — every other kube write for a preview
// happens elsewhere in Up.
//
// sourceSHAs is what resolveMembers resolved each member's branch to for THIS
// run; prior is what the last successful build recorded
// (bifrost/built-images). There are two ways a member can end up reused, tried
// in this order and never both:
//
//  1. prior has an entry for it. Then it is skipped iff the two agree — its
//     currently-resolved commit is exactly the commit whose image was last
//     built — and its recorded short SHA is carried straight through to
//     render, which is what makes the skip a real reuse of the existing
//     `preview-<sha>` image rather than a rebuild in disguise. An entry whose
//     commit DISAGREES is a definitive "this member moved": it is built, with
//     no lookup, because the annotation has already answered the question.
//     See builtImagesAnnotation for why prior must come from that annotation
//     and never from bifrost/source-shas.
//  2. prior has NO entry for it, so the annotation cannot answer at all —
//     which is exactly what a preview that has been torn down and recreated
//     looks like, since Down deletes the namespace the annotation lived on.
//     Only then does findPriorBuild ask Cloud Build whether the image is
//     nonetheless already built.
//
// That ordering is what keeps the ordinary paths free. A live preview's
// re-run — including every auto-update tick — has an entry for every member
// either way, so it costs exactly the API calls it always did. Only a run
// that would otherwise have built blind pays for a lookup.
//
// The returned map covers every member, skipped and built alike, so the
// caller can both render and re-record all of them. A member reused via (2) is
// recorded identically to one that was really built, so the NEXT run takes the
// cheap annotation path rather than querying again.
//
// Two consequences of skipping are accepted rather than solved, and are
// documented in docs/preview-environments.md:
//
//   - a skipped member keeps its OLD BASE IMAGE. Same commit and same
//     Dockerfile means an identical app layer, but not a newer
//     python:3.13-slim / alpine:latest underneath it.
//   - if the image has been deleted from Artifact Registry, the skip applies a
//     Deployment naming an image that is gone. There is no cleanup policy
//     today; the failure is clean when there is one (ImagePullBackOff, caught
//     by the readiness wait), but any future policy needs a retention window
//     longer than previews live.
func (o *Orchestrator) buildMembers(
	ctx context.Context,
	ns, branch string,
	members []string,
	sourceSHAs map[string]string,
	prior map[string]builtImage,
) (map[string]builtImage, error) {
	built := make(map[string]builtImage, len(members))
	var needed []string
	for _, svc := range members {
		if b, ok := prior[svc]; ok {
			if b.Commit == sourceSHAs[svc] {
				built[svc] = b
			} else {
				needed = append(needed, svc)
			}
			continue
		}
		// No entry at all — the only case worth an API call. See
		// findPriorBuild.
		if b, ok := o.findPriorBuild(ctx, svc, sourceSHAs[svc]); ok {
			built[svc] = b
			continue
		}
		needed = append(needed, svc)
	}

	o.step(ctx, ns, buildStepText(members, needed))
	if len(needed) == 0 {
		return built, nil
	}

	// Start every trigger first — RunTrigger returns as soon as Cloud Build
	// has accepted the build — and only then wait on any of them, so an
	// n-member preview costs one build's wall time rather than n. A start
	// failure aborts before any waiting: whatever already started keeps
	// running in Cloud Build, which is harmless (nothing consumes an image
	// this run won't reach) and much cheaper than cancellation machinery.
	type startedBuild struct{ svc, buildID string }
	started := make([]startedBuild, 0, len(needed))
	for _, svc := range needed {
		triggerID := o.TriggerIDs[svc+"-preview-build"]
		if triggerID == "" {
			return nil, fmt.Errorf("no preview build trigger configured for %s", svc)
		}
		buildID, err := o.Builds.RunTrigger(ctx, triggerID, branch)
		if err != nil {
			return nil, fmt.Errorf("starting build for %s: %w", svc, err)
		}
		started = append(started, startedBuild{svc: svc, buildID: buildID})
	}

	// One goroutine per started build, each writing only its own slot, so the
	// results need no lock. Bounded by member count (rarely more than two or
	// three), so no worker pool: awaitBuild is a sleep-and-poll loop, not work.
	shortSHAs := make([]string, len(started))
	errs := make([]error, len(started))
	var wg sync.WaitGroup
	for i, b := range started {
		wg.Add(1)
		go func() {
			defer wg.Done()
			shortSHAs[i], errs[i] = o.awaitBuild(ctx, b.buildID)
		}()
	}
	wg.Wait()

	// Collected in `needed` order (which is members order, sorted), so a
	// multi-failure run always reports the same member rather than whichever
	// goroutine happened to lose first.
	var firstErr error
	for i, b := range started {
		if errs[i] != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("build for %s: %w", b.svc, errs[i])
				continue
			}
			// Only one failure can be the bifrost/error annotation, which has
			// to stay a single readable line (see buildFailure); the rest
			// would otherwise vanish, so they're logged instead of dropped.
			slog.Warn("preview: another member's build also failed",
				"ns", ns, "service", b.svc, "err", errs[i])
			continue
		}
		built[b.svc] = builtImage{Commit: sourceSHAs[b.svc], ShortSHA: shortSHAs[i]}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return built, nil
}

// findPriorBuild asks Cloud Build whether svc's preview trigger has ALREADY
// built commit successfully, and if so what image tag that build produced.
//
// This is the reuse path that survives a teardown. bifrost/built-images lives
// on the preview's namespace, and Down deletes the namespace, so the record of
// what was built dies with it: an up -> down -> up at the same commit used to
// rebuild everything from scratch even though the images were still sitting in
// Artifact Registry under exactly the tags it was about to recreate. Cloud
// Build's build history outlives the namespace and can be asked instead.
//
// Cloud Build rather than Artifact Registry deliberately, even though
// Artifact Registry answers the more direct question. bifrost already holds a
// Cloud Build client and the service account already has
// cloudbuild.builds.viewer, so this needs no new dependency, no IAM change and
// no `pulumi up`; the registry would need all three. What that trades away is
// exactness: a successful build is a PROXY for a live image, and the two
// diverge only if the image is deleted after being built. That is the same
// exposure the annotation path above already carries and already documents —
// there is no Artifact Registry cleanup policy today, and if one is ever added
// the failure is a clean ImagePullBackOff caught by the readiness wait, from
// either path.
//
// Every failure mode returns false, which means "build it" — the safe
// direction, and the behavior this feature is an optimization over. A lookup
// error is logged and swallowed rather than failing the preview: bifrost could
// not tell, and could-not-tell is not a reason to refuse to build. A commit
// that resolveMembers left empty, or a service with no configured preview
// trigger, likewise just returns false; the missing trigger is reported
// properly by buildMembers' own start loop, which is the one place that error
// belongs — reporting it here would fail a run on a lookup, which is exactly
// what this must never do.
func (o *Orchestrator) findPriorBuild(ctx context.Context, svc, commit string) (builtImage, bool) {
	triggerID := o.TriggerIDs[svc+"-preview-build"]
	if triggerID == "" || commit == "" {
		return builtImage{}, false
	}
	shortSHA, found, err := o.Builds.FindSuccessfulBuild(ctx, triggerID, commit)
	if err != nil {
		slog.Warn("preview: looking up an existing build failed; building instead",
			"service", svc, "commit", commit, "err", err)
		return builtImage{}, false
	}
	if !found {
		return builtImage{}, false
	}
	// Logged because the step text deliberately doesn't distinguish this from
	// an annotation-backed reuse (see buildStepText): "reusing" is what is
	// happening either way, and the difference is bifrost's bookkeeping rather
	// than a fact about the preview. An operator who tore a preview down and
	// is surprised that the recreate built nothing gets their answer here.
	slog.Info("preview: reusing an image from an earlier build of this commit",
		"service", svc, "commit", commit, "shortSHA", shortSHA)
	return builtImage{Commit: commit, ShortSHA: shortSHA}, true
}

// buildStepText narrates the build stage for an operator watching `bif preview
// up` (or the Previews tab) sit still for minutes. The old wording numbered a
// sequence — "building footstrike-api (1/2)", rewritten per member — which is
// no longer true in either direction: the builds run concurrently, so there is
// no 1-then-2, and some members may not be built at all.
//
// So it states what is actually happening, in members order, in one write for
// the whole stage:
//
//	building footstrike-api, footstrike-dashboard
//	building footstrike-dashboard (reusing footstrike-api)
//	reusing images for footstrike-api, footstrike-dashboard
//
// The last case still gets a step rather than none. It is the one an operator
// is most likely to be confused by ("why didn't my change show up?"), and it
// is what a failure in a LATER stage will leave behind as its retained
// diagnostic.
//
// A member reused because Cloud Build had already built its commit
// (findPriorBuild) is narrated with the SAME "reusing" wording as one reused
// off bifrost/built-images, deliberately. The step answers "what is happening
// right now", and the answer is identical in both cases: no build is running,
// and an existing `preview-<sha>` image is being deployed. Where they differ
// is only in which record bifrost consulted — its own annotation, or Cloud
// Build's history — which is bookkeeping, not a fact about the preview, and a
// second vocabulary for the same outcome would make the same result read two
// different ways depending on state the operator cannot see. The one place
// that difference is genuinely interesting — a teardown-then-recreate that
// surprisingly builds nothing — is served by findPriorBuild's log line, which
// names the service and commit and does not have to fit in a table cell.
func buildStepText(members, needed []string) string {
	if len(needed) == 0 {
		return "reusing images for " + strings.Join(members, ", ")
	}
	text := "building " + strings.Join(needed, ", ")
	var reused []string
	for _, svc := range members {
		if !slices.Contains(needed, svc) {
			reused = append(reused, svc)
		}
	}
	if len(reused) > 0 {
		text += " (reusing " + strings.Join(reused, ", ") + ")"
	}
	return text
}

// recordBuiltImages writes bifrost/built-images for what the build stage just
// settled on — the whole map, freshly-built and reused members alike, so the
// annotation always describes the CURRENT membership and stale entries for a
// member that has since left the preview are pruned rather than accumulated.
//
// Called only after the build stage SUCCEEDS. That ordering is the entire
// point of the annotation (see builtImagesAnnotation): recorded before, or on
// failure, it would claim an image exists for a build that never produced one,
// and the next run would skip straight to applying a Deployment naming a tag
// that isn't there.
//
// Best-effort, like step()'s write and unlike fail()'s: a failure here is
// logged and swallowed rather than failing an otherwise-healthy preview. It
// can only ever cost one redundant rebuild — the next run finds no record (or
// a stale one), and rebuilds — which is strictly better than failing a preview
// whose images are all built and pushed over a bookkeeping write. It is a
// separate call rather than folded into the "branching databases" step write
// because the two have opposite semantics: this one is a fact about the
// cluster, that one is cosmetic prose.
func (o *Orchestrator) recordBuiltImages(ctx context.Context, ns string, members []string, built map[string]builtImage) {
	if err := o.Kube.AnnotateNamespace(ctx, ns, map[string]string{
		builtImagesAnnotationKey: builtImagesAnnotation(members, built),
	}); err != nil {
		slog.Warn("preview: recording built images failed; the next run will rebuild rather than reuse",
			"ns", ns, "err", err)
	}
}

// awaitBuild polls buildID every buildPollInterval until it reaches a
// terminal status, returning its short SHA on success. It respects ctx
// cancellation while waiting between polls.
func (o *Orchestrator) awaitBuild(ctx context.Context, buildID string) (string, error) {
	for {
		status, err := o.Builds.GetBuild(ctx, buildID)
		if err != nil {
			return "", fmt.Errorf("checking build status: %w", err)
		}
		if status.InProgress() {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(buildPollInterval):
			}
			continue
		}
		// Cloud Build's terminal statuses are SUCCESS, and the failure-ish
		// set status.Failed() names (FAILURE/INTERNAL_ERROR/TIMEOUT/EXPIRED),
		// plus CANCELLED (deliberate, not a Failed()). Anything other than a
		// clean SUCCESS means the run doesn't get a usable image.
		if status.Status != "SUCCESS" {
			return "", buildFailure(buildID, status)
		}
		return status.SHA, nil
	}
}

// buildFailure turns a non-SUCCESS terminal build into the error that
// eventually lands in bifrost/error — the string `bif preview up`, `bif preview
// list`, the Previews tab and the detail page all display verbatim.
//
// It carries the build's console LOG URL, because "build ended with status
// FAILURE" on its own names a status and gives an operator nothing to act on:
// the next move is always to go read that build's log, and without the link
// that means hand-navigating the Cloud Build console guessing which run was
// theirs. The URL is the one piece of build diagnosis that is safe to put
// here. Build LOGS deliberately are not, and must not be added later — a log
// line can carry anything, secrets included, and this annotation is served
// over the API and rendered in a browser (the same reasoning that keeps
// waitForPods from ever fetching pod logs). A console URL is a pointer, not
// content: it discloses nothing on its own and still requires the reader's
// own GCP authorization.
//
// LogURL is populated by Cloud Build for every triggered build, failures
// included, so the linked form is what an operator sees in practice. The
// bare-status fallback is for the case where the API returned one without it
// — then the build ID goes in instead, which is enough for `gcloud builds
// log <id>`, rather than emitting a dangling "see: " that points nowhere.
// The ID is omitted from the linked form on purpose: the URL already ends in
// it, and this string has to stay readable in a table cell.
func buildFailure(buildID string, status gcb.BuildStatus) error {
	switch {
	case status.LogURL != "":
		return fmt.Errorf("build ended with status %s: %s", status.Status, status.LogURL)
	case buildID != "":
		return fmt.Errorf("build ended with status %s (build %s)", status.Status, buildID)
	default:
		return fmt.Errorf("build ended with status %s", status.Status)
	}
}

// branchNeonDatabases ensures a preview-<tag> Neon branch exists for every
// member with a registry-declared Neon reference, returning each one's
// connection URI. Errors are wrapped without ever including the URI itself
// (it's a secret) — only service/project identifiers appear in the returned
// error.
func (o *Orchestrator) branchNeonDatabases(ctx context.Context, tag string, members []string) (map[string]string, error) {
	dbURIs := make(map[string]string, len(members))
	for _, svc := range members {
		ref := o.Registry[svc].Neon
		if ref == nil {
			continue
		}
		uri, err := o.ensureNeonBranch(ctx, *ref, tag)
		if err != nil {
			return nil, fmt.Errorf("neon branch for %s: %w", svc, err)
		}
		dbURIs[svc] = uri
	}
	return dbURIs, nil
}

// branchIDByName returns the ID of the branch named name, or "" if no branch
// in branches carries that name.
func branchIDByName(branches []neon.Branch, name string) string {
	for _, b := range branches {
		if b.Name == name {
			return b.ID
		}
	}
	return ""
}

// errMissingNeonParent is the one error text for "the registry names a parent
// branch this project doesn't have", shared by the pre-flight and the
// branch-creation path so the two can never drift. It names both the project
// and the missing branch, since neither alone identifies the registry line to
// fix.
//
// This must stay an error and never degrade to "branch from the default
// instead": the default IS production in every project here, so a silent
// fallback would restore the exact bug parent: was added to fix, invisibly.
func errMissingNeonParent(ref NeonRef) error {
	return fmt.Errorf("neon project %s has no branch named %q (registry preview.neon.parent)", ref.Project, ref.Parent)
}

// resolveNeonParentID maps ref's configured parent branch NAME to its Neon
// branch ID against an already-fetched branch list. A ref with no parent
// resolves to "" — the empty parentID CreateBranch treats as "use the
// project's default branch", i.e. exactly the pre-parent behavior.
func resolveNeonParentID(branches []neon.Branch, ref NeonRef) (string, error) {
	if ref.Parent == "" {
		return "", nil
	}
	id := branchIDByName(branches, ref.Parent)
	if id == "" {
		return "", errMissingNeonParent(ref)
	}
	return id, nil
}

// verifyNeonParents is Up's pre-flight for registry-declared Neon parent
// branches: every member whose neon: block names a parent must have that
// branch present in its project before the run touches the cluster.
//
// It is deliberately redundant with the check ensureNeonBranch does at
// creation time. That one is the real gate — it runs against the branch list
// the create is actually based on — but it fires at stage 5, minutes into a
// run, AFTER EnsureNamespace and after every member has been built. A typo in
// registry.yaml's parent: would fail there as a `failed`-phase namespace,
// i.e. a zombie preview to clean up by hand, plus wasted build minutes, for
// what is purely a static config error knowable up front. So it is also
// checked here, alongside the required-key pre-flight, where a failure
// returns before the namespace is ever created.
//
// A ListBranches error is fatal rather than skipped: "couldn't check" is not
// evidence the parent exists, and letting the run continue would land it on
// Neon's default (production) branch if the parent really was missing.
func (o *Orchestrator) verifyNeonParents(ctx context.Context, members []string) error {
	for _, svc := range members {
		ref := o.Registry[svc].Neon
		if ref == nil || ref.Parent == "" {
			continue
		}
		branches, err := o.Neon.ListBranches(ctx, ref.Project)
		if err != nil {
			return fmt.Errorf("neon parent branch for %s: listing branches: %w", svc, err)
		}
		if _, err := resolveNeonParentID(branches, *ref); err != nil {
			return fmt.Errorf("neon parent branch for %s: %w", svc, err)
		}
	}
	return nil
}

// ensureNeonBranch finds (or creates) ref's project's preview-<tag> branch
// and returns its connection URI for ref's database/role.
//
// A created branch is cut from ref's registry-declared parent (resolved from
// name to ID against the same ListBranches result used to find an existing
// preview branch — no extra API call), or from the project's default branch
// when the registry declares none.
func (o *Orchestrator) ensureNeonBranch(ctx context.Context, ref NeonRef, tag string) (string, error) {
	branchName := previewBranchName(tag)
	branches, err := o.Neon.ListBranches(ctx, ref.Project)
	if err != nil {
		return "", fmt.Errorf("listing branches: %w", err)
	}
	branchID := branchIDByName(branches, branchName)
	if branchID == "" {
		parentID, err := resolveNeonParentID(branches, ref)
		if err != nil {
			return "", err
		}
		created, err := o.Neon.CreateBranch(ctx, ref.Project, branchName, parentID)
		if err != nil {
			return "", fmt.Errorf("creating branch: %w", err)
		}
		branchID = created.ID
	}
	uri, err := o.Neon.ConnectionURI(ctx, ref.Project, branchID, ref.Database, ref.Role)
	if err != nil {
		return "", fmt.Errorf("fetching connection uri: %w", err)
	}
	return uri, nil
}

// copySecrets copies each Neon-backed member's staging secret into the
// preview namespace (overriding DATABASE_URL with its preview branch's URI),
// plus the shared wildcard TLS cert every preview namespace needs.
func (o *Orchestrator) copySecrets(ctx context.Context, ns string, members []string, dbURIs map[string]string) error {
	for _, svc := range members {
		if o.Registry[svc].Neon == nil {
			continue
		}
		overrides := map[string][]byte{"DATABASE_URL": []byte(dbURIs[svc])}
		if err := o.Kube.CopySecret(ctx, svc+"-staging", svc+"-staging-secrets", ns, previewSecretName(svc), overrides); err != nil {
			return fmt.Errorf("copy secret for %s: %w", svc, err)
		}
	}
	if err := o.Kube.CopySecret(ctx, "previews", previewTLSSecret, ns, previewTLSSecret, nil); err != nil {
		return fmt.Errorf("copy wildcard tls secret: %w", err)
	}
	return nil
}

// renderAndApply fetches each member's k8s/ tree, computes its env config,
// renders its manifests, and applies them into ns.
//
// It returns each member's applied app-container image, read back out of the
// objects it actually applied rather than reconstructed from the naming
// convention — waitForPods uses it to tell this run's pods apart from the
// previous run's (see podsForMember), and a reconstruction that drifted from
// what render really produced would silently match nothing and time out
// every preview.
func (o *Orchestrator) renderAndApply(ctx context.Context, ns, tag, branch string, members []string, built map[string]builtImage) (map[string]string, error) {
	appImages := make(map[string]string, len(members))
	for _, svc := range members {
		k8sFiles, err := o.GitHub.FetchK8s(ctx, o.Fleet.RepoFor(svc), branch)
		if err != nil {
			return nil, fmt.Errorf("fetch k8s files for %s: %w", svc, err)
		}
		stagingData, err := parseStagingEnv(k8sFiles)
		if err != nil {
			return nil, fmt.Errorf("parse staging env for %s: %w", svc, err)
		}
		// No extra wrap here: envConfigFor's own error already names both the
		// service and the stage ("preview: env config for %s: ..."); adding
		// another "env config for %s: %w" around it would just duplicate
		// that segment in the bifrost/error annotation a human reads.
		envConfig, err := envConfigFor(svc, tag, members, stagingData, o.Cfg, o.Registry, o.Fleet)
		if err != nil {
			return nil, err
		}
		secretName := ""
		if o.Registry[svc].Neon != nil {
			secretName = previewSecretName(svc)
		}
		objs, err := Render(RenderInput{
			Service:    svc,
			Tag:        tag,
			ShortSHA:   built[svc].ShortSHA,
			K8sFiles:   k8sFiles,
			EnvConfig:  envConfig,
			SecretName: secretName,
			Migrate:    o.Registry[svc].Migrate,
		})
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", svc, err)
		}
		if err := o.Kube.ApplyObjects(ctx, ns, objs); err != nil {
			return nil, fmt.Errorf("apply %s: %w", svc, err)
		}
		if image := appliedAppImage(objs, svc); image != "" {
			appImages[svc] = image
		}
	}
	return appImages, nil
}

// appliedAppImage returns the image of svc's app container in the objects
// just applied for it — the Deployment named svc (enforced by the generated
// deployment patch's target) holding the container named svc (enforced by
// render.go's detectBase). "" when it
// can't be found, which callers must treat as "don't filter on image"
// rather than "match nothing".
func appliedAppImage(objs []*unstructured.Unstructured, svc string) string {
	for _, o := range objs {
		if o.GetKind() != "Deployment" || o.GetName() != svc {
			continue
		}
		containers, found, err := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
		if err != nil || !found {
			return ""
		}
		if c, ok := findContainerNamed(containers, svc); ok {
			image, _ := c["image"].(string)
			return image
		}
	}
	return ""
}

func previewSecretName(svc string) string { return svc + "-preview-secrets" }

// waitForPods polls ns until every member has at least one Deployment-managed
// pod and all of those pods report ready, or until podReadyTimeout expires.
// It's the last stage of Up, and the one that gives bifrost/phase=ready its
// meaning.
//
// Failure modes, all of them ending in a sanitized message safe for the
// bifrost/error annotation (member name + the pod's own reason — never a
// connection URI, token, or env value, and never pod logs, which are not
// sanitizable at all):
//
//   - a crash-looping container fails immediately rather than waiting out
//     the bound: CrashLoopBackOff is already the kubelet's verdict on a
//     container that failed and was restarted, so there's nothing left to
//     wait for.
//   - anything else that hasn't converged — including a member with no pods
//     at all, which is what a preview whose member rendered no Deployment
//     looks like — keeps polling until the bound and then fails with
//     whatever the last observed reason was. "No pods" must never loop
//     forever waiting for something that will never appear.
//
// A ListPods error does NOT fail the run on the spot: it's kept as the
// current reason and retried until the bound, so one API blip can't destroy
// a preview whose builds just took ten minutes. If it never clears, the
// timeout message carries it.
//
// appImages (member -> the image renderAndApply just applied for it) scopes
// the wait to this run's pods; see podsForMember for why that matters.
func (o *Orchestrator) waitForPods(ctx context.Context, ns string, members []string, appImages map[string]string) error {
	deadline := time.Now().Add(podReadyTimeout)
	// Always assigned before the deadline check below can read it: every
	// path through the loop body either returns or records a reason.
	var lastReason string
	for {
		pods, err := o.Kube.ListPods(ctx, ns)
		if err != nil {
			lastReason = "listing pods failed: " + sanitizeReason(err.Error())
		} else {
			notReady := membersNotReady(pods, members, appImages)
			if len(notReady) == 0 {
				return nil
			}
			for _, nr := range notReady {
				if nr.fatal {
					return errors.New(nr.String())
				}
			}
			reasons := make([]string, 0, len(notReady))
			for _, nr := range notReady {
				reasons = append(reasons, nr.String())
			}
			lastReason = strings.Join(reasons, "; ")
		}

		// Sleep the poll interval, but never past the deadline: the loop
		// always gets one final check exactly at the bound before giving up.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("timed out after %s waiting for pods: %s", podReadyTimeout, lastReason)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for pods: %w", ctx.Err())
		case <-time.After(min(podPollInterval, remaining)):
		}
	}
}

// memberNotReady is one member's reason for not being ready yet. fatal marks
// a reason that will not resolve on its own, so the wait can stop early
// instead of burning the whole bound.
type memberNotReady struct {
	member string
	reason string
	fatal  bool
}

func (m memberNotReady) String() string { return m.member + " not ready: " + m.reason }

// membersNotReady returns one entry per member that isn't ready yet, in
// members order (deterministic, so a multi-member failure message doesn't
// shuffle between polls). A ready member contributes nothing.
func membersNotReady(pods []kube.PodInfo, members []string, appImages map[string]string) []memberNotReady {
	var out []memberNotReady
	for _, svc := range members {
		owned, anyGeneration := podsForMember(pods, svc, appImages[svc])
		if len(owned) == 0 {
			reason := "no pods found"
			if anyGeneration {
				// The member has pods, just none from this apply yet — the
				// deployment controller hasn't created the new ReplicaSet's
				// pods. Saying "no pods found" here would send an operator
				// hunting for a missing Deployment that's right there.
				reason = "no pods running this preview's image yet"
			}
			out = append(out, memberNotReady{member: svc, reason: reason})
			continue
		}
		var found *memberNotReady
		for _, p := range owned {
			reason, fatal := podNotReadyReason(p)
			if reason == "" {
				continue
			}
			// Report the first unready pod, but let a fatal one anywhere in
			// the member's set win: a crash-looping replica alongside a
			// merely-still-starting one is the diagnosis worth surfacing.
			if found == nil || fatal {
				found = &memberNotReady{member: svc, reason: reason, fatal: fatal}
			}
			if fatal {
				break
			}
		}
		if found != nil {
			out = append(out, *found)
		}
	}
	return out
}

// podsForMember picks out the pods this run's Deployment for svc owns, and
// reports separately whether svc has any pods at all (from any generation).
//
// The join is via the pod's controlling ReplicaSet, whose name is always
// "<deployment>-<pod-template-hash>" and whose Deployment is named after the
// service. That naming isn't merely conventional: the generated overlay's
// deployment patch targets metadata.name: <svc>, so kustomize fails the
// render outright if the base has no Deployment by that name. (detectBase
// separately pins the container name and image repo, but not this.)
// Going through the ReplicaSet
// rather than matching pod names directly also structurally excludes
// Job-owned pods — a member's CronJob is suspended in a preview, but a
// leftover job pod named "<svc>-purge-..." would otherwise match a
// name-prefix test and could hold a preview un-ready forever. Pods in phase
// Succeeded are skipped for the same reason: a completed pod is not
// something to wait on.
//
// wantImage (the image renderAndApply just applied for svc, "" if it
// couldn't be determined) then narrows that to the generation this run
// created. This is what makes re-running Up a real recovery path rather
// than a guaranteed second failure: a rolling update keeps the previous
// generation's pods around until the new ones are ready, so a preview being
// re-run to FIX a crash-looping migration still has the broken pod sitting
// in its namespace the whole time the fixed one starts. Judging readiness on
// that pod would fail the fix on the strength of the bug it fixes.
//
// A re-run that produces the same image (rebuilding an unchanged commit) is
// deliberately not distinguished: those pods are running exactly what this
// run applied, so they are the right thing to judge.
func podsForMember(pods []kube.PodInfo, svc, wantImage string) (owned []kube.PodInfo, anyGeneration bool) {
	for _, p := range pods {
		if p.OwnerKind != "ReplicaSet" || p.Phase == "Succeeded" {
			continue
		}
		if p.OwnerName != svc && !strings.HasPrefix(p.OwnerName, svc+"-") {
			continue
		}
		anyGeneration = true
		if wantImage != "" && !runsImage(p, wantImage) {
			continue
		}
		owned = append(owned, p)
	}
	return owned, anyGeneration
}

// runsImage reports whether any of p's containers (app or init — the
// migrate initContainer shares the app image) runs image exactly.
func runsImage(p kube.PodInfo, image string) bool {
	for _, c := range p.Containers {
		if c.Image == image {
			return true
		}
	}
	for _, c := range p.InitContainers {
		if c.Image == image {
			return true
		}
	}
	return false
}

// podNotReadyReason returns "" when p is ready, else a short sanitized
// reason and whether that reason is terminal.
//
// Readiness itself is decided by the app containers alone — exactly what
// Kubernetes means by a ready pod, and the only rule that stays correct for
// a base declaring a sidecar-style init container that never terminates.
// Init containers are consulted only to *explain* a not-ready pod, and they
// go first: while one is running or failing, every app container reports the
// contentless "PodInitializing", so "migrate initContainer CrashLoopBackOff"
// is the difference between an operator knowing their migration failed and
// an operator staring at a generic pod message.
func podNotReadyReason(p kube.PodInfo) (string, bool) {
	ready := len(p.Containers) > 0
	for _, c := range p.Containers {
		if !c.Ready {
			ready = false
			break
		}
	}
	if ready {
		return "", false
	}
	for _, c := range p.InitContainers {
		if reason, fatal, problem := initContainerProblem(c); problem {
			return sanitizeReason(c.Name + " initContainer " + reason), fatal
		}
	}
	for _, c := range p.Containers {
		if c.Ready {
			continue
		}
		if c.WaitingReason != "" {
			return sanitizeReason(c.WaitingReason), c.WaitingReason == crashLoopBackOff
		}
	}
	if p.Phase != "" && p.Phase != "Running" {
		return sanitizeReason("pod phase " + p.Phase), false
	}
	return "containers not ready", false
}

// initContainerProblem reports whether an init container is visibly in
// trouble (problem), what to call it, and whether that's terminal. A
// successfully completed init container has exit code 0 and no waiting
// reason, so it reports nothing; one that's simply still running is likewise
// no problem in itself (the app containers' own state governs readiness).
//
// A non-zero exit code is a problem but not terminal: it's the state a
// failing init container passes through before the kubelet has restarted it
// enough times to call it CrashLoopBackOff, and the next poll upgrades it.
func initContainerProblem(c kube.ContainerInfo) (reason string, fatal, problem bool) {
	switch {
	case c.WaitingReason == crashLoopBackOff:
		return c.WaitingReason, true, true
	case c.WaitingReason != "" && c.WaitingReason != podInitializing:
		return c.WaitingReason, false, true
	case c.ExitCode != nil && *c.ExitCode != 0:
		if c.TerminatedReason != "" && c.TerminatedReason != "Error" {
			return fmt.Sprintf("%s (exit %d)", c.TerminatedReason, *c.ExitCode), false, true
		}
		return fmt.Sprintf("exited %d", *c.ExitCode), false, true
	}
	return "", false, false
}

// sanitizeReason bounds what a pod-derived reason can put into the
// bifrost/error annotation, which is world-readable in-cluster and served
// over the JSON API. Container reasons are short CamelCase tokens and are
// safe by construction; a client-go error string is free-form, so flatten
// whitespace and cap the length rather than trusting the source. Nothing
// secret-bearing is ever passed in here (no env values, no connection URIs,
// no pod logs) — this is the second line of defense, not the first.
func sanitizeReason(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const maxLen = 160
	if r := []rune(s); len(r) > maxLen {
		s = string(r[:maxLen]) + "..."
	}
	return s
}

// Down tears tag's preview down: delete its namespace, then best-effort
// delete a preview-<tag> Neon branch from every registry service that
// declares a Neon reference — not just the tag's current members, since a
// re-created preview may have changed its membership since the branch was
// created, and Down has no other way to know which projects to check. Every
// step is attempted regardless of earlier failures; all errors are joined
// and returned.
func (o *Orchestrator) Down(ctx context.Context, tag string) error {
	if !o.acquire(tag) {
		return ErrBusy
	}
	defer o.release(tag)

	var errs []error
	if err := o.Kube.DeleteNamespace(ctx, previewNamespace(tag)); err != nil {
		errs = append(errs, fmt.Errorf("delete namespace: %w", err))
	}

	svcs := o.Registry.Names() // already sorted: deterministic order for logs/tests

	branchName := previewBranchName(tag)
	for _, svc := range svcs {
		ref := o.Registry[svc].Neon
		if ref == nil {
			continue
		}
		branches, err := o.Neon.ListBranches(ctx, ref.Project)
		if err != nil {
			errs = append(errs, fmt.Errorf("neon list branches for %s: %w", svc, err))
			continue
		}
		for _, b := range branches {
			if b.Name != branchName {
				continue
			}
			if err := o.Neon.DeleteBranch(ctx, ref.Project, b.ID); err != nil {
				errs = append(errs, fmt.Errorf("neon delete branch for %s: %w", svc, err))
			}
			break
		}
	}
	return errors.Join(errs...)
}
