package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eswan18/bifrost/internal/gcb"
	"github.com/eswan18/bifrost/internal/kube"
	"github.com/eswan18/bifrost/internal/oci"
	"github.com/eswan18/bifrost/internal/promote"
	"github.com/eswan18/bifrost/internal/registry"
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

// digestResolver is the slice of the registry `bif status --attention` needs:
// one image reference in, the digest of the IMAGE MANIFEST behind it out. It is
// a read of a third party, like Cloud Build, and narrower than either of the
// two above — there is exactly one question to ask and no way to write.
//
// The unit is the image manifest and never the index that may contain it. See
// internal/oci: every buildx-built tag in this fleet is an index holding the
// image plus a per-build provenance attestation, so index digests differ on
// every build for byte-identical images, and comparing them would reproduce the
// false alarm this exists to remove.
type digestResolver interface {
	ManifestDigest(ctx context.Context, image string) (string, error)
}

// oci.Resolver has to satisfy digestResolver, or the fake the tests drive would
// be shaped like an interface the real resolver doesn't implement.
var _ digestResolver = (*oci.Resolver)(nil)

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

// statusCmd implements every form of `bif status`.
//
// dialDigests is reached by --attention alone, and only when a service's build
// tag and staging tag differ — see resolveSameContent, which does not so much
// as dial when nothing qualifies. -q makes no build lookup and no registry
// lookup: its offline guarantee is unchanged.
func statusCmd(ctx context.Context, args []string, stdout, stderr io.Writer, connect func() (podLister, error), dialBuilds func(context.Context) (buildLister, error), dialDigests func(context.Context) (digestResolver, error)) int {
	args, quiet := takeFlag(args, "-q", "--quiet")
	args, attention := takeFlag(args, "-a", "--attention")

	// -q and --attention are two different answers to "what should stdout be",
	// and there is no sensible winner. Letting -q win would silently drop the
	// mode the operator asked for; letting --attention win would break -q's
	// scriptable contract AND its offline guarantee, since --attention cannot
	// work without a Cloud Build read and -q deliberately makes none. So the
	// combination is refused rather than resolved.
	if quiet && attention {
		outf(stderr, "Error: -q and --attention are different output modes; pass one or the other.\n")
		return 1
	}

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

	// Any number of names narrows the fleet list to those names, in the order
	// given. Every form below — the table, -q, --attention — already loops over
	// `apps`, so several names cost this one line and nothing else: the
	// single-name case is just the list of length one it always was, and the
	// build lookup stays the single fleet-wide call it is for every other form.
	if len(args) > 0 {
		named, ok := resolveApps(stdout, apps, args)
		if !ok {
			return 1
		}
		apps = named
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

	if attention {
		return attentionReport(ctx, stdout, stderr, cluster, reg, apps, set, dialDigests, time.Now())
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

// ---- `bif status --attention` -------------------------------------------

// attentionSkippedNote is what stdout says when the Cloud Build read failed.
//
// `bif status`'s build lookup is best-effort by design: the column degrades to
// "(build status unavailable)" and everything else about the command is
// unchanged. That is right for `status`, whose answer comes from the cluster,
// and WRONG here. Two of --attention's four checks — a build running right
// now, and a successful build staging never picked up — are unknowable without
// build data, so a run that printed nothing and exited 0 would be claiming an
// all-clear on a half-finished inspection. The entire value of this mode is
// that silence can be trusted; one false all-clear during the one incident it
// was built for costs more than every true one it will ever produce.
//
// So the skip is stated on STDOUT, not just stderr. The underlying error goes
// to stderr where statusCmd already reports it, but an operator who ran
// `bif status -a 2>/dev/null` must still not be able to read this output as
// "nothing to do".
const attentionSkippedNote = "! Cloud Build could not be read, so two of the four checks did not run:\n" +
	"    whether a build is running now, and whether a successful build never reached staging.\n" +
	"  This is NOT an all-clear."

// attentionAllClear is the positive answer, printed only when all four checks
// ran and none fired. It is a line rather than silence on purpose: this mode
// exists to be trusted, so it always says which of its three outcomes it
// reached — findings, all-clear, or checks skipped.
const attentionAllClear = "Nothing needs attention."

// attentionReport implements `bif status --attention` (and `-a`): every service
// with something noteworthy, and what.
//
// Exit codes. 0 when every check ran and nothing qualified; 1 when something
// did — the same two-valued contract as -q, which is what makes this usable as
// a cron guard (`bif status -a || notify`). A failed build lookup also exits 1,
// deliberately: 0 is reserved for "I checked all four conditions and the fleet
// is clean", which is the only state in which silence means anything. A third
// code for "could not check" was considered and rejected — every caller that
// did not special-case it would fall back to treating it as attention-worthy,
// which is the behaviour we want anyway, so the extra code would buy nothing
// but a third thing to document and get wrong.
//
// An explicit app argument is ACCEPTED rather than rejected. `bif status
// footstrike-api -a` asks the same four questions about one service, which is a
// real thing to want — a per-service CI or cron guard, or a first look at the
// service you already suspect. It composes exactly the way `bif status <app>
// -q` does, it costs the same single LatestBuilds call (that call answers for
// the whole fleet regardless), and refusing it would make one flag combination
// an error for no reason a user could predict.
func attentionReport(ctx context.Context, stdout, stderr io.Writer, cluster podLister, reg registry.Registry, apps []string, set buildSet, dialDigests func(context.Context) (digestResolver, error), now time.Time) int {
	// The cluster is read for every service FIRST, and the registry afterwards,
	// because the second read's size depends on the first: only a service whose
	// build tag and staging tag differ has anything to resolve, and on a fleet
	// where none do this phase costs no dial, no token and no request at all.
	type appState struct {
		staging, prod []string
		build         buildCell
	}
	states := make(map[string]appState, len(apps))
	candidates := make(map[string]stalledSync, len(apps))
	for _, app := range apps {
		st := appState{
			staging: normalize(deployedImages(ctx, cluster, stderr, app+"-staging")),
			prod:    normalize(deployedImages(ctx, cluster, stderr, app+"-prod")),
			build:   set.cellFor(reg.RepoFor(app)),
		}
		states[app] = st
		if c, ok := stalledSyncCandidate(st.staging, st.build); ok {
			candidates[app] = c
		}
	}
	sameContent := resolveSameContent(ctx, dialDigests, candidates)

	type line struct {
		app string
		attentionLine
	}
	var lines []line
	attentionCount := 0
	width := 0
	for _, app := range apps {
		st := states[app]
		finding := syncFinding{}
		if c, ok := candidates[app]; ok {
			finding = syncFinding{stalled: &c, sameContent: sameContent[app]}
		}
		for _, l := range attentionFor(app, st.staging, st.prod, st.build, finding, now) {
			lines = append(lines, line{app: app, attentionLine: l})
			width = max(width, len(app))
			if !l.informational {
				attentionCount++
			}
		}
	}

	// One line per reason, each naming its own service, so every line stands
	// alone: `bif status -a | grep bifrost` gets everything about bifrost, and
	// `... | grep 'never reached staging'` gets the stalled syncs across the
	// fleet. Continuation lines under a single service name would read more
	// tidily and grep worse, and this output's job is to be acted on.
	for _, l := range lines {
		outf(stdout, "%-*s  %s\n", width, l.app, l.text)
	}

	if !set.known {
		outln(stdout, attentionSkippedNote)
		return 1
	}
	// Informational lines are printed and then not counted. The exit code is a
	// contract about ATTENTION — `bif status -a || notify` — and "staging is
	// running the latest build's content under an older tag" is the absence of
	// a finding, explained out loud rather than left as an unexplained silence.
	if attentionCount == 0 {
		outln(stdout, attentionAllClear)
		return 0
	}
	return 1
}

// attentionLine is one line of the report: what it says, and whether it is
// something to act on. See attentionReport for why the two are not the same
// thing.
type attentionLine struct {
	text          string
	informational bool
}

// syncFinding is the CI-versus-staging conclusion for one service, already
// resolved: whether the tags differ, and — when they do — whether the registry
// says the two tags are nevertheless the same image.
type syncFinding struct {
	stalled     *stalledSync // nil when the tags say there is nothing to report
	sameContent bool         // ...but both tags resolve to one image manifest
}

// attentionFor returns every reason app is worth a human's attention right
// now, or nil when there is none.
//
// Four conditions qualify. They are emitted in decreasing order of "this is
// broken" rather than in any numbered order, because that is how the reader
// wants them — a stalled sync explains why the promote listed under it will
// appear to do nothing:
//
//   - STALLED SYNC — the newest build succeeded, and staging is not running its
//     CONTENT. This is the reason this mode exists; see stalledSyncCandidate.
//   - Staging and prod differ: there is something to promote.
//   - Two or more distinct images inside one environment: a deploy in progress.
//     `bif status -q` already marks this with "*".
//   - A build is running right now (QUEUED/PENDING/WORKING).
//
// The last three are each already visible somewhere — in `bif status`'s own
// table, in -q's output, on the Apps tab. The stalled sync is visible NOWHERE,
// and it is not a duplicate of "staging and prod differ" however much it may
// look like one: that compares two environments to each other, this compares CI
// to staging. They answer different questions and they fail independently.
//
// One line this returns is not a condition at all: when the build's tag and
// staging's tag turn out to name the same image, that is said out loud as an
// INFORMATIONAL line (see stalledSync.sameContentNote). It prints like the
// others so it can be read and grepped alongside them, and it does not qualify
// the service — the exit code goes on meaning "something needs a human".
func attentionFor(app string, staging, prod []string, build buildCell, finding syncFinding, now time.Time) []attentionLine {
	var reasons []attentionLine
	warn := func(text string) { reasons = append(reasons, attentionLine{text: text}) }

	// The CI-versus-staging comparison arrives already made, because settling
	// it needs the registry (see resolveSameContent) and this function is pure.
	if finding.stalled != nil {
		if finding.sameContent {
			reasons = append(reasons, attentionLine{text: finding.stalled.sameContentNote(), informational: true})
		} else {
			warn(finding.stalled.warning(now))
		}
	}

	// promote.StatusOf is the same decision `bif status` and the server make,
	// reused rather than re-derived: "staging and prod differ" has to mean here
	// exactly what it means everywhere else, including the unpinned-prod case
	// (bifrost#30) that reads as out of sync.
	//
	// It stays a TAG comparison, deliberately, where the check above no longer
	// is: footstrike-dashboard builds {sha}-staging and {sha}-prod as genuinely
	// separate images — its Vite API/identity/client-id values are baked in at
	// build time from per-environment --build-arg values, verified in that
	// repo's cloudbuild.yaml and Dockerfile — so their manifests are never
	// equal and a digest-based verdict would mark it permanently out of sync.
	if status := promote.StatusOf(staging, prod); status.State == promote.OutOfSync {
		warn(fmt.Sprintf("staging and prod differ: staging %s, prod %s (bif promote %s)",
			status.StagingTag, status.ProdTag, app))
	}

	// Both environments are checked, not just the one promote.StatusOf would
	// have named: a prod rollout and a staging rollout are equally worth
	// knowing about, and unlike -q's single "*" there is room to say which.
	for _, env := range []struct {
		name   string
		images []string
	}{{"staging", staging}, {"prod", prod}} {
		if len(env.images) > 1 {
			warn(fmt.Sprintf("deploy in progress: %s is running %d images (%s)",
				env.name, len(env.images), strings.Join(tagsOf(env.images), ", ")))
		}
	}

	if build.known && build.status.InProgress() {
		warn(inFlightBuildReason(build.status, now))
	}
	return reasons
}

// stalledSync is the condition this whole mode was built for, as far as TAGS
// can carry it: the most recent build SUCCEEDED, and staging is not running its
// SHA. Whether that is drift or merely a new tag over the same image is settled
// by the registry — see resolveSameContent — so this carries the image
// references that settle it as well as the text that reports it.
type stalledSync struct {
	buildSHA    string
	finish      time.Time
	stagingTags []string // every tag staging runs, for the message
	// buildImage is the image reference the newest build pushed FOR STAGING,
	// and stagingImages are the staging images that carry a commit. These two
	// are what get resolved to manifest digests; both are derived from an
	// image staging is actually running, never from the app's name — the same
	// rule promote.ReplaceTag exists to keep (see its comment).
	buildImage    string
	stagingImages []string
}

// stalledSyncCandidate implements the tag half of that condition.
//
// DO NOT "simplify" this away as redundant with "staging and prod differ". It
// is not. It detects a failure mode this fleet has actually hit and that
// nothing else surfaces: when ArgoCD's github-eswan18-repocreds PAT expires,
// the Applications go to ComparisonError and syncs SILENTLY STOP. Builds keep
// going green, staging quietly stays on last week's image, and `bif promote`
// reports success while prod never moves — because the write lands in the
// Application and nothing ever reconciles it. A successful build whose SHA
// never reached staging is the signature of that state, and it is the only
// signal in this tool that points at it.
//
// It is a CANDIDATE and not a verdict, because a tag difference is not by
// itself a content difference. bifrost's image builds ./cmd/bifrost; a commit
// touching only ./cmd/bif produces a new tag over a byte-identical image, and
// image-updater correctly does not move staging. That is the false alarm that
// sent an owner chasing a deploy which had already happened, and it is why the
// answer here is handed to the registry before it is printed.
//
// DO NOT "simplify" this away as redundant with "staging and prod differ". It
// is not. It detects a failure mode this fleet has actually hit and that
// nothing else surfaces: when ArgoCD's github-eswan18-repocreds PAT expires,
// the Applications go to ComparisonError and syncs SILENTLY STOP. Builds keep
// going green, staging quietly stays on last week's image, and `bif promote`
// reports success while prod never moves — because the write lands in the
// Application and nothing ever reconciles it. A successful build whose SHA
// never reached staging is the signature of that state, and it is the only
// signal in this tool that points at it.
//
// There is deliberately NO grace period and no threshold. The window between a
// build going green and image-updater moving staging is short and it is a state
// worth being able to SEE — running this seconds after a push and being told
// "nothing to see" would defeat the point. So the transient case is reported,
// not suppressed, and made legible instead: the reason carries how long ago the
// build finished, rendered by the same `ago` the build column uses, so
//
//	build abc1234 succeeded just now, staging still on def5678
//
// and
//
//	build abc1234 succeeded 3d ago, staging still on def5678
//
// look nothing alike at a glance. The reader draws the conclusion; the command
// does not decide for them by hiding one of the two.
//
// What this does NOT fire on, because these are "cannot tell", not "fine":
// Cloud Build unreadable (attentionSkippedNote says so out loud instead), no
// recent build, a build that is running or failed rather than succeeded, a
// staging with no pods, and a staging whose tags carry no parseable SHA (an
// unpinned `latest`, whose content genuinely cannot be compared to a commit).
func stalledSyncCandidate(staging []string, build buildCell) (stalledSync, bool) {
	if !build.known {
		return stalledSync{}, false
	}
	b := build.status
	if b.Status != "SUCCESS" || b.SHA == "" {
		return stalledSync{}, false
	}

	// comparable is the single "could we even tell?" guard, and it covers both
	// absences at once: a staging with no pods contributes no tags, and a
	// staging pinned to a mutable `latest` contributes tags with no commit in
	// them. An explicit len(staging) == 0 above would be a second spelling of
	// the first case and dead by the time it was read.
	tags := tagsOf(staging)
	out := stalledSync{buildSHA: b.SHA, finish: b.FinishTime, stagingTags: tags}
	for i, tag := range tags {
		sha := promote.ExtractSHA(tag)
		if sha == "" {
			continue
		}
		// Any staging image on the build's commit means staging picked it up.
		// Checking every image rather than requiring a single one is what keeps
		// a rollout that is halfway to the new SHA from reading as stalled — it
		// is mid-deploy, which is its own condition and says so itself.
		if shaMatches(sha, b.SHA) {
			return stalledSync{}, false
		}
		out.stagingImages = append(out.stagingImages, staging[i])
		if out.buildImage == "" {
			out.buildImage = promote.ReplaceTag(staging[i], buildTagFor(tag, b.SHA))
		}
	}
	if len(out.stagingImages) == 0 {
		return stalledSync{}, false
	}
	return out, true
}

// warning is what this has always printed, unchanged: a green build whose SHA
// staging is not running.
//
// There is deliberately NO grace period and no threshold. The window between a
// build going green and image-updater moving staging is short and it is a state
// worth being able to SEE — running this seconds after a push and being told
// "nothing to see" would defeat the point. So the transient case is reported,
// not suppressed, and made legible instead: the reason carries how long ago the
// build finished, rendered by the same `ago` the build column uses, so
//
//	build abc1234 succeeded just now, staging still on def5678
//
// and
//
//	build abc1234 succeeded 3d ago, staging still on def5678
//
// look nothing alike at a glance. The reader draws the conclusion; the command
// does not decide for them by hiding one of the two.
func (s stalledSync) warning(now time.Time) string {
	when := ago(s.finish, now)
	if when == "" {
		// SUCCESS with no finish time is not a state Cloud Build produces, but
		// the field is parsed from a string and a zero here must not render as
		// "succeeded , staging still on ...".
		when = "at an unknown time"
	}
	return fmt.Sprintf("build %s succeeded %s, staging still on %s", s.buildSHA, when, strings.Join(s.stagingTags, ", "))
}

// sameContentNote is the answer when the two tags turn out to be one image.
//
// It is a printed line rather than silence on purpose. The operator can see
// that a build went green and that staging's tag is an older commit; saying
// nothing would leave them exactly where the false alarm did — wondering why a
// build never appeared to deploy — with the difference that now the tool is
// silently sure. So it says which two things it compared, and BOTH SHAs stay
// on screen: the tag is the link back to GitHub, and a digest is an input to
// this verdict, never a replacement for the commit in the display.
func (s stalledSync) sameContentNote() string {
	return fmt.Sprintf("staging matches the latest build's content (running %s, built %s)",
		strings.Join(s.stagingTags, ", "), s.buildSHA)
}

// buildTagFor returns the tag the newest build pushed FOR STAGING, in the tag
// scheme staging is already running.
//
// Most services build one environment-agnostic {sha}. footstrike-dashboard
// builds {sha}-staging and {sha}-prod as separate images — its Vite API,
// identity and client-id values are baked in at build time from per-environment
// --build-arg values (verified in that repo's cloudbuild.yaml and Dockerfile) —
// so a bare {sha} names an image that was never pushed for it, and asking the
// registry for one would 404 into a needless fallback. It follows the STAGING
// tag for the same reason promote.NewProdTag follows it: the scheme belongs to
// the artifact, not to the app's name.
func buildTagFor(stagingTag, buildSHA string) string {
	if strings.Contains(stagingTag, "-staging") {
		return buildSHA + "-staging"
	}
	return buildSHA
}

// digestLookupTimeout bounds the whole Artifact Registry phase of --attention,
// in the spirit of buildLookupTimeout and for the same reason: this is a
// command reached for during an incident, the registry is a third party, and an
// answer that has not arrived in a few seconds is not worth waiting for when
// the fallback is the warning this command has always printed. It bounds the
// PHASE rather than each request because the requests run concurrently. A var
// so the tests can prove the bound is applied without waiting for it.
var digestLookupTimeout = 3 * time.Second

// resolveSameContent asks the registry, for each candidate stalled sync,
// whether the build's tag and staging's tag are the same image after all.
//
// Three properties are deliberate:
//
//   - NOTHING happens when there are no candidates. No dial, no token, no
//     request. A healthy fleet's `bif status -a` costs exactly what it did
//     before this existed.
//   - One dial, therefore one credential, for the whole invocation — the
//     resolver fetches its token in its constructor (internal/oci.New) — and
//     the services resolve CONCURRENTLY, the way fetchBuilds already overlaps
//     the build read with the cluster read.
//   - Every failure answers "not the same": a dial that fails, a request that
//     fails, a tag that isn't there, a timeout. That direction is the whole
//     safety argument. A registry problem must never be able to SUPPRESS a real
//     stalled sync — the mode exists to catch a failure whose signature is
//     silence, so the fallback is the warning, never the quiet.
func resolveSameContent(ctx context.Context, dial func(context.Context) (digestResolver, error), candidates map[string]stalledSync) map[string]bool {
	if len(candidates) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, digestLookupTimeout)
	defer cancel()

	resolver, err := dial(ctx)
	if err != nil {
		return nil
	}

	var mu sync.Mutex
	same := make(map[string]bool, len(candidates))
	var wg sync.WaitGroup
	for app, candidate := range candidates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !sameImageManifest(ctx, resolver, candidate) {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			same[app] = true
		}()
	}
	wg.Wait()
	return same
}

// sameImageManifest reports whether the build's image and one staging is
// running resolve to the same image manifest.
//
// ANY staging image matching is enough, for the same reason the tag comparison
// accepts any: a rollout that is halfway onto the content is not stalled. A
// lookup that fails is skipped rather than fatal — with two staging images, one
// unreadable tag must not decide the other.
func sameImageManifest(ctx context.Context, resolver digestResolver, s stalledSync) bool {
	built, err := resolver.ManifestDigest(ctx, s.buildImage)
	if err != nil || built == "" {
		return false
	}
	for _, img := range s.stagingImages {
		running, err := resolver.ManifestDigest(ctx, img)
		if err != nil {
			continue
		}
		if running == built {
			return true
		}
	}
	return false
}

// inFlightBuildReason renders a build that is running right now. A queued build
// has no start time yet, so it gets no elapsed time — the same distinction
// buildLabel draws, for the same reason: an invented "0s" reads as stuck.
func inFlightBuildReason(b gcb.BuildStatus, now time.Time) string {
	what := "build " + b.SHA
	if b.SHA == "" {
		what = "a build"
	}
	if b.StartTime.IsZero() {
		return what + " is queued"
	}
	return fmt.Sprintf("%s is building (%s)", what, elapsed(now.Sub(b.StartTime)))
}

// shaMatches reports whether a deployed tag's SHA and a build's SHORT_SHA name
// the same commit. Both are abbreviations of one hash and are not guaranteed to
// be abbreviated to the same length — Cloud Build's $SHORT_SHA is seven
// characters, while promote.ExtractSHA accepts seven or more — so the shorter
// being a PREFIX of the longer is the match. Anchored at the front, which is
// what makes this a hash abbreviation rather than a substring search.
func shaMatches(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// tagsOf extracts the tag from each image ref, preserving order.
func tagsOf(images []string) []string {
	tags := make([]string, 0, len(images))
	for _, img := range images {
		tags = append(tags, promote.ExtractTag(img))
	}
	return tags
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
