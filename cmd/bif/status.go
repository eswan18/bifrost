package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/eswan18/bifrost/internal/gcb"
	"github.com/eswan18/bifrost/internal/kube"
	"github.com/eswan18/bifrost/internal/promote"
)

// podLister is the slice of kube.Client that `bif status` needs: pods, and
// nothing else. Narrow on purpose — status is a read, and a wider seam here
// would invite a later command to reach for a write through it.
type podLister interface {
	ListPods(ctx context.Context, namespace string) ([]kube.PodInfo, error)
}

// kube.Client has to satisfy podLister, or the fake the tests drive would be
// shaped like an interface the real client doesn't implement.
var _ podLister = (kube.Client)(nil)

// buildLister is the slice of gcb.Client that `bif status` needs: the whole
// fleet's most recent builds, in one call. Narrow for the same reason
// podLister is, and pointedly narrower in one place — gcb.Client can also
// START builds (RunTrigger), and `bif status` has no business writing to
// Cloud Build any more than it has business patching an Application.
type buildLister interface {
	LatestBuilds(ctx context.Context) (map[string]gcb.BuildStatus, error)
}

// gcb.Client has to satisfy buildLister, for the same reason kube.Client has
// to satisfy podLister.
var _ buildLister = (gcb.Client)(nil)

// buildLookupTimeout bounds the Cloud Build read, and is deliberately much
// tighter than the command's own context, which it does not replace. `bif
// status` is what you reach for during an incident: the build column is worth
// a couple of seconds of latency and not one second more, and Cloud Build is
// precisely the kind of third party that is slow or unreachable at the moment
// the cluster's state matters most. A var rather than a const so the tests can
// prove the bound is applied without waiting for it.
var buildLookupTimeout = 3 * time.Second

// buildSet is one `bif status` invocation's entire view of Cloud Build: every
// repo's most recent build, plus whether the read happened at all. err is
// carried rather than printed by the reader — see fetchBuilds.
type buildSet struct {
	byRepo map[string]gcb.BuildStatus
	known  bool
	err    error
}

// cellFor picks one service's build out of the set. The key is the REPO name,
// never the service name: LatestBuilds keys on Cloud Build's REPO_NAME
// substitution, and asset-manager's repo is asset_manager — so a lookup by
// service name silently finds no build for it while looking perfectly correct
// for the other six services. registry.RepoFor is the mapping;
// internal/web/fleet.go resolves the same key the same way for the Apps tab.
func (s buildSet) cellFor(repo string) buildCell {
	if !s.known {
		return buildCell{}
	}
	return buildCell{status: s.byRepo[repo], known: true}
}

// buildCell is one service's build as the status table shows it. known is
// separate from the status because "Cloud Build says this service has no
// recent build" and "we could not ask Cloud Build" are different facts, and an
// operator staring at a service that ought to be building needs to tell them
// apart.
type buildCell struct {
	status gcb.BuildStatus
	known  bool
}

// fetchBuilds starts the fleet-wide Cloud Build read and hands back a channel
// carrying its one result.
//
// ONE call per invocation, for the whole fleet: LatestBuilds returns every
// repo's newest build in a single API call, so `bif status` costs the same one
// call whether it renders one service or all seven.
//
// It is started before the cluster connection so the two overlap, and it
// reports failure through the buildSet instead of writing to stderr itself. A
// caller that gives up early — an unreachable cluster — never reads the
// channel, and a goroutine still writing to stderr after statusCmd returned
// would be a data race on the command's own output.
func fetchBuilds(ctx context.Context, dial func(context.Context) (buildLister, error)) <-chan buildSet {
	ch := make(chan buildSet, 1)
	go func() {
		ctx, cancel := context.WithTimeout(ctx, buildLookupTimeout)
		defer cancel()
		builds, err := dial(ctx)
		if err != nil {
			ch <- buildSet{err: err}
			return
		}
		byRepo, err := builds.LatestBuilds(ctx)
		if err != nil {
			ch <- buildSet{err: err}
			return
		}
		ch <- buildSet{byRepo: byRepo, known: true}
	}()
	return ch
}

// verdict is the tri-state ib.py's status() returns: True (in sync), False
// (out of sync), None (indeterminate). Keeping all three is the point —
// inSync and indeterminate both exit 0, so collapsing them would look
// harmless right up until something scripted the difference.
type verdict int

const (
	indeterminate verdict = iota
	inSync
	outOfSync
)

// exitCode maps the verdicts of a status run to ib's exit status, mirroring
// ib.py's main(): only a definite "out of sync" is a failure, and the mapping
// is the same whether or not -q was passed. Indeterminate — mid-deploy, no
// pods, an unparseable staging tag — exits 0, so a script asking "is there
// anything to promote?" gets "no" rather than an error when the answer isn't
// knowable yet. For the whole-fleet form, one out-of-sync service is enough to
// exit 1, and the others still print.
func exitCode(verdicts []verdict) int {
	if slices.Contains(verdicts, outOfSync) {
		return 1
	}
	return 0
}

// statusCmd implements all four forms of `bif status`.
func statusCmd(ctx context.Context, args []string, stdout, stderr io.Writer, connect func() (podLister, error), dialBuilds func(context.Context) (buildLister, error)) int {
	args, quiet := takeFlag(args, "-q", "--quiet")

	// The service list comes from the embedded registry, not a constant. That
	// is what retires ib.py's hand-maintained SERVICES, and it costs nothing
	// against the offline requirement: registry.yaml is go:embed'd, so this is
	// a parse of bytes already inside the binary. The registry itself is kept,
	// not just its names, because the build column needs RepoFor.
	reg, ok := loadRegistry(stderr)
	if !ok {
		return 1
	}
	apps := reg.Names()

	if len(args) > 0 {
		app := args[0]
		if !validateApp(stdout, apps, app) {
			return 1
		}
		apps = []string{app}
	}

	// -q gets no build lookup at all. Its output is a scriptable contract —
	// bare app names, "*" for mid-deploy — so there is nothing to render, and
	// skipping the call keeps the quiet form exactly as cheap and as offline
	// as it was. Started here, before connect, so the Cloud Build read and the
	// cluster reads overlap rather than adding up.
	var builds <-chan buildSet
	if !quiet {
		builds = fetchBuilds(ctx, dialBuilds)
	}

	cluster, err := connect()
	if err != nil {
		outf(stderr, "Error: connecting to the cluster: %v\n", err)
		return 1
	}

	var set buildSet
	if builds != nil {
		set = <-builds
		if set.err != nil {
			// Reported, then carried on from, exactly as a failed pod List is:
			// the build column degrades to unknown and nothing else about the
			// command changes. Once per invocation, not once per service, and
			// on stderr so stdout stays what it was.
			outf(stderr, "Warning: could not read Cloud Build status: %v\n", set.err)
		}
	}

	verdicts := make([]verdict, 0, len(apps))
	for _, app := range apps {
		staging := deployedImages(ctx, cluster, stderr, app+"-staging")
		prod := deployedImages(ctx, cluster, stderr, app+"-prod")
		verdicts = append(verdicts, statusOne(stdout, app, staging, prod, quiet, set.cellFor(reg.RepoFor(app))))
	}
	return exitCode(verdicts)
}

// deployedImages returns the images running in a namespace. A List failure is
// reported and then read as "no pods": ib.py does the same thing (its kubectl
// helper prints the error and exits, and get_deployed_images catches that exit
// and returns an empty set), so a namespace that doesn't exist yet reads as
// indeterminate instead of aborting a whole-fleet status.
func deployedImages(ctx context.Context, cluster podLister, stderr io.Writer, ns string) []string {
	pods, err := cluster.ListPods(ctx, ns)
	if err != nil {
		outf(stderr, "Error: listing pods in %s: %v\n", ns, err)
		return nil
	}
	return kube.Images(pods)
}

// statusOne renders one service and returns its verdict.
//
// The verdict comes from promote.StatusOf — the same decision logic the server
// runs, which is the whole reason for this port. The table does NOT: it is
// rendered straight from the image lists, exactly as ib.py renders it.
//
// That split is deliberate, and it is how this resolves Task 1's second
// finding (promote.Status zeroes StagingTag/ProdTag when either side is
// mid-deploy or has no pods, where ib.py still displays the other side's tag).
// The alternative was widening promote.Status, and it is the wrong shape: what
// the mid-deploy display needs is every tag, so Status would have to carry
// []string per environment — its own input handed back — and gain fields whose
// meaning changed with State. cmd/bif already holds the image lists, having
// just fetched them. So promote decides, and cmd/bif displays, and no verdict
// moved to make the output match.
// The build cell is the one thing here that is neither ib.py's nor a verdict:
// it is information, and it never moves the exit code. A service whose last
// build failed is not thereby "out of sync" — the images running in the two
// namespaces are what that word means, and a red build says something about
// the next deploy, not this one.
func statusOne(w io.Writer, app string, stagingImages, prodImages []string, quiet bool, build buildCell) verdict {
	staging := normalize(stagingImages)
	prod := normalize(prodImages)
	status := promote.StatusOf(staging, prod)

	if quiet {
		// Quiet mode prints only the names a script cares about: bare for out
		// of sync, "*"-suffixed for mid-deploy. In sync and indeterminate
		// print nothing at all — and no build cell, which is why statusCmd
		// does not even look one up for -q.
		switch status.State {
		case promote.MidDeploy:
			outf(w, "%s*\n", app)
		case promote.OutOfSync:
			outln(w, app)
		}
		return verdictOf(status.State)
	}

	outf(w, "\n%s deployment status:\n", app)
	outln(w, strings.Repeat("-", 50))
	writeImages(w, "staging", staging)
	writeImages(w, "prod", prod)
	writeCell(w, "build", buildLabel(build, time.Now()))

	switch status.State {
	case promote.MidDeploy:
		// Which environment is rolling is a property of the image lists, and
		// staging is checked first because ib.py checks it first — with both
		// mid-deploy, staging is the one named.
		if len(staging) > 1 {
			outln(w, "\n⚠ Staging has an image mismatch (deployment in progress?)")
		} else {
			outln(w, "\n⚠ Prod has an image mismatch (deployment in progress?)")
		}
	case promote.InSync:
		outln(w, "\n✓ In sync")
	case promote.OutOfSync:
		outln(w, "\n✗ Out of sync")
		// ib.py points at itself here; this points at this binary, because as
		// of `bif promote` the hint is true. It is the one line of status
		// output that deliberately differs from the oracle — see
		// oraclePromoteHint in status_test.go.
		outf(w, "  To promote: bif promote %s\n", app)
		outf(w, "  This will deploy %s to prod\n", status.NewProdTag)
	default:
		// Indeterminate: the table is the whole answer, and ib.py still ends
		// with a blank line here.
		outln(w)
	}
	return verdictOf(status.State)
}

func verdictOf(state promote.State) verdict {
	switch state {
	case promote.InSync:
		return inSync
	case promote.OutOfSync:
		return outOfSync
	default:
		return indeterminate
	}
}

// writeImages renders one environment's line(s). ib.py pads the labels to a
// fixed width ("  staging: x" / "  prod:    x") but drops the padding when it
// has several images to list under a header, so both forms are reproduced.
func writeImages(w io.Writer, label string, images []string) {
	switch len(images) {
	case 0:
		writeCell(w, label, "(no pods found)")
	case 1:
		writeCell(w, label, promote.ExtractTag(images[0]))
	default:
		outf(w, "  %s:\n", label)
		for _, img := range images {
			outf(w, "    - %s\n", promote.ExtractTag(img))
		}
	}
}

// writeCell renders one labelled row of the table. ib.py's padding is the
// contract here — it is what makes "build:" line its value up under prod's,
// and every byte of it is pinned by the oracle fixtures.
func writeCell(w io.Writer, label, value string) {
	outf(w, "  %-8s %s\n", label+":", value)
}

// buildLabel renders one service's most recent build.
//
// The shape is glyph, SHA, state, time, in that order for every state, so the
// SHAs line up down a whole-fleet run and the eye can scan one column for the
// red one. It borrows the Apps tab's vocabulary (internal/web's buildViewFor)
// — ◌ in flight, ✓ succeeded, ✗ failed — but spells the state out, because a
// terminal has no column header to lean on:
//
//	◌ 0ab11f2 building (2m)
//	◌ 0ab11f2 queued
//	✓ 0ab11f2 succeeded 3h ago
//	✗ 0ab11f2 failed 12m ago
//	· 0ab11f2 cancelled 2d ago
//
// The two parenthesised forms are the absences, and they are different
// answers: Cloud Build having no recent build for this repo, versus not
// having been able to ask it.
func buildLabel(c buildCell, now time.Time) string {
	if !c.known {
		return "(build status unavailable)"
	}
	b := c.status
	if b.Status == "" {
		return "(no recent build)"
	}

	var glyph, state, when string
	switch {
	case b.InProgress():
		// A queued build has no start time yet, so there is no elapsed time to
		// show and "queued" is the whole of what is known.
		glyph = "◌"
		if b.StartTime.IsZero() {
			state = "queued"
		} else {
			state, when = "building", "("+elapsed(now.Sub(b.StartTime))+")"
		}
	case b.Status == "SUCCESS":
		glyph, state, when = "✓", "succeeded", ago(b.FinishTime, now)
	case b.Failed():
		glyph, state, when = "✗", "failed", ago(b.FinishTime, now)
	default:
		// CANCELLED today, and whatever Cloud Build adds tomorrow: terminal,
		// but neither a success nor a failure, so it gets a neutral mark and
		// its own name rather than being rounded into one of the two.
		glyph, state, when = "·", strings.ToLower(b.Status), ago(b.FinishTime, now)
	}

	parts := []string{glyph}
	if b.SHA != "" {
		parts = append(parts, b.SHA)
	}
	parts = append(parts, state)
	if when != "" {
		parts = append(parts, when)
	}
	return strings.Join(parts, " ")
}

// ago renders how long before now t was, in one coarse unit; "" for the zero
// time, so a build with no finish time renders without a trailing dangler.
// Anything under a minute — including the small negative a clock skew between
// Cloud Build and this machine produces — is "just now".
func ago(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	if d := now.Sub(t); d >= time.Minute {
		return elapsed(d) + " ago"
	}
	return "just now"
}

// elapsed renders a duration in a single coarse unit, the way `bif preview`'s
// TTL column and the web UI's "running 2m" both do. Negative clamps to zero.
func elapsed(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(int(d/time.Second), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}

// normalize dedupes and sorts image refs. ib.py keeps them in a set and prints
// sorted(...), so both the mid-deploy count and the listing order follow from
// that — note it sorts full image refs, not the extracted tags. kube.Images
// already dedupes; doing it again here means statusOne renders identically
// from any caller's list, including the oracle fixtures'.
func normalize(images []string) []string {
	if len(images) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(images))
	out := make([]string, 0, len(images))
	for _, img := range images {
		if _, dup := seen[img]; dup {
			continue
		}
		seen[img] = struct{}{}
		out = append(out, img)
	}
	sort.Strings(out)
	return out
}
