package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eswan18/bifrost/internal/gcb"
	"github.com/eswan18/bifrost/internal/kube"
	"github.com/eswan18/bifrost/internal/oracle"
	"github.com/eswan18/bifrost/internal/registry"
)

// ---- fake cluster -------------------------------------------------------

// fakeCluster serves ListPods from a namespace -> container images map. It
// synthesizes one single-container pod per image rather than returning images
// directly, so the tests exercise kube.Images (Job-pod exclusion, deduping)
// on the way through instead of stepping over it.
//
// It also records every PatchAppImage call, which is how the promote tests
// tell a promotion from a refusal: a decision that should not write must leave
// patches empty, and "no write happened" is not something an output assertion
// can establish.
type fakeCluster struct {
	images map[string][]string
	errs   map[string]error
	calls  []string

	patches  []patchCall
	patchErr error
	// patchErrs fails the patch for named apps only. A several-service promote
	// has to keep going after one service's write fails, and a single
	// all-or-nothing patchErr cannot express the cluster state that proves it.
	patchErrs map[string]error
}

// patchCall is one write to an ArgoCD Application, as promote asked for it.
type patchCall struct {
	app   string
	env   string
	image string
}

func (f *fakeCluster) ListPods(_ context.Context, ns string) ([]kube.PodInfo, error) {
	f.calls = append(f.calls, ns)
	if err := f.errs[ns]; err != nil {
		return nil, err
	}
	var pods []kube.PodInfo
	for i, img := range f.images[ns] {
		pods = append(pods, kube.PodInfo{
			Namespace:  ns,
			Name:       fmt.Sprintf("%s-%d", ns, i),
			OwnerKind:  "ReplicaSet",
			OwnerName:  ns,
			Phase:      "Running",
			Containers: []kube.ContainerInfo{{Name: "app", Image: img, Ready: true}},
		})
	}
	return pods, nil
}

func (f *fakeCluster) PatchAppImage(_ context.Context, app, env, image string) error {
	f.patches = append(f.patches, patchCall{app: app, env: env, image: image})
	if err := f.patchErrs[app]; err != nil {
		return err
	}
	return f.patchErr
}

// promotedApps is the apps this cluster actually had prod moved for, in the
// order the writes arrived. Assertions about which services were acted on read
// this rather than scanning stdout: two services' output lines differ only by
// name, and strings.Contains over the whole run will happily match the wrong
// one.
func (f *fakeCluster) promotedApps() []string {
	out := make([]string, 0, len(f.patches))
	for _, p := range f.patches {
		out = append(out, p.app)
	}
	return out
}

// patchedImage is what prod was pinned to for one app, or "" if it never was.
func (f *fakeCluster) patchedImage(app string) string {
	for _, p := range f.patches {
		if p.app == app {
			return p.image
		}
	}
	return ""
}

func (f *fakeCluster) connect() (promoter, error) { return f, nil }

const testRegistry = "us-central1-docker.pkg.dev/ethans-services/containers"

func image(app, tag string) string { return fmt.Sprintf("%s/%s:%s", testRegistry, app, tag) }

// exec runs one whole bif invocation against the fake and returns its stdout
// and exit code. Stdin is empty, so a command that reads it gets EOF —
// promote's tests that care pass their own (see execStdin).
func exec(t *testing.T, cluster *fakeCluster, args ...string) (string, int) {
	t.Helper()
	return execStdin(t, cluster, "", args...)
}

// execStdin is exec with the operator's keystrokes supplied.
func execStdin(t *testing.T, cluster *fakeCluster, stdin string, args ...string) (string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(stdin), &stdout, &stderr, cluster.connect, unreachableBuilds, noPreview)
	return stdout.String(), code
}

// ---- fake Cloud Build ---------------------------------------------------

// fakeBuilds serves LatestBuilds from a repo -> build map. It counts both the
// dials and the calls, because "one API call for the whole fleet" is a
// property no output assertion can see.
//
// The counters are mutex-guarded: the lookup runs on its own goroutine (see
// fetchBuilds), and a test that reads them is a second one.
type fakeBuilds struct {
	byRepo  map[string]gcb.BuildStatus
	dialErr error // no credentials, no project, ...
	listErr error // the API answered with an error
	hang    bool  // never answer; wait for the context instead

	mu    sync.Mutex
	dials int
	calls int
}

func (f *fakeBuilds) dial(context.Context) (buildLister, error) {
	f.mu.Lock()
	f.dials++
	f.mu.Unlock()
	if f.dialErr != nil {
		return nil, f.dialErr
	}
	return f, nil
}

func (f *fakeBuilds) LatestBuilds(ctx context.Context) (map[string]gcb.BuildStatus, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byRepo, nil
}

func (f *fakeBuilds) counts() (dials, calls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dials, f.calls
}

// execBuilds is exec with a Cloud Build to talk to, and it returns stderr too
// — where the build lookup's failures are reported, and where they must stay.
func execBuilds(t *testing.T, cluster *fakeCluster, builds *fakeBuilds, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(context.Background(), args, strings.NewReader(""), &out, &errb, cluster.connect, builds.dial, noPreview)
	return out.String(), errb.String(), code
}

// ---- the oracle fixtures ------------------------------------------------

// statusRow mirrors one row of testdata/oracle/status.json — the images that
// were deployed, and what ib.py's status() printed and returned for them.
type statusRow struct {
	App           string   `json:"app"`
	StagingImages []string `json:"stagingImages"`
	ProdImages    []string `json:"prodImages"`
	State         string   `json:"state"`
	QuietReturn   *bool    `json:"quietReturn"`
	QuietStdout   string   `json:"quietStdout"`
	VerboseReturn *bool    `json:"verboseReturn"`
	VerboseStdout string   `json:"verboseStdout"`
}

func (r statusRow) key() string {
	return r.App + " staging=[" + strings.Join(r.StagingImages, " ") + "] prod=[" + strings.Join(r.ProdImages, " ") + "]"
}

// verdictFromOracle maps ib.py's True/False/None onto the Go tri-state.
func verdictFromOracle(b *bool) verdict {
	switch {
	case b == nil:
		return indeterminate
	case *b:
		return inSync
	default:
		return outOfSync
	}
}

func (v verdict) String() string {
	switch v {
	case inSync:
		return "inSync"
	case outOfSync:
		return "outOfSync"
	default:
		return "indeterminate"
	}
}

// divergence is a fixture row where Go deliberately does not reproduce ib.py.
// Both sides are pinned: the fixture must still say what we recorded it
// saying, and Go must still do what we decided it should do. Changing either
// one breaks the test rather than quietly redefining the contract.
type divergence struct {
	pythonStdout  string
	pythonVerdict verdict
	goStdout      string
	goVerdict     verdict
	why           string
}

// verboseDivergences / quietDivergences: DIVERGENCE #1 from Task 1, the only
// place `bif status` deliberately disagrees with ib.py.
//
// ib.py's status() calls an unparseable PROD tag indeterminate, because it
// requires both SHAs before it will compare them. promote.StatusOf requires
// only the staging SHA — the artifact being promoted — and calls the same
// state out of sync.
//
// Go is right, and it is a divergence from ib.py's *status*, not its
// *promote*: ib.py's own promote() accepts exactly this state and promotes
// from it. The state is real and recoverable — the prod Application lost its
// image pin and fell back to the mutable `latest`/`prod` tag in the repo
// manifests — and calling it "unknown" is what left bifrost refusing to
// promote with no way out (bifrost#30).
//
// The visible cost, which is why it is spelled out here rather than left to
// be discovered: `bif status -q` now prints the app and exits 1 where ib.py
// printed nothing and exited 0. That is a scriptable contract changing.
var verboseDivergences = map[string]divergence{
	"bifrost staging=[us-central1-docker.pkg.dev/ethans-services/containers/bifrost:abc1234] prod=[us-central1-docker.pkg.dev/ethans-services/containers/bifrost:latest]": {
		pythonStdout: "\nbifrost deployment status:\n" + strings.Repeat("-", 50) +
			"\n  staging: abc1234\n  prod:    latest\n\n",
		pythonVerdict: indeterminate,
		goStdout: "\nbifrost deployment status:\n" + strings.Repeat("-", 50) +
			"\n  staging: abc1234\n  prod:    latest\n" +
			"\n✗ Out of sync\n  To promote: bif promote bifrost\n  This will deploy abc1234 to prod\n",
		goVerdict: outOfSync,
		why:       "prod unpinned on a mutable tag; ib.py status() needs both SHAs, ib.py promote() does not (bifrost#30)",
	},
	"footstrike-dashboard staging=[us-central1-docker.pkg.dev/ethans-services/containers/footstrike-dashboard:abc1234-staging] prod=[us-central1-docker.pkg.dev/ethans-services/containers/footstrike-dashboard:prod]": {
		pythonStdout: "\nfootstrike-dashboard deployment status:\n" + strings.Repeat("-", 50) +
			"\n  staging: abc1234-staging\n  prod:    prod\n\n",
		pythonVerdict: indeterminate,
		goStdout: "\nfootstrike-dashboard deployment status:\n" + strings.Repeat("-", 50) +
			"\n  staging: abc1234-staging\n  prod:    prod\n" +
			"\n✗ Out of sync\n  To promote: bif promote footstrike-dashboard\n  This will deploy abc1234-prod to prod\n",
		goVerdict: outOfSync,
		why:       "same, on the {sha}-{env} tagging scheme",
	},
}

var quietDivergences = map[string]divergence{
	"bifrost staging=[us-central1-docker.pkg.dev/ethans-services/containers/bifrost:abc1234] prod=[us-central1-docker.pkg.dev/ethans-services/containers/bifrost:latest]": {
		pythonStdout: "", pythonVerdict: indeterminate,
		goStdout: "bifrost\n", goVerdict: outOfSync,
		why: "bifrost#30: -q prints the app and exits 1 where ib.py printed nothing and exited 0",
	},
	"footstrike-dashboard staging=[us-central1-docker.pkg.dev/ethans-services/containers/footstrike-dashboard:abc1234-staging] prod=[us-central1-docker.pkg.dev/ethans-services/containers/footstrike-dashboard:prod]": {
		pythonStdout: "", pythonVerdict: indeterminate,
		goStdout: "footstrike-dashboard\n", goVerdict: outOfSync,
		why: "same, on the {sha}-{env} tagging scheme",
	},
}

// The out-of-sync hint is the one line where `bif status` deliberately does
// not reproduce the oracle's bytes: ib.py named itself, and this names the
// binary the reader is already running. `bif promote` is the implementation
// and `ib promote` no longer exists. Nothing else about the line moves — same
// indent, same position, same following line.
const (
	oraclePromoteHint = "  To promote: ib promote "
	bifPromoteHint    = "  To promote: bif promote "
)

// retargetPromoteHint applies that one substitution to a captured ib.py
// output, so the golden comparison stays byte-exact everywhere else instead of
// being weakened to a "contains" check.
func retargetPromoteHint(oracleStdout string) string {
	return strings.ReplaceAll(oracleStdout, oraclePromoteHint, bifPromoteHint)
}

// unknownBuild is the build cell for tests that are not about the build
// column: Cloud Build was not reachable, which is the state every one of them
// runs its cluster assertions in.
var unknownBuild = buildCell{}

// buildLinePrefix is the build cell's label as writeCell renders it — the
// padding included, since lining up under "prod:" is the point of it.
const buildLinePrefix = "  build:   "

// stripBuildLine removes the build line from `bif status` output and fails if
// there is not exactly one.
//
// The oracle predates the build column by an entire tool, so its captured
// output cannot contain one, and the golden comparison below would otherwise
// have to be weakened to a "contains". Requiring exactly one before removing
// it turns the strip into an assertion of its own: every oracle row — every
// cluster state ib.py was ever captured against, in sync, mid-deploy, no pods
// — now also proves the build line is rendered there, exactly once.
func stripBuildLine(t *testing.T, out string) string {
	t.Helper()
	lines := strings.Split(out, "\n")
	kept := make([]string, 0, len(lines))
	found := 0
	for _, line := range lines {
		if strings.HasPrefix(line, buildLinePrefix) {
			found++
			continue
		}
		kept = append(kept, line)
	}
	if found != 1 {
		t.Fatalf("output has %d lines starting %q, want exactly 1:\n%s", found, buildLinePrefix, out)
	}
	return strings.Join(kept, "\n")
}

// TestStatusRenderingMatchesOracle is the golden comparison: for every cluster
// state ib.py was captured against, `bif status` must print exactly what ib.py
// printed and return the same verdict — byte for byte, including the table
// alignment, the blank lines and the ⚠/✓/✗ glyphs. The exceptions are the
// promote hint (see retargetPromoteHint) and the build line, which ib.py had
// no notion of (see stripBuildLine).
func TestStatusRenderingMatchesOracle(t *testing.T) {
	rows, err := oracle.Load[statusRow]("status.json")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}

	// Guard the substitution before relying on it. If ib.py's captured output
	// no longer contains the hint at all, retargetPromoteHint is a silent
	// no-op and every out-of-sync row would then be asserting that `bif
	// status` prints no hint whatsoever — a passing test for a broken tool.
	hinted := 0
	for _, r := range rows {
		if strings.Contains(r.VerboseStdout, oraclePromoteHint) {
			hinted++
		}
	}
	if hinted == 0 {
		t.Fatalf("no fixture row contains %q; the promote-hint rewrite is a no-op and this test no longer checks the hint", oraclePromoteHint)
	}

	for _, mode := range []struct {
		name        string
		quiet       bool
		stdout      func(statusRow) string
		ret         func(statusRow) *bool
		divergences map[string]divergence
		// got normalizes what bif printed before it is compared to ib.py's
		// bytes. Verbose strips the build line, which ib.py could not have
		// printed; quiet has no build line to strip. See stripBuildLine.
		got func(*testing.T, string) string
	}{
		{
			name:        "verbose",
			quiet:       false,
			stdout:      func(r statusRow) string { return retargetPromoteHint(r.VerboseStdout) },
			ret:         func(r statusRow) *bool { return r.VerboseReturn },
			divergences: verboseDivergences,
			got:         stripBuildLine,
		},
		{
			name:        "quiet",
			quiet:       true,
			stdout:      func(r statusRow) string { return r.QuietStdout },
			ret:         func(r statusRow) *bool { return r.QuietReturn },
			divergences: quietDivergences,
			got:         func(_ *testing.T, out string) string { return out },
		},
	} {
		t.Run(mode.name, func(t *testing.T) {
			for _, r := range rows {
				t.Run(r.key(), func(t *testing.T) {
					var raw bytes.Buffer
					got := statusOne(&raw, r.App, r.StagingImages, r.ProdImages, mode.quiet, unknownBuild)
					buf := mode.got(t, raw.String())

					wantStdout := mode.stdout(r)
					wantVerdict := verdictFromOracle(mode.ret(r))

					if d, known := mode.divergences[r.key()]; known {
						// Guard the oracle first: if ib.py's captured output
						// moved, the divergence note is describing a Python
						// that no longer exists and has to be re-derived.
						if wantStdout != d.pythonStdout || wantVerdict != d.pythonVerdict {
							t.Fatalf("oracle changed: ib.py now prints %q / returns %v; divergence table records %q / %v",
								wantStdout, wantVerdict, d.pythonStdout, d.pythonVerdict)
						}
						if buf != d.goStdout {
							t.Fatalf("known divergence changed:\n got %q\nwant %q\n(%s)", buf, d.goStdout, d.why)
						}
						if got != d.goVerdict {
							t.Fatalf("known divergence changed: verdict %v, was %v (%s)", got, d.goVerdict, d.why)
						}
						return
					}

					if buf != wantStdout {
						t.Errorf("stdout mismatch\n got %q\nib.py %q", buf, wantStdout)
					}
					if got != wantVerdict {
						t.Errorf("verdict = %v, ib.py returned %v", got, wantVerdict)
					}
				})
			}
		})
	}
}

// TestMidDeployKeepsTheOtherEnvironmentsTag pins the resolution of Task 1's
// DIVERGENCE #2. promote.Status zeroes StagingTag/ProdTag whenever either side
// is mid-deploy or empty, so a `cmd/bif` that rendered from Status would print
// a table with a hole in it. It renders from the image lists instead, and this
// is the assertion that says so: prod's tag survives a staging rollout, and
// staging's survives a prod one.
//
// The oracle rows already cover these states; this test exists on top of them
// to name the property, so that a future refactor toward "just use Status"
// fails with the reason rather than as a diff.
func TestMidDeployKeepsTheOtherEnvironmentsTag(t *testing.T) {
	tests := []struct {
		name     string
		staging  []string
		prod     []string
		wantLine string
	}{
		{
			name:     "staging rolling, prod tag still shown",
			staging:  []string{image("bifrost", "abc1234"), image("bifrost", "def5678")},
			prod:     []string{image("bifrost", "abc1234")},
			wantLine: "  prod:    abc1234",
		},
		{
			name:     "prod rolling, staging tag still shown",
			staging:  []string{image("bifrost", "abc1234")},
			prod:     []string{image("bifrost", "abc1234"), image("bifrost", "def5678")},
			wantLine: "  staging: abc1234",
		},
		{
			name:     "no staging pods, prod tag still shown",
			staging:  nil,
			prod:     []string{image("bifrost", "abc1234")},
			wantLine: "  prod:    abc1234",
		},
		{
			name:     "no prod pods, staging tag still shown",
			staging:  []string{image("bifrost", "abc1234")},
			prod:     nil,
			wantLine: "  staging: abc1234",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if v := statusOne(&buf, "bifrost", tc.staging, tc.prod, false, unknownBuild); v != indeterminate {
				t.Fatalf("verdict = %v, want indeterminate", v)
			}
			if !slices.Contains(strings.Split(buf.String(), "\n"), tc.wantLine) {
				t.Errorf("output is missing %q:\n%s", tc.wantLine, buf.String())
			}
		})
	}
}

// TestUnparseableProdTagIsOutOfSync is DIVERGENCE #1 stated as a contract
// rather than as a fixture exception: an unpinned prod is promotable, so
// `bif status -q` names it and exits 1. ib.py exited 0 here. See
// verboseDivergences for why Go is the correct side.
func TestUnparseableProdTagIsOutOfSync(t *testing.T) {
	for _, tc := range []struct {
		app        string
		stagingTag string
		prodTag    string
		wantPromo  string
	}{
		{"bifrost", "abc1234", "latest", "abc1234"},
		{"footstrike-dashboard", "abc1234-staging", "prod", "abc1234-prod"},
	} {
		t.Run(tc.app+"/"+tc.prodTag, func(t *testing.T) {
			cluster := &fakeCluster{images: map[string][]string{
				tc.app + "-staging": {image(tc.app, tc.stagingTag)},
				tc.app + "-prod":    {image(tc.app, tc.prodTag)},
			}}

			out, code := exec(t, cluster, "status", tc.app, "-q")
			if out != tc.app+"\n" {
				t.Errorf("quiet stdout = %q, want %q (ib.py printed nothing here)", out, tc.app+"\n")
			}
			if code != 1 {
				t.Errorf("exit = %d, want 1 (ib.py exited 0 here)", code)
			}

			out, code = exec(t, cluster, "status", tc.app)
			if !strings.Contains(out, "This will deploy "+tc.wantPromo+" to prod") {
				t.Errorf("verbose output does not offer the promotion:\n%s", out)
			}
			if code != 1 {
				t.Errorf("verbose exit = %d, want 1", code)
			}
		})
	}
}

// TestOutOfSyncHintNamesBif: the hint has to name a command that exists and
// does the thing. It said `ib promote` while promote lived only in the Python;
// now that `bif promote` is the implementation and the Python is deleted,
// pointing at `ib` would send an operator to a command that isn't there.
func TestOutOfSyncHintNamesBif(t *testing.T) {
	c := fleet(t)
	c.images["bifrost-prod"] = []string{image("bifrost", "def5678")}

	out, code := exec(t, c, "status", "bifrost")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(out, bifPromoteHint+"bifrost\n") {
		t.Errorf("status does not offer `bif promote bifrost`:\n%s", out)
	}
	if strings.Contains(out, oraclePromoteHint) {
		t.Errorf("status still points at the Python CLI:\n%s", out)
	}
}

// ---- exit codes, all four forms ----------------------------------------

// fleet builds a whole-registry cluster where every service is in sync,
// so a test can make exactly one service interesting.
func fleet(t *testing.T) *fakeCluster {
	t.Helper()
	reg, err := registry.Load()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	images := map[string][]string{}
	for _, app := range reg.Names() {
		images[app+"-staging"] = []string{image(app, "abc1234")}
		images[app+"-prod"] = []string{image(app, "abc1234")}
	}
	return &fakeCluster{images: images}
}

// TestExitCodesAllForms is the exit-status contract for every form of
// `bif status`. The mapping is ib.py's and does not vary with -q: only a
// definite out-of-sync exits 1.
func TestExitCodesAllForms(t *testing.T) {
	// state applies one service's cluster state onto an otherwise in-sync
	// fleet.
	type state struct {
		staging []string
		prod    []string
	}
	states := map[string]state{
		"in sync":            {[]string{image("bifrost", "abc1234")}, []string{image("bifrost", "abc1234")}},
		"out of sync":        {[]string{image("bifrost", "abc1234")}, []string{image("bifrost", "def5678")}},
		"mid deploy":         {[]string{image("bifrost", "abc1234"), image("bifrost", "def5678")}, []string{image("bifrost", "abc1234")}},
		"no pods":            {nil, []string{image("bifrost", "abc1234")}},
		"unreadable staging": {[]string{image("bifrost", "latest")}, []string{image("bifrost", "abc1234")}},
	}

	tests := []struct {
		state string
		// wantCode is the same for all four forms — that is the property.
		wantCode int
		// wantQuiet is what the -q forms print for bifrost.
		wantQuiet string
	}{
		{"in sync", 0, ""},
		{"out of sync", 1, "bifrost\n"},
		{"mid deploy", 0, "bifrost*\n"},
		{"no pods", 0, ""},
		{"unreadable staging", 0, ""},
	}

	for _, tc := range tests {
		t.Run(tc.state, func(t *testing.T) {
			st := states[tc.state]

			// Form 2: bif status <app>
			c := fleet(t)
			c.images["bifrost-staging"], c.images["bifrost-prod"] = st.staging, st.prod
			if _, code := exec(t, c, "status", "bifrost"); code != tc.wantCode {
				t.Errorf("`bif status bifrost` exit = %d, want %d", code, tc.wantCode)
			}

			// Form 4: bif status <app> -q
			c = fleet(t)
			c.images["bifrost-staging"], c.images["bifrost-prod"] = st.staging, st.prod
			out, code := exec(t, c, "status", "bifrost", "-q")
			if code != tc.wantCode {
				t.Errorf("`bif status bifrost -q` exit = %d, want %d", code, tc.wantCode)
			}
			if out != tc.wantQuiet {
				t.Errorf("`bif status bifrost -q` stdout = %q, want %q", out, tc.wantQuiet)
			}

			// Form 1: bif status (whole fleet; every other service in sync)
			c = fleet(t)
			c.images["bifrost-staging"], c.images["bifrost-prod"] = st.staging, st.prod
			if _, code := exec(t, c, "status"); code != tc.wantCode {
				t.Errorf("`bif status` exit = %d, want %d", code, tc.wantCode)
			}

			// Form 3: bif status -q. Only the interesting service prints, so
			// the whole-fleet quiet output is exactly the one-service output.
			c = fleet(t)
			c.images["bifrost-staging"], c.images["bifrost-prod"] = st.staging, st.prod
			out, code = exec(t, c, "status", "-q")
			if code != tc.wantCode {
				t.Errorf("`bif status -q` exit = %d, want %d", code, tc.wantCode)
			}
			if out != tc.wantQuiet {
				t.Errorf("`bif status -q` stdout = %q, want %q", out, tc.wantQuiet)
			}
		})
	}
}

// TestQuietFleetListsEveryOutOfSyncService: -q is a list, not a first hit, and
// the "*" suffix distinguishes mid-deploy from a service genuinely awaiting a
// promote.
func TestQuietFleetListsEveryOutOfSyncService(t *testing.T) {
	c := fleet(t)
	c.images["bifrost-prod"] = []string{image("bifrost", "def5678")}
	c.images["identity-prod"] = []string{image("identity", "def5678")}
	c.images["comms-staging"] = []string{image("comms", "abc1234"), image("comms", "def5678")}

	out, code := exec(t, c, "status", "-q")
	// Registry order is alphabetical, so comms precedes the other two.
	want := "bifrost\ncomms*\nidentity\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
}

// TestQuietFlagAcceptedAnywhere: ib.py filters -q/--quiet out of the argument
// list wherever they appear, and both spellings work.
func TestQuietFlagAcceptedAnywhere(t *testing.T) {
	for _, args := range [][]string{
		{"status", "bifrost", "-q"},
		{"status", "-q", "bifrost"},
		{"status", "bifrost", "--quiet"},
		{"status", "--quiet", "bifrost"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			c := fleet(t)
			c.images["bifrost-prod"] = []string{image("bifrost", "def5678")}
			out, code := exec(t, c, args...)
			if out != "bifrost\n" || code != 1 {
				t.Errorf("stdout = %q, exit = %d; want %q and 1", out, code, "bifrost\n")
			}
		})
	}
}

// ---- service names and the cluster reads --------------------------------

// TestServiceNamesComeFromTheRegistry pins the registry as the source of the
// fleet list — the thing that retires ib.py's hand-maintained SERVICES. The
// literal is ib.py's list as captured; if the registry gains a service, this
// fails and the port has a decision to make rather than a silent drift.
func TestServiceNamesComeFromTheRegistry(t *testing.T) {
	reg, err := registry.Load()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	ibPySERVICES := []string{
		"asset-manager", "bifrost", "comms", "footstrike-api",
		"footstrike-dashboard", "forecasting", "identity",
	}
	if got := reg.Names(); !slices.Equal(got, ibPySERVICES) {
		t.Errorf("registry.Names() = %v, ib.py SERVICES = %v", got, ibPySERVICES)
	}
}

// TestStatusReadsEveryRegistryNamespace: the whole-fleet form must visit both
// namespaces of every registered service, in registry order.
func TestStatusReadsEveryRegistryNamespace(t *testing.T) {
	c := fleet(t)
	if _, code := exec(t, c, "status"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	reg, err := registry.Load()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	var want []string
	for _, app := range reg.Names() {
		want = append(want, app+"-staging", app+"-prod")
	}
	if !slices.Equal(c.calls, want) {
		t.Errorf("namespaces read = %v, want %v", c.calls, want)
	}
}

// TestUnknownServiceRejectedBeforeConnecting: a typo'd name fails the same way
// whether or not there is a cluster to reach, and the message lists the
// registry's names.
func TestUnknownServiceRejectedBeforeConnecting(t *testing.T) {
	var stdout, stderr bytes.Buffer
	connected := false
	code := run(context.Background(), []string{"status", "bifrsot"}, strings.NewReader(""), &stdout, &stderr,
		func() (promoter, error) {
			connected = true
			return nil, errors.New("should not have been called")
		}, unreachableBuilds, noPreview)

	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if connected {
		t.Error("connected to the cluster before validating the service name")
	}
	want := "Unknown service: bifrsot\nKnown services: asset-manager, bifrost, comms, footstrike-api, footstrike-dashboard, forecasting, identity\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

// TestListPodsErrorReadsAsIndeterminate: ib.py's get_deployed_images swallows
// a failed kubectl into an empty set, so an unreachable namespace is
// indeterminate (exit 0) rather than a hard failure that would take a
// whole-fleet status down with it.
func TestListPodsErrorReadsAsIndeterminate(t *testing.T) {
	c := fleet(t)
	c.errs = map[string]error{"bifrost-prod": errors.New("namespaces \"bifrost-prod\" not found")}

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"status", "bifrost"}, strings.NewReader(""), &stdout, &stderr, c.connect, unreachableBuilds, noPreview)
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "prod:    (no pods found)") {
		t.Errorf("stdout does not report the missing pods:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "bifrost-prod") {
		t.Errorf("the list error was swallowed silently; stderr = %q", stderr.String())
	}
}

// TestConnectFailureExitsNonZero: no kubeconfig at all is a real failure, not
// an indeterminate status. ib.py would traceback here.
func TestConnectFailureExitsNonZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"status"}, strings.NewReader(""), &stdout, &stderr,
		func() (promoter, error) { return nil, errors.New("no kubeconfig") }, unreachableBuilds, noPreview)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no kubeconfig") {
		t.Errorf("stderr = %q, want the connection error", stderr.String())
	}
}

// TestJobPodsExcluded: a completed job pod keeps the image it ran with, which
// would read as a permanent mid-deploy. kube.Images drops them and this
// asserts `bif status` gets the benefit.
func TestJobPodsExcluded(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cluster := &jobCluster{}
	code := run(context.Background(), []string{"status", "bifrost", "-q"}, strings.NewReader(""), &stdout, &stderr,
		func() (promoter, error) { return cluster, nil }, unreachableBuilds, noPreview)
	if stdout.String() != "" || code != 0 {
		t.Errorf("stdout = %q, exit = %d; want in-sync (no output, 0) with the job pod ignored", stdout.String(), code)
	}
}

// jobCluster serves a namespace holding one live pod plus a leftover Job pod
// on an older image.
type jobCluster struct{}

// PatchAppImage exists to satisfy promoter; a status test that reached it
// would be a status test performing a write.
func (jobCluster) PatchAppImage(_ context.Context, app, env, image string) error {
	panic("status must not patch: " + app + "/" + env + " -> " + image)
}

func (jobCluster) ListPods(_ context.Context, ns string) ([]kube.PodInfo, error) {
	pods := []kube.PodInfo{{
		Namespace: ns, Name: ns + "-app", OwnerKind: "ReplicaSet", Phase: "Running",
		Containers: []kube.ContainerInfo{{Name: "app", Image: image("bifrost", "abc1234")}},
	}}
	if ns == "bifrost-staging" {
		pods = append(pods, kube.PodInfo{
			Namespace: ns, Name: ns + "-migrate", OwnerKind: "Job", OwnerName: "migrate", Phase: "Succeeded",
			Containers: []kube.ContainerInfo{{Name: "app", Image: image("bifrost", "0000000")}},
		})
	}
	return pods, nil
}

// ---- the build column ---------------------------------------------------

// TestBuildLabel is the wording, one state at a time. The shape is fixed —
// glyph, SHA, state, time — so a whole-fleet run has a column of SHAs to scan
// and one glyph per line to scan them by.
func TestBuildLabel(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	tests := []struct {
		name string
		cell buildCell
		want string
	}{
		{
			name: "in flight",
			cell: known(gcb.BuildStatus{Status: "WORKING", SHA: "0ab11f2", StartTime: ago(2 * time.Minute)}),
			want: "◌ 0ab11f2 building (2m)",
		},
		{
			// Queued builds have no start time yet, so there is no elapsed
			// time to print and inventing one ("0s") would read as stuck.
			name: "queued, not yet started",
			cell: known(gcb.BuildStatus{Status: "QUEUED", SHA: "0ab11f2"}),
			want: "◌ 0ab11f2 queued",
		},
		{
			name: "succeeded",
			cell: known(gcb.BuildStatus{Status: "SUCCESS", SHA: "0ab11f2", FinishTime: ago(3 * time.Hour)}),
			want: "✓ 0ab11f2 succeeded 3h ago",
		},
		{
			name: "failed",
			cell: known(gcb.BuildStatus{Status: "FAILURE", SHA: "0ab11f2", FinishTime: ago(12 * time.Minute)}),
			want: "✗ 0ab11f2 failed 12m ago",
		},
		{
			name: "timed out is a failure too",
			cell: known(gcb.BuildStatus{Status: "TIMEOUT", SHA: "0ab11f2", FinishTime: ago(30 * time.Hour)}),
			want: "✗ 0ab11f2 failed 1d ago",
		},
		{
			// CANCELLED is deliberate, so it is neither ✓ nor ✗ — see
			// gcb.BuildStatus.Failed, which excludes it for the same reason.
			name: "cancelled keeps its own name",
			cell: known(gcb.BuildStatus{Status: "CANCELLED", SHA: "0ab11f2", FinishTime: ago(2 * 24 * time.Hour)}),
			want: "· 0ab11f2 cancelled 2d ago",
		},
		{
			name: "just finished",
			cell: known(gcb.BuildStatus{Status: "SUCCESS", SHA: "0ab11f2", FinishTime: ago(5 * time.Second)}),
			want: "✓ 0ab11f2 succeeded just now",
		},
		{
			// Cloud Build's clock and this machine's are not the same clock;
			// a few seconds of skew must not print "-1s ago".
			name: "future finish time from clock skew",
			cell: known(gcb.BuildStatus{Status: "SUCCESS", SHA: "0ab11f2", FinishTime: now.Add(3 * time.Second)}),
			want: "✓ 0ab11f2 succeeded just now",
		},
		{
			name: "no recent build for this repo",
			cell: known(gcb.BuildStatus{}),
			want: "(no recent build)",
		},
		{
			// The distinction the whole buildCell type exists for: Cloud Build
			// saying "nothing" is not the same as not having asked it.
			name: "Cloud Build could not be read",
			cell: unknownBuild,
			want: "(build status unavailable)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildLabel(tc.cell, now); got != tc.want {
				t.Errorf("buildLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

// known wraps a build Cloud Build actually answered with.
func known(b gcb.BuildStatus) buildCell { return buildCell{status: b, known: true} }

// TestBuildColumnUsesTheRegistryRepoName is the trap this codebase has now
// walked into three times: LatestBuilds keys on Cloud Build's REPO_NAME
// substitution, which is the GitHub repo, NOT the registry key. asset-manager
// lives in asset_manager, so a lookup by service name finds nothing for it —
// and finds the right thing for the other six services, which is why it
// survives review.
//
// Both directions are asserted. Keying the fake by the repo name must produce
// the build; keying it by the service name must produce none, which is what
// fails if RepoFor is dropped from the lookup.
func TestBuildColumnUsesTheRegistryRepoName(t *testing.T) {
	reg, err := registry.Load()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	const svc = "asset-manager"
	repo := reg.RepoFor(svc)
	if repo == svc {
		t.Fatalf("%s's repo no longer differs from its registry key; this test covers nothing until it is pointed at a service where they differ", svc)
	}

	build := gcb.BuildStatus{Status: "SUCCESS", SHA: "0ab11f2", FinishTime: time.Now().Add(-3 * time.Hour)}

	t.Run("keyed by repo name", func(t *testing.T) {
		out, _, code := execBuilds(t, fleet(t), &fakeBuilds{byRepo: map[string]gcb.BuildStatus{repo: build}}, "status", svc)
		if want := buildLinePrefix + "✓ 0ab11f2 succeeded 3h ago\n"; !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
		if code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
	})

	t.Run("keyed by service name finds nothing", func(t *testing.T) {
		out, _, _ := execBuilds(t, fleet(t), &fakeBuilds{byRepo: map[string]gcb.BuildStatus{svc: build}}, "status", svc)
		if want := buildLinePrefix + "(no recent build)\n"; !strings.Contains(out, want) {
			t.Errorf("a build keyed by the service name must not be found (Cloud Build keys on %q):\n%s", repo, out)
		}
	})
}

// TestBuildColumnRendersInFlightAndCompleted drives the whole command for the
// two states an operator actually looks for, and pins the build line's place
// in the table: under prod, above the verdict.
func TestBuildColumnRendersInFlightAndCompleted(t *testing.T) {
	now := time.Now()
	builds := &fakeBuilds{byRepo: map[string]gcb.BuildStatus{
		"bifrost":  {Status: "WORKING", SHA: "0ab11f2", StartTime: now.Add(-2 * time.Minute)},
		"identity": {Status: "FAILURE", SHA: "def5678", FinishTime: now.Add(-12 * time.Minute)},
	}}

	out, _, code := execBuilds(t, fleet(t), builds, "status")
	if code != 0 {
		t.Fatalf("exit = %d, want 0: a build is not a verdict", code)
	}

	wantBlocks := []string{
		"  staging: abc1234\n  prod:    abc1234\n  build:   ◌ 0ab11f2 building (2m)\n\n✓ In sync\n",
		"  staging: abc1234\n  prod:    abc1234\n  build:   ✗ def5678 failed 12m ago\n\n✓ In sync\n",
		// A service Cloud Build returned nothing for still gets a cell.
		"  staging: abc1234\n  prod:    abc1234\n  build:   (no recent build)\n\n✓ In sync\n",
	}
	for _, want := range wantBlocks {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing:\n%s\ngot:\n%s", want, out)
		}
	}
}

// TestBuildLookupIsOneCallForTheWholeFleet: LatestBuilds answers for every
// repo at once, so the whole-fleet form must not turn into seven round trips
// to Google — the shape that makes a status page slow enough to stop using.
func TestBuildLookupIsOneCallForTheWholeFleet(t *testing.T) {
	reg, err := registry.Load()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if len(reg.Names()) < 2 {
		t.Fatal("a one-service registry cannot tell one call from one per service")
	}

	builds := &fakeBuilds{byRepo: map[string]gcb.BuildStatus{}}
	if _, _, code := execBuilds(t, fleet(t), builds, "status"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if dials, calls := builds.counts(); dials != 1 || calls != 1 {
		t.Errorf("dialled %d times and called LatestBuilds %d times for %d services; want 1 and 1", dials, calls, len(reg.Names()))
	}
}

// TestQuietModeMakesNoBuildLookup: `bif status -q` is a scriptable contract —
// bare app names, "*" for mid-deploy — so it gains no build text, and since
// there is nothing to render there is nothing to ask for either. That is not
// only tidiness: -q is what a script runs in a loop, and it stays exactly as
// cheap and as offline as it was before the build column existed.
func TestQuietModeMakesNoBuildLookup(t *testing.T) {
	for _, args := range [][]string{
		{"status", "-q"},
		{"status", "bifrost", "-q"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			c := fleet(t)
			c.images["bifrost-prod"] = []string{image("bifrost", "def5678")}
			builds := &fakeBuilds{byRepo: map[string]gcb.BuildStatus{
				"bifrost": {Status: "WORKING", SHA: "0ab11f2", StartTime: time.Now()},
			}}

			out, errOut, code := execBuilds(t, c, builds, args...)
			if out != "bifrost\n" || code != 1 {
				t.Errorf("stdout = %q, exit = %d; want %q and 1", out, code, "bifrost\n")
			}
			if errOut != "" {
				t.Errorf("stderr = %q, want nothing: -q reports no build state, so it has none to fail at", errOut)
			}
			if dials, calls := builds.counts(); dials != 0 || calls != 0 {
				t.Errorf("-q dialled Cloud Build %d times and called it %d times; want 0 and 0", dials, calls)
			}
		})
	}
}

// TestBuildColumnFailuresLeaveStatusIntact is the best-effort property, which
// is the whole reason this is safe to put on the incident path. `bif status`
// is what you run when something is already wrong; Cloud Build being slow,
// unreachable or unauthenticated must cost the build cell and nothing else —
// not the table, not the verdict, not the exit code, and not the wait.
func TestBuildColumnFailuresLeaveStatusIntact(t *testing.T) {
	tests := []struct {
		name    string
		builds  *fakeBuilds
		wantErr string // what stderr must name, so the degradation is diagnosable
	}{
		{
			name:    "no credentials",
			builds:  &fakeBuilds{dialErr: errors.New("could not find default credentials")},
			wantErr: "could not find default credentials",
		},
		{
			name:    "the API returns an error",
			builds:  &fakeBuilds{listErr: errors.New("403 caller lacks cloudbuild.builds.list")},
			wantErr: "cloudbuild.builds.list",
		},
		{
			// Unreachable rather than refused: the failure mode a timeout
			// exists for, and the one that would otherwise hang the command.
			name:    "Cloud Build never answers",
			builds:  &fakeBuilds{hang: true},
			wantErr: "context deadline exceeded",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Shortened so the hang case proves the bound without waiting for
			// it. The bound itself is what is under test; its value is a
			// judgement call about incident latency, not a contract.
			restore := buildLookupTimeout
			buildLookupTimeout = 20 * time.Millisecond
			t.Cleanup(func() { buildLookupTimeout = restore })

			c := fleet(t)
			c.images["bifrost-prod"] = []string{image("bifrost", "def5678")}

			done := make(chan struct{})
			var out, errOut string
			var code int
			go func() {
				defer close(done)
				out, errOut, code = execBuilds(t, c, tc.builds, "status", "bifrost")
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("`bif status` did not return: a build lookup must never be able to hang the command")
			}

			// The deployment status is untouched, down to the promote hint.
			for _, want := range []string{
				"  staging: abc1234\n",
				"  prod:    def5678\n",
				"\n✗ Out of sync\n",
				bifPromoteHint + "bifrost\n",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("output is missing %q:\n%s", want, out)
				}
			}
			// The column says it does not know, rather than saying nothing or
			// claiming there is no build.
			if want := buildLinePrefix + "(build status unavailable)\n"; !strings.Contains(out, want) {
				t.Errorf("output is missing %q:\n%s", want, out)
			}
			// Out of sync, because prod is behind staging — not because a
			// build failed, and not because Cloud Build did.
			if code != 1 {
				t.Errorf("exit = %d, want 1 (the cluster's verdict, unchanged)", code)
			}
			if !strings.Contains(errOut, tc.wantErr) {
				t.Errorf("stderr = %q, want it to name %q", errOut, tc.wantErr)
			}
		})
	}
}

// TestFailedBuildIsNotAVerdict: the build column is information. A service
// whose last build failed is still in sync if the images match — what is
// deployed is what "in sync" means, and a red build is a statement about the
// next deploy, not this one. Exiting 1 here would break every script that
// treats `bif status` as "is there anything to promote?".
func TestFailedBuildIsNotAVerdict(t *testing.T) {
	builds := &fakeBuilds{byRepo: map[string]gcb.BuildStatus{
		"bifrost": {Status: "FAILURE", SHA: "0ab11f2", FinishTime: time.Now().Add(-12 * time.Minute)},
	}}

	out, _, code := execBuilds(t, fleet(t), builds, "status", "bifrost")
	if code != 0 {
		t.Errorf("exit = %d, want 0: a failed build must not make an in-sync service out of sync", code)
	}
	if !strings.Contains(out, "\n✓ In sync\n") {
		t.Errorf("verdict changed with the build state:\n%s", out)
	}
	if !strings.Contains(out, buildLinePrefix+"✗ 0ab11f2 failed 12m ago\n") {
		t.Errorf("the failed build is not reported at all:\n%s", out)
	}
}

// ---- `bif status --attention` -------------------------------------------

// attentionFleet is `fleet` with a Cloud Build to talk to: every service in
// sync on abc1234, and only the repos named in builds have any build at all —
// so a test can make exactly one service noteworthy and assert on the whole of
// stdout rather than on a substring of it.
func attentionFleet(t *testing.T, builds map[string]gcb.BuildStatus) (*fakeCluster, *fakeBuilds) {
	t.Helper()
	if builds == nil {
		builds = map[string]gcb.BuildStatus{}
	}
	return fleet(t), &fakeBuilds{byRepo: builds}
}

// TestAttentionNamesEveryConditionAndItsReason drives the whole command once
// per qualifying condition, with the rest of the fleet quiet, and pins both the
// exit code and the exact line. A bare list of service names would not be
// actionable, so the reason text is the contract, not decoration.
func TestAttentionNamesEveryConditionAndItsReason(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name    string
		images  map[string][]string // overrides onto an otherwise in-sync fleet
		builds  map[string]gcb.BuildStatus
		want    string
		wantOne string // the condition this row exists for, named for the failure message
	}{
		{
			name:    "staging and prod differ",
			images:  map[string][]string{"bifrost-prod": {image("bifrost", "def5678")}},
			want:    "bifrost  staging and prod differ: staging abc1234, prod def5678 (bif promote bifrost)\n",
			wantOne: "condition 1: there is something to promote",
		},
		{
			name: "two images in one environment",
			images: map[string][]string{
				"bifrost-staging": {image("bifrost", "abc1234"), image("bifrost", "def5678")},
			},
			want:    "bifrost  deploy in progress: staging is running 2 images (abc1234, def5678)\n",
			wantOne: "condition 2: a deploy in progress, which -q marks with *",
		},
		{
			name: "a deploy in progress in prod, not staging",
			images: map[string][]string{
				"bifrost-prod": {image("bifrost", "abc1234"), image("bifrost", "def5678")},
			},
			want:    "bifrost  deploy in progress: prod is running 2 images (abc1234, def5678)\n",
			wantOne: "condition 2, on the environment -q's single * cannot distinguish",
		},
		{
			name:    "a build is running",
			builds:  map[string]gcb.BuildStatus{"bifrost": {Status: "WORKING", SHA: "0ab11f2", StartTime: now.Add(-2 * time.Minute)}},
			want:    "bifrost  build 0ab11f2 is building (2m)\n",
			wantOne: "condition 3: a build in flight",
		},
		{
			// QUEUED has no start time yet, so there is no elapsed time to show.
			name:    "a build is queued",
			builds:  map[string]gcb.BuildStatus{"bifrost": {Status: "QUEUED", SHA: "0ab11f2"}},
			want:    "bifrost  build 0ab11f2 is queued\n",
			wantOne: "condition 3, before the build starts executing",
		},
		{
			name:    "a successful build staging never picked up",
			builds:  map[string]gcb.BuildStatus{"bifrost": {Status: "SUCCESS", SHA: "0ab11f2", FinishTime: now.Add(-22 * time.Minute)}},
			want:    "bifrost  build 0ab11f2 succeeded 22m ago, staging still on abc1234\n",
			wantOne: "condition 4: the headline — a green build that never reached staging",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, builds := attentionFleet(t, tc.builds)
			for ns, imgs := range tc.images {
				c.images[ns] = imgs
			}

			out, _, code := execBuilds(t, c, builds, "status", "--attention")
			if out != tc.want {
				t.Errorf("stdout = %q, want %q (%s)", out, tc.want, tc.wantOne)
			}
			if code != 1 {
				t.Errorf("exit = %d, want 1: something qualified (%s)", code, tc.wantOne)
			}
		})
	}
}

// TestAttentionReportsAStalledSyncWithNoGracePeriod is the anti-threshold
// assertion. A build that went green seconds ago and has not reached staging
// yet is a state an operator explicitly wants to be able to SEE right after a
// push — so it is reported, not suppressed, and the elapsed time in the line is
// what tells a moment's lag apart from a sync that stopped days ago.
//
// If a grace period, threshold or "settle" delay is ever added to condition 4,
// the "just now" row here fails.
func TestAttentionReportsAStalledSyncWithNoGracePeriod(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name     string
		finished time.Duration
		want     string
	}{
		{"seconds after the push", 0, "bifrost  build 0ab11f2 succeeded just now, staging still on abc1234\n"},
		{"a minute after the push", 40 * time.Second, "bifrost  build 0ab11f2 succeeded just now, staging still on abc1234\n"},
		{"ten minutes later", 10 * time.Minute, "bifrost  build 0ab11f2 succeeded 10m ago, staging still on abc1234\n"},
		{"three days later", 3 * 24 * time.Hour, "bifrost  build 0ab11f2 succeeded 3d ago, staging still on abc1234\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, builds := attentionFleet(t, map[string]gcb.BuildStatus{
				"bifrost": {Status: "SUCCESS", SHA: "0ab11f2", FinishTime: now.Add(-tc.finished)},
			})
			out, _, code := execBuilds(t, c, builds, "status", "--attention")
			if out != tc.want {
				t.Errorf("stdout = %q, want %q", out, tc.want)
			}
			if code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
		})
	}
}

// TestAttentionQuietWhenStagingHasTheBuild is the other half of condition 4:
// the states it must NOT fire on. Firing on a healthy fleet is how this command
// becomes one you learn to ignore, and an ignored command detects nothing.
func TestAttentionQuietWhenStagingHasTheBuild(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name   string
		images map[string][]string
		build  gcb.BuildStatus
		// want is the whole of stdout: the all-clear, unless some OTHER
		// condition legitimately fires. What no row may produce is a stalled
		// sync.
		want     string
		wantCode int
		why      string
	}{
		{
			name:  "staging is running the build",
			build: gcb.BuildStatus{Status: "SUCCESS", SHA: "abc1234", FinishTime: now.Add(-3 * time.Hour)},
			why:   "the SHA reached staging; there is nothing stalled",
		},
		{
			name: "staging is mid-rollout onto the build",
			images: map[string][]string{
				"bifrost-staging": {image("bifrost", "abc1234"), image("bifrost", "0ab11f2")},
				"bifrost-prod":    {image("bifrost", "abc1234")},
			},
			build: gcb.BuildStatus{Status: "SUCCESS", SHA: "0ab11f2", FinishTime: now.Add(-3 * time.Hour)},
			// The rollout reports itself as a rollout — and as nothing else.
			want:     "bifrost  deploy in progress: staging is running 2 images (0ab11f2, abc1234)\n",
			wantCode: 1,
			why:      "one of staging's images is the build, so the SHA did reach staging; it is rolling, not stuck",
		},
		{
			name:  "the newest build failed",
			build: gcb.BuildStatus{Status: "FAILURE", SHA: "0ab11f2", FinishTime: now.Add(-3 * time.Hour)},
			why:   "a failed build was never going to reach staging; nothing is stuck",
		},
		{
			name:  "the tag carries a longer abbreviation of the same commit",
			build: gcb.BuildStatus{Status: "SUCCESS", SHA: "abc1234", FinishTime: now.Add(-3 * time.Hour)},
			images: map[string][]string{
				"bifrost-staging": {image("bifrost", "abc12345678")},
				"bifrost-prod":    {image("bifrost", "abc12345678")},
			},
			why: "SHORT_SHA and a tag's SHA are abbreviations of one hash, not necessarily to the same length",
		},
		{
			name:   "staging has no pods to compare",
			build:  gcb.BuildStatus{Status: "SUCCESS", SHA: "0ab11f2", FinishTime: now.Add(-3 * time.Hour)},
			images: map[string][]string{"bifrost-staging": nil},
			why:    "no pods is 'cannot tell', and `bif status` already calls that indeterminate",
		},
		{
			name:  "staging is on an unpinned tag",
			build: gcb.BuildStatus{Status: "SUCCESS", SHA: "0ab11f2", FinishTime: now.Add(-3 * time.Hour)},
			images: map[string][]string{
				"bifrost-staging": {image("bifrost", "latest")},
				"bifrost-prod":    {image("bifrost", "latest")},
			},
			why: "a mutable tag's content cannot be compared to a commit",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, builds := attentionFleet(t, map[string]gcb.BuildStatus{"bifrost": tc.build})
			for ns, imgs := range tc.images {
				c.images[ns] = imgs
			}
			want := tc.want
			if want == "" {
				want = attentionAllClear + "\n"
			}

			out, _, code := execBuilds(t, c, builds, "status", "--attention")
			if strings.Contains(out, "succeeded") {
				t.Errorf("a stalled sync was reported but %s:\n%s", tc.why, out)
			}
			if code != tc.wantCode {
				t.Errorf("exit = %d, want %d (%s):\n%s", code, tc.wantCode, tc.why, out)
			}
			if out != want {
				t.Errorf("stdout = %q, want %q (%s)", out, want, tc.why)
			}
		})
	}
}

// TestAttentionAllClearIsSaidOutLoud: the positive answer is a line, not
// silence, so the reader can always tell "I checked and it is fine" from "I
// could not check" and from "I did not run".
func TestAttentionAllClearIsSaidOutLoud(t *testing.T) {
	c, builds := attentionFleet(t, nil)
	out, errOut, code := execBuilds(t, c, builds, "status", "--attention")
	if out != attentionAllClear+"\n" {
		t.Errorf("stdout = %q, want %q", out, attentionAllClear+"\n")
	}
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want nothing", errOut)
	}
}

// TestAttentionBuildFailureIsNeverAnAllClear is the property that makes this
// command worth trusting. Two of the four checks are unknowable without build
// data, so a run that could not read Cloud Build must not print the all-clear
// and must not exit 0 — that would be claiming a clean fleet on a half-finished
// inspection, on the one command whose entire value is that its silence means
// something.
//
// The skip is asserted on STDOUT, not merely stderr: `bif status -a
// 2>/dev/null` must still be unreadable as "nothing to do".
func TestAttentionBuildFailureIsNeverAnAllClear(t *testing.T) {
	tests := []struct {
		name    string
		builds  *fakeBuilds
		wantErr string
	}{
		{"no credentials", &fakeBuilds{dialErr: errors.New("could not find default credentials")}, "could not find default credentials"},
		{"the API returns an error", &fakeBuilds{listErr: errors.New("403 caller lacks cloudbuild.builds.list")}, "cloudbuild.builds.list"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, errOut, code := execBuilds(t, fleet(t), tc.builds, "status", "--attention")

			if code != 1 {
				t.Errorf("exit = %d, want 1: two of four checks did not run, which is not 'fine'", code)
			}
			if strings.Contains(out, attentionAllClear) {
				t.Errorf("stdout claims an all-clear it could not have established:\n%s", out)
			}
			if !strings.Contains(out, attentionSkippedNote) {
				t.Errorf("stdout does not say the build checks were skipped:\n%s", out)
			}
			if !strings.Contains(errOut, tc.wantErr) {
				t.Errorf("stderr = %q, want it to name %q", errOut, tc.wantErr)
			}
		})
	}
}

// TestAttentionStillReportsClusterFindingsWhenBuildsAreUnreadable: the two
// cluster-only conditions do not need Cloud Build, so they must still be
// reported — degraded, but not blank. The skip note comes after them.
func TestAttentionStillReportsClusterFindingsWhenBuildsAreUnreadable(t *testing.T) {
	c := fleet(t)
	c.images["bifrost-prod"] = []string{image("bifrost", "def5678")}

	out, _, code := execBuilds(t, c, &fakeBuilds{dialErr: errors.New("no credentials")}, "status", "--attention")
	want := "bifrost  staging and prod differ: staging abc1234, prod def5678 (bif promote bifrost)\n" + attentionSkippedNote + "\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
}

// TestAttentionListsEveryReasonForOneService: a service can be noteworthy for
// more than one reason at once, and the stalled sync leads — it is the one that
// explains why the promote suggested under it would appear to do nothing.
func TestAttentionListsEveryReasonForOneService(t *testing.T) {
	c, builds := attentionFleet(t, map[string]gcb.BuildStatus{
		"bifrost": {Status: "SUCCESS", SHA: "0ab11f2", FinishTime: time.Now().Add(-3 * time.Hour)},
	})
	c.images["bifrost-prod"] = []string{image("bifrost", "def5678")}

	out, _, code := execBuilds(t, c, builds, "status", "--attention")
	want := "bifrost  build 0ab11f2 succeeded 3h ago, staging still on abc1234\n" +
		"bifrost  staging and prod differ: staging abc1234, prod def5678 (bif promote bifrost)\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
}

// TestAttentionListsEveryQualifyingService: this is a list, not a first hit,
// and every line names its own service so each one stands alone under grep.
func TestAttentionListsEveryQualifyingService(t *testing.T) {
	c, builds := attentionFleet(t, map[string]gcb.BuildStatus{
		"comms": {Status: "WORKING", SHA: "4f2a1b0", StartTime: time.Now().Add(-3 * time.Minute)},
	})
	c.images["bifrost-prod"] = []string{image("bifrost", "def5678")}
	c.images["identity-staging"] = []string{image("identity", "abc1234"), image("identity", "def5678")}

	out, _, code := execBuilds(t, c, builds, "status", "--attention")
	// Registry order is alphabetical, and the service column is padded to the
	// widest name that actually appears.
	want := "bifrost   staging and prod differ: staging abc1234, prod def5678 (bif promote bifrost)\n" +
		"comms     build 4f2a1b0 is building (3m)\n" +
		"identity  deploy in progress: staging is running 2 images (abc1234, def5678)\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
}

// TestAttentionUsesTheRegistryRepoName is the trap this codebase has now walked
// into three times, asserted for the new build-based checks: LatestBuilds keys
// on Cloud Build's REPO_NAME, which is the GitHub repo and NOT the registry
// key. asset-manager lives in asset_manager, so a lookup by service name finds
// nothing for it — and finds the right thing for the other six, which is
// exactly why it survives review.
//
// Both directions are asserted: keyed by the repo the stalled sync is found,
// keyed by the service name the command wrongly reports an all-clear.
func TestAttentionUsesTheRegistryRepoName(t *testing.T) {
	reg, err := registry.Load()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	const svc = "asset-manager"
	repo := reg.RepoFor(svc)
	if repo == svc {
		t.Fatalf("%s's repo no longer differs from its registry key; this test covers nothing until it is pointed at a service where they differ", svc)
	}
	build := gcb.BuildStatus{Status: "SUCCESS", SHA: "0ab11f2", FinishTime: time.Now().Add(-3 * time.Hour)}

	t.Run("keyed by repo name", func(t *testing.T) {
		c, builds := attentionFleet(t, map[string]gcb.BuildStatus{repo: build})
		out, _, code := execBuilds(t, c, builds, "status", "--attention")
		want := "asset-manager  build 0ab11f2 succeeded 3h ago, staging still on abc1234\n"
		if out != want {
			t.Errorf("stdout = %q, want %q", out, want)
		}
		if code != 1 {
			t.Errorf("exit = %d, want 1", code)
		}
	})

	t.Run("keyed by service name finds nothing", func(t *testing.T) {
		c, builds := attentionFleet(t, map[string]gcb.BuildStatus{svc: build})
		out, _, code := execBuilds(t, c, builds, "status", "--attention")
		if out != attentionAllClear+"\n" || code != 0 {
			t.Errorf("a build keyed by the service name must not be found (Cloud Build keys on %q); stdout = %q, exit = %d", repo, out, code)
		}
	})
}

// TestAttentionIsOneBuildCallForTheWholeFleet: --attention needs build data for
// every service, and LatestBuilds answers for every repo at once. Seven round
// trips to Google is the shape that makes a command slow enough to stop
// running from cron.
func TestAttentionIsOneBuildCallForTheWholeFleet(t *testing.T) {
	reg, err := registry.Load()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if len(reg.Names()) < 2 {
		t.Fatal("a one-service registry cannot tell one call from one per service")
	}

	c, builds := attentionFleet(t, nil)
	if _, _, code := execBuilds(t, c, builds, "status", "--attention"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if dials, calls := builds.counts(); dials != 1 || calls != 1 {
		t.Errorf("dialled %d times and called LatestBuilds %d times for %d services; want 1 and 1", dials, calls, len(reg.Names()))
	}
}

// TestAttentionFlagAcceptedAnywhere: -a and --attention are the same flag, and
// like -q they may appear before or after the service name.
func TestAttentionFlagAcceptedAnywhere(t *testing.T) {
	for _, args := range [][]string{
		{"status", "-a"},
		{"status", "--attention"},
		{"status", "bifrost", "-a"},
		{"status", "-a", "bifrost"},
		{"status", "bifrost", "--attention"},
		{"status", "--attention", "bifrost"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			c, builds := attentionFleet(t, nil)
			c.images["bifrost-prod"] = []string{image("bifrost", "def5678")}

			out, _, code := execBuilds(t, c, builds, args...)
			want := "bifrost  staging and prod differ: staging abc1234, prod def5678 (bif promote bifrost)\n"
			if out != want {
				t.Errorf("stdout = %q, want %q", out, want)
			}
			if code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
		})
	}
}

// TestAttentionWithAnAppIsScopedToThatApp: the combination is accepted, not
// rejected — the same four questions about one service is a real thing to want
// — and it must answer for that service ALONE, or it is just the fleet form
// with extra typing.
func TestAttentionWithAnAppIsScopedToThatApp(t *testing.T) {
	c, builds := attentionFleet(t, map[string]gcb.BuildStatus{
		"identity": {Status: "WORKING", SHA: "4f2a1b0", StartTime: time.Now().Add(-3 * time.Minute)},
	})
	c.images["bifrost-prod"] = []string{image("bifrost", "def5678")}

	// identity is noteworthy too, and must not appear.
	out, _, code := execBuilds(t, c, builds, "status", "bifrost", "-a")
	want := "bifrost  staging and prod differ: staging abc1234, prod def5678 (bif promote bifrost)\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}

	// And a quiet service scoped to itself is a real all-clear, exit 0, even
	// though the fleet as a whole is not clean.
	c2, builds2 := attentionFleet(t, nil)
	c2.images["bifrost-prod"] = []string{image("bifrost", "def5678")}
	out, _, code = execBuilds(t, c2, builds2, "status", "comms", "-a")
	if out != attentionAllClear+"\n" || code != 0 {
		t.Errorf("stdout = %q, exit = %d; want the all-clear and 0", out, code)
	}
}

// TestAttentionAndQuietAreRefusedTogether: they are two different answers to
// "what should stdout be", with no sensible winner. Resolving it either way
// would silently take something away — -q's offline guarantee, or the mode the
// operator actually asked for — so the combination is an error, and it is one
// diagnosed on argv alone, before anything is dialled.
func TestAttentionAndQuietAreRefusedTogether(t *testing.T) {
	for _, args := range [][]string{
		{"status", "-q", "-a"},
		{"status", "-a", "-q"},
		{"status", "bifrost", "--quiet", "--attention"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			c, builds := attentionFleet(t, nil)
			out, errOut, code := execBuilds(t, c, builds, args...)
			if code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			if out != "" {
				t.Errorf("stdout = %q, want nothing", out)
			}
			if !strings.Contains(errOut, "-q") || !strings.Contains(errOut, "--attention") {
				t.Errorf("stderr = %q, want it to name both flags", errOut)
			}
			if dials, calls := builds.counts(); dials != 0 || calls != 0 {
				t.Errorf("dialled Cloud Build %d times and called it %d times before rejecting the invocation; want 0 and 0", dials, calls)
			}
			if len(c.calls) != 0 {
				t.Errorf("read %v before rejecting the invocation; want nothing", c.calls)
			}
		})
	}
}

// ---- `bif status <app> <app> ...` ---------------------------------------

// distinctFleet is fleet(t) with the named services made out of sync on tags
// that are unique to each of them.
//
// The distinctness is the point, and it is this package's recurring trap: a
// fixture where two services carry the same tags cannot tell "both were
// rendered" from "one was rendered twice", and it cannot tell which one a
// `strings.Contains` over the whole output matched. Every service here gets its
// own staging SHA and its own prod SHA, so every assertion below names a string
// that can only have come from the service it is about.
func distinctFleet(t *testing.T, staging, prod map[string]string) *fakeCluster {
	t.Helper()
	c := fleet(t)
	for app, tag := range staging {
		c.images[app+"-staging"] = []string{image(app, tag)}
	}
	for app, tag := range prod {
		c.images[app+"-prod"] = []string{image(app, tag)}
	}
	return c
}

// sectionOrder returns the services whose `bif status` table appeared in out,
// in the order they appeared. It matches the header line specifically rather
// than searching for the name anywhere, because a service's name also shows up
// in another service's output — the promote hint under a neighbouring "Out of
// sync" says `bif promote <name>` — and a whole-output scan would count that.
func sectionOrder(out string) []string {
	var apps []string
	for _, line := range strings.Split(out, "\n") {
		if app, ok := strings.CutSuffix(line, " deployment status:"); ok {
			apps = append(apps, app)
		}
	}
	return apps
}

// TestStatusAcceptsSeveralServices is the bug this fixes, stated as the
// behaviour that replaces it: before, `bif status bifrost comms` printed
// bifrost and SILENTLY DROPPED comms. Every named service must be rendered,
// with its own images.
func TestStatusAcceptsSeveralServices(t *testing.T) {
	c := distinctFleet(t,
		map[string]string{"bifrost": "aaa1111", "comms": "bbb2222", "identity": "ccc3333"},
		map[string]string{"bifrost": "ddd4444", "comms": "eee5555", "identity": "ccc3333"})

	out, code := exec(t, c, "status", "bifrost", "comms", "identity")

	if got := sectionOrder(out); !slices.Equal(got, []string{"bifrost", "comms", "identity"}) {
		t.Errorf("rendered %v, want all three named services", got)
	}
	// Each service's own tags, not just "three sections appeared".
	for _, want := range []string{
		"  staging: aaa1111", "  prod:    ddd4444", // bifrost
		"  staging: bbb2222", "  prod:    eee5555", // comms
		"  staging: ccc3333", // identity, in sync
	} {
		if !slices.Contains(strings.Split(out, "\n"), want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	// One out-of-sync service anywhere in the list is enough to exit 1.
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	// Services that were not named must not be read at all — narrowing has to
	// still narrow.
	for _, ns := range c.calls {
		if strings.HasPrefix(ns, "forecasting-") || strings.HasPrefix(ns, "asset-manager-") {
			t.Errorf("read %s, which was not named", ns)
		}
	}
}

// TestStatusFollowsTheOrderGiven: the operator typed a sequence and reads the
// output back in it. Registry order is what `bif status` with NO names uses,
// and this is the case that tells the two apart — the names here are in reverse
// registry order, so an implementation that sorted or re-registry-ordered them
// fails.
func TestStatusFollowsTheOrderGiven(t *testing.T) {
	c := fleet(t)
	out, code := exec(t, c, "status", "identity", "comms", "bifrost")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	want := []string{"identity", "comms", "bifrost"}
	if got := sectionOrder(out); !slices.Equal(got, want) {
		t.Errorf("rendered %v, want %v (the order given, not registry order)", got, want)
	}
}

// TestStatusDedupesRepeatedNames: a name given twice is one service. Rendering
// it twice would be noise, and reading its namespaces twice would be two
// cluster round-trips for one answer.
func TestStatusDedupesRepeatedNames(t *testing.T) {
	c := fleet(t)
	out, code := exec(t, c, "status", "bifrost", "comms", "bifrost")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	want := []string{"bifrost", "comms"}
	if got := sectionOrder(out); !slices.Equal(got, want) {
		t.Errorf("rendered %v, want %v (the repeat collapses onto its first position)", got, want)
	}
	if got := len(c.calls); got != 4 {
		t.Errorf("read %d namespaces (%v), want 4: two services, two namespaces each", got, c.calls)
	}
}

// TestStatusValidatesEveryNameBeforeReadingTheCluster: a typo in the THIRD
// argument must fail before the FIRST cluster read. Otherwise `bif status`
// spends two round-trips on bifrost and comms before telling the operator their
// argv was wrong — and, on the promote side, the same shape of mistake would
// have written to prod first.
func TestStatusValidatesEveryNameBeforeReadingTheCluster(t *testing.T) {
	c := fleet(t)
	out, code := exec(t, c, "status", "bifrost", "comms", "identtiy")

	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if len(c.calls) != 0 {
		t.Errorf("read %v before rejecting the bad name; want nothing", c.calls)
	}
	if !strings.HasPrefix(out, "Unknown service: identtiy\n") {
		t.Errorf("stdout = %q, want it to name the bad argument first", out)
	}
	if strings.Contains(out, "deployment status:") {
		t.Errorf("rendered a service despite the bad name:\n%s", out)
	}
}

// TestStatusQuietListsEveryNamedService: -q needed no change, and this is what
// "no change" has to mean — it narrows to the names given and lists exactly the
// out-of-sync ones among them, in the order given.
func TestStatusQuietListsEveryNamedService(t *testing.T) {
	c := distinctFleet(t,
		map[string]string{"comms": "bbb2222", "identity": "ccc3333"},
		map[string]string{"comms": "eee5555", "identity": "fff6666"})

	out, code := exec(t, c, "status", "identity", "bifrost", "comms", "-q")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	// bifrost is in sync, so it prints nothing; the other two print in the
	// order given, which is not registry order.
	if want := "identity\ncomms\n"; out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
}

// TestStatusAttentionCoversEveryNamedService: --attention needed no change
// either. Both named services qualify, for reasons unique to each.
func TestStatusAttentionCoversEveryNamedService(t *testing.T) {
	c, builds := attentionFleet(t, nil)
	c.images["bifrost-prod"] = []string{image("bifrost", "ddd4444")}
	c.images["comms-prod"] = []string{image("comms", "eee5555")}

	out, _, code := execBuilds(t, c, builds, "status", "bifrost", "comms", "-a")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	want := "bifrost  staging and prod differ: staging abc1234, prod ddd4444 (bif promote bifrost)\n" +
		"comms    staging and prod differ: staging abc1234, prod eee5555 (bif promote comms)\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
}
