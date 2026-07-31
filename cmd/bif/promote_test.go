package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eswan18/bifrost/internal/oracle"
)

// ---- the oracle fixture -------------------------------------------------

// promoteRow mirrors one row of testdata/oracle/promote_decision.json: the
// images that were deployed, and what ib.py's promote() printed, returned, and
// would have handed to kubectl. The rows were captured by running the real
// promote() with its subprocess call intercepted, so KubectlArgv and Patch are
// the exact strings that would have reached the cluster.
type promoteRow struct {
	App            string   `json:"app"`
	StagingImages  []string `json:"stagingImages"`
	ProdImages     []string `json:"prodImages"`
	Promoted       bool     `json:"promoted"`
	KubectlArgv    []string `json:"kubectlArgv"`
	Patch          *string  `json:"patch"`
	KustomizeImage *string  `json:"kustomizeImage"`
	Return         *struct {
		SystemExit *int `json:"systemExit"`
	} `json:"return"`
	Stdout string `json:"stdout"`
}

func (r promoteRow) key() string {
	return r.App + " staging=[" + strings.Join(r.StagingImages, " ") +
		"] prod=[" + strings.Join(r.ProdImages, " ") + "]"
}

// exitCode is what ib.py's process exited with for this row: a bare return
// from promote() falls through main() and exits 0; sys.exit(n) is recorded.
func (r promoteRow) exitCode() int {
	if r.Return == nil || r.Return.SystemExit == nil {
		return 0
	}
	return *r.Return.SystemExit
}

// newImage is the full image reference ib.py put on the right-hand side of the
// kustomize override ("<base>=<base>:<tag>") — the thing that determines what
// prod actually runs.
func (r promoteRow) newImage(t *testing.T) string {
	t.Helper()
	ki := oracle.Str(r.KustomizeImage)
	i := strings.Index(ki, "=")
	if i < 0 {
		t.Fatalf("fixture kustomizeImage %q is not <base>=<image>", ki)
	}
	return ki[i+1:]
}

// clusterFor builds a fake holding exactly the row's cluster state.
func clusterFor(r promoteRow) *fakeCluster {
	return &fakeCluster{images: map[string][]string{
		r.App + "-staging": r.StagingImages,
		r.App + "-prod":    r.ProdImages,
	}}
}

// TestPromoteMatchesOracle is the golden comparison for the command that
// changes production: for every cluster state ib.py's promote() was captured
// against, `bif promote <app> -y` must print exactly what ib.py printed, exit
// the way ib.py exited, and write exactly what ib.py would have written —
// including writing nothing at all where ib.py refused.
//
// The "wrote nothing" half is why the fake records patches: no assertion on
// stdout can distinguish a refusal from a promotion whose output happened to
// look like one.
func TestPromoteMatchesOracle(t *testing.T) {
	rows, err := oracle.Load[promoteRow]("promote_decision.json")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}

	// The fixture has to cover both sides of every branch this test claims to
	// check, or a row-by-row loop over it proves less than it appears to.
	var promoted, refused int
	for _, r := range rows {
		if r.Promoted {
			promoted++
		} else {
			refused++
		}
	}
	if promoted == 0 || refused == 0 {
		t.Fatalf("fixture covers %d promotions and %d refusals; need both", promoted, refused)
	}

	for _, r := range rows {
		t.Run(r.key(), func(t *testing.T) {
			cluster := clusterFor(r)
			out, code := exec(t, cluster, "promote", r.App, "-y")

			if out != r.Stdout {
				t.Errorf("stdout mismatch\n got %q\nib.py %q", out, r.Stdout)
			}
			if code != r.exitCode() {
				t.Errorf("exit = %d, ib.py exited %d", code, r.exitCode())
			}

			if !r.Promoted {
				if len(cluster.patches) != 0 {
					t.Fatalf("ib.py wrote nothing here; bif patched %+v", cluster.patches)
				}
				return
			}
			if len(cluster.patches) != 1 {
				t.Fatalf("patches = %+v, want exactly one", cluster.patches)
			}
			got := cluster.patches[0]
			// ib.py patched `application <app>-prod`; the Go client builds
			// that name from (app, env).
			if got.app != r.App || got.env != "prod" {
				t.Errorf("patched %s-%s, ib.py patched %s-prod", got.app, got.env, r.App)
			}
			if want := r.newImage(t); got.image != want {
				t.Errorf("patched image = %q, ib.py wrote %q", got.image, want)
			}
		})
	}
}

// TestPromoteTargetsTheApplicationTheOracleNamed pins the object being
// patched against the fixture's captured argv rather than against a name this
// test made up: `kubectl patch application <name> -n <namespace>`. The patch
// body itself is asserted in internal/kube (TestPatchAppImageMatchesOracle),
// which is where it is built.
func TestPromoteTargetsTheApplicationTheOracleNamed(t *testing.T) {
	rows, err := oracle.Load[promoteRow]("promote_decision.json")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	checked := 0
	for _, r := range rows {
		if !r.Promoted {
			continue
		}
		t.Run(r.key(), func(t *testing.T) {
			// kubectl patch application <name> -n <namespace> ...
			if len(r.KubectlArgv) < 6 {
				t.Fatalf("fixture argv is too short to name an object: %v", r.KubectlArgv)
			}
			wantName, wantNS := r.KubectlArgv[3], r.KubectlArgv[5]

			cluster := clusterFor(r)
			if _, code := exec(t, cluster, "promote", r.App, "-y"); code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if len(cluster.patches) != 1 {
				t.Fatalf("patches = %+v, want exactly one", cluster.patches)
			}
			if gotName := cluster.patches[0].app + "-" + cluster.patches[0].env; gotName != wantName {
				t.Errorf("patched Application %q, ib.py patched %q", gotName, wantName)
			}
			if argoNamespace != wantNS {
				t.Errorf("bif patches in namespace %q, ib.py used %q", argoNamespace, wantNS)
			}
			checked++
		})
	}
	if checked == 0 {
		t.Fatal("no promoted fixture rows; this test asserted nothing")
	}
}

// TestOverrideKeyComesFromTheImageNotTheAppName is Task 1's DISAGREEMENT #3,
// made reachable.
//
// ib.py builds the kustomize override key as REGISTRY + "/" + app — from the
// ArgoCD app name and a hardcoded registry, never looking at what is running.
// So for a service whose image repository is not named after it, ib.py writes
// an override keyed on an image the Deployment does not reference. Kustomize
// ignores an override whose key matches nothing, so the promote reports
// success, ArgoCD syncs, and prod does not move — the worst available failure
// mode for this command.
//
// footstrike-api is that service: it was called fitness-api until July 2026
// and its image path still is. Go derives the key from the running image
// (promote.ReplaceTag over promote.ImageBase) and is correct. ib.py's value is
// read from the oracle rather than written here, so this fails if the Python
// ever stops doing it and the divergence needs re-deciding.
func TestOverrideKeyComesFromTheImageNotTheAppName(t *testing.T) {
	type imageBaseRow struct {
		App                  string `json:"app"`
		DeployedImage        string `json:"deployedImage"`
		ImageBaseFromAppName string `json:"imageBaseFromAppName"`
	}
	rows, err := oracle.Load[imageBaseRow]("image_base.json")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}

	const app = "footstrike-api"
	var pythonBase, runningImage string
	for _, r := range rows {
		if r.App == app && !strings.HasPrefix(r.DeployedImage, testRegistry+"/"+app+":") {
			pythonBase, runningImage = r.ImageBaseFromAppName, r.DeployedImage
		}
	}
	if pythonBase == "" {
		t.Fatalf("image_base.json no longer has a %s row whose image repo differs from its name; "+
			"the divergence this test pins may no longer exist", app)
	}

	// Staging is the row's image; prod is the same repository on an older SHA.
	stagingImage := runningImage
	prodImage := strings.Replace(runningImage, ":abc1234", ":def5678", 1)
	if prodImage == stagingImage {
		t.Fatalf("fixture image %q did not have the tag this test expects to vary", runningImage)
	}

	cluster := &fakeCluster{images: map[string][]string{
		app + "-staging": {stagingImage},
		app + "-prod":    {prodImage},
	}}
	out, code := exec(t, cluster, "promote", app, "-y")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	if len(cluster.patches) != 1 {
		t.Fatalf("patches = %+v, want exactly one", cluster.patches)
	}

	got := cluster.patches[0].image
	const wantGo = testRegistry + "/fitness-api:abc1234"
	if got != wantGo {
		t.Errorf("patched image = %q, want %q (the repository the Deployment actually runs)", got, wantGo)
	}
	// The failure this exists to prevent, stated directly: ib.py would have
	// keyed the override on the app's name.
	if pythonImage := pythonBase + ":abc1234"; got == pythonImage {
		t.Errorf("bif ported ib.py's app-name-keyed override (%q); kustomize would silently ignore it "+
			"and the promote would do nothing", pythonImage)
	}
	// The ArgoCD Application, unlike the image, IS named after the app.
	if cluster.patches[0].app != app {
		t.Errorf("patched Application for %q, want %q", cluster.patches[0].app, app)
	}
}

// TestAlreadyInSyncReportsTheSHANotTheTag covers a gap in the capture: every
// in-sync row in promote_decision.json uses the plain-{sha} scheme, where the
// tag and the SHA are the same string, so a `bif` that printed the tag would
// satisfy the whole fixture. ib.py prints staging_sha (ib.py:463), which on
// the {sha}-{env} scheme is "abc1234" and not "abc1234-staging" — the SHA is
// what the two environments actually have in common, and the tags differ by
// construction there.
func TestAlreadyInSyncReportsTheSHANotTheTag(t *testing.T) {
	const app = "footstrike-dashboard"
	cluster := &fakeCluster{images: map[string][]string{
		app + "-staging": {image(app, "abc1234-staging")},
		app + "-prod":    {image(app, "abc1234-prod")},
	}}
	out, code := exec(t, cluster, "promote", app, "-y")

	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if len(cluster.patches) != 0 {
		t.Fatalf("nothing to promote, but bif patched %+v", cluster.patches)
	}
	want := "\n✓ Already in sync (both on abc1234)\n"
	if !strings.HasSuffix(out, want) {
		t.Errorf("output does not end with %q:\n%s", want, out)
	}
}

// ---- the confirmation prompt --------------------------------------------

// TestPromoteConfirmation drives the prompt both ways. Declining must leave
// the cluster untouched — not "print something reassuring", but issue no
// patch at all — which is the assertion the fake's recorder exists for.
//
// The accept rule is ib.py's exactly: `input().strip().lower() != "y"` aborts,
// so "yes" declines. That reads as unfriendly right up until you remember the
// keystroke is irreversible.
func TestPromoteConfirmation(t *testing.T) {
	tests := []struct {
		name      string
		stdin     string
		wantPatch bool
	}{
		{"y proceeds", "y\n", true},
		{"uppercase Y proceeds", "Y\n", true},
		{"surrounding whitespace is stripped", "  y  \n", true},
		{"no trailing newline still proceeds", "y", true},
		{"n declines", "n\n", false},
		{"yes is not y", "yes\n", false},
		{"empty line declines", "\n", false},
		{"EOF declines", "", false},
		{"anything else declines", "sure\n", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cluster := &fakeCluster{images: map[string][]string{
				"bifrost-staging": {image("bifrost", "abc1234")},
				"bifrost-prod":    {image("bifrost", "def5678")},
			}}
			out, code := execStdin(t, cluster, tc.stdin, "promote", "bifrost")

			if !strings.Contains(out, "\nProceed? [y/N] ") {
				t.Errorf("no confirmation prompt in:\n%s", out)
			}
			if code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}

			if !tc.wantPatch {
				if len(cluster.patches) != 0 {
					t.Fatalf("declined, but bif patched %+v", cluster.patches)
				}
				want := "\nProceed? [y/N] Aborted.\n"
				if !strings.HasSuffix(out, want) {
					t.Errorf("output does not end with %q:\n%s", want, out)
				}
				if strings.Contains(out, "Running: kubectl patch") {
					t.Errorf("declined, but bif announced the patch:\n%s", out)
				}
				return
			}
			if len(cluster.patches) != 1 {
				t.Fatalf("accepted, but patches = %+v", cluster.patches)
			}
			if want := image("bifrost", "abc1234"); cluster.patches[0].image != want {
				t.Errorf("patched %q, want %q", cluster.patches[0].image, want)
			}
			if !strings.Contains(out, "✓ Promoted bifrost prod to abc1234") {
				t.Errorf("output does not report the promotion:\n%s", out)
			}
		})
	}
}

// TestPromoteYesSkipsThePrompt: -y/--yes must not read stdin at all. The stdin
// here would decline if it were consulted, so a prompt that leaked back in
// fails this rather than silently blocking a scripted promote.
func TestPromoteYesSkipsThePrompt(t *testing.T) {
	for _, args := range [][]string{
		{"promote", "bifrost", "-y"},
		{"promote", "bifrost", "--yes"},
		{"promote", "-y", "bifrost"},
		{"promote", "--yes", "bifrost"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cluster := &fakeCluster{images: map[string][]string{
				"bifrost-staging": {image("bifrost", "abc1234")},
				"bifrost-prod":    {image("bifrost", "def5678")},
			}}
			out, code := execStdin(t, cluster, "n\n", args...)
			if code != 0 {
				t.Fatalf("exit = %d, want 0\n%s", code, out)
			}
			if strings.Contains(out, "Proceed?") {
				t.Errorf("-y still prompted:\n%s", out)
			}
			if len(cluster.patches) != 1 {
				t.Fatalf("patches = %+v, want exactly one", cluster.patches)
			}
		})
	}
}

// ---- guards and failures ------------------------------------------------

// TestStagingMismatchRefusesProdMismatchDoesNot is the asymmetry, asserted as
// one property rather than two coincidences. ib.py refuses a staging mismatch
// and only warns about a prod one, and the reason is not symmetry-blindness:
// a staging rollout in flight means the artifact being promoted is not settled
// and might be the wrong one, while a prod rollout in flight is just the last
// promote still landing — re-pinning it is how a bad one gets corrected.
//
// promote.StatusOf calls both MidDeploy. The distinction lives in cmd/bif
// because it is about what a promote may do, not about what the cluster is.
func TestStagingMismatchRefusesProdMismatchDoesNot(t *testing.T) {
	t.Run("staging mismatch refuses", func(t *testing.T) {
		cluster := &fakeCluster{images: map[string][]string{
			"bifrost-staging": {image("bifrost", "abc1234"), image("bifrost", "def5678")},
			"bifrost-prod":    {image("bifrost", "0000000")},
		}}
		out, code := exec(t, cluster, "promote", "bifrost", "-y")
		if code != 1 {
			t.Errorf("exit = %d, want 1", code)
		}
		if len(cluster.patches) != 0 {
			t.Fatalf("refused, but bif patched %+v", cluster.patches)
		}
		if !strings.Contains(out, "Wait for the deployment to complete before promoting.") {
			t.Errorf("output does not explain the refusal:\n%s", out)
		}
	})

	t.Run("prod mismatch warns and continues", func(t *testing.T) {
		cluster := &fakeCluster{images: map[string][]string{
			"bifrost-staging": {image("bifrost", "abc1234")},
			"bifrost-prod":    {image("bifrost", "def5678"), image("bifrost", "0000000")},
		}}
		out, code := exec(t, cluster, "promote", "bifrost", "-y")
		if code != 0 {
			t.Errorf("exit = %d, want 0\n%s", code, out)
		}
		if len(cluster.patches) != 1 {
			t.Fatalf("patches = %+v, want exactly one — a prod mismatch warns, it does not refuse", cluster.patches)
		}
		want := "Warning: Prod has an image mismatch (deployment in progress?)\n" +
			"  Images found:\n    - 0000000\n    - def5678\n\n" +
			"\nbifrost promotion check:\n"
		if !strings.HasPrefix(out, want) {
			t.Errorf("output does not start with the warning\n got %q\nwant prefix %q", out, want)
		}
		if !strings.Contains(out, "✓ Promoted bifrost prod to abc1234") {
			t.Errorf("output does not report the promotion:\n%s", out)
		}
	})
}

// TestPromoteWriteFailure: the patch is the only thing that can fail after the
// decision, and when it does the operator has to be told the promotion did not
// happen — with the cluster's own message, and a non-zero exit for whatever
// invoked this.
func TestPromoteWriteFailure(t *testing.T) {
	cluster := &fakeCluster{
		images: map[string][]string{
			"bifrost-staging": {image("bifrost", "abc1234")},
			"bifrost-prod":    {image("bifrost", "def5678")},
		},
		patchErr: errors.New("applications.argoproj.io \"bifrost-prod\" not found\n"),
	}
	out, code := exec(t, cluster, "promote", "bifrost", "-y")

	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if len(cluster.patches) != 1 {
		t.Fatalf("patches = %+v, want exactly one attempt", cluster.patches)
	}
	want := "\n✗ Promotion failed\n  applications.argoproj.io \"bifrost-prod\" not found\n"
	if !strings.HasSuffix(out, want) {
		t.Errorf("output does not end with the failure\n got %q\nwant suffix %q", out, want)
	}
	if strings.Contains(out, "✓ Promoted") {
		t.Errorf("a failed patch reported success:\n%s", out)
	}
}

// TestPromoteUnknownServiceRejectedBeforeConnecting: a typo'd name is rejected
// against the embedded registry before the cluster is dialled — so before
// anything could be written — and the message is ib.py's.
func TestPromoteUnknownServiceRejectedBeforeConnecting(t *testing.T) {
	var stdout, stderr bytes.Buffer
	connected := false
	code := run(context.Background(), []string{"promote", "bifrsot", "-y"}, strings.NewReader(""),
		&stdout, &stderr, func() (promoter, error) {
			connected = true
			return nil, errors.New("should not have been called")
		})

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

// TestPromoteConnectFailureExitsNonZero: no kubeconfig is a real failure, and
// nothing may be reported as promoted.
func TestPromoteConnectFailureExitsNonZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"promote", "bifrost", "-y"}, strings.NewReader(""),
		&stdout, &stderr, func() (promoter, error) { return nil, errors.New("no kubeconfig") })

	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no kubeconfig") {
		t.Errorf("stderr = %q, want the connection error", stderr.String())
	}
	if strings.Contains(stdout.String(), "Promoted") {
		t.Errorf("stdout = %q, want no promotion claim", stdout.String())
	}
}

// TestPromoteUsageRequiresAnApp: `bif promote` alone is a usage error, not a
// fleet-wide promotion. ib.py exits 1 here and so does this; there is no form
// of this command that promotes everything.
func TestPromoteUsageRequiresAnApp(t *testing.T) {
	for _, args := range [][]string{{"promote"}, {"promote", "-y"}, {"promote", "--yes"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cluster := fleet(t)
			out, code := exec(t, cluster, args...)
			if code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			if out != promoteUsage+"\n" {
				t.Errorf("stdout = %q, want %q", out, promoteUsage+"\n")
			}
			if len(cluster.patches) != 0 {
				t.Fatalf("patched %+v with no app named", cluster.patches)
			}
		})
	}
}
