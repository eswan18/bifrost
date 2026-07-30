package preview

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eswan18/bifrost/internal/gcb"
	"github.com/eswan18/bifrost/internal/kube"
)

// ---- fixtures ---------------------------------------------------------------

const (
	// autoBranch/autoTag/autoNS are the single preview most tests here use.
	autoBranch = "hae-cadence"
	autoTag    = "hae-cadence"
	autoNS     = "preview-hae-cadence"
	// movedSHA is what a member's branch points at after someone pushes.
	// Deliberately unlike defaultBranchSHA in every character, so an
	// assertion that finds it can only have found the new value.
	movedSHA = "c0ffee1122334455667788"
	// appliesPerRun is how many ApplyObjects calls one Up makes for a
	// two-member preview — one per member. The auto-update tests count
	// applies to tell a real re-run from an inert pass.
	appliesPerRun = 2
)

// seedPreview stands up a real preview for branch through the real Up, so
// every annotation the watcher reads (branch, apps, source-shas, auto-update,
// expires-at, phase) is written by the code that actually writes it in prod
// rather than by a hand-built fixture. A hand-built one could not catch the
// two halves drifting apart, which is the failure this whole feature is one
// long comparison against.
func seedPreview(t *testing.T, d *testDeps, branch string, opts UpOptions) {
	t.Helper()
	if err := d.orch.Up(context.Background(), branch, opts); err != nil {
		t.Fatalf("seeding Up for %s failed: %v", branch, err)
	}
}

// newAutoUpdateDeps is seedPreview for the single-preview case: a two-member
// preview of autoBranch, already deployed with opts.
func newAutoUpdateDeps(t *testing.T, opts UpOptions) *testDeps {
	t.Helper()
	d := newTwoMemberDeps(t)
	seedPreview(t, d, autoBranch, opts)
	return d
}

// pushTo moves repo's branch to sha — the event the whole watcher exists to
// notice.
func pushTo(d *testDeps, repo, sha string) {
	if d.github.shas == nil {
		d.github.shas = map[string]string{}
	}
	d.github.shas[repo] = sha
}

// ---- PollAutoUpdates: the comparison ----------------------------------------

// TestPollAutoUpdatesRedeploysWhenAMemberSHAMoves is the feature: a push to
// one member's branch re-runs the whole preview.
//
// It asserts the redeploy three ways, because each alone is weak. The
// returned tag alone would pass if Up silently no-opped; the apply count is
// what proves manifests really went back into the cluster; and the rewritten
// bifrost/source-shas is what proves the run deployed the NEW commit rather
// than rebuilding the old one.
func TestPollAutoUpdatesRedeploysWhenAMemberSHAMoves(t *testing.T) {
	d := newAutoUpdateDeps(t, UpOptions{AutoUpdate: true})
	before := d.kube.appliesFor(autoNS)
	pushTo(d, "footstrike-api", movedSHA)

	refreshed, err := d.orch.PollAutoUpdates(context.Background())
	if err != nil {
		t.Fatalf("PollAutoUpdates failed: %v", err)
	}
	if !slices.Equal(refreshed, []string{autoTag}) {
		t.Errorf("refreshed = %v, want [%s]", refreshed, autoTag)
	}
	if got := d.kube.appliesFor(autoNS); got != before+appliesPerRun {
		t.Errorf("applies for %s = %d, want %d (a full re-run)", autoNS, got, before+appliesPerRun)
	}
	if got := d.kube.annotations(autoNS)["bifrost/source-shas"]; !strings.Contains(got, "footstrike-api="+movedSHA) {
		t.Errorf("bifrost/source-shas = %q, want it updated to the pushed SHA", got)
	}
	if got := d.kube.annotations(autoNS)["bifrost/phase"]; got != "ready" {
		t.Errorf("bifrost/phase = %q, want ready after the refresh", got)
	}
}

// TestPollAutoUpdatesLeavesAnUnchangedPreviewAlone is the other half of the
// same rule and the one that costs money if it regresses: a watcher that
// re-ran unconditionally would rebuild every opted-in preview every two
// minutes forever. The apply count is the only assertion that can see this —
// a re-run with an unchanged SHA writes back byte-identical annotations.
func TestPollAutoUpdatesLeavesAnUnchangedPreviewAlone(t *testing.T) {
	d := newAutoUpdateDeps(t, UpOptions{AutoUpdate: true})
	before := d.kube.appliesFor(autoNS)

	refreshed, err := d.orch.PollAutoUpdates(context.Background())
	if err != nil {
		t.Fatalf("PollAutoUpdates failed: %v", err)
	}
	if len(refreshed) != 0 {
		t.Errorf("refreshed = %v, want nothing for a preview whose branch hasn't moved", refreshed)
	}
	if got := d.kube.appliesFor(autoNS); got != before {
		t.Errorf("applies for %s = %d, want it unchanged at %d", autoNS, got, before)
	}
}

// TestPollAutoUpdatesIgnoresPreviewsThatDidNotOptIn: auto-update is opt-in,
// and a preview that didn't ask for it must not be touched even though its
// branch has moved just as far. The opted-in preview alongside it is the
// control — without it, a watcher that did nothing at all would pass.
func TestPollAutoUpdatesIgnoresPreviewsThatDidNotOptIn(t *testing.T) {
	d := newTwoMemberDeps(t)
	seedPreview(t, d, "opted-in", UpOptions{AutoUpdate: true})
	seedPreview(t, d, "opted-out", UpOptions{})
	const inNS, outNS = "preview-opted-in", "preview-opted-out"
	inBefore, outBefore := d.kube.appliesFor(inNS), d.kube.appliesFor(outNS)
	pushTo(d, "footstrike-api", movedSHA)

	refreshed, err := d.orch.PollAutoUpdates(context.Background())
	if err != nil {
		t.Fatalf("PollAutoUpdates failed: %v", err)
	}
	if !slices.Equal(refreshed, []string{"opted-in"}) {
		t.Errorf("refreshed = %v, want only [opted-in]", refreshed)
	}
	if got := d.kube.appliesFor(inNS); got != inBefore+appliesPerRun {
		t.Errorf("applies for %s = %d, want %d", inNS, got, inBefore+appliesPerRun)
	}
	if got := d.kube.appliesFor(outNS); got != outBefore {
		t.Errorf("applies for %s = %d, want it untouched at %d", outNS, got, outBefore)
	}
	if got := d.kube.annotations(outNS)["bifrost/source-shas"]; strings.Contains(got, movedSHA) {
		t.Errorf("bifrost/source-shas for the opted-out preview = %q, want the old SHA", got)
	}
}

// ---- PollAutoUpdates: preserving the preview's state -------------------------

// TestPollAutoUpdatesPreservesTheExistingExpiry is why UpOptions.ExpiresAt is
// an absolute instant rather than a TTL. The expiry is checked for EQUALITY
// with what the preview already had: a watcher that passed a duration through
// (or re-derived one from "now") would push it forward on every refresh, so an
// actively-developed preview — the only kind that auto-updates — would never
// expire at all. One that passed UpOptions{} would clear it outright, which
// the same assertion catches.
func TestPollAutoUpdatesPreservesTheExistingExpiry(t *testing.T) {
	// A fixed instant far from now, so neither "extended by a ttl" nor
	// "recomputed from now" could coincidentally match it.
	expiry := time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC)
	d := newAutoUpdateDeps(t, UpOptions{AutoUpdate: true, ExpiresAt: expiry})
	want := d.kube.annotations(autoNS)["bifrost/expires-at"]
	if want != expiry.Format(time.RFC3339) {
		t.Fatalf("seeded bifrost/expires-at = %q, want %q", want, expiry.Format(time.RFC3339))
	}
	pushTo(d, "footstrike-api", movedSHA)

	refreshed, err := d.orch.PollAutoUpdates(context.Background())
	if err != nil {
		t.Fatalf("PollAutoUpdates failed: %v", err)
	}
	if len(refreshed) != 1 {
		t.Fatalf("refreshed = %v, want the preview to have been refreshed at all", refreshed)
	}
	if got := d.kube.annotations(autoNS)["bifrost/expires-at"]; got != want {
		t.Errorf("bifrost/expires-at = %q after an auto-update, want it preserved exactly as %q", got, want)
	}
}

// TestPollAutoUpdatesKeepsFollowingAfterARefresh: the re-run must pass
// AutoUpdate back in. Since Up CLEARS bifrost/auto-update when it isn't
// asked for, a watcher that forgot would disable the feature on the very
// first update it performed — a preview that auto-updates exactly once. The
// second poll proves it's still live, not just still annotated.
func TestPollAutoUpdatesKeepsFollowingAfterARefresh(t *testing.T) {
	d := newAutoUpdateDeps(t, UpOptions{AutoUpdate: true})
	pushTo(d, "footstrike-api", movedSHA)
	if _, err := d.orch.PollAutoUpdates(context.Background()); err != nil {
		t.Fatalf("first PollAutoUpdates failed: %v", err)
	}
	if got := d.kube.annotations(autoNS)["bifrost/auto-update"]; got != "true" {
		t.Fatalf("bifrost/auto-update = %q after an auto-update, want it still true", got)
	}

	before := d.kube.appliesFor(autoNS)
	pushTo(d, "footstrike-api", "second1122334455667788")
	refreshed, err := d.orch.PollAutoUpdates(context.Background())
	if err != nil {
		t.Fatalf("second PollAutoUpdates failed: %v", err)
	}
	if !slices.Equal(refreshed, []string{autoTag}) {
		t.Errorf("refreshed = %v, want the preview to still be following its branch", refreshed)
	}
	if got := d.kube.appliesFor(autoNS); got != before+appliesPerRun {
		t.Errorf("applies for %s = %d, want %d", autoNS, got, before+appliesPerRun)
	}
}

// ---- PollAutoUpdates: skips -------------------------------------------------

// TestPollAutoUpdatesSkipsBusyCreatingAndTerminating covers the three states
// that mean "leave this preview alone", all of them silent (no error) and all
// of them with the branch moved, so only the skip itself can explain the
// absence of a re-run.
func TestPollAutoUpdatesSkipsBusyCreatingAndTerminating(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, d *testDeps)
	}{
		{"busy", func(t *testing.T, d *testDeps) {
			t.Helper()
			if !d.orch.acquire(autoTag) {
				t.Fatal("could not acquire the tag to simulate an in-flight Up/Down")
			}
			t.Cleanup(func() { d.orch.release(autoTag) })
		}},
		{"creating", func(_ *testing.T, d *testDeps) {
			d.kube.namespaces[autoNS].annotations["bifrost/phase"] = "creating"
		}},
		{"terminating", func(_ *testing.T, d *testDeps) {
			d.kube.namespaces[autoNS].phase = "Terminating"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newAutoUpdateDeps(t, UpOptions{AutoUpdate: true})
			pushTo(d, "footstrike-api", movedSHA)
			before := d.kube.appliesFor(autoNS)
			tc.setup(t, d)

			refreshed, err := d.orch.PollAutoUpdates(context.Background())
			if err != nil {
				t.Fatalf("PollAutoUpdates failed: %v, want a silent skip", err)
			}
			if len(refreshed) != 0 {
				t.Errorf("refreshed = %v, want nothing", refreshed)
			}
			if got := d.kube.appliesFor(autoNS); got != before {
				t.Errorf("applies for %s = %d, want it untouched at %d", autoNS, got, before)
			}
		})
	}
}

// TestPollAutoUpdatesSkipsAPreviewWhoseBranchIsGone: deleting the branch
// after a merge is the normal end of a preview's life, not a failure. The
// preview is left exactly as it was and no error is reported — a watcher that
// treated ErrNoBranch as a real error would log one every two minutes until
// someone tore the preview down.
func TestPollAutoUpdatesSkipsAPreviewWhoseBranchIsGone(t *testing.T) {
	d := newAutoUpdateDeps(t, UpOptions{AutoUpdate: true})
	before := d.kube.appliesFor(autoNS)
	// The branch is gone from footstrike-api's repo, so BranchSHA reports
	// github.ErrNoBranch for a service bifrost/apps still lists.
	d.github.members["footstrike-api"] = false

	refreshed, err := d.orch.PollAutoUpdates(context.Background())
	if err != nil {
		t.Fatalf("PollAutoUpdates returned %v, want a deleted branch to be a skip rather than an error", err)
	}
	if len(refreshed) != 0 {
		t.Errorf("refreshed = %v, want nothing", refreshed)
	}
	if got := d.kube.appliesFor(autoNS); got != before {
		t.Errorf("applies for %s = %d, want it untouched at %d", autoNS, got, before)
	}
}

// ---- PollAutoUpdates: failure handling --------------------------------------

// TestPollAutoUpdatesDoesNotRetryTheSameSHA is the worst-outcome guard: a
// watcher that retried a failing build every two minutes forever would burn
// Cloud Build quota indefinitely on a commit that cannot succeed.
//
// It works because Up records bifrost/source-shas in the EnsureNamespace call
// at the top of the run, so a run that dies in the build stage has still
// recorded the commit it attempted. The third phase is what keeps this from
// being satisfied by a watcher that gave up permanently: a NEW push is still
// picked up.
func TestPollAutoUpdatesDoesNotRetryTheSameSHA(t *testing.T) {
	d := newAutoUpdateDeps(t, UpOptions{AutoUpdate: true})
	seeded := d.gcb.runCallsFor("trig-api")

	// A push whose build fails.
	d.gcb.statuses = map[string][]gcb.BuildStatus{"trig-api": {{Status: "FAILURE"}}}
	pushTo(d, "footstrike-api", movedSHA)
	if _, err := d.orch.PollAutoUpdates(context.Background()); err == nil {
		t.Fatal("PollAutoUpdates succeeded, want the failed build reported")
	}
	if got := d.kube.annotations(autoNS)["bifrost/phase"]; got != "failed" {
		t.Fatalf("bifrost/phase = %q, want failed", got)
	}
	if got := d.gcb.runCallsFor("trig-api"); got != seeded+1 {
		t.Fatalf("RunTrigger calls = %d, want %d (the seed plus this one attempt)", got, seeded+1)
	}

	// The next tick, with nothing new pushed: it must not try again.
	refreshed, err := d.orch.PollAutoUpdates(context.Background())
	if err != nil {
		t.Errorf("second PollAutoUpdates = %v, want a quiet no-op", err)
	}
	if len(refreshed) != 0 {
		t.Errorf("refreshed = %v, want nothing", refreshed)
	}
	if got := d.gcb.runCallsFor("trig-api"); got != seeded+1 {
		t.Fatalf("RunTrigger calls = %d after a second poll, want still %d — the same failing SHA must never be retried", got, seeded+1)
	}

	// A fresh push, however, is picked up: the preview isn't stuck.
	pushTo(d, "footstrike-api", "fixed11223344556677")
	if _, err := d.orch.PollAutoUpdates(context.Background()); err == nil {
		t.Fatal("third PollAutoUpdates succeeded, want the (still scripted) build failure")
	}
	if got := d.gcb.runCallsFor("trig-api"); got != seeded+2 {
		t.Errorf("RunTrigger calls = %d, want %d — a NEW commit must still be tried", got, seeded+2)
	}
}

// TestPollAutoUpdatesContinuesAfterOnePreviewFails: two opted-in previews,
// both with a moved branch, the first of which fails its readiness wait. The
// second must still be refreshed, and the first's failure must come back
// named — a pass that aborted on the first error would leave the second
// preview stale with nothing saying why.
func TestPollAutoUpdatesContinuesAfterOnePreviewFails(t *testing.T) {
	// The broken preview's re-run fails by waiting out this (shrunken) bound.
	shrinkPodWait(t, 30*time.Millisecond, 5*time.Millisecond)
	d := newTwoMemberDeps(t)
	// Sorted namespace order puts aaa-broken first, so the healthy preview is
	// only reached if the broken one's failure didn't abort the pass.
	seedPreview(t, d, "aaa-broken", UpOptions{AutoUpdate: true})
	seedPreview(t, d, "zzz-healthy", UpOptions{AutoUpdate: true})
	const brokenNS, healthyNS = "preview-aaa-broken", "preview-zzz-healthy"
	healthyBefore := d.kube.appliesFor(healthyNS)

	// From here on aaa-broken's namespace never reports a pod (so its Up
	// fails at the readiness wait) while zzz-healthy's converges as usual.
	d.kube.podScript = map[string][][]kube.PodInfo{
		brokenNS: {nil},
		healthyNS: {{
			readyPod("footstrike-api", apiImage),
			readyPod("footstrike-dashboard", dashImage),
		}},
	}
	pushTo(d, "footstrike-api", movedSHA)

	refreshed, err := d.orch.PollAutoUpdates(context.Background())
	if err == nil {
		t.Fatal("PollAutoUpdates succeeded, want the broken preview's failure reported")
	}
	if !strings.Contains(err.Error(), "aaa-broken") {
		t.Errorf("error = %v, want it to name the preview that failed", err)
	}
	if !slices.Equal(refreshed, []string{"zzz-healthy"}) {
		t.Errorf("refreshed = %v, want [zzz-healthy] — one failure must not stop the others", refreshed)
	}
	if got := d.kube.appliesFor(healthyNS); got != healthyBefore+appliesPerRun {
		t.Errorf("applies for %s = %d, want %d (it really redeployed)", healthyNS, got, healthyBefore+appliesPerRun)
	}
	if got := d.kube.annotations(brokenNS)["bifrost/phase"]; got != "failed" {
		t.Errorf("bifrost/phase for the broken preview = %q, want failed", got)
	}
}

// ---- RunAutoUpdates ---------------------------------------------------------

// TestRunAutoUpdatesPollsOnEveryTick is what keeps the loop itself honest:
// every other test in this file calls PollAutoUpdates directly, so a
// RunAutoUpdates whose tick case did nothing at all would pass all of them.
func TestRunAutoUpdatesPollsOnEveryTick(t *testing.T) {
	d := newAutoUpdateDeps(t, UpOptions{AutoUpdate: true})
	// Pushed BEFORE the loop starts: nothing may mutate the fakes once the
	// watcher goroutine is reading them.
	pushTo(d, "footstrike-api", movedSHA)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.orch.RunAutoUpdates(ctx, 10*time.Millisecond)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(d.kube.annotations(autoNS)["bifrost/source-shas"], movedSHA) {
		if time.Now().After(deadline) {
			t.Fatal("preview-hae-cadence was never redeployed in 5s of 10ms ticks: RunAutoUpdates never polled")
		}
		time.Sleep(2 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunAutoUpdates did not return after its context was cancelled")
	}
}
