package promote

import (
	"strings"
	"testing"

	"github.com/eswan18/bifrost/internal/oracle"
)

// Differential tests against infra/ib.py, the implementation that promotes to
// production today. The expected values are NOT computed here -- every one is
// read out of testdata/oracle/, captured from the real ib.py by
// scripts/capture-oracle.sh. A test that built its expectation by calling the
// Go function would agree with any behaviour at all.
//
// Where the two implementations disagree, the disagreement is pinned in a
// `divergences` table rather than hidden: each entry records what Go does,
// what ib.py does, and why the Go behaviour is (or is not) acceptable. The
// tests assert the divergence still holds, so changing either side breaks the
// build and forces the question to be answered again on purpose.

// divergence records a known, deliberate difference from the oracle.
type divergence struct {
	python string // what ib.py produces
	golang string // what this package produces
	why    string
}

func TestOracleMetadata(t *testing.T) {
	meta, err := oracle.LoadMeta()
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}
	if meta.Source.FileLastCommitSHA == "" {
		t.Error("fixtures do not record which ib.py they came from")
	}
	if meta.Source.FileUncommittedChanges {
		t.Error("fixtures were captured from an ib.py with uncommitted changes; re-capture from a clean tree")
	}
	t.Logf("oracle: %s @ %s", meta.Source.Path, meta.Source.FileLastCommitSHA)
}

func TestExtractTagMatchesOracle(t *testing.T) {
	type row struct {
		Image string `json:"image"`
		Tag   string `json:"tag"`
	}
	rows, err := oracle.Load[row]("extract_tag.json")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	for _, r := range rows {
		t.Run(r.Image, func(t *testing.T) {
			if got := ExtractTag(r.Image); got != r.Tag {
				t.Errorf("ExtractTag(%q) = %q, ib.py extract_tag = %q", r.Image, got, r.Tag)
			}
		})
	}
}

func TestExtractSHAMatchesOracle(t *testing.T) {
	type row struct {
		Tag string  `json:"tag"`
		SHA *string `json:"sha"` // Python None -> Go ""
	}
	rows, err := oracle.Load[row]("extract_sha.json")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}

	// DISAGREEMENT (not load-bearing, deliberately left as-is).
	//
	// Python's `$` matches at the end of the string OR immediately before a
	// trailing newline; Go's `$` (without the `m` flag) matches only at the
	// end of the text. So ib.py reads a SHA out of a tag with a trailing
	// newline and this package does not.
	//
	// VERDICT: Go is correct and Python is accidentally lenient. A tag is a
	// substring of a container image reference read out of the Kubernetes
	// API; it cannot contain a newline, so no reachable input distinguishes
	// them. Anchoring with `\z` in Go to reproduce Python's quirk would be
	// copying a bug. Pinned so the port does not silently acquire it.
	divergences := map[string]divergence{
		"abc1234\n":         {python: "abc1234", golang: "", why: "Python `$` also matches before a trailing newline"},
		"abc1234-staging\n": {python: "abc1234", golang: "", why: "Python `$` also matches before a trailing newline"},
	}

	for _, r := range rows {
		t.Run(strings.ReplaceAll(r.Tag, "\n", "\\n"), func(t *testing.T) {
			got := ExtractSHA(r.Tag)
			want := oracle.Str(r.SHA)
			if d, known := divergences[r.Tag]; known {
				if want != d.python {
					t.Fatalf("oracle changed: ib.py extract_sha(%q) = %q, divergence table says %q", r.Tag, want, d.python)
				}
				if got != d.golang {
					t.Fatalf("known divergence changed: ExtractSHA(%q) = %q, was %q (%s)", r.Tag, got, d.golang, d.why)
				}
				return
			}
			if got != want {
				t.Errorf("ExtractSHA(%q) = %q, ib.py extract_sha = %q", r.Tag, got, want)
			}
		})
	}
}

func TestNewProdTagMatchesOracle(t *testing.T) {
	type row struct {
		StagingTag string  `json:"stagingTag"`
		ProdTag    *string `json:"prodTag"`
		StagingSHA string  `json:"stagingSha"`
		NewProdTag string  `json:"newProdTag"`
	}
	rows, err := oracle.Load[row]("new_prod_tag_for.json")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	// Note the argument order differs: ib.py is
	// new_prod_tag_for(staging_tag, prod_tag, staging_sha) and Go is
	// NewProdTag(sha, stagingTag, prodTag). Getting this mapping wrong is
	// exactly the kind of thing this suite exists to catch, so it is written
	// out once, here.
	for _, r := range rows {
		prodDesc := "<null>"
		if r.ProdTag != nil {
			prodDesc = *r.ProdTag
		}
		t.Run("staging="+r.StagingTag+",prod="+prodDesc, func(t *testing.T) {
			got := NewProdTag(r.StagingSHA, r.StagingTag, oracle.Str(r.ProdTag))
			if got != r.NewProdTag {
				t.Errorf("NewProdTag(sha=%q, staging=%q, prod=%q) = %q, ib.py new_prod_tag_for = %q",
					r.StagingSHA, r.StagingTag, oracle.Str(r.ProdTag), got, r.NewProdTag)
			}
		})
	}
}

// statusRow mirrors one row of testdata/oracle/status.json.
type statusRow struct {
	App           string   `json:"app"`
	StagingImages []string `json:"stagingImages"`
	ProdImages    []string `json:"prodImages"`
	State         string   `json:"state"`
	StagingTag    *string  `json:"stagingTag"`
	ProdTag       *string  `json:"prodTag"`
	NewProdTag    *string  `json:"newProdTag"`
}

func (r statusRow) key() string {
	return r.App + " staging=[" + strings.Join(r.StagingImages, " ") + "] prod=[" + strings.Join(r.ProdImages, " ") + "]"
}

func TestStatusOfMatchesOracle(t *testing.T) {
	rows, err := oracle.Load[statusRow]("status.json")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}

	// DISAGREEMENT #1 -- STATE. See the per-entry notes below.
	//
	// ib.py's status() returns "indeterminate" when the PROD tag has no
	// parseable SHA, because it requires both SHAs before it will compare
	// them. StatusOf requires only the staging SHA and calls the same state
	// out_of_sync.
	//
	// VERDICT: Go is right, and this is a deliberate divergence from ib.py
	// *status*, not from ib.py *promote*. ib.py's promote() accepts exactly
	// this state and promotes from it (see promote_decision.json, where both
	// unparseable-prod rows promote successfully) -- only its status()
	// display is stricter. The state is real and recoverable: the Footstrike
	// 5b cutover recreated the prod Applications, they lost their image pin
	// and fell back to the mutable `latest`/`prod` tags in the repo
	// manifests, and calling that "unknown" is what left bifrost refusing to
	// promote with no way out (bifrost#30). Reproducing ib.py here would
	// re-break that.
	//
	// CONSEQUENCE FOR THE PORT: `ib status` in Go will report out-of-sync
	// where the Python printed nothing conclusive, and `ib status -q` will
	// print the app name and exit 1 where the Python exited 0. That is a
	// visible behaviour change in a scriptable contract and must be called
	// out in Task 2, not discovered by a script.
	//
	// The goNewProdTag on each entry is the tag Go would offer to promote
	// where ib.py's status() offers nothing. It matches what ib.py's own
	// promote() writes for the same cluster state (promote_decision.json),
	// which is what makes this divergence safe.
	type stateDivergence struct {
		python       string // ib.py status()
		golang       string // StatusOf state
		goNewProdTag string
		why          string
	}
	stateDivergences := map[string]stateDivergence{
		"bifrost staging=[us-central1-docker.pkg.dev/ethans-services/containers/bifrost:abc1234] prod=[us-central1-docker.pkg.dev/ethans-services/containers/bifrost:latest]": {
			python: "unknown", golang: "out_of_sync", goNewProdTag: "abc1234",
			why: "prod unpinned on a mutable tag; ib.py status() needs both SHAs, ib.py promote() does not (bifrost#30)",
		},
		"footstrike-dashboard staging=[us-central1-docker.pkg.dev/ethans-services/containers/footstrike-dashboard:abc1234-staging] prod=[us-central1-docker.pkg.dev/ethans-services/containers/footstrike-dashboard:prod]": {
			python: "unknown", golang: "out_of_sync", goNewProdTag: "abc1234-prod",
			why: "same, on the {sha}-{env} tagging scheme",
		},
	}

	// DISAGREEMENT #2 -- TAGS CARRIED ALONGSIDE THE STATE.
	//
	// When one environment is mid-deploy or has no pods, ib.py still prints
	// the other environment's tag; promote.Status zeroes both. The decision
	// is identical -- mid_deploy and unknown are equally un-promotable on
	// both sides -- but the Go struct cannot render the table ib.py renders.
	//
	// VERDICT: ib.py is more useful and Go loses information. Not a promote
	// risk, but Task 2 has to reach past StatusOf for the tags (or widen
	// Status) if `ib status` is to keep printing "prod: abc1234" while
	// staging is rolling. Pinned here so that surfaces as a decision.
	tagsDropped := map[string]string{
		"bifrost staging=[us-central1-docker.pkg.dev/ethans-services/containers/bifrost:abc1234 us-central1-docker.pkg.dev/ethans-services/containers/bifrost:def5678] prod=[us-central1-docker.pkg.dev/ethans-services/containers/bifrost:abc1234]": "mid-deploy on staging; ib.py still shows prod abc1234",
		"bifrost staging=[us-central1-docker.pkg.dev/ethans-services/containers/bifrost:abc1234] prod=[us-central1-docker.pkg.dev/ethans-services/containers/bifrost:abc1234 us-central1-docker.pkg.dev/ethans-services/containers/bifrost:def5678]": "mid-deploy on prod; ib.py still shows staging abc1234",
		"bifrost staging=[us-central1-docker.pkg.dev/ethans-services/containers/bifrost:abc1234 docker.io/library/nginx:1.27] prod=[us-central1-docker.pkg.dev/ethans-services/containers/bifrost:abc1234]":                                          "two containers read as mid-deploy; ib.py still shows prod abc1234",
		"bifrost staging=[] prod=[us-central1-docker.pkg.dev/ethans-services/containers/bifrost:abc1234]":                                                                                                                                            "no staging pods; ib.py still shows prod abc1234",
		"bifrost staging=[us-central1-docker.pkg.dev/ethans-services/containers/bifrost:abc1234] prod=[]":                                                                                                                                            "no prod pods; ib.py still shows staging abc1234",
	}

	for _, r := range rows {
		t.Run(r.key(), func(t *testing.T) {
			got := StatusOf(r.StagingImages, r.ProdImages)

			wantState := r.State
			if d, known := stateDivergences[r.key()]; known {
				if wantState != d.python {
					t.Fatalf("oracle changed: ib.py status = %q, divergence table says %q", wantState, d.python)
				}
				if string(got.State) != d.golang {
					t.Fatalf("known divergence changed: StatusOf = %q, was %q (%s)", got.State, d.golang, d.why)
				}
				if got.NewProdTag != d.goNewProdTag {
					t.Fatalf("known divergence changed: NewProdTag = %q, was %q", got.NewProdTag, d.goNewProdTag)
				}
				if oracle.Str(r.NewProdTag) != "" {
					t.Fatalf("oracle changed: ib.py status now offers to promote %q here", oracle.Str(r.NewProdTag))
				}
				// The tag ib.py could not parse is still reported by both.
				if got.StagingTag != oracle.Str(r.StagingTag) || got.ProdTag != oracle.Str(r.ProdTag) {
					t.Errorf("tags = (%q, %q), ib.py printed (%q, %q)",
						got.StagingTag, got.ProdTag, oracle.Str(r.StagingTag), oracle.Str(r.ProdTag))
				}
				return
			}

			if string(got.State) != wantState {
				t.Errorf("StatusOf state = %q, ib.py status = %q", got.State, wantState)
			}
			if got.NewProdTag != oracle.Str(r.NewProdTag) {
				t.Errorf("NewProdTag = %q, ib.py printed %q", got.NewProdTag, oracle.Str(r.NewProdTag))
			}
			if why, dropped := tagsDropped[r.key()]; dropped {
				if got.StagingTag != "" || got.ProdTag != "" {
					t.Fatalf("known divergence changed: StatusOf now reports tags (%q, %q) where it used to drop them (%s)",
						got.StagingTag, got.ProdTag, why)
				}
				return
			}
			if got.StagingTag != oracle.Str(r.StagingTag) {
				t.Errorf("StagingTag = %q, ib.py printed %q", got.StagingTag, oracle.Str(r.StagingTag))
			}
			if got.ProdTag != oracle.Str(r.ProdTag) {
				t.Errorf("ProdTag = %q, ib.py printed %q", got.ProdTag, oracle.Str(r.ProdTag))
			}
		})
	}
}

// TestPromoteDecisionMatchesOracle checks the decision ib.py actually acts on:
// which tag it writes to prod, for the same cluster state. promote_decision.json
// is captured by running ib.py's real promote() with its kubectl call
// intercepted, so these are the exact strings that would have been patched
// into the ArgoCD Application.
//
// Task 3 asserts the full patch body against the same fixture. This test
// covers the part internal/promote owns today: the tag.
//
// NOTE FOR TASK 3: the patch bodies are semantically identical but NOT byte
// identical. json.dumps separates keys with ": " and ", ", encoding/json
// emits neither, so ib.py writes
//
//	{"spec": {"source": {"kustomize": {"images": ["<base>=<base>:<tag>"]}}}}
//
// where kube.PatchAppImage writes the same object with no spaces. A merge
// patch is parsed, not compared, so the effect on the Application is the
// same -- but the plan's "byte-equivalent to the Python's" has to be read as
// equivalent-after-decoding, or Task 3 will chase whitespace.
func TestPromoteDecisionMatchesOracle(t *testing.T) {
	type row struct {
		App            string   `json:"app"`
		StagingImages  []string `json:"stagingImages"`
		ProdImages     []string `json:"prodImages"`
		Promoted       bool     `json:"promoted"`
		KustomizeImage *string  `json:"kustomizeImage"`
	}
	rows, err := oracle.Load[row]("promote_decision.json")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}

	// The load-bearing half of disagreement #1 in TestStatusOfMatchesOracle:
	// ib.py's status() calls an unparseable prod tag indeterminate, but its
	// promote() promotes from it. If that ever stopped being true, StatusOf's
	// out_of_sync would no longer be backed by the oracle and bifrost#30's
	// reasoning would need revisiting -- so it is asserted, not assumed.
	promotedFrom := map[string]bool{}
	for _, r := range rows {
		promotedFrom[strings.Join(r.ProdImages, " ")] = r.Promoted
	}
	for _, prod := range []string{
		"us-central1-docker.pkg.dev/ethans-services/containers/bifrost:latest",
		"us-central1-docker.pkg.dev/ethans-services/containers/footstrike-dashboard:prod",
	} {
		if !promotedFrom[prod] {
			t.Errorf("ib.py promote() no longer promotes from unparseable prod tag %q", prod)
		}
	}

	for _, r := range rows {
		if !r.Promoted {
			continue
		}
		t.Run(r.App+" "+strings.Join(r.ProdImages, " "), func(t *testing.T) {
			// "<base>=<base>:<tag>" -- take the tag ib.py chose.
			ki := oracle.Str(r.KustomizeImage)
			wantTag := ki[strings.LastIndex(ki, ":")+1:]

			stagingTag := ExtractTag(r.StagingImages[0])
			sha := ExtractSHA(stagingTag)
			prodTag := ExtractTag(r.ProdImages[0])
			got := NewProdTag(sha, stagingTag, prodTag)
			if got != wantTag {
				t.Errorf("promoting %s: Go would write %q, ib.py wrote %q", r.App, got, wantTag)
			}
		})
	}
}

// TestImageBaseMatchesOracle compares the kustomize override KEY.
//
// The two implementations derive it differently and there is no ib.py
// function to import: promote() builds it as REGISTRY + "/" + app, from the
// ArgoCD app name, and never looks at the image that is actually running.
// ImageBase parses it out of the running image instead.
func TestImageBaseMatchesOracle(t *testing.T) {
	type row struct {
		App                  string `json:"app"`
		DeployedImage        string `json:"deployedImage"`
		ImageBaseFromAppName string `json:"imageBaseFromAppName"`
	}
	rows, err := oracle.Load[row]("image_base.json")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}

	// DISAGREEMENT #3 -- only where the image repository is not named after
	// the app.
	//
	// VERDICT: Go is right. ib.py assumes image repo == app name and
	// hardcodes its own registry; when they part company it writes a
	// kustomize override keyed on an image the Deployment does not reference,
	// which kustomize silently ignores -- the promote appears to succeed and
	// prod never moves. Every service in the fleet happens to satisfy the
	// assumption today (which is why this has never bitten), but
	// footstrike-api was called fitness-api until July 2026, so the fleet has
	// been one rename away from it. Do NOT port ib.py's version.
	divergences := map[string]divergence{
		"footstrike-api": {
			python: "us-central1-docker.pkg.dev/ethans-services/containers/footstrike-api",
			golang: "us-central1-docker.pkg.dev/ethans-services/containers/fitness-api",
			why:    "ib.py keys the override off the app name; the image repo is what the Deployment actually references",
		},
		"bifrost@localhost:5000/bifrost:abc1234": {
			python: "us-central1-docker.pkg.dev/ethans-services/containers/bifrost",
			golang: "localhost:5000/bifrost",
			why:    "ib.py hardcodes REGISTRY and cannot express an image from anywhere else",
		},
	}
	keyFor := func(app, image string) string {
		if strings.HasPrefix(image, "us-central1-docker.pkg.dev/") {
			return app
		}
		return app + "@" + image
	}

	for _, r := range rows {
		t.Run(keyFor(r.App, r.DeployedImage), func(t *testing.T) {
			got := ImageBase(r.DeployedImage)
			if d, known := divergences[keyFor(r.App, r.DeployedImage)]; known {
				if r.ImageBaseFromAppName != d.python {
					t.Fatalf("oracle changed: ib.py base = %q, divergence table says %q", r.ImageBaseFromAppName, d.python)
				}
				if got != d.golang {
					t.Fatalf("known divergence changed: ImageBase = %q, was %q (%s)", got, d.golang, d.why)
				}
				return
			}
			if got != r.ImageBaseFromAppName {
				t.Errorf("ImageBase(%q) = %q, ib.py used %q", r.DeployedImage, got, r.ImageBaseFromAppName)
			}
		})
	}
}
