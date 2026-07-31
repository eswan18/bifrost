package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

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
	return f.patchErr
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
	code := run(context.Background(), args, strings.NewReader(stdin), &stdout, &stderr, cluster.connect, noPreview)
	return stdout.String(), code
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
// not reproduce the oracle's bytes: ib.py names itself, and this names the
// binary the reader is already running, because as of `bif promote` that hint
// is true and `ib promote` is the one that is going away. Nothing else about
// the line moves — same indent, same position, same following line.
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

// TestStatusRenderingMatchesOracle is the golden comparison: for every cluster
// state ib.py was captured against, `bif status` must print exactly what ib.py
// printed and return the same verdict — byte for byte, including the table
// alignment, the blank lines and the ⚠/✓/✗ glyphs. The single exception is the
// promote hint; see retargetPromoteHint.
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
	}{
		{
			name:        "verbose",
			quiet:       false,
			stdout:      func(r statusRow) string { return retargetPromoteHint(r.VerboseStdout) },
			ret:         func(r statusRow) *bool { return r.VerboseReturn },
			divergences: verboseDivergences,
		},
		{
			name:        "quiet",
			quiet:       true,
			stdout:      func(r statusRow) string { return r.QuietStdout },
			ret:         func(r statusRow) *bool { return r.QuietReturn },
			divergences: quietDivergences,
		},
	} {
		t.Run(mode.name, func(t *testing.T) {
			for _, r := range rows {
				t.Run(r.key(), func(t *testing.T) {
					var buf bytes.Buffer
					got := statusOne(&buf, r.App, r.StagingImages, r.ProdImages, mode.quiet)

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
						if buf.String() != d.goStdout {
							t.Fatalf("known divergence changed:\n got %q\nwant %q\n(%s)", buf.String(), d.goStdout, d.why)
						}
						if got != d.goVerdict {
							t.Fatalf("known divergence changed: verdict %v, was %v (%s)", got, d.goVerdict, d.why)
						}
						return
					}

					if buf.String() != wantStdout {
						t.Errorf("stdout mismatch\n got %q\nib.py %q", buf.String(), wantStdout)
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
			if v := statusOne(&buf, "bifrost", tc.staging, tc.prod, false); v != indeterminate {
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
// now that `bif promote` is the implementation, pointing at `ib` would send an
// operator to the CLI this one is replacing.
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
// `ib status`. The mapping is ib.py's and does not vary with -q: only a
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

			// Form 2: ib status <app>
			c := fleet(t)
			c.images["bifrost-staging"], c.images["bifrost-prod"] = st.staging, st.prod
			if _, code := exec(t, c, "status", "bifrost"); code != tc.wantCode {
				t.Errorf("`ib status bifrost` exit = %d, want %d", code, tc.wantCode)
			}

			// Form 4: ib status <app> -q
			c = fleet(t)
			c.images["bifrost-staging"], c.images["bifrost-prod"] = st.staging, st.prod
			out, code := exec(t, c, "status", "bifrost", "-q")
			if code != tc.wantCode {
				t.Errorf("`ib status bifrost -q` exit = %d, want %d", code, tc.wantCode)
			}
			if out != tc.wantQuiet {
				t.Errorf("`ib status bifrost -q` stdout = %q, want %q", out, tc.wantQuiet)
			}

			// Form 1: ib status (whole fleet; every other service in sync)
			c = fleet(t)
			c.images["bifrost-staging"], c.images["bifrost-prod"] = st.staging, st.prod
			if _, code := exec(t, c, "status"); code != tc.wantCode {
				t.Errorf("`ib status` exit = %d, want %d", code, tc.wantCode)
			}

			// Form 3: ib status -q. Only the interesting service prints, so
			// the whole-fleet quiet output is exactly the one-service output.
			c = fleet(t)
			c.images["bifrost-staging"], c.images["bifrost-prod"] = st.staging, st.prod
			out, code = exec(t, c, "status", "-q")
			if code != tc.wantCode {
				t.Errorf("`ib status -q` exit = %d, want %d", code, tc.wantCode)
			}
			if out != tc.wantQuiet {
				t.Errorf("`ib status -q` stdout = %q, want %q", out, tc.wantQuiet)
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
		}, noPreview)

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
	code := run(context.Background(), []string{"status", "bifrost"}, strings.NewReader(""), &stdout, &stderr, c.connect, noPreview)
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
		func() (promoter, error) { return nil, errors.New("no kubeconfig") }, noPreview)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no kubeconfig") {
		t.Errorf("stderr = %q, want the connection error", stderr.String())
	}
}

// TestJobPodsExcluded: a completed job pod keeps the image it ran with, which
// would read as a permanent mid-deploy. kube.Images drops them and this
// asserts `ib status` gets the benefit.
func TestJobPodsExcluded(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cluster := &jobCluster{}
	code := run(context.Background(), []string{"status", "bifrost", "-q"}, strings.NewReader(""), &stdout, &stderr,
		func() (promoter, error) { return cluster, nil }, noPreview)
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
