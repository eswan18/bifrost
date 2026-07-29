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
	// expiresAtAnnotationKey holds a preview's reclaim time as an absolute
	// RFC3339 instant; see expiresAtAnnotation for who writes it.
	expiresAtAnnotationKey = "bifrost/expires-at"
)

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
//   - bifrost/phase=creating: an Up is still running. Deleting the namespace
//     out from under it races a create that may be minutes from finishing,
//     and the preview will still be expired on the next pass.
//   - a tag that's Busy: an Up or Down holds it. Down would refuse it with
//     ErrBusy anyway; checking first keeps that off the error list, where it
//     would read as a failure rather than as the sweep deferring.
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
		if ns.Phase == "Terminating" || ns.Annotations["bifrost/phase"] == "creating" {
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
		if o.Busy(tag) {
			continue
		}
		if err := o.Down(ctx, tag); err != nil {
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
			reclaimed, err := o.PurgeExpired(ctx, time.Now().UTC())
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
