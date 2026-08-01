package preview

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/eswan18/bifrost/internal/github"
	"github.com/eswan18/bifrost/internal/kube"
)

// autoUpdatePollBudget bounds one whole poll — every preview it decides to
// refresh, not one of them.
//
// Deliberately much LARGER than the interval the watcher runs at, unlike the
// reaper's sweepBudget (which is an order of magnitude smaller than its
// hourly one). A sweep tears namespaces down in seconds; a poll may run a
// full Up, whose builds alone take minutes, so a budget under the interval
// would guarantee truncation rather than prevent a wedge. Overlap isn't the
// hazard that trades against: RunAutoUpdater is a single goroutine, so a poll
// that outlives its interval simply delays the next tick (time.Ticker drops
// ticks nobody is receiving) instead of running two Ups at once — and the
// busy set would refuse the duplicate anyway.
//
// 30 minutes matches the API layer's own per-Up ceiling
// (asyncOrchestrationTimeout), which is the same work by the same code path.
// A poll that hits it truncates whatever Up is in flight, which fail() marks
// failed; the next tick does NOT retry it, because that run already recorded
// the SHA it attempted (see sourceSHAsAnnotation).
const autoUpdatePollBudget = 30 * time.Minute

// PollAutoUpdates re-runs Up for every preview that opted into auto-update
// (bifrost/auto-update = "true") and whose branch has moved since it was
// deployed, returning the tags it successfully refreshed.
//
// The comparison is per member: bifrost/apps names them, bifrost/source-shas
// records the commit each was deployed from, and one BranchSHA call per
// member says where that repo's branch points now. ANY member differing
// re-runs the whole preview — Up is all-or-nothing (it rebuilds and reapplies
// every member), so there is no such thing as refreshing one of them.
//
// Every skip below is silent and none is an error, matching the reaper:
//
//   - a preview without bifrost/auto-update = "true" — the default, and the
//     overwhelmingly common case.
//   - a namespace already Terminating: it is being torn down.
//   - bifrost/phase = creating: an Up is already running. Re-entering would
//     be refused by the busy set anyway, and once that Up finishes it will
//     have recorded whatever SHAs it deployed.
//   - a Busy tag: an Up or Down holds it. Up's own acquire covers the race;
//     checking first keeps a deferral off the error list, where it would read
//     as a failure.
//   - a namespace labelled bifrost/preview=true but not named preview-<tag>:
//     there is no tag to derive safely. Unlike the reaper this is not even
//     logged — that loop runs hourly, this one every two minutes, and a
//     warning nothing can act on is not worth 720 log lines a day.
//   - a preview with no members recorded in bifrost/apps: nothing to compare.
//
// One further skip IS logged, because it is the one that silently stops a
// preview from ever updating again: a member whose branch has been DELETED
// from its repo (github.ErrNoBranch). That is not an error — deleting the
// branch after merging is the normal end of a preview's life — but the
// preview then sits at whatever it last deployed until someone tears it down,
// so it says so once per tick rather than never.
//
// Membership CHANGES are out of scope: a repo that newly gained the branch is
// not picked up, because this only ever asks about the members already
// recorded on the namespace. That needs a manual `bif preview up` re-run, and
// is documented as a limitation in docs/preview-environments.md.
//
// A re-run preserves the preview's existing state rather than resetting it:
// bifrost/expires-at is parsed off the namespace and passed straight back
// through UpOptions.ExpiresAt (an absolute instant precisely so this cannot
// extend it — see UpOptions), and AutoUpdate is passed back as true so the
// preview keeps following its branch. An auto-update must never extend,
// clear, or disable anything the user set.
//
// One preview's failure doesn't stop the others: errors accumulate across the
// pass and come back joined, exactly as the reaper's do. A ListNamespaces
// failure aborts the pass — with nothing listed there is nothing to compare.
func (o *Orchestrator) PollAutoUpdates(ctx context.Context) ([]string, error) {
	namespaces, err := o.Kube.ListNamespaces(ctx, previewLabelSelector)
	if err != nil {
		return nil, fmt.Errorf("preview: PollAutoUpdates: list namespaces: %w", err)
	}

	var (
		refreshed []string
		errs      []error
	)
	for _, ns := range namespaces {
		tag, ok := autoUpdatable(ns)
		if !ok {
			continue
		}
		if o.Busy(tag) {
			continue
		}
		changes, err := o.movedMembers(ctx, ns)
		if err != nil {
			if errors.Is(err, errBranchGone) {
				slog.Info("preview: auto-update skipping a preview whose branch is gone",
					"tag", tag, "err", err)
				continue
			}
			errs = append(errs, fmt.Errorf("auto-update %s: %w", tag, err))
			continue
		}
		if len(changes) == 0 {
			continue
		}
		// Read here rather than up front: parseExpiry warns about an
		// unparseable annotation, and doing that once per tick for a preview
		// nothing is about to happen to would be pure noise.
		expiresAt, _ := parseExpiry(ns)
		branch := ns.Annotations[branchAnnotationKey]
		// Logged BEFORE the run, and unconditionally: this redeploys an
		// environment someone may be using at this moment, and the log line
		// is the only notice they get. Before rather than after so a run that
		// wedges or dies with the process is still accounted for, and naming
		// the moved member because "why did my preview restart" is answered
		// by which commit arrived, not by the fact that one did.
		slog.Info("preview: auto-updating preview",
			"tag", tag,
			"branch", branch,
			"changed", strings.Join(changes, ", "),
		)
		if err := o.Up(ctx, branch, UpOptions{ExpiresAt: expiresAt, AutoUpdate: true}); err != nil {
			errs = append(errs, fmt.Errorf("auto-update %s: %w", tag, err))
			continue
		}
		refreshed = append(refreshed, tag)
	}
	return refreshed, errors.Join(errs...)
}

// autoUpdatable reports whether ns is a preview this pass may consider, and
// the tag to act on. See PollAutoUpdates for why each rejection is silent.
func autoUpdatable(ns kube.NamespaceInfo) (string, bool) {
	if ns.Annotations[autoUpdateAnnotationKey] != "true" {
		return "", false
	}
	if ns.Phase == "Terminating" || ns.Annotations[phaseAnnotationKey] == "creating" {
		return "", false
	}
	tag, named := strings.CutPrefix(ns.Name, previewNSPrefix)
	if !named || tag == "" {
		return "", false
	}
	return tag, true
}

// errBranchGone marks the one BranchSHA failure that isn't a failure: the
// branch no longer exists in a member's repo (github.ErrNoBranch), which
// happens the moment a merged branch is deleted. PollAutoUpdates skips and
// logs it rather than reporting it, so a tidy merge doesn't produce an error
// every two minutes until someone runs `bif preview down`.
var errBranchGone = errors.New("branch no longer exists in this repo")

// movedMembers returns one human-readable entry per member of ns whose branch
// SHA differs from what bifrost/source-shas says was deployed — the input to
// both the "should this re-run" decision and the log line that announces it.
//
// A member with no recorded SHA (an unreadable or partial annotation) counts
// as moved. That errs toward one redundant redeploy rather than a preview
// that silently never updates, and it converges: the re-run writes the
// annotation properly, so the next tick compares two real SHAs.
//
// Any BranchSHA error aborts the whole preview rather than being treated as
// "unchanged" — a repo bifrost can't reach says nothing about whether its
// branch moved, and half-answering the question would redeploy on partial
// evidence. github.ErrNoBranch is folded into errBranchGone with the service
// named, since the caller handles it differently from a real failure.
func (o *Orchestrator) movedMembers(ctx context.Context, ns kube.NamespaceInfo) ([]string, error) {
	branch := ns.Annotations[branchAnnotationKey]
	if branch == "" {
		return nil, errors.New("namespace has no bifrost/branch annotation")
	}
	deployed := parseSourceSHAs(ns.Annotations[sourceSHAsAnnotationKey])

	var changes []string
	for _, svc := range splitAnnotationList(ns.Annotations[appsAnnotationKey]) {
		sha, err := o.GitHub.BranchSHA(ctx, o.Fleet.RepoFor(svc), branch)
		switch {
		case errors.Is(err, github.ErrNoBranch):
			return nil, fmt.Errorf("%s: %w", svc, errBranchGone)
		case err != nil:
			return nil, fmt.Errorf("checking branch for %s: %w", svc, err)
		}
		if was := deployed[svc]; was != sha {
			changes = append(changes, fmt.Sprintf("%s %s -> %s", svc, orUnknown(was), sha))
		}
	}
	return changes, nil
}

// orUnknown renders an absent recorded SHA for the log line, so a member that
// had none reads as "(unknown) -> abc123" rather than " -> abc123".
func orUnknown(sha string) string {
	if sha == "" {
		return "(unknown)"
	}
	return sha
}

// parseSourceSHAs reads bifrost/source-shas back into service -> full SHA.
// It is the exact inverse of sourceSHAsAnnotation (orchestrator.go), which is
// the only thing that writes that annotation.
//
// Every malformed piece is dropped rather than failing the read, and every
// drop lands on the same side: the member reads as having no recorded SHA,
// which movedMembers counts as moved. An annotation bifrost can't parse
// therefore costs one redundant redeploy, never a preview that stops
// updating without saying so.
func parseSourceSHAs(raw string) map[string]string {
	shas := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		svc, sha, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		svc, sha = strings.TrimSpace(svc), strings.TrimSpace(sha)
		if svc == "" || sha == "" {
			continue
		}
		shas[svc] = sha
	}
	return shas
}

// splitAnnotationList splits a comma-joined annotation (bifrost/apps) into
// its non-empty, trimmed entries — the same reading the web layer's record
// derivation applies, so a trailing comma can't conjure a phantom member.
func splitAnnotationList(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// RunAutoUpdates polls for previews whose branch has moved every `every`
// until ctx is done. every must be positive (it feeds a time.Ticker).
//
// Polling, not a GitHub webhook, and that is a deliberate trade. A tick only
// ever sees where the branch points NOW, so five pushes in a minute coalesce
// into one redeploy for free — a webhook would fire five times and need a
// debounce queue to avoid five builds. It also adds no new unauthenticated
// public endpoint to an internet-facing service and needs no per-repo webhook
// configuration. The cost is one BranchSHA call per member of each opted-in
// preview per tick, and there are rarely more than two previews at all.
//
// The first poll happens one full interval in, never at startup, for the same
// reason RunReaper's first sweep does: bifrost restarts often (spot-node
// preemptions are routine here), and a restart is not a reason to redeploy
// anyone's environment.
//
// Like RunReaper, the loop stops on ctx but each poll runs on a context
// DETACHED from it, bounded by autoUpdatePollBudget. That matches how every
// other Up already runs — the API layer hands Up a WithoutCancel context on
// its own 30-minute budget — so an in-flight auto-update behaves on shutdown
// exactly as a manual `bif preview up` does: it is not cancelled, but nothing
// waits on it either, so a process that exits fast can still leave the
// preview at bifrost/phase=creating (the documented stuck-creating case,
// recovered by re-running up).
//
// Prod runs a single bifrost replica, so exactly one of these loops exists.
// Two replicas would double-poll; the busy set makes that harmless in the
// same way it does for the reaper, but nothing here elects a leader.
func (o *Orchestrator) RunAutoUpdates(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	slog.Info("preview: auto-update watcher started", "interval", every.String())
	for {
		select {
		case <-ctx.Done():
			slog.Info("preview: auto-update watcher stopped")
			return
		case <-ticker.C:
			refreshed, err := o.poll(ctx)
			if err != nil {
				// Logged, not fatal: the next tick re-decides from the
				// cluster, and whatever else this pass refreshed still counts.
				slog.Error("preview: auto-update poll had failures", "err", err)
			}
			if len(refreshed) > 0 {
				slog.Info("preview: auto-update poll complete", "refreshed", strings.Join(refreshed, ","))
			} else {
				// The overwhelmingly common outcome — every two minutes,
				// forever — and worth nothing at info level.
				slog.Debug("preview: auto-update poll refreshed nothing")
			}
		}
	}
}

// poll runs one PollAutoUpdates pass on a context detached from ctx's
// cancellation and bounded by autoUpdatePollBudget. See RunAutoUpdates.
func (o *Orchestrator) poll(ctx context.Context) ([]string, error) {
	pollCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), autoUpdatePollBudget)
	defer cancel()
	return o.PollAutoUpdates(pollCtx)
}
