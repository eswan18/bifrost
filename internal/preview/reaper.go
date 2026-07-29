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
	// previewNSPrefix is what previewNamespace prepends to a tag; the sweep
	// trims it back off to recover the tag Down wants. Shared so the two
	// directions can't drift apart.
	previewNSPrefix = "preview-"
	// previewLabelSelector selects preview namespaces — the same label Up
	// writes, and the same selector the web layer lists previews with.
	previewLabelSelector = "bifrost/preview=true"
)

// sweepBudget bounds one whole PurgeExpired call. A sweep is one List plus,
// per expired preview, a namespace delete and a couple of Neon calls per
// project — seconds each, given client-go's own 15s per-call cap. Ten minutes
// is far more than any realistic sweep needs (dozens of teardowns against a
// slow API server) while staying an order of magnitude under the hourly
// interval, so a wedged sweep can never still be running when the next tick
// arrives. Its real job is to stop a hung call from pinning the detached
// context open indefinitely, not to pace the work.
const sweepBudget = 10 * time.Minute

// PurgeExpired tears down every preview whose bifrost/expires-at has passed,
// returning the tags it reclaimed. now is a parameter rather than a call to
// time.Now so callers (and tests) fix the instant the whole sweep is judged
// against.
//
// It reclaims only on unambiguous evidence that a preview is past due.
// Anything else is skipped silently — not reported as an error, because none
// of these are one:
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
//     reclaim it. That is deliberate: see the note below.
//   - a tag that's Busy: an Up or Down holds it. Down would refuse it with
//     ErrBusy anyway; checking first keeps that off the error list, where it
//     would read as a failure rather than as the sweep deferring.
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
		if ns.Phase == "Terminating" || ns.Annotations[phaseAnnotationKey] == "creating" {
			continue
		}
		expiry, ok := parseExpiry(ns)
		if !ok || !expiry.Before(now) {
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
// lets that in-flight teardown finish through the shutdown drain. It is not
// absolute: nothing waits on this goroutine, so a process that exits fast
// enough can still cut a sweep short. It removes the guaranteed truncation,
// not the possibility of one.
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
			reclaimed, err := o.sweep(ctx)
			if err != nil {
				// Logged, not fatal: the next sweep retries, and whatever
				// else this one reclaimed still counts.
				slog.Error("preview: expiry sweep had failures", "err", err)
			}
			if len(reclaimed) == 0 {
				// The overwhelmingly common outcome — hourly forever, and
				// worth nothing at info level.
				slog.Debug("preview: expiry sweep reclaimed nothing")
				continue
			}
			slog.Info("preview: expiry sweep complete", "reclaimed", strings.Join(reclaimed, ","))
		}
	}
}

// sweep runs one PurgeExpired on a context detached from ctx's cancellation
// and bounded by sweepBudget. See RunReaper for why a teardown must not be
// cancellable halfway through.
func (o *Orchestrator) sweep(ctx context.Context) ([]string, error) {
	sweepCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sweepBudget)
	defer cancel()
	return o.PurgeExpired(sweepCtx, time.Now().UTC())
}
