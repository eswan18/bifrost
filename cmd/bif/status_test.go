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
	code := run(context.Background(), args, strings.NewReader(stdin), &stdout, &stderr, cluster.connect, unreachableBuilds, unreachableDigests, noPreview)
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
	code = run(context.Background(), args, strings.NewReader(""), &out, &errb, cluster.connect, builds.dial, unreachableDigests, noPreview)
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

// quietDivergences: the only fixture rows where `bif status` deliberately
// disagrees with ib.py.
//
// ib.py's status() called an unparseable PROD tag indeterminate, because it
// required both SHAs before it would compare them. promote.StatusOf requires
// only the staging SHA — the artifact being promoted — and calls the same
// state out of sync. ib.py's own promote() accepted exactly this state and
// promoted from it: the prod Application lost its image pin and fell back to
// the mutable `latest`/`prod` tag, and calling it "unknown" is what left
// bifrost refusing to promote with no way out (bifrost#30).
var quietDivergences = map[string]divergence{
	"bifrost staging=[us-central1-docker.pkg.dev/ethans-services/containers/bifrost:abc1234] prod=[us-central1-docker.pkg.dev/ethans-services/containers/bifrost:latest]": {
		pythonStdout: "", pythonVerdict: indeterminate,
		goStdout: "bifrost\n", goVerdict: outOfSync,
		why: "bifrost#30: the list prints the app and exits 1 where ib.py printed nothing and exited 0",
	},
	"footstrike-dashboard staging=[us-central1-docker.pkg.dev/ethans-services/containers/footstrike-dashboard:abc1234-staging] prod=[us-central1-docker.pkg.dev/ethans-services/containers/footstrike-dashboard:prod]": {
		pythonStdout: "", pythonVerdict: indeterminate,
		goStdout: "footstrike-dashboard\n", goVerdict: outOfSync,
		why: "same, on the {sha}-{env} tagging scheme",
	},
}

// unknownBuild is the build cell for tests that are not about the build
// column: Cloud Build was not reachable, which is the state every one of them
// runs its cluster assertions in.
var unknownBuild = buildCell{}

// buildLinePrefix is the build cell's label as -vv renders it — the padding
// included, since lining up under "promote:" is the point of it.
const buildLinePrefix = "  build:   "

// statusOf builds one service's status straight from image lists, the shape
// the oracle fixtures record.
func statusOf(app string, staging, prod []string, build buildCell) serviceStatus {
	return newServiceStatus(app, envState{images: staging}, envState{images: prod}, build)
}

// TestDefaultListMatchesOracle runs the default output — the list -q has
// always printed, and still a scriptable contract — over every cluster state
// ib.py was captured against, and checks the verdict for each. The -v and -vv
// renderings are bif's own and are no longer compared to ib.py's; the fixtures
// stay because they are the widest matrix of cluster states this package has.
func TestDefaultListMatchesOracle(t *testing.T) {
	rows, err := oracle.Load[statusRow]("status.json")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	for _, r := range rows {
		t.Run(r.key(), func(t *testing.T) {
			s := statusOf(r.App, r.StagingImages, r.ProdImages, unknownBuild)
			var buf bytes.Buffer
			writeList(&buf, []serviceStatus{s})
			got, gotVerdict := buf.String(), s.verdict()

			wantStdout, wantVerdict := r.QuietStdout, verdictFromOracle(r.QuietReturn)
			if d, known := quietDivergences[r.key()]; known {
				// Guard the oracle first: if the captured output moved, the
				// divergence note describes a Python that no longer exists.
				if wantStdout != d.pythonStdout || wantVerdict != d.pythonVerdict {
					t.Fatalf("oracle changed: ib.py now prints %q / returns %v; divergence table records %q / %v",
						wantStdout, wantVerdict, d.pythonStdout, d.pythonVerdict)
				}
				wantStdout, wantVerdict = d.goStdout, d.goVerdict
			}
			if got != wantStdout {
				t.Errorf("stdout = %q, want %q", got, wantStdout)
			}
			if gotVerdict != wantVerdict {
				t.Errorf("verdict = %v, want %v", gotVerdict, wantVerdict)
			}
		})
	}
}

// TestMidDeployKeepsTheOtherEnvironmentsTag: promote.Status zeroes its tags
// whenever either side is mid-deploy or empty, so a table rendered from Status
// would have a hole in it. It renders from the image lists instead, and this
// says so: prod's tag survives a staging rollout, and staging's survives a
// prod one.
func TestMidDeployKeepsTheOtherEnvironmentsTag(t *testing.T) {
	tests := []struct {
		name    string
		staging []string
		prod    []string
		wantRow []string // SERVICE, STATE, STAGING, PROD
	}{
		{
			name:    "staging rolling, prod tag still shown",
			staging: []string{image("bifrost", "abc1234"), image("bifrost", "def5678")},
			prod:    []string{image("bifrost", "abc1234")},
			wantRow: []string{"bifrost", "⚠ deploying", "abc1234, def5678", "abc1234"},
		},
		{
			name:    "prod rolling, staging tag still shown",
			staging: []string{image("bifrost", "abc1234")},
			prod:    []string{image("bifrost", "abc1234"), image("bifrost", "def5678")},
			wantRow: []string{"bifrost", "⚠ deploying", "abc1234", "abc1234, def5678"},
		},
		{
			name:    "no staging pods, prod tag still shown",
			staging: nil,
			prod:    []string{image("bifrost", "abc1234")},
			wantRow: []string{"bifrost", "? unknown", "(no pods)", "abc1234"},
		},
		{
			name:    "no prod pods, staging tag still shown",
			staging: []string{image("bifrost", "abc1234")},
			prod:    nil,
			wantRow: []string{"bifrost", "? unknown", "abc1234", "(no pods)"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := statusOf("bifrost", tc.staging, tc.prod, unknownBuild)
			if v := s.verdict(); v != indeterminate {
				t.Fatalf("verdict = %v, want indeterminate", v)
			}
			var buf bytes.Buffer
			writeTable(&buf, []serviceStatus{s}, time.Now())
			if got := tableCells(t, buf.String(), "bifrost")[:4]; !slices.Equal(got, tc.wantRow) {
				t.Errorf("row = %q, want %q\n%s", got, tc.wantRow, buf.String())
			}
		})
	}
}

// tableCells splits app's row of a -v table into its cells, by the column
// starts the header row sets. Splitting on runs of spaces would not do: a
// mid-deploy cell is "abc1234, def5678" and a state is "✓ in sync".
func tableCells(t *testing.T, out, app string) []string {
	t.Helper()
	lines := strings.Split(out, "\n")
	header := []rune(lines[0])
	var starts []int
	for i, r := range header {
		if r != ' ' && (i == 0 || header[i-1] == ' ') {
			starts = append(starts, i)
		}
	}
	for _, line := range lines[1:] {
		row := []rune(line)
		if !strings.HasPrefix(line, app+" ") {
			continue
		}
		cells := make([]string, len(starts))
		for i, st := range starts {
			end := len(row)
			if i+1 < len(starts) {
				end = min(starts[i+1], len(row))
			}
			if st < len(row) {
				cells[i] = strings.TrimSpace(string(row[st:end]))
			}
		}
		return cells
	}
	t.Fatalf("no row for %s in:\n%s", app, out)
	return nil
}

// TestUnparseableProdTagIsOutOfSync is DIVERGENCE #1 stated as a contract
// rather than as a fixture exception: an unpinned prod is promotable, so
// `bif status` names it and exits 1. ib.py exited 0 here. See
// quietDivergences for why Go is the correct side.
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

			out, code := exec(t, cluster, "status", tc.app)
			if out != tc.app+"\n" {
				t.Errorf("stdout = %q, want %q (ib.py printed nothing here)", out, tc.app+"\n")
			}
			if code != 1 {
				t.Errorf("exit = %d, want 1 (ib.py exited 0 here)", code)
			}

			out, code = exec(t, cluster, "status", tc.app, "-vv")
			if want := "  promote: bif promote " + tc.app + " (deploys " + tc.wantPromo + " to prod)\n"; !strings.Contains(out, want) {
				t.Errorf("-vv output does not offer the promotion %q:\n%s", want, out)
			}
			if code != 1 {
				t.Errorf("-vv exit = %d, want 1", code)
			}
		})
	}
}

// TestPromoteLineNamesEveryOutOfSyncService: -v ends with ONE promote command
// covering everything out of sync, in the order the table listed them —
// promote takes several services and shows one plan, so that line is the whole
// next step. A mid-deploy service is not on it: promote would refuse a staging
// rollout, and a prod one is not something to promote over.
func TestPromoteLineNamesEveryOutOfSyncService(t *testing.T) {
	c := fleet(t)
	c.images["bifrost-prod"] = []string{image("bifrost", "def5678")}
	c.images["identity-prod"] = []string{image("identity", "def5678")}
	c.images["comms-staging"] = []string{image("comms", "abc1234"), image("comms", "def5678")}

	out, code := exec(t, c, "status", "-v")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.HasSuffix(out, "\nTo promote: bif promote bifrost identity\n") {
		t.Errorf("output does not end with one promote line for bifrost and identity:\n%s", out)
	}

	// Nothing out of sync, no line at all — not an empty `bif promote`.
	out, _ = exec(t, fleet(t), "status", "-v")
	if strings.Contains(out, "To promote") {
		t.Errorf("an in-sync fleet still printed a promote line:\n%s", out)
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

// withRepoOverride runs the rest of the test against the embedded registry
// with svc's GitHub repo renamed (to svc+"-repo"), and returns that repo. The
// repo-name trap (TestBuildColumnUsesTheRegistryRepoName and
// TestAttentionUsesTheRegistryRepoName) only bites a service whose repo
// differs from its registry key, and since asset-manager became sensibility
// no real one does.
func withRepoOverride(t *testing.T, svc string) string {
	t.Helper()
	reg, err := registry.Load()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	entry, ok := reg[svc]
	if !ok {
		t.Fatalf("%s is not in the registry", svc)
	}
	entry.Repo = svc + "-repo"
	reg[svc] = entry
	prev := loadEmbeddedRegistry
	loadEmbeddedRegistry = func() (registry.Registry, error) { return reg, nil }
	t.Cleanup(func() { loadEmbeddedRegistry = prev })
	return entry.Repo
}

// TestExitCodesAllForms is the exit-status contract for every form of
// `bif status`: only a definite out-of-sync exits 1, and how much is printed
// never changes that — the default, -q, -v and -vv all agree, for one service
// and for the whole fleet.
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
		// wantCode is the same for every form — that is the property.
		wantCode int
		// wantList is what the default and -q forms print. Only bifrost is
		// interesting, so the whole-fleet list is exactly the one-service one.
		wantList string
	}{
		{"in sync", 0, ""},
		{"out of sync", 1, "bifrost\n"},
		{"mid deploy", 0, "bifrost*\n"},
		{"no pods", 0, ""},
		{"unreadable staging", 0, ""},
	}

	forms := []struct {
		args []string
		list bool // prints the list, so its stdout is checked too
	}{
		{[]string{"status"}, true},
		{[]string{"status", "bifrost"}, true},
		{[]string{"status", "-q"}, true},
		{[]string{"status", "bifrost", "-q"}, true},
		{[]string{"status", "-v"}, false},
		{[]string{"status", "bifrost", "-v"}, false},
		{[]string{"status", "-vv"}, false},
		{[]string{"status", "bifrost", "-vv"}, false},
	}

	for _, tc := range tests {
		t.Run(tc.state, func(t *testing.T) {
			st := states[tc.state]
			for _, form := range forms {
				c := fleet(t)
				c.images["bifrost-staging"], c.images["bifrost-prod"] = st.staging, st.prod
				out, code := exec(t, c, form.args...)
				cmd := "`bif " + strings.Join(form.args, " ") + "`"
				if code != tc.wantCode {
					t.Errorf("%s exit = %d, want %d", cmd, code, tc.wantCode)
				}
				if form.list && out != tc.wantList {
					t.Errorf("%s stdout = %q, want %q", cmd, out, tc.wantList)
				}
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

// TestVerbosityFlagsAcceptedAnywhere: -v, --verbose and -vv are filtered out
// wherever they appear, as -q is, and repeating -v is -vv.
func TestVerbosityFlagsAcceptedAnywhere(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want verbosity
	}{
		{[]string{"bifrost", "-v"}, tableOutput},
		{[]string{"-v", "bifrost"}, tableOutput},
		{[]string{"--verbose", "bifrost"}, tableOutput},
		{[]string{"bifrost", "-vv"}, detailOutput},
		{[]string{"-v", "bifrost", "-v"}, detailOutput},
		{[]string{"-vv", "-v", "bifrost"}, detailOutput},
		{[]string{"bifrost"}, listOutput},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			rest, got := takeVerbosity(tc.args)
			if got != tc.want || !slices.Equal(rest, []string{"bifrost"}) {
				t.Errorf("takeVerbosity = %v, %d; want [bifrost], %d", rest, got, tc.want)
			}
		})
	}
}

// TestDetailShowsThePods is what -vv adds over -v: the pods behind each tag,
// with what makes a rollout stuck rather than landing — readiness, restarts,
// a waiting reason — plus the image repository, which is not always the
// service's name, and the build's duration and log.
func TestDetailShowsThePods(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	staging := envOf([]kube.PodInfo{
		{Name: "bifrost-old-aaaaa", OwnerKind: "ReplicaSet", Phase: "Running",
			Containers: []kube.ContainerInfo{{Image: image("fitness-api", "abc1234"), Ready: true}}},
		{Name: "bifrost-new-bbbbb", OwnerKind: "ReplicaSet", Phase: "Running",
			Containers: []kube.ContainerInfo{{Image: image("fitness-api", "def5678"), RestartCount: 4, WaitingReason: "CrashLoopBackOff"}}},
		// A finished migration is not serving anything and must not be listed.
		{Name: "bifrost-migrate-ccccc", OwnerKind: "Job", Phase: "Succeeded",
			Containers: []kube.ContainerInfo{{Image: image("fitness-api", "0000000")}}},
	})
	prod := envOf([]kube.PodInfo{
		{Name: "bifrost-prod-ddddd", OwnerKind: "ReplicaSet", Phase: "Running",
			Containers: []kube.ContainerInfo{{Image: image("fitness-api", "abc1234"), Ready: true}}},
	})
	build := known(gcb.BuildStatus{
		Status: "SUCCESS", SHA: "def5678", LogURL: "https://console.cloud.google.com/cloud-build/builds/x",
		StartTime: now.Add(-3*time.Hour - 2*time.Minute), FinishTime: now.Add(-3 * time.Hour),
	})

	var buf bytes.Buffer
	writeDetail(&buf, []serviceStatus{newServiceStatus("bifrost", staging, prod, build)}, now)

	want := "bifrost  ⚠ deploying: staging is running 2 images\n" +
		"  staging: abc1234, def5678\n" +
		"    bifrost-old-aaaaa  abc1234  Running  1/1 ready\n" +
		"    bifrost-new-bbbbb  def5678  Running  0/1 ready  4 restarts  CrashLoopBackOff\n" +
		"  prod:    abc1234\n" +
		"    bifrost-prod-ddddd  abc1234  Running  1/1 ready\n" +
		"  image:   " + testRegistry + "/fitness-api\n" +
		"  build:   ✓ def5678 succeeded 3h ago, took 2m\n" +
		"           https://console.cloud.google.com/cloud-build/builds/x\n"
	if got := buf.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// ---- service names and the cluster reads --------------------------------

// TestServiceNamesComeFromTheRegistry pins the registry as the source of the
// fleet list — the thing that retires ib.py's hand-maintained SERVICES. The
// literal is ib.py's list as captured, with asset-manager since renamed
// sensibility (Oct 2026); if the registry gains a service, this
// fails and the port has a decision to make rather than a silent drift.
func TestServiceNamesComeFromTheRegistry(t *testing.T) {
	reg, err := registry.Load()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	ibPySERVICES := []string{
		"bifrost", "comms", "footstrike-api",
		"footstrike-dashboard", "haruspex", "identity", "sensibility",
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
		}, unreachableBuilds, unreachableDigests, noPreview)

	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if connected {
		t.Error("connected to the cluster before validating the service name")
	}
	want := "Unknown service: bifrsot\nKnown services: bifrost, comms, footstrike-api, footstrike-dashboard, haruspex, identity, sensibility\n"
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
	code := run(context.Background(), []string{"status", "bifrost", "-vv"}, strings.NewReader(""), &stdout, &stderr, c.connect, unreachableBuilds, unreachableDigests, noPreview)
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "bifrost  ? unknown: no pods in prod\n") || !strings.Contains(stdout.String(), "  prod:    (no pods)\n") {
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
		func() (promoter, error) { return nil, errors.New("no kubeconfig") }, unreachableBuilds, unreachableDigests, noPreview)
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
		func() (promoter, error) { return cluster, nil }, unreachableBuilds, unreachableDigests, noPreview)
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
// substitution, which is the GitHub repo, NOT the registry key. When a
// service's repo differs from its key, a lookup by service name finds nothing
// for it, and finds the right thing for every other service, which is why it
// survives review. No real service differs since asset-manager became
// sensibility (Oct 2026), so withRepoOverride gives one a different repo.
//
// Both directions are asserted. Keying the fake by the repo name must produce
// the build; keying it by the service name must produce none, which is what
// fails if RepoFor is dropped from the lookup.
func TestBuildColumnUsesTheRegistryRepoName(t *testing.T) {
	const svc = "sensibility"
	repo := withRepoOverride(t, svc)

	build := gcb.BuildStatus{Status: "SUCCESS", SHA: "0ab11f2", FinishTime: time.Now().Add(-3 * time.Hour)}

	t.Run("keyed by repo name", func(t *testing.T) {
		out, _, code := execBuilds(t, fleet(t), &fakeBuilds{byRepo: map[string]gcb.BuildStatus{repo: build}}, "status", svc, "-vv")
		if want := buildLinePrefix + "✓ 0ab11f2 succeeded 3h ago\n"; !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
		if code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
	})

	t.Run("keyed by service name finds nothing", func(t *testing.T) {
		out, _, _ := execBuilds(t, fleet(t), &fakeBuilds{byRepo: map[string]gcb.BuildStatus{svc: build}}, "status", svc, "-vv")
		if want := buildLinePrefix + "(no recent build)\n"; !strings.Contains(out, want) {
			t.Errorf("a build keyed by the service name must not be found (Cloud Build keys on %q):\n%s", repo, out)
		}
	})
}

// TestBuildColumnRendersInFlightAndCompleted drives the whole -v table for the
// two states an operator actually looks for: a build running and a build that
// failed, each beside a service that is still in sync.
func TestBuildColumnRendersInFlightAndCompleted(t *testing.T) {
	now := time.Now()
	builds := &fakeBuilds{byRepo: map[string]gcb.BuildStatus{
		"bifrost":  {Status: "WORKING", SHA: "0ab11f2", StartTime: now.Add(-2 * time.Minute)},
		"identity": {Status: "FAILURE", SHA: "def5678", FinishTime: now.Add(-12 * time.Minute)},
	}}

	out, _, code := execBuilds(t, fleet(t), builds, "status", "-v")
	if code != 0 {
		t.Fatalf("exit = %d, want 0: a build is not a verdict", code)
	}

	for app, want := range map[string][]string{
		"bifrost":  {"bifrost", "✓ in sync", "abc1234", "abc1234", "◌ 0ab11f2 building (2m)"},
		"identity": {"identity", "✓ in sync", "abc1234", "abc1234", "✗ def5678 failed 12m ago"},
		// A service Cloud Build returned nothing for still gets a cell.
		"comms": {"comms", "✓ in sync", "abc1234", "abc1234", "(no recent build)"},
	} {
		if got := tableCells(t, out, app); !slices.Equal(got, want) {
			t.Errorf("%s row = %q, want %q", app, got, want)
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
	if _, _, code := execBuilds(t, fleet(t), builds, "status", "-v"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if dials, calls := builds.counts(); dials != 1 || calls != 1 {
		t.Errorf("dialled %d times and called LatestBuilds %d times for %d services; want 1 and 1", dials, calls, len(reg.Names()))
	}
}

// TestQuietModeMakesNoBuildLookup: the default list (and -q, its explicit
// spelling) is a scriptable contract — bare app names, "*" for mid-deploy — so
// it gains no build text, and since there is nothing to render there is
// nothing to ask for either. That is not only tidiness: it is what a script
// runs in a loop, and it stays exactly as cheap and as offline as it was
// before the build column existed.
func TestQuietModeMakesNoBuildLookup(t *testing.T) {
	for _, args := range [][]string{
		{"status"},
		{"status", "bifrost"},
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
				out, errOut, code = execBuilds(t, c, tc.builds, "status", "bifrost", "-vv")
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("`bif status` did not return: a build lookup must never be able to hang the command")
			}

			// The deployment status is untouched, down to the promote hint.
			for _, want := range []string{
				"bifrost  ✗ out of sync\n",
				"  staging: abc1234\n",
				"  prod:    def5678\n",
				"  promote: bif promote bifrost (deploys abc1234 to prod)\n",
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

	out, _, code := execBuilds(t, fleet(t), builds, "status", "bifrost", "-vv")
	if code != 0 {
		t.Errorf("exit = %d, want 0: a failed build must not make an in-sync service out of sync", code)
	}
	if !strings.HasPrefix(out, "bifrost  ✓ in sync\n") {
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
// key. When the two differ, a lookup by service name finds nothing for that
// service and the right thing for every other, which is exactly why it
// survives review. withRepoOverride supplies a service where they differ.
//
// Both directions are asserted: keyed by the repo the stalled sync is found,
// keyed by the service name the command wrongly reports an all-clear.
func TestAttentionUsesTheRegistryRepoName(t *testing.T) {
	const svc = "sensibility"
	repo := withRepoOverride(t, svc)
	build := gcb.BuildStatus{Status: "SUCCESS", SHA: "0ab11f2", FinishTime: time.Now().Add(-3 * time.Hour)}

	t.Run("keyed by repo name", func(t *testing.T) {
		c, builds := attentionFleet(t, map[string]gcb.BuildStatus{repo: build})
		out, _, code := execBuilds(t, c, builds, "status", "--attention")
		want := "sensibility  build 0ab11f2 succeeded 3h ago, staging still on abc1234\n"
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
func TestOutputModesAreRefusedTogether(t *testing.T) {
	for _, args := range [][]string{
		{"status", "-q", "-a"},
		{"status", "-a", "-q"},
		{"status", "bifrost", "--quiet", "--attention"},
		{"status", "-v", "-a"},
		{"status", "-vv", "--attention"},
		{"status", "-q", "-v"},
		{"status", "bifrost", "--verbose", "--quiet"},
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
			if !strings.Contains(errOut, "-q") || !strings.Contains(errOut, "-v") || !strings.Contains(errOut, "--attention") {
				t.Errorf("stderr = %q, want it to name every mode", errOut)
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

// sectionOrder returns the services whose `bif status -vv` section appeared in
// out, in the order they appeared. It matches the unindented heading line
// specifically rather than searching for the name anywhere, because a
// service's name also shows up in its pods' names and in its promote line.
func sectionOrder(out string) []string {
	var apps []string
	for _, line := range strings.Split(out, "\n") {
		if line != "" && !strings.HasPrefix(line, " ") {
			apps = append(apps, strings.Fields(line)[0])
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

	out, code := exec(t, c, "status", "bifrost", "comms", "identity", "-vv")

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
		if strings.HasPrefix(ns, "haruspex-") || strings.HasPrefix(ns, "sensibility-") {
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
	out, code := exec(t, c, "status", "identity", "comms", "bifrost", "-vv")
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
	out, code := exec(t, c, "status", "bifrost", "comms", "bifrost", "-vv")
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
	out, code := exec(t, c, "status", "bifrost", "comms", "identtiy", "-vv")

	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if len(c.calls) != 0 {
		t.Errorf("read %v before rejecting the bad name; want nothing", c.calls)
	}
	if !strings.HasPrefix(out, "Unknown service: identtiy\n") {
		t.Errorf("stdout = %q, want it to name the bad argument first", out)
	}
	if strings.Contains(out, "in sync") {
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

// ---- --attention's content comparison -----------------------------------

// The real digests behind the false alarm this fixes. Three consecutive
// bifrost tags, three different OCI INDEXES — each holding the same image
// manifest plus that build's own provenance attestation:
//
//	eb12dfa  index: [61acb248b85e (image), d5d58103a519 (attestation)]
//	0cbfc9f  index: [61acb248b85e (image), 59b130d8de1f (attestation)]
//
// The commits between them touched cmd/bif only, and the image builds
// ./cmd/bifrost, so the server image was byte-identical and image-updater
// correctly did not move staging. `bif status -a` called that a stalled deploy.
const (
	sharedManifest = "sha256:61acb248b85e"
	otherManifest  = "sha256:aa11bb22cc33"
)

// fakeDigests serves ManifestDigest from an image ref -> digest map, and counts
// what a run cost: the dials (one credential per invocation, not one per
// service) and every reference asked for (which tag was resolved, in a fleet
// where the tag scheme is not the same for every service).
type fakeDigests struct {
	byImage map[string]string
	dialErr error
	errFor  map[string]error // this reference cannot be read
	hang    bool             // never answer; wait for the context instead

	// concurrent, when non-zero, makes every call block until that many are in
	// flight at once. A resolver that runs the fleet one service at a time
	// never reaches the count and every lookup dies on the context instead —
	// which shows up as the warnings coming back.
	concurrent int
	reached    chan struct{}

	mu       sync.Mutex
	dials    int
	asked    []string
	inFlight int
}

func (f *fakeDigests) dial(context.Context) (digestResolver, error) {
	f.mu.Lock()
	f.dials++
	f.mu.Unlock()
	if f.dialErr != nil {
		return nil, f.dialErr
	}
	return f, nil
}

func (f *fakeDigests) ManifestDigest(ctx context.Context, image string) (string, error) {
	f.mu.Lock()
	f.asked = append(f.asked, image)
	if f.concurrent > 0 {
		f.inFlight++
		if f.inFlight == f.concurrent {
			close(f.reached)
		}
	}
	gate := f.reached
	f.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if f.hang {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if err := f.errFor[image]; err != nil {
		return "", err
	}
	digest, ok := f.byImage[image]
	if !ok {
		return "", fmt.Errorf("manifest unknown for %s", image)
	}
	return digest, nil
}

func (f *fakeDigests) counts() (dials int, asked []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dials, append([]string(nil), f.asked...)
}

// newDigests is a resolver that knows the given refs, with the concurrency
// plumbing off.
func newDigests(byImage map[string]string) *fakeDigests {
	return &fakeDigests{byImage: byImage}
}

// execAttention is execBuilds with a registry to talk to as well.
func execAttention(t *testing.T, cluster *fakeCluster, builds *fakeBuilds, digests *fakeDigests, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(context.Background(), args, strings.NewReader(""), &out, &errb, cluster.connect, builds.dial, digests.dial, noPreview)
	return out.String(), errb.String(), code
}

// stalledFleet is attentionFleet with bifrost set up as the false alarm:
// staging running an older TAG than the newest successful build, which is the
// only state that reaches the registry at all.
func stalledFleet(t *testing.T, stagingTag, buildSHA string, finished time.Duration) (*fakeCluster, *fakeBuilds) {
	t.Helper()
	c, builds := attentionFleet(t, map[string]gcb.BuildStatus{
		"bifrost": {Status: "SUCCESS", SHA: buildSHA, FinishTime: time.Now().Add(-finished)},
	})
	c.images["bifrost-staging"] = []string{image("bifrost", stagingTag)}
	c.images["bifrost-prod"] = []string{image("bifrost", stagingTag)}
	return c, builds
}

// TestAttentionSameContentIsNotDrift is the false alarm itself, end to end.
// Staging's tag is an older commit than the newest successful build, and both
// tags resolve to ONE image manifest — so nothing is stalled, and the run says
// so instead of sending its reader after a deploy that already happened.
//
// The line names BOTH SHAs on purpose: the tag is the link back to GitHub, and
// the digest that settled the question is an input to the verdict, never a
// replacement for the commit on screen.
func TestAttentionSameContentIsNotDrift(t *testing.T) {
	c, builds := stalledFleet(t, "eb12dfa", "0cbfc9f", 26*time.Minute)
	digests := newDigests(map[string]string{
		image("bifrost", "eb12dfa"): sharedManifest,
		image("bifrost", "0cbfc9f"): sharedManifest,
	})

	out, _, code := execAttention(t, c, builds, digests, "status", "--attention")
	want := "bifrost  staging matches the latest build's content (running eb12dfa, built 0cbfc9f)\n" +
		attentionAllClear + "\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if code != 0 {
		t.Errorf("exit = %d, want 0: two tags over one image manifest are not a stalled sync", code)
	}
	if strings.Contains(out, "still on") {
		t.Errorf("the stalled-sync warning was printed for identical content:\n%s", out)
	}
}

// TestAttentionDifferentContentWarnsExactlyAsToday is the other side, and the
// one that keeps the fix from being "stop reporting stalled syncs": when the
// two tags are different images, the line is the one this mode has always
// printed, byte for byte, and the exit code is still 1.
func TestAttentionDifferentContentWarnsExactlyAsToday(t *testing.T) {
	c, builds := stalledFleet(t, "eb12dfa", "0cbfc9f", 22*time.Minute)
	digests := newDigests(map[string]string{
		image("bifrost", "eb12dfa"): sharedManifest,
		image("bifrost", "0cbfc9f"): otherManifest,
	})

	out, _, code := execAttention(t, c, builds, digests, "status", "--attention")
	want := "bifrost  build 0cbfc9f succeeded 22m ago, staging still on eb12dfa\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if code != 1 {
		t.Errorf("exit = %d, want 1: staging really is behind", code)
	}
}

// TestAttentionRegistryFailuresWarnAnyway is the safety property, and it is not
// negotiable: a registry problem must never be able to SUPPRESS a real stalled
// sync. Every way the lookup can fail to answer falls back to the tag
// comparison and warns — which is what this command did before the comparison
// existed.
//
// The stalled sync is the signature of a failure mode whose own symptom is
// silence (an expired ArgoCD PAT, syncs stopped, builds still green). A
// fallback that went quiet would make this command lie in exactly the state it
// was written for.
func TestAttentionRegistryFailuresWarnAnyway(t *testing.T) {
	const want = "bifrost  build 0cbfc9f succeeded 22m ago, staging still on eb12dfa\n"

	tests := []struct {
		name    string
		digests *fakeDigests
		why     string
	}{
		{
			name:    "no credentials, so the resolver never dials",
			digests: &fakeDigests{dialErr: errors.New("no credentials")},
			why:     "an unauthenticated operator gets the warning, not silence",
		},
		{
			name: "the build's tag is not in the registry",
			digests: newDigests(map[string]string{
				image("bifrost", "eb12dfa"): sharedManifest,
			}),
			why: "a missing tag is 'could not tell', and could-not-tell warns",
		},
		{
			name: "the staging tag cannot be read",
			digests: &fakeDigests{
				byImage: map[string]string{image("bifrost", "0cbfc9f"): sharedManifest},
				errFor:  map[string]error{image("bifrost", "eb12dfa"): errors.New("500 from the registry")},
			},
			why: "half an answer is no answer",
		},
		{
			name:    "the registry never answers",
			digests: &fakeDigests{hang: true},
			why:     "the phase is bounded; a hung registry falls back like any other failure",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.digests.hang {
				restore := digestLookupTimeout
				digestLookupTimeout = 50 * time.Millisecond
				t.Cleanup(func() { digestLookupTimeout = restore })
			}
			c, builds := stalledFleet(t, "eb12dfa", "0cbfc9f", 22*time.Minute)

			out, _, code := execAttention(t, c, builds, tc.digests, "status", "--attention")
			if out != want {
				t.Errorf("stdout = %q, want %q (%s)", out, want, tc.why)
			}
			if code != 1 {
				t.Errorf("exit = %d, want 1 (%s)", code, tc.why)
			}
		})
	}
}

// TestAttentionResolvesTheStagingTagScheme: the reference handed to the
// registry follows the tag scheme STAGING is running, not the bare build SHA.
//
// footstrike-dashboard builds {sha}-staging and {sha}-prod as separate images
// (its Vite values are baked in at build time), so the bare {sha} names an
// image that was never pushed for it — asking for one would 404 into a
// needless fallback and this service would never get a content comparison at
// all. It is the same rule promote.NewProdTag follows: the scheme belongs to
// the artifact, not to the app's name.
func TestAttentionResolvesTheStagingTagScheme(t *testing.T) {
	c, builds := attentionFleet(t, map[string]gcb.BuildStatus{
		"footstrike-dashboard": {Status: "SUCCESS", SHA: "0ab11f2", FinishTime: time.Now().Add(-time.Hour)},
	})
	c.images["footstrike-dashboard-staging"] = []string{image("footstrike-dashboard", "abc1234-staging")}
	c.images["footstrike-dashboard-prod"] = []string{image("footstrike-dashboard", "abc1234-prod")}

	digests := newDigests(map[string]string{
		image("footstrike-dashboard", "abc1234-staging"): sharedManifest,
		image("footstrike-dashboard", "0ab11f2-staging"): sharedManifest,
		// The bare-SHA tag exists for no service that tags by environment; if
		// it is ever asked for, this map does not have it and the run warns.
	})

	out, _, code := execAttention(t, c, builds, digests, "status", "--attention")
	_, asked := digests.counts()
	if !slices.Contains(asked, image("footstrike-dashboard", "0ab11f2-staging")) {
		t.Errorf("resolved %v; want the build's STAGING tag %s", asked, image("footstrike-dashboard", "0ab11f2-staging"))
	}
	if slices.Contains(asked, image("footstrike-dashboard", "0ab11f2")) {
		t.Errorf("resolved the bare build SHA %s, which this service never pushes", image("footstrike-dashboard", "0ab11f2"))
	}
	want := "footstrike-dashboard  staging matches the latest build's content (running abc1234-staging, built 0ab11f2)\n" +
		attentionAllClear + "\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
}

// TestAttentionTouchesTheRegistryOnlyWhenTagsDiffer: the content comparison is
// the answer to one question, and a fleet that is not asking it must not pay
// for it. No candidate, no dial, no credential, no request — `bif status -a` on
// a healthy fleet costs exactly what it did before this existed.
func TestAttentionTouchesTheRegistryOnlyWhenTagsDiffer(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fakeCluster)
		build gcb.BuildStatus
		why   string
	}{
		{
			name:  "nothing is noteworthy at all",
			build: gcb.BuildStatus{Status: "SUCCESS", SHA: "abc1234", FinishTime: time.Now().Add(-time.Hour)},
			why:   "staging is already on the build's commit",
		},
		{
			name:  "only staging and prod differ",
			setup: func(c *fakeCluster) { c.images["bifrost-prod"] = []string{image("bifrost", "def5678")} },
			build: gcb.BuildStatus{Status: "SUCCESS", SHA: "abc1234", FinishTime: time.Now().Add(-time.Hour)},
			why:   "the staging-versus-prod verdict is tag-based and asks the registry nothing",
		},
		{
			name:  "the newest build failed",
			build: gcb.BuildStatus{Status: "FAILURE", SHA: "0ab11f2", FinishTime: time.Now().Add(-time.Hour)},
			why:   "a failed build was never going to reach staging; there is nothing to compare",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, builds := attentionFleet(t, map[string]gcb.BuildStatus{"bifrost": tc.build})
			if tc.setup != nil {
				tc.setup(c)
			}
			digests := newDigests(nil)

			execAttention(t, c, builds, digests, "status", "--attention")
			if dials, asked := digests.counts(); dials != 0 || len(asked) != 0 {
				t.Errorf("dialled the registry %d times and made %d lookups (%v); want none (%s)", dials, len(asked), asked, tc.why)
			}
		})
	}
}

// TestAttentionResolvesServicesConcurrentlyOnOneCredential: two round trips per
// stalled service, several services, one command an operator is waiting on. The
// services resolve at the same time — the way fetchBuilds already overlaps the
// build read with the cluster read — and the credential is fetched once for the
// whole run rather than once per service.
//
// The concurrency half is not an output assertion: the fake blocks every lookup
// until three are in flight at once, so a sequential resolver never gets past
// the first, dies on the bounded context, and comes back with warnings instead
// of the three informational lines below.
func TestAttentionResolvesServicesConcurrentlyOnOneCredential(t *testing.T) {
	restore := digestLookupTimeout
	digestLookupTimeout = 2 * time.Second
	t.Cleanup(func() { digestLookupTimeout = restore })

	apps := []string{"bifrost", "comms", "identity"}
	byRepo := map[string]gcb.BuildStatus{}
	byImage := map[string]string{}
	c := fleet(t)
	for _, app := range apps {
		byRepo[app] = gcb.BuildStatus{Status: "SUCCESS", SHA: "0cbfc9f", FinishTime: time.Now().Add(-time.Hour)}
		c.images[app+"-staging"] = []string{image(app, "eb12dfa")}
		c.images[app+"-prod"] = []string{image(app, "eb12dfa")}
		byImage[image(app, "eb12dfa")] = sharedManifest
		byImage[image(app, "0cbfc9f")] = sharedManifest
	}
	builds := &fakeBuilds{byRepo: byRepo}
	digests := &fakeDigests{byImage: byImage, concurrent: len(apps), reached: make(chan struct{})}

	out, _, code := execAttention(t, c, builds, digests, "status", "--attention")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stdout:\n%s", code, out)
	}
	width := 0
	for _, app := range apps {
		width = max(width, len(app))
	}
	for _, app := range apps {
		line := fmt.Sprintf("%-*s  staging matches the latest build's content (running eb12dfa, built 0cbfc9f)", width, app)
		if !strings.Contains(out, line) {
			t.Errorf("%s did not resolve concurrently; stdout:\n%s", app, out)
		}
	}
	if dials, asked := digests.counts(); dials != 1 || len(asked) != 2*len(apps) {
		t.Errorf("dialled %d times for %d lookups across %d services; want 1 dial and %d lookups",
			dials, len(asked), len(apps), 2*len(apps))
	}
}

// TestAttentionInformationalLineDoesNotSuppressTheRest: the content comparison
// answers ONE question, and a service can be noteworthy for other reasons at
// the same time. The informational line prints alongside them and does not
// qualify the service by itself — the exit code goes on meaning "something
// needs a human".
func TestAttentionInformationalLineDoesNotSuppressTheRest(t *testing.T) {
	c, builds := stalledFleet(t, "eb12dfa", "0cbfc9f", 22*time.Minute)
	c.images["bifrost-prod"] = []string{image("bifrost", "def5678")}
	digests := newDigests(map[string]string{
		image("bifrost", "eb12dfa"): sharedManifest,
		image("bifrost", "0cbfc9f"): sharedManifest,
	})

	out, _, code := execAttention(t, c, builds, digests, "status", "--attention")
	want := "bifrost  staging matches the latest build's content (running eb12dfa, built 0cbfc9f)\n" +
		"bifrost  staging and prod differ: staging eb12dfa, prod def5678 (bif promote bifrost)\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if code != 1 {
		t.Errorf("exit = %d, want 1: there is still something to promote", code)
	}
}

// TestQuietMakesNoRegistryLookup is -q's offline guarantee, restated for the
// third party this change added. Its output is a scriptable contract and it
// makes no network call beyond the cluster: no Cloud Build read (see
// TestQuietModeMakesNoBuildLookup) and no registry read either.
func TestQuietMakesNoRegistryLookup(t *testing.T) {
	c := fleet(t)
	c.images["bifrost-staging"] = []string{image("bifrost", "eb12dfa")}
	builds := &fakeBuilds{byRepo: map[string]gcb.BuildStatus{
		"bifrost": {Status: "SUCCESS", SHA: "0cbfc9f", FinishTime: time.Now().Add(-time.Hour)},
	}}
	digests := newDigests(map[string]string{
		image("bifrost", "eb12dfa"): sharedManifest,
		image("bifrost", "0cbfc9f"): sharedManifest,
	})

	out, _, code := execAttention(t, c, builds, digests, "status", "-q")
	if out != "bifrost\n" {
		t.Errorf("stdout = %q, want %q: -q's output is a contract and this change is not part of it", out, "bifrost\n")
	}
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if dials, asked := digests.counts(); dials != 0 || len(asked) != 0 {
		t.Errorf("-q dialled the registry %d times and made %d lookups (%v); want none", dials, len(asked), asked)
	}
}
