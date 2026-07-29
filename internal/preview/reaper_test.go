package preview

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eswan18/bifrost/internal/neon"
)

// ---- fixtures ---------------------------------------------------------------

// sweepNow is the instant every PurgeExpired call in this file is told it is.
// Fixed, not time.Now(): PurgeExpired takes now as a parameter precisely so
// these tests need no clock injection and no sleeping.
var sweepNow = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

// reaperNeonProject is the Neon project every preview here branches. Down
// deletes preview-<tag> from it, and that deletion is how these tests tell a
// real teardown apart from a namespace delete alone.
const reaperNeonProject = "proj-api"

// expiryAt renders a bifrost/expires-at value offset from sweepNow: negative
// is past due, positive is not yet.
func expiryAt(offset time.Duration) string {
	return sweepNow.Add(offset).Format(time.RFC3339)
}

// expiredAnnotation is the annotation set of a preview two hours past due —
// the ordinary case the sweep exists to reclaim, reused by the tests whose
// subject is some OTHER reason the sweep should leave it alone.
func expiredAnnotation() map[string]string {
	return map[string]string{"bifrost/expires-at": expiryAt(-2 * time.Hour)}
}

// reaperDeps is the slice of the orchestrator an expiry sweep touches: a fake
// cluster holding preview namespaces and a fake Neon holding each one's
// branch. PurgeExpired reaches everything through ListNamespaces and Down, so
// no GitHub/Cloud Build wiring is involved.
type reaperDeps struct {
	orch *Orchestrator
	kube *fakeKube
	neon *fakeNeon
}

func newReaperDeps() *reaperDeps {
	kc := newFakeKube()
	nc := &fakeNeon{branches: map[string][]neon.Branch{}}
	o := &Orchestrator{
		Kube: kc,
		Neon: nc,
		Registry: Registry{
			"footstrike-api": {Neon: &NeonRef{Project: reaperNeonProject, Database: "fitnessdb", Role: "fitness_owner"}},
		},
	}
	return &reaperDeps{orch: o, kube: kc, neon: nc}
}

// addPreview registers a ready, Active preview namespace for tag — labelled
// the way Up labels one, with the Neon branch Up would have created —
// merging annotations over the defaults.
func (d *reaperDeps) addPreview(tag string, annotations map[string]string) {
	d.addPreviewInPhase(tag, "Active", annotations)
}

// addPreviewInPhase is addPreview with the namespace's Kubernetes status
// phase spelled out ("Active" | "Terminating").
func (d *reaperDeps) addPreviewInPhase(tag, nsPhase string, annotations map[string]string) {
	ann := map[string]string{"bifrost/phase": "ready"}
	maps.Copy(ann, annotations)
	d.kube.namespaces[previewNamespace(tag)] = &fakeNamespace{
		labels:      map[string]string{"bifrost/preview": "true"},
		annotations: ann,
		phase:       nsPhase,
	}
	d.neon.branches[reaperNeonProject] = append(d.neon.branches[reaperNeonProject],
		neon.Branch{ID: "br-" + tag, Name: "preview-" + tag})
}

// assertTornDown asserts Down really ran for tag: its namespace is gone AND
// its Neon branch with it. Asserting on PurgeExpired's returned tags alone
// would pass just as happily if teardown silently no-opped.
func (d *reaperDeps) assertTornDown(t *testing.T, tag string) {
	t.Helper()
	if _, present := d.kube.namespaces[previewNamespace(tag)]; present {
		t.Errorf("namespace %s still present, want it deleted", previewNamespace(tag))
	}
	if d.hasNeonBranch(tag) {
		t.Errorf("neon branch preview-%s still present, want it deleted", tag)
	}
}

// assertUntouched is assertTornDown's inverse: neither half of the teardown
// happened, so the preview is still usable.
func (d *reaperDeps) assertUntouched(t *testing.T, tag string) {
	t.Helper()
	if _, present := d.kube.namespaces[previewNamespace(tag)]; !present {
		t.Errorf("namespace %s was deleted, want it left alone", previewNamespace(tag))
	}
	if !d.hasNeonBranch(tag) {
		t.Errorf("neon branch preview-%s was deleted, want it left alone", tag)
	}
}

func (d *reaperDeps) hasNeonBranch(tag string) bool {
	return slices.ContainsFunc(d.neon.branches[reaperNeonProject], func(b neon.Branch) bool {
		return b.Name == "preview-"+tag
	})
}

func assertReclaimed(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("PurgeExpired reclaimed %v, want %v", got, want)
	}
}

// ---- PurgeExpired -----------------------------------------------------------

func TestPurgeExpiredReclaimsOnlyPastDuePreviews(t *testing.T) {
	d := newReaperDeps()
	d.addPreview("stale", map[string]string{"bifrost/expires-at": expiryAt(-2 * time.Hour)})
	d.addPreview("fresh", map[string]string{"bifrost/expires-at": expiryAt(2 * time.Hour)})
	// Past due and named like a preview, but carrying no bifrost/preview
	// label: the label is what makes a namespace ours, and the sweep must
	// select on it rather than filtering the whole cluster by name.
	d.kube.namespaces["preview-not-ours"] = &fakeNamespace{
		annotations: map[string]string{"bifrost/expires-at": expiryAt(-2 * time.Hour)},
	}

	reclaimed, err := d.orch.PurgeExpired(context.Background(), sweepNow)
	if err != nil {
		t.Fatalf("PurgeExpired failed: %v", err)
	}
	assertReclaimed(t, reclaimed, "stale")
	d.assertTornDown(t, "stale")
	d.assertUntouched(t, "fresh")
	if _, present := d.kube.namespaces["preview-not-ours"]; !present {
		t.Error("preview-not-ours was deleted, want an unlabelled namespace left alone")
	}
}

// TestPurgeExpiredIgnoresMissingAndMalformedExpiry pins the single most
// important property of the sweep: absent, empty, and unparseable all mean
// "no expiry", never "expired long ago". The past-due control proves the
// sweep really ran, so a purge that silently listed nothing can't pass this
// by doing no work at all.
func TestPurgeExpiredIgnoresMissingAndMalformedExpiry(t *testing.T) {
	d := newReaperDeps()
	d.addPreview("stale", expiredAnnotation()) // control: this one must go
	d.addPreview("no-annotation", nil)
	d.addPreview("empty-annotation", map[string]string{"bifrost/expires-at": ""})
	d.addPreview("not-a-time", map[string]string{"bifrost/expires-at": "not-a-time"})
	// A duration where an instant belongs is the malformed value most likely
	// to actually occur, since the TTL the user types is one ("--ttl 8h").
	d.addPreview("duration", map[string]string{"bifrost/expires-at": "8h"})

	reclaimed, err := d.orch.PurgeExpired(context.Background(), sweepNow)
	// Not an error, either: a malformed annotation is a preview to skip, not
	// a sweep to fail.
	if err != nil {
		t.Fatalf("PurgeExpired failed: %v", err)
	}
	assertReclaimed(t, reclaimed, "stale")
	d.assertTornDown(t, "stale")
	for _, tag := range []string{"no-annotation", "empty-annotation", "not-a-time", "duration"} {
		d.assertUntouched(t, tag)
	}
}

func TestPurgeExpiredSkipsCreatingAndTerminating(t *testing.T) {
	d := newReaperDeps()
	d.addPreview("stale", expiredAnnotation()) // control: this one must go
	// Past due, but its Up is still running: tearing down underneath an
	// in-flight create races it, and waiting one more sweep costs nothing.
	creating := expiredAnnotation()
	creating["bifrost/phase"] = "creating"
	d.addPreview("mid-create", creating)
	// Past due, but the namespace is already being deleted — nothing left to
	// reclaim, and Down would only re-issue a delete for it.
	d.addPreviewInPhase("going-away", "Terminating", expiredAnnotation())

	reclaimed, err := d.orch.PurgeExpired(context.Background(), sweepNow)
	if err != nil {
		t.Fatalf("PurgeExpired failed: %v", err)
	}
	assertReclaimed(t, reclaimed, "stale")
	d.assertTornDown(t, "stale")
	d.assertUntouched(t, "mid-create")
	d.assertUntouched(t, "going-away")
}

func TestPurgeExpiredSkipsBusyTags(t *testing.T) {
	d := newReaperDeps()
	d.addPreview("stale", expiredAnnotation()) // control: this one must go
	d.addPreview("in-flight", expiredAnnotation())
	if !d.orch.acquire("in-flight") {
		t.Fatal("acquire(in-flight) = false on a fresh orchestrator")
	}
	defer d.orch.release("in-flight")

	reclaimed, err := d.orch.PurgeExpired(context.Background(), sweepNow)
	// The nil error matters as much as the untouched namespace. Without the
	// Busy check the sweep would still leave in-flight standing — Down
	// refuses a busy tag with ErrBusy — but it would report that refusal as a
	// sweep failure. Asserting only "untouched" would pass for that wrong
	// reason.
	if err != nil {
		t.Fatalf("PurgeExpired failed: %v", err)
	}
	assertReclaimed(t, reclaimed, "stale")
	d.assertTornDown(t, "stale")
	d.assertUntouched(t, "in-flight")
}

func TestPurgeExpiredContinuesAfterAFailedTeardown(t *testing.T) {
	d := newReaperDeps()
	// Namespaces sweep in name order, so aaa-doomed's failure lands before
	// zzz-healthy is even looked at.
	d.addPreview("aaa-doomed", expiredAnnotation())
	d.addPreview("zzz-healthy", expiredAnnotation())
	d.kube.deleteErrByNS = map[string]error{
		previewNamespace("aaa-doomed"): errors.New("namespace delete: forbidden"),
	}

	reclaimed, err := d.orch.PurgeExpired(context.Background(), sweepNow)
	if err == nil {
		t.Fatal("expected the failed teardown to be reported, got nil")
	}
	if !strings.Contains(err.Error(), "aaa-doomed") || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("error = %q, want it to name both the tag and the cause", err)
	}
	assertReclaimed(t, reclaimed, "zzz-healthy")
	d.assertTornDown(t, "zzz-healthy")
	if _, present := d.kube.namespaces[previewNamespace("aaa-doomed")]; !present {
		t.Error("preview-aaa-doomed was deleted even though its delete failed")
	}
}

// TestPurgeExpiredSkipsNamespacesOutsideThePreviewPrefix covers the one way a
// tag can be ambiguous: a namespace carrying the preview label but not the
// preview-<tag> name. Trimming a prefix that isn't there yields the
// namespace's own name as the "tag", and tearing THAT down deletes
// preview-<name> — which here is a real, unexpired preview.
func TestPurgeExpiredSkipsNamespacesOutsideThePreviewPrefix(t *testing.T) {
	d := newReaperDeps()
	d.addPreview("shadow", nil) // no expiry: must survive
	d.kube.namespaces["shadow"] = &fakeNamespace{
		labels: map[string]string{"bifrost/preview": "true"},
		annotations: map[string]string{
			"bifrost/phase":      "ready",
			"bifrost/expires-at": expiryAt(-2 * time.Hour),
		},
	}

	reclaimed, err := d.orch.PurgeExpired(context.Background(), sweepNow)
	if err != nil {
		t.Fatalf("PurgeExpired failed: %v", err)
	}
	assertReclaimed(t, reclaimed)
	d.assertUntouched(t, "shadow")
}

// ---- RunReaper ---------------------------------------------------------------

func TestRunReaperStopsOnContextCancel(t *testing.T) {
	d := newReaperDeps()
	// Expired against the REAL clock, not sweepNow: RunReaper supplies its
	// own time.Now() to each sweep, so a fixture anchored to sweepNow would
	// be "expired" only in an hour's imagination and this test would pass
	// even if the reaper did sweep at startup — which is precisely what it
	// exists to rule out.
	d.addPreview("stale", map[string]string{
		"bifrost/expires-at": time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.orch.RunReaper(ctx, time.Hour)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunReaper did not return after its context was cancelled")
	}
	// A full interval has to pass before the first sweep, so an hourly reaper
	// that has just started has swept nothing. Restarts are routine here
	// (spot-node preemptions), and each one purging on its way up would be
	// both surprising and pointless — the next tick would have done it.
	d.assertUntouched(t, "stale")
}
