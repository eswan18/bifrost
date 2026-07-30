package preview

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/eswan18/bifrost/internal/kube"
)

const (
	// previewNSPrefix is what previewNamespace and previewBranchName prepend
	// to a tag; both sweeps trim it back off to recover the tag. Shared so the
	// compose and match directions can't drift apart.
	previewNSPrefix = "preview-"
	// previewLabelSelector selects preview namespaces — the same label Up
	// writes, and the same selector the web layer lists previews with.
	previewLabelSelector = "bifrost/preview=true"
)

// sweepBudget bounds one whole sweep — PurgeExpired and PurgeOrphanedBranches
// together. A sweep is one namespace List plus, per expired preview, a
// namespace delete and a couple of Neon calls per project, plus one further
// List per pass and one ListBranches per distinct Neon project — seconds each,
// given client-go's own 15s per-call cap. Ten minutes is far more than any
// realistic sweep needs (dozens of teardowns against a slow API server) while
// staying an order of magnitude under the hourly interval, so a wedged sweep
// can never still be running when the next tick arrives. Its real job is to
// stop a hung call from pinning the detached context open indefinitely, not to
// pace the work.
const sweepBudget = 10 * time.Minute

// PurgeExpired tears down every preview whose bifrost/expires-at has passed,
// returning the tags it reclaimed. now is a parameter rather than a call to
// time.Now so callers (and tests) fix the instant the whole sweep is judged
// against.
//
// It reclaims only on unambiguous evidence that a preview is past due. Every
// rule about a namespace's own state lives in reclaimable; a namespace it
// rejects is skipped silently — not reported as an error, because none of
// those cases are one. Three further skips are the sweep's own:
//
//   - a namespace labelled bifrost/preview=true but not named preview-<tag>:
//     there is no tag to derive from it safely. Logged as a warning (silent
//     like the rest as far as the returned error goes), since it means
//     something outside bifrost applied that label.
//   - a tag that's Busy: an Up or Down holds it. Down would refuse it with
//     ErrBusy anyway; checking first keeps that off the error list, where it
//     would read as a failure rather than as the sweep deferring.
//   - a namespace whose re-read, immediately before teardown, no longer says
//     it is reclaimable — or that has vanished by then. See below.
//
// The rules are applied twice, to two different reads of the same namespace.
// ListNamespaces at the top is a snapshot, and a sweep is not instantaneous:
// an Up that starts AND FINISHES while the loop works through the namespaces
// ahead of this one leaves that snapshot describing a preview that no longer
// exists — one just renewed with a fresh 24h expiry, or back at creating.
// Busy here and Down's own acquire both cover an Up still in flight; neither
// covers one that already completed. So the decision is re-confirmed against
// a GetNamespace taken immediately before Down, through the same predicate,
// and only a namespace still reclaimable on that fresh copy is torn down.
// A namespace that has vanished by then is skipped silently: something else
// already tore it down, which is the outcome the sweep wanted anyway. A
// GetNamespace that actually fails skips it too, but is recorded as an error
// — a namespace bifrost cannot read is not evidence of anything, least of all
// a reason to delete it.
//
// Deleting an environment someone is still using is the failure mode this
// function exists to avoid, and every one of those rules trades a preview
// living up to one extra interval for never making that mistake.
//
// One preview's teardown failing does not abort the sweep: errors accumulate
// and come back joined, the same way Down accumulates its own Neon failures.
func (o *Orchestrator) PurgeExpired(ctx context.Context, now time.Time) ([]string, error) {
	namespaces, err := o.Kube.ListNamespaces(ctx, previewLabelSelector)
	if err != nil {
		return nil, fmt.Errorf("preview: PurgeExpired: list namespaces: %w", err)
	}

	var (
		reclaimed []string
		errs      []error
	)
	for _, ns := range namespaces {
		if _, ok := reclaimable(ns, now); !ok {
			continue
		}
		// A labelled namespace that isn't named preview-<tag> has no tag to
		// derive: TrimPrefix would hand back its own name, and Down would go
		// on to delete preview-<that name> — some other preview entirely.
		tag, named := strings.CutPrefix(ns.Name, previewNSPrefix)
		if !named || tag == "" {
			slog.Warn("preview: expiry sweep skipping an off-convention namespace", "ns", ns.Name)
			continue
		}
		// Two halves of one rule, not a check plus a redundant fallback. This
		// one is the fast path and the documented rule; Down's own acquire is
		// what actually makes it race-free, and the ErrBusy arm below is how
		// its verdict comes back when a tag goes busy in the window between
		// here and there (an operator's own `ib preview down` landing in
		// between). Neither is dead code, and no test can tell them apart —
		// their observable behavior is identical by construction.
		if o.Busy(tag) {
			continue
		}
		// The snapshot above may be minutes old by now; re-decide on a fresh
		// copy. Both halves of Down — the namespace AND the Neon branch — are
		// unrecoverable, so this read is worth its cost on the rare path that
		// reaches it (only already-expired previews get this far).
		fresh, found, err := o.Kube.GetNamespace(ctx, ns.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("purge %s: re-read namespace: %w", tag, err))
			continue
		}
		if !found {
			continue
		}
		expiry, ok := reclaimable(fresh, now)
		if !ok {
			continue
		}
		if err := o.Down(ctx, tag); err != nil {
			// The sweep deferring, exactly as the pre-check above is — not a
			// failure to report at Error every hour.
			if errors.Is(err, ErrBusy) {
				continue
			}
			errs = append(errs, fmt.Errorf("purge %s: %w", tag, err))
			continue
		}
		// Logged individually, at info: an operator whose environment
		// disappeared must be able to find out from bifrost's logs that it
		// was reclaimed, when it had been due, and how long it overran.
		slog.Info("preview: reclaimed expired preview",
			"tag", tag,
			"expired_at", expiry.Format(time.RFC3339),
			"overdue", now.Sub(expiry).Round(time.Second).String(),
		)
		reclaimed = append(reclaimed, tag)
	}
	return reclaimed, errors.Join(errs...)
}

// reclaimable reports whether ns — on the evidence THIS copy of it carries —
// is a preview the sweep may tear down at now, returning the expiry it judged
// so the caller can log what it acted on.
//
// It is the single definition of that rule, deliberately: PurgeExpired asks it
// once about the List snapshot and again about the namespace re-read just
// before teardown, and two hand-written copies of these conditions drifting
// apart would be a bug of exactly the kind the re-read exists to prevent.
//
// False means "skip, silently" — none of these are errors:
//
//   - no bifrost/expires-at, an empty one, or one that doesn't parse. The
//     annotation is optional and most previews carry none; a value bifrost
//     can't read means "no expiry", never "expired long ago".
//   - an expiry still in the future.
//   - a namespace already Terminating: its teardown is underway.
//   - bifrost/phase=creating: an Up is still running, and deleting the
//     namespace out from under it races a create that may be minutes from
//     finishing. Note what this does NOT say: that the preview will be
//     reclaimed on a later pass. Nothing moves a namespace out of creating
//     except the Up that wrote it (to ready, or to failed via fail()), so a
//     preview whose Up died with the process — a spot-node preemption, the
//     routine case — sits at creating forever and this sweep will never
//     reclaim it. That is deliberate: see below.
//
// The permanently-creating case is left to a human on purpose. Bounding it
// would mean guessing, from bifrost/step-since, when an Up stopped making
// progress — and that annotation is best-effort (step() swallows its write
// errors), absent on a preview that died before its first step, and legally
// stale for a long while: a real Up can spend minutes in a build and five
// more waiting for pods, under a 30-minute API-layer ceiling. A bound short
// enough to be useful is a bound that eventually deletes a live create, which
// is the one outcome this whole design refuses. A wedged creating preview is
// visible as such in the UI and `ib preview list`, and `ib preview down`
// tears it down on demand (teardown does not consult the phase at all), so
// the manual path is short. Recorded as a known limitation, not an oversight.
func reclaimable(ns kube.NamespaceInfo, now time.Time) (time.Time, bool) {
	if ns.Phase == "Terminating" || ns.Annotations[phaseAnnotationKey] == "creating" {
		return time.Time{}, false
	}
	expiry, ok := parseExpiry(ns)
	if !ok || !expiry.Before(now) {
		return time.Time{}, false
	}
	return expiry, true
}

// parseExpiry reads ns's bifrost/expires-at. ok=false means "no expiry":
// absent, empty, and unparseable are deliberately one case, since the only
// safe reading of a value bifrost can't interpret is that the preview isn't
// due. A malformed one is logged (it means something wrote the annotation by
// hand, or wrote it wrong) but never acted on.
func parseExpiry(ns kube.NamespaceInfo) (time.Time, bool) {
	raw := ns.Annotations[expiresAtAnnotationKey]
	if raw == "" {
		return time.Time{}, false
	}
	expiry, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		slog.Warn("preview: ignoring unparseable expiry annotation", "ns", ns.Name, "expires_at", raw)
		return time.Time{}, false
	}
	return expiry, true
}

// minOrphanAge is how long a preview-<tag> Neon branch with no namespace must
// have existed before PurgeOrphanedBranches will delete it.
//
// Belt-and-braces, NOT a fix for a known race. Up creates the namespace
// (EnsureNamespace) before it creates the branch (branchNeonDatabases), so
// there is no instant at which a branch exists and its namespace does not: an
// in-flight create can never look like an orphan, whatever its age. The floor
// is insurance against that invariant being broken later — a future reordering
// of Up would then degrade this sweep into a delay (a branch reclaimed up to
// an hour late) instead of into data loss (a live preview's database deleted
// out from under a create still running). One hour is also one full sweep
// interval, so it costs at most one extra pass on the path it does govern.
const minOrphanAge = time.Hour

// PurgeOrphanedBranches deletes every preview-<tag> Neon branch, in every Neon
// project the registry references, that no longer has a preview namespace to
// belong to. It returns "<project>/<branch>" for each branch it deleted —
// qualified by project because a branch name is only unique within one, and
// two projects can each hold a preview-<tag> for the same tag. now is a
// parameter rather than a call to time.Now for the same reason PurgeExpired's
// is: it fixes the instant the whole sweep is judged against.
//
// This is the recovery path for the one failure Down cannot recover from
// itself. Down deletes the namespace first and the Neon branches second, so an
// interruption in between — the process exiting on a spot-node preemption,
// sweepBudget expiring mid-teardown — leaves a branch that nothing can ever
// find again: the preview is gone from ListNamespaces, so no later expiry
// sweep and no repeat `ib preview down` can name it, and what's left is a paid,
// invisible database. Reclaiming from the Neon side (rather than reversing
// Down's order) avoids ever having a window where a preview's pods run against
// a database that has already been deleted.
//
// It is cheap by construction: the work is O(distinct Neon projects), not
// O(previews). ListBranches returns every branch in a project in one call —
// exactly what Down already does per project — and the registry names two
// distinct projects today, so a full orphan check is two extra HTTP calls per
// sweep however many previews exist.
//
// Every skip is silent and none is an error, matching PurgeExpired:
//
//   - a branch outside the preview-<tag> convention, or named exactly
//     "preview-" with nothing after it: there is no tag to derive, and a
//     branch bifrost didn't name is not bifrost's to delete.
//   - a tag that still has a namespace, Terminating included. A Terminating
//     namespace still exists, so the preview is still visible to the expiry
//     sweep and to `ib preview down`; its branch isn't orphaned.
//   - a Busy tag: an Up or Down holds it, so a missing namespace means a
//     teardown in flight — Down sitting between its two halves — rather than
//     one that died.
//   - a branch younger than minOrphanAge.
//
// A project whose ListBranches fails is recorded and skipped, and the
// remaining projects are still swept, the same way Down accumulates its own
// Neon failures. A ListNamespaces failure, by contrast, aborts the whole pass:
// an empty live set alongside a full branch list is indistinguishable from
// "every preview is an orphan", and this function deletes databases.
func (o *Orchestrator) PurgeOrphanedBranches(ctx context.Context, now time.Time) ([]string, error) {
	live, err := o.livePreviewTags(ctx)
	if err != nil {
		return nil, fmt.Errorf("preview: PurgeOrphanedBranches: %w", err)
	}

	var (
		deleted []string
		errs    []error
	)
	for _, project := range o.neonProjects() {
		branches, err := o.Neon.ListBranches(ctx, project)
		if err != nil {
			errs = append(errs, fmt.Errorf("orphan sweep %s: list branches: %w", project, err))
			continue
		}
		// Nothing anywhere else in bifrost counts branches, storage, compute,
		// or build minutes against Neon's own quotas, so this is the only
		// signal of where a project stands before a limit is hit rather than
		// only after (a Neon error surfacing one). It costs nothing beyond
		// what ListBranches above already fetched, and — like "sweep
		// reclaimed nothing" in RunReaper — it fires every project, every
		// hour, forever, which is exactly why it's debug rather than info:
		// routine state, not something that happened.
		slog.Debug("preview: orphan sweep branch count", "project", project, "count", len(branches))
		for _, b := range branches {
			tag, named := strings.CutPrefix(b.Name, previewNSPrefix)
			if !named || tag == "" {
				continue
			}
			// The orphan test, and with it the entire safety argument for
			// this function.
			//
			// INVARIANT THIS DEPENDS ON: Up (orchestrator.go) calls
			// o.Kube.EnsureNamespace BEFORE o.branchNeonDatabases.
			// A preview therefore has its namespace before it has its branch,
			// and no window exists in which the branch exists and the
			// namespace does not — which is what makes "branch with no
			// namespace" mean "orphan" rather than "create in progress". IF
			// THAT ORDER IS EVER REVERSED, this line begins deleting the
			// databases of previews that are still being created, and the
			// minOrphanAge floor below is the only thing standing between that
			// reordering and immediate data loss. Move the Neon step earlier in
			// Up and this sweep must be re-derived, not merely re-tested.
			//
			// Busy covers the only transient that does produce a branch with no
			// namespace: Down itself, caught between its two halves.
			if live[tag] || o.Busy(tag) {
				continue
			}
			age := now.Sub(b.CreatedAt)
			if age < minOrphanAge {
				continue
			}
			if err := o.Neon.DeleteBranch(ctx, project, b.ID); err != nil {
				errs = append(errs, fmt.Errorf("orphan sweep %s: delete branch %s: %w", project, b.Name, err))
				continue
			}
			// Logged individually, at info, and never conditionally: this
			// deletes a database branch, and the log line is the only record
			// that it happened. Project, branch and age are what an operator
			// needs to reconcile it against Neon's own console afterwards.
			slog.Info("preview: deleted orphaned neon branch",
				"project", project,
				"branch", b.Name,
				"age", age.Round(time.Second).String(),
			)
			deleted = append(deleted, project+"/"+b.Name)
		}
	}
	return deleted, errors.Join(errs...)
}

// livePreviewTags is the set of tags that still have a preview namespace.
//
// Terminating namespaces are deliberately included: a namespace mid-delete
// still exists, so its branch belongs to the Down that is tearing it down (or
// to a later expiry sweep), not to the orphan sweep. Off-convention names are
// dropped for the same reason PurgeExpired refuses to act on them — there is
// no tag to derive — which is safe in this direction too, since a namespace
// that yields no tag can't be shadowing a branch that does.
func (o *Orchestrator) livePreviewTags(ctx context.Context) (map[string]bool, error) {
	namespaces, err := o.Kube.ListNamespaces(ctx, previewLabelSelector)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	live := make(map[string]bool, len(namespaces))
	for _, ns := range namespaces {
		if tag, named := strings.CutPrefix(ns.Name, previewNSPrefix); named && tag != "" {
			live[tag] = true
		}
	}
	return live, nil
}

// neonProjects is the distinct Neon project IDs the registry references, in
// service-name order (Names is sorted, so the result is deterministic for logs
// and tests).
//
// Deduplicated by project ID: nothing stops two services from sharing one Neon
// project — a project holds many databases, and a registry entry names a
// database and role within one — and listing the same project twice would
// double the sweep's API calls and reach a second verdict on branches already
// judged. Down can be indifferent to this (it matches one exact branch name
// per service and a second pass simply finds nothing), but a sweep that
// deletes on an inferred condition should not evaluate anything twice.
func (o *Orchestrator) neonProjects() []string {
	seen := make(map[string]bool)
	var projects []string
	for _, svc := range o.Registry.Names() {
		ref := o.Registry[svc].Neon
		if ref == nil || seen[ref.Project] {
			continue
		}
		seen[ref.Project] = true
		projects = append(projects, ref.Project)
	}
	return projects
}

// RunReaper sweeps expired previews every `every` until ctx is done. every
// must be positive (it feeds a time.Ticker).
//
// The first sweep happens one full interval in, never at startup: bifrost
// restarts often (spot-node preemptions are routine in this cluster), and a
// purge firing on each of those is both surprising and unnecessary — a
// preview that outlives its expiry by up to one interval is the accepted cost
// of this whole design.
//
// Prod runs a single bifrost replica, so exactly one of these loops exists.
// Two replicas would double-fire, which teardown survives (Down is idempotent
// and both loops go through the same busy set), but this is written down as
// an observation rather than as a guarantee anything here provides: nothing
// coordinates leadership between replicas.
//
// The loop stops on ctx (that is how shutdown reaches it), but each sweep runs
// on a context DETACHED from it, bounded by sweepBudget — the same
// WithTimeout(WithoutCancel(...)) shape the API layer wraps its async
// Up/Down in, and for a sharper reason here. Down deletes the namespace
// first and the Neon branches second. A SIGTERM landing in between would
// cancel the Neon calls, and once the namespace is gone the preview is no
// longer in ListNamespaces at all — no later sweep would ever see it again,
// so the branch would be orphaned with nothing left to retry it. Detaching
// buys that in-flight teardown whatever time the process has left, instead of
// cancelling it the instant the signal lands — and that is less than the
// shutdown drain suggests: main gives srv.Shutdown a 10s ceiling, but Shutdown
// returns as soon as connections are idle and main returns right behind it, so
// the real post-SIGTERM window is usually well under a second. It is not
// absolute: nothing waits on this goroutine, so a process that exits fast
// enough can still cut a sweep short. It removes the guaranteed truncation,
// not the possibility of one — which is why PurgeOrphanedBranches exists to
// reclaim what a truncated teardown leaves behind.
func (o *Orchestrator) RunReaper(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	slog.Info("preview: expiry sweep started", "interval", every.String())
	for {
		select {
		case <-ctx.Done():
			slog.Info("preview: expiry sweep stopped")
			return
		case <-ticker.C:
			reclaimed, orphans, err := o.sweep(ctx)
			if err != nil {
				// Logged, not fatal: the next sweep retries, and whatever
				// else this one reclaimed still counts.
				slog.Error("preview: expiry sweep had failures", "err", err)
			}
			if len(reclaimed) > 0 {
				slog.Info("preview: expiry sweep complete", "reclaimed", strings.Join(reclaimed, ","))
			}
			if len(orphans) > 0 {
				slog.Info("preview: orphan sweep reclaimed neon branches", "branches", strings.Join(orphans, ","))
			}
			if len(reclaimed) == 0 && len(orphans) == 0 {
				// The overwhelmingly common outcome — hourly forever, and
				// worth nothing at info level.
				slog.Debug("preview: sweep reclaimed nothing")
			}
		}
	}
}

// sweep runs one full pass — PurgeExpired, then PurgeOrphanedBranches — on a
// single context detached from ctx's cancellation and bounded by sweepBudget.
// See RunReaper for why a teardown must not be cancellable halfway through.
//
// Both passes always run: they answer different questions (a live preview past
// its expiry vs. a branch whose preview is already gone), and one failing says
// nothing about the other, so their errors are joined rather than sequenced.
//
// The orphan pass runs SECOND, deliberately. It reads the cluster through its
// own ListNamespaces, so going last means it sees the state PurgeExpired has
// just left behind: a teardown in this very sweep whose Neon half failed after
// its namespace delete succeeded is reclaimed on this same tick rather than an
// hour later. Running it first would find that same branch still shadowed by a
// namespace, and skip it.
//
// One now for both passes, taken once: the sweep is a single decision about a
// single instant, exactly as each pass's own now parameter is. It also errs
// safely for the orphan pass, which runs later than that instant — every
// branch is judged marginally younger than it really is, never older.
func (o *Orchestrator) sweep(ctx context.Context) (reclaimed, orphans []string, err error) {
	sweepCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sweepBudget)
	defer cancel()
	now := time.Now().UTC()
	reclaimed, expiredErr := o.PurgeExpired(sweepCtx, now)
	orphans, orphanErr := o.PurgeOrphanedBranches(sweepCtx, now)
	return reclaimed, orphans, errors.Join(expiredErr, orphanErr)
}
