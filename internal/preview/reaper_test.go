package preview

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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
	// The nil error matters as much as the untouched namespace: a busy tag is
	// the sweep deferring, not failing. With neither half of the busy rule in
	// place the namespace would still be left standing — Down refuses a busy
	// tag with ErrBusy — so asserting only "untouched" would pass for that
	// wrong reason. (The pre-check and the ErrBusy arm are behaviorally
	// identical by construction, so this covers the rule, not either line.)
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

// ---- the pre-teardown re-read -----------------------------------------------

// TestPurgeExpiredRechecksTheNamespaceBeforeTearingItDown covers the one gap
// the busy set does not: an Up that starts AND FINISHES inside a single sweep,
// after ListNamespaces has already snapshotted its namespace. Busy(tag) and
// Down's acquire both cover an Up still in flight; neither sees one that has
// come and gone, and acting on the snapshot alone would reclaim a preview
// carrying a freshly renewed expiry — namespace and Neon branch both.
//
// Both mutations are worth covering because they are different halves of the
// same predicate: a renewed expiry (the re-`up --ttl 24h` case) and a phase
// back at creating (a re-`up` still running when the loop arrives).
func TestPurgeExpiredRechecksTheNamespaceBeforeTearingItDown(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(ns *fakeNamespace)
	}{
		{
			name:   "its expiry was renewed",
			mutate: func(ns *fakeNamespace) { ns.annotations["bifrost/expires-at"] = expiryAt(24 * time.Hour) },
		},
		{
			name:   "a new Up put it back in creating",
			mutate: func(ns *fakeNamespace) { ns.annotations["bifrost/phase"] = "creating" },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newReaperDeps()
			// Namespaces sweep in name order, so aaa-first's teardown is the
			// moment mid-sweep at which zzz-renewed changes underneath the
			// snapshot — the same staging a real concurrent Up would produce,
			// without needing one to actually run.
			d.addPreview("aaa-first", expiredAnnotation())
			d.addPreview("zzz-renewed", expiredAnnotation())
			d.kube.onDeleteNamespace = func(deleting string) {
				if deleting == previewNamespace("aaa-first") {
					tc.mutate(d.kube.namespaces[previewNamespace("zzz-renewed")])
				}
			}

			reclaimed, err := d.orch.PurgeExpired(context.Background(), sweepNow)
			if err != nil {
				t.Fatalf("PurgeExpired failed: %v", err)
			}
			// The control matters as much as the survivor: a sweep that
			// reclaimed nothing at all would satisfy the untouched assertion
			// for entirely the wrong reason.
			assertReclaimed(t, reclaimed, "aaa-first")
			d.assertTornDown(t, "aaa-first")
			d.assertUntouched(t, "zzz-renewed")
		})
	}
}

// TestPurgeExpiredSkipsAPreviewThatVanishedMidSweep is the other outcome of
// that re-read: the namespace is simply gone (an operator's `ib preview down`
// landed while the sweep was working). Nothing to reclaim and nothing to
// report — silence, not an error, and above all no Down, whose Neon half would
// still have run against a preview bifrost no longer owns.
func TestPurgeExpiredSkipsAPreviewThatVanishedMidSweep(t *testing.T) {
	d := newReaperDeps()
	d.addPreview("aaa-first", expiredAnnotation())
	d.addPreview("zzz-gone", expiredAnnotation())
	d.kube.onDeleteNamespace = func(deleting string) {
		if deleting == previewNamespace("aaa-first") {
			delete(d.kube.namespaces, previewNamespace("zzz-gone"))
		}
	}

	reclaimed, err := d.orch.PurgeExpired(context.Background(), sweepNow)
	if err != nil {
		t.Fatalf("PurgeExpired failed: %v", err)
	}
	assertReclaimed(t, reclaimed, "aaa-first")
	d.assertTornDown(t, "aaa-first")
	// The namespace is gone either way, so the Neon branch is the only thing
	// that can tell "skipped" apart from "torn down after the fact".
	if !d.hasNeonBranch("zzz-gone") {
		t.Error("neon branch preview-zzz-gone was deleted: Down ran for a namespace that had already gone")
	}
}

// TestPurgeExpiredReportsAFailedRecheck pins the fail-closed direction: a
// re-read that errors is not a licence to fall back on the stale snapshot. The
// preview stays, and the failure is reported like any other — an unreadable
// namespace is not evidence of anything, least of all a reason to delete it.
func TestPurgeExpiredReportsAFailedRecheck(t *testing.T) {
	d := newReaperDeps()
	d.addPreview("aaa-unreadable", expiredAnnotation())
	d.addPreview("zzz-healthy", expiredAnnotation()) // control: this one must go
	d.kube.getErrByNS = map[string]error{
		previewNamespace("aaa-unreadable"): errors.New("get namespace: connection refused"),
	}

	reclaimed, err := d.orch.PurgeExpired(context.Background(), sweepNow)
	if err == nil {
		t.Fatal("expected the failed re-read to be reported, got nil")
	}
	if !strings.Contains(err.Error(), "aaa-unreadable") || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error = %q, want it to name both the tag and the cause", err)
	}
	assertReclaimed(t, reclaimed, "zzz-healthy")
	d.assertTornDown(t, "zzz-healthy")
	d.assertUntouched(t, "aaa-unreadable")
}

// ---- PurgeOrphanedBranches --------------------------------------------------

// The orphan sweep's two Neon projects. proj-api is shared by two registry
// services below — a single Neon project holds many databases, so nothing stops
// that, and the sweep must still list it once.
const (
	orphanProjAPI      = "proj-api"
	orphanProjIdentity = "proj-identity"
)

// orphanDeps is the slice of the orchestrator an orphan sweep touches: a fake
// cluster whose preview namespaces are the live set, and a fake Neon whose
// branches are the candidates. Its registry deliberately covers all three
// shapes the real one can have — two services sharing a project, a service
// with a project of its own, and a previewable service with no Neon reference
// at all (footstrike-dashboard, exactly as in registry.yaml).
type orphanDeps struct {
	orch *Orchestrator
	kube *fakeKube
	neon *fakeNeon
}

func newOrphanDeps() *orphanDeps {
	kc := newFakeKube()
	nc := &fakeNeon{branches: map[string][]neon.Branch{}}
	o := &Orchestrator{
		Kube: kc,
		Neon: nc,
		Registry: Registry{
			"footstrike-api":       {Neon: &NeonRef{Project: orphanProjAPI, Database: "fitnessdb", Role: "fitness_owner"}},
			"footstrike-reporting": {Neon: &NeonRef{Project: orphanProjAPI, Database: "reportingdb", Role: "fitness_owner"}},
			"identity":             {Neon: &NeonRef{Project: orphanProjIdentity, Database: "identitydb", Role: "identity_owner"}},
			"footstrike-dashboard": {},
		},
	}
	return &orphanDeps{orch: o, kube: kc, neon: nc}
}

// addBranch registers a Neon branch created at createdAt. Every orphan test
// states a branch's age explicitly: the age floor is one of the rules under
// test, and a fixture defaulting to the zero time would make every branch look
// ancient without saying so.
func (d *orphanDeps) addBranch(project, name string, createdAt time.Time) {
	d.neon.branches[project] = append(d.neon.branches[project],
		neon.Branch{ID: "br-" + project + "-" + name, Name: name, CreatedAt: createdAt})
}

// addOrphan is the control every test below carries: a day-old preview-<tag>
// branch with no namespace anywhere, which the sweep must reclaim. Without it,
// an assertion that some OTHER branch survived would pass just as happily
// against a sweep that listed nothing and deleted nothing at all.
func (d *orphanDeps) addOrphan(tag string) {
	d.addBranch(orphanProjAPI, previewBranchName(tag), sweepNow.Add(-24*time.Hour))
}

// addNamespace gives tag a preview namespace in a Kubernetes status phase
// ("Active" | "Terminating"), labelled the way Up labels one.
func (d *orphanDeps) addNamespace(tag, nsPhase string) {
	d.kube.namespaces[previewNamespace(tag)] = &fakeNamespace{
		labels:      map[string]string{"bifrost/preview": "true"},
		annotations: map[string]string{"bifrost/phase": "ready"},
		phase:       nsPhase,
	}
}

func (d *orphanDeps) assertBranchGone(t *testing.T, project, name string) {
	t.Helper()
	if d.neon.hasBranch(project, name) {
		t.Errorf("branch %s/%s still present, want it deleted", project, name)
	}
}

func (d *orphanDeps) assertBranchKept(t *testing.T, project, name string) {
	t.Helper()
	if !d.neon.hasBranch(project, name) {
		t.Errorf("branch %s/%s was deleted, want it left alone", project, name)
	}
}

func assertPurged(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("PurgeOrphanedBranches deleted %v, want %v", got, want)
	}
}

// TestPurgeOrphanedBranchesDeletesABranchWithNoNamespace is the base case: the
// exact residue of a teardown that died between its namespace delete and its
// Neon delete. Nothing else in bifrost can see this branch — it is absent from
// ListNamespaces, so no expiry sweep and no `ib preview down` can name it.
func TestPurgeOrphanedBranchesDeletesABranchWithNoNamespace(t *testing.T) {
	d := newOrphanDeps()
	d.addOrphan("gone")

	purged, err := d.orch.PurgeOrphanedBranches(context.Background(), sweepNow)
	if err != nil {
		t.Fatalf("PurgeOrphanedBranches failed: %v", err)
	}
	assertPurged(t, purged, orphanProjAPI+"/preview-gone")
	d.assertBranchGone(t, orphanProjAPI, "preview-gone")
}

// TestPurgeOrphanedBranchesKeepsBranchesWithALiveNamespace is the rule the
// whole feature turns on, in its most consequential direction: this is a
// working preview's database.
func TestPurgeOrphanedBranchesKeepsBranchesWithALiveNamespace(t *testing.T) {
	d := newOrphanDeps()
	d.addOrphan("gone") // control: this one must go
	d.addBranch(orphanProjAPI, "preview-live", sweepNow.Add(-24*time.Hour))
	d.addNamespace("live", "Active")

	purged, err := d.orch.PurgeOrphanedBranches(context.Background(), sweepNow)
	if err != nil {
		t.Fatalf("PurgeOrphanedBranches failed: %v", err)
	}
	assertPurged(t, purged, orphanProjAPI+"/preview-gone")
	d.assertBranchGone(t, orphanProjAPI, "preview-gone")
	d.assertBranchKept(t, orphanProjAPI, "preview-live")
}

// TestPurgeOrphanedBranchesKeepsBranchesWhoseNamespaceIsTerminating covers the
// state a real cluster spends seconds to minutes in after every teardown. The
// namespace still EXISTS — it is still in ListNamespaces, so the preview is
// still visible to bifrost and its branch still belongs to the Down that is
// removing it. Filtering Terminating out of the live set would make every
// ordinary teardown briefly look like an orphan.
func TestPurgeOrphanedBranchesKeepsBranchesWhoseNamespaceIsTerminating(t *testing.T) {
	d := newOrphanDeps()
	d.addOrphan("gone") // control: this one must go
	d.addBranch(orphanProjAPI, "preview-dying", sweepNow.Add(-24*time.Hour))
	d.addNamespace("dying", "Terminating")

	purged, err := d.orch.PurgeOrphanedBranches(context.Background(), sweepNow)
	if err != nil {
		t.Fatalf("PurgeOrphanedBranches failed: %v", err)
	}
	assertPurged(t, purged, orphanProjAPI+"/preview-gone")
	d.assertBranchGone(t, orphanProjAPI, "preview-gone")
	d.assertBranchKept(t, orphanProjAPI, "preview-dying")
}

// TestPurgeOrphanedBranchesSkipsBusyTags covers the one transient that really
// does produce a branch with no namespace: Down, caught between its two halves.
// A sweep firing during a teardown would race the delete it is duplicating —
// and, worse, is indistinguishable from the sweep deleting a branch out from
// under an Up if Up's ordering ever changes.
func TestPurgeOrphanedBranchesSkipsBusyTags(t *testing.T) {
	d := newOrphanDeps()
	d.addOrphan("gone") // control: this one must go
	d.addBranch(orphanProjAPI, "preview-teardown", sweepNow.Add(-24*time.Hour))
	if !d.orch.acquire("teardown") {
		t.Fatal("acquire(teardown) = false on a fresh orchestrator")
	}
	defer d.orch.release("teardown")

	purged, err := d.orch.PurgeOrphanedBranches(context.Background(), sweepNow)
	// The nil error matters as much as the surviving branch: a busy tag is the
	// sweep deferring, not failing.
	if err != nil {
		t.Fatalf("PurgeOrphanedBranches failed: %v", err)
	}
	assertPurged(t, purged, orphanProjAPI+"/preview-gone")
	d.assertBranchGone(t, orphanProjAPI, "preview-gone")
	d.assertBranchKept(t, orphanProjAPI, "preview-teardown")
}

// TestPurgeOrphanedBranchesSkipsBranchesYoungerThanMinOrphanAge pins the age
// floor and its boundary in both directions. The floor is insurance rather
// than a fix for a live race (Up creates the namespace before the branch, so a
// create in flight never looks like an orphan at any age) — but a floor that
// silently didn't apply would be no insurance at all.
func TestPurgeOrphanedBranchesSkipsBranchesYoungerThanMinOrphanAge(t *testing.T) {
	d := newOrphanDeps()
	d.addOrphan("gone") // control: this one must go
	d.addBranch(orphanProjAPI, "preview-justmade", sweepNow.Add(-30*time.Minute))
	// Exactly at the floor, so the comparison's direction is pinned too:
	// "younger than minOrphanAge" is skipped, "exactly minOrphanAge" is not.
	d.addBranch(orphanProjAPI, "preview-onthedot", sweepNow.Add(-minOrphanAge))

	purged, err := d.orch.PurgeOrphanedBranches(context.Background(), sweepNow)
	if err != nil {
		t.Fatalf("PurgeOrphanedBranches failed: %v", err)
	}
	assertPurged(t, purged, orphanProjAPI+"/preview-gone", orphanProjAPI+"/preview-onthedot")
	d.assertBranchGone(t, orphanProjAPI, "preview-gone")
	d.assertBranchGone(t, orphanProjAPI, "preview-onthedot")
	d.assertBranchKept(t, orphanProjAPI, "preview-justmade")
}

// TestPurgeOrphanedBranchesIgnoresBranchesOutsideTheConvention is the blast
// radius test. These projects are not bifrost's alone: main is the branch
// every preview branches FROM, and losing it would take the staging database
// with it. A branch named exactly "preview-" yields an empty tag, which would
// match a namespace-less "" and be deleted by a sweep that trimmed the prefix
// without checking what was left.
func TestPurgeOrphanedBranchesIgnoresBranchesOutsideTheConvention(t *testing.T) {
	d := newOrphanDeps()
	d.addOrphan("gone") // control: this one must go
	for _, name := range []string{"main", "dev", "staging-restore", "preview", "preview-"} {
		d.addBranch(orphanProjAPI, name, sweepNow.Add(-30*24*time.Hour))
	}

	purged, err := d.orch.PurgeOrphanedBranches(context.Background(), sweepNow)
	if err != nil {
		t.Fatalf("PurgeOrphanedBranches failed: %v", err)
	}
	assertPurged(t, purged, orphanProjAPI+"/preview-gone")
	for _, name := range []string{"main", "dev", "staging-restore", "preview", "preview-"} {
		d.assertBranchKept(t, orphanProjAPI, name)
	}
}

// TestPurgeOrphanedBranchesListsEachProjectOnce pins the deduplication. Two
// registry services sharing one Neon project is legal (a project holds many
// databases), and the sweep's whole cost argument — O(distinct projects), not
// O(services or previews) — depends on not listing the same one twice. No
// assertion on the resulting deletions can see this: a second pass over the
// same project finds the branches already gone and deletes nothing either way,
// so the call count is the only witness.
func TestPurgeOrphanedBranchesListsEachProjectOnce(t *testing.T) {
	d := newOrphanDeps()
	d.addOrphan("gone")
	d.addBranch(orphanProjIdentity, "preview-gone-too", sweepNow.Add(-24*time.Hour))

	purged, err := d.orch.PurgeOrphanedBranches(context.Background(), sweepNow)
	if err != nil {
		t.Fatalf("PurgeOrphanedBranches failed: %v", err)
	}
	// Ordered by service name (footstrike-api, then identity), which is what
	// makes both this and the log output deterministic.
	assertPurged(t, purged, orphanProjAPI+"/preview-gone", orphanProjIdentity+"/preview-gone-too")
	if got := d.neon.listCallsFor(orphanProjAPI); got != 1 {
		t.Errorf("ListBranches(%s) called %d times, want 1 — footstrike-api and footstrike-reporting share that project", orphanProjAPI, got)
	}
	if got := d.neon.listCallsFor(orphanProjIdentity); got != 1 {
		t.Errorf("ListBranches(%s) called %d times, want 1", orphanProjIdentity, got)
	}
}

// TestPurgeOrphanedBranchesContinuesPastAFailedList mirrors Down's own
// best-effort shape: one project's Neon API being unavailable must not leave
// the other project's orphans billing for another hour.
func TestPurgeOrphanedBranchesContinuesPastAFailedList(t *testing.T) {
	d := newOrphanDeps()
	// proj-api is swept first (footstrike-api sorts before identity), so its
	// failure lands before proj-identity is even looked at.
	d.neon.listErr = map[string]error{orphanProjAPI: errors.New("neon: proj-api unavailable")}
	d.addBranch(orphanProjIdentity, "preview-gone", sweepNow.Add(-24*time.Hour))

	purged, err := d.orch.PurgeOrphanedBranches(context.Background(), sweepNow)
	if err == nil {
		t.Fatal("expected the failed list to be reported, got nil")
	}
	if !strings.Contains(err.Error(), orphanProjAPI) || !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("error = %q, want it to name both the project and the cause", err)
	}
	assertPurged(t, purged, orphanProjIdentity+"/preview-gone")
	d.assertBranchGone(t, orphanProjIdentity, "preview-gone")
}

// TestPurgeOrphanedBranchesDeletesNothingWhenNamespacesCannotBeListed is the
// fail-closed direction, and the most dangerous failure this function has: an
// empty live set alongside a full branch list is indistinguishable from "every
// preview is an orphan". A pass that ignored the ListNamespaces error would
// delete every preview database in the fleet in one tick.
//
// The dead context is what fails the list — fakeKube.ListNamespaces is
// context-respecting, as a real List is, while the Neon fake is not, so an
// implementation that carried on regardless really would reach the deletes.
func TestPurgeOrphanedBranchesDeletesNothingWhenNamespacesCannotBeListed(t *testing.T) {
	d := newOrphanDeps()
	d.addBranch(orphanProjAPI, "preview-live", sweepNow.Add(-24*time.Hour))
	d.addNamespace("live", "Active")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	purged, err := d.orch.PurgeOrphanedBranches(ctx, sweepNow)
	if err == nil {
		t.Fatal("expected the failed namespace list to be reported, got nil")
	}
	assertPurged(t, purged)
	d.assertBranchKept(t, orphanProjAPI, "preview-live")
}

// ---- branch-count visibility ------------------------------------------------

// captureDebugLogs points the default slog logger at a buffer for the
// duration of the calling test, at a level low enough to catch Debug, and
// restores the previous default on cleanup. The orphan sweep's branch-count
// line is deliberately Debug (see PurgeOrphanedBranches), so a handler left
// at the package default (Info) would silently see nothing.
func captureDebugLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestPurgeOrphanedBranchesLogsBranchCountPerProject covers the one new piece
// of visibility this sweep adds on top of its existing deletion logic:
// nothing else in bifrost counts branches, storage, compute, or build minutes
// against Neon's quotas, so this line is the only signal of where a project
// stands BEFORE a quota bound is hit rather than only after (a Neon error
// surfacing one). It must come from the same ListBranches call the deletion
// logic already makes — no new API call — so the count is exactly what was
// listed, unfiltered by the orphan rules that run afterward.
func TestPurgeOrphanedBranchesLogsBranchCountPerProject(t *testing.T) {
	d := newOrphanDeps()
	// Two branches in orphanProjAPI, one of them not even preview-shaped —
	// the count is every branch ListBranches returned, not just the ones the
	// orphan rules go on to consider.
	d.addBranch(orphanProjAPI, "preview-one", sweepNow.Add(-30*time.Minute))
	d.addBranch(orphanProjAPI, "main", sweepNow.Add(-30*24*time.Hour))
	d.addBranch(orphanProjIdentity, "preview-solo", sweepNow.Add(-30*time.Minute))

	buf := captureDebugLogs(t)
	if _, err := d.orch.PurgeOrphanedBranches(context.Background(), sweepNow); err != nil {
		t.Fatalf("PurgeOrphanedBranches failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "project="+orphanProjAPI) || !strings.Contains(out, "count=2") {
		t.Errorf("log output = %q, want a line with project=%s count=2", out, orphanProjAPI)
	}
	if !strings.Contains(out, "project="+orphanProjIdentity) || !strings.Contains(out, "count=1") {
		t.Errorf("log output = %q, want a line with project=%s count=1", out, orphanProjIdentity)
	}
}

// TestPurgeOrphanedBranchesLogsZeroForAProjectWithNoBranches makes sure the
// count line fires even when there is nothing to count: an empty project is
// exactly the state that later fills up toward a quota, and "nothing to
// report" must not mean "nothing logged" — it must read count=0, not be
// silently skipped.
func TestPurgeOrphanedBranchesLogsZeroForAProjectWithNoBranches(t *testing.T) {
	d := newOrphanDeps() // neither project has any branches at all

	buf := captureDebugLogs(t)
	if _, err := d.orch.PurgeOrphanedBranches(context.Background(), sweepNow); err != nil {
		t.Fatalf("PurgeOrphanedBranches failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "project="+orphanProjAPI) || !strings.Contains(out, "count=0") {
		t.Errorf("log output = %q, want a line with project=%s count=0", out, orphanProjAPI)
	}
}

// ---- RunReaper ---------------------------------------------------------------

// TestRunReaperSweepsOnEveryTick is the test that keeps the whole feature from
// shipping inert: everything else here calls PurgeExpired directly, so a
// RunReaper whose tick case did nothing at all would still pass every one of
// them. It asserts on the fake cluster — the namespace gone and its Neon
// branch with it — after a tick, not on any return value.
func TestRunReaperSweepsOnEveryTick(t *testing.T) {
	d := newReaperDeps()
	// Past due against the real clock, since RunReaper judges each sweep by
	// its own time.Now().
	d.addPreview("stale", map[string]string{
		"bifrost/expires-at": time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.orch.RunReaper(ctx, 10*time.Millisecond)
	}()

	// Poll through the fake's lock: the sweep is running concurrently, so
	// reading its maps directly would be a data race.
	deadline := time.Now().Add(5 * time.Second)
	for d.kube.hasNamespace(previewNamespace("stale")) {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("preview-stale survived 5s of 10ms ticks: RunReaper never swept")
		}
		time.Sleep(2 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunReaper did not return after its context was cancelled")
	}
	// Safe to read unlocked now that the goroutine has returned. The
	// namespace going is only half of a teardown; the Neon branch is the
	// half that proves the tick reached Down rather than some delete of its
	// own.
	d.assertTornDown(t, "stale")
}

// TestSweepDetachesFromShutdownCancellation models the SIGTERM case: the
// loop's context is already dead, and the sweep must still run to completion.
// Down deletes the namespace before the Neon branches, and a preview whose
// namespace is gone never appears in ListNamespaces again — so a teardown cut
// off in that window orphans the branch permanently, with nothing left to
// retry it.
func TestSweepDetachesFromShutdownCancellation(t *testing.T) {
	d := newReaperDeps()
	d.addPreview("stale", map[string]string{
		"bifrost/expires-at": time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the shutdown signal has already landed

	reclaimed, _, err := d.orch.sweep(ctx)
	if err != nil {
		t.Fatalf("sweep failed on an already-cancelled parent context: %v", err)
	}
	assertReclaimed(t, reclaimed, "stale")
	d.assertTornDown(t, "stale")
}

// TestSweepPurgesOrphansAfterExpiry covers the wiring and the ordering in one
// scenario, because they are the same fact: the orphan pass runs on the state
// PurgeExpired has just left behind.
//
// The staging is the real failure this feature exists for, reproduced exactly:
// a teardown whose namespace delete succeeded and whose Neon delete then
// failed. Nothing can ever see that branch again — the namespace it was
// derived from is gone — and before this change it billed forever.
//
// The ordering is what the assertion turns on. Run the orphan pass FIRST and
// preview-stale still has its namespace, so it is correctly skipped as a live
// preview, and the branch survives the tick. Run it second, as sweep does, and
// the same branch is reclaimed on the same tick rather than an hour later.
func TestSweepPurgesOrphansAfterExpiry(t *testing.T) {
	d := newOrphanDeps()
	// Judged against the real clock, since sweep supplies its own time.Now().
	d.kube.namespaces[previewNamespace("stale")] = &fakeNamespace{
		labels: map[string]string{"bifrost/preview": "true"},
		annotations: map[string]string{
			"bifrost/phase":      "ready",
			"bifrost/expires-at": time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
		},
	}
	// proj-identity, not proj-api: only one registry service references it, so
	// Down makes exactly one delete attempt for this branch. (Down does not
	// deduplicate projects, so a branch in proj-api would get a second attempt
	// from footstrike-reporting and succeed on it, leaving nothing orphaned.)
	d.addBranch(orphanProjIdentity, previewBranchName("stale"), time.Now().UTC().Add(-24*time.Hour))
	// Down's Neon half fails once — the process-exit case, without needing a
	// process to exit. The orphan pass's own delete then succeeds.
	d.neon.deleteErrOnce = map[string]error{orphanProjIdentity: errors.New("neon: connection reset")}

	reclaimed, orphans, err := d.orch.sweep(context.Background())
	if err == nil {
		t.Fatal("expected the failed Neon delete to be reported by the expiry pass, got nil")
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("error = %q, want it to carry the Neon failure", err)
	}
	// Nothing "reclaimed": PurgeExpired counts only a Down that fully
	// succeeded, and this one didn't. That is the shape of the bug — an
	// operator gets one error line, and until this change the branch behind it
	// was unreachable forever after.
	assertReclaimed(t, reclaimed)
	if _, present := d.kube.namespaces[previewNamespace("stale")]; present {
		t.Error("namespace preview-stale still present: the expiry pass never tore it down")
	}
	// The orphan pass, running second, is what finishes the job.
	assertPurged(t, orphans, orphanProjIdentity+"/preview-stale")
	d.assertBranchGone(t, orphanProjIdentity, "preview-stale")
}

// TestRunReaperPurgesOrphanedBranchesOnATick is the test that keeps the orphan
// sweep from shipping inert: every other test in this section calls
// PurgeOrphanedBranches (or sweep) directly, so a RunReaper tick that never
// reached it would still pass all of them. It asserts on the fake Neon — the
// branch actually gone — after a tick, not on any return value.
func TestRunReaperPurgesOrphanedBranchesOnATick(t *testing.T) {
	d := newOrphanDeps()
	// No namespaces at all: this is a branch bifrost has already lost track of.
	// Aged against the real clock, since RunReaper judges each sweep by its own
	// time.Now().
	d.addBranch(orphanProjAPI, "preview-orphan", time.Now().UTC().Add(-24*time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.orch.RunReaper(ctx, 10*time.Millisecond)
	}()

	// Poll through the fake's lock: the sweep runs concurrently.
	deadline := time.Now().Add(5 * time.Second)
	for d.neon.hasBranch(orphanProjAPI, "preview-orphan") {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("preview-orphan survived 5s of 10ms ticks: RunReaper never swept orphaned branches")
		}
		time.Sleep(2 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunReaper did not return after its context was cancelled")
	}
}

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
