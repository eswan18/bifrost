package preview

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/eswan18/bifrost/internal/config"
)

// These cover a repo whose manifests are not in k8s/base and k8s/staging.
// haruspex is the one such repo: its trees are k8s/haruspex-base and
// k8s/haruspex-staging, left that way by the forecasting -> haruspex rename.
// Every other fixture here was already in the fleet's layout, which is how a
// renderer that could not find haruspex's manifests at all went unnoticed.

// loadK8sTreeFixture reads everything under testdata/<repo> into a map keyed
// by its path relative to that directory, which is the shape github.FetchK8s
// returns for a repo's whole k8s/ tree. Unlike loadFullK8sFixture it assumes
// nothing about what the directories are called.
func loadK8sTreeFixture(t *testing.T, repo string) map[string][]byte {
	t.Helper()
	root := filepath.Join("testdata", repo)
	files := make(map[string][]byte)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = content
		return nil
	})
	if err != nil {
		t.Fatalf("reading fixture tree %s: %v", root, err)
	}
	if len(files) == 0 {
		t.Fatalf("fixture tree %s is empty", root)
	}
	return files
}

// newHaruspexMemberDeps sets up a single-member preview of haruspex, fed
// haruspex's real k8s/ tree and the real registry.
func newHaruspexMemberDeps(t *testing.T) *testDeps {
	t.Helper()
	gh := &fakeGitHub{
		members:  map[string]bool{"haruspex": true},
		k8sFiles: map[string]map[string][]byte{"haruspex": loadK8sTreeFixture(t, "haruspex")},
	}
	nc := &fakeNeon{}
	nc.seedRegistryNeonBranches(t)
	gc := &fakeGCB{}
	kc := newFakeKube()
	o := &Orchestrator{
		Cfg:        &config.Config{PreviewOAuthClientID: "preview-client-id"},
		Kube:       kc,
		GitHub:     gh,
		Neon:       nc,
		Builds:     gc,
		Registry:   testRegistry(t),
		Fleet:      testFleet(t),
		TriggerIDs: map[string]string{"haruspex-preview-build": "trig-haruspex"},
	}
	return &testDeps{orch: o, kube: kc, github: gh, neon: nc, gcb: gc}
}

// appliedConfigMapData finds the ConfigMap named name among everything Up
// handed to ApplyObjects and returns its data.
func appliedConfigMapData(t *testing.T, calls [][]*unstructured.Unstructured, name string) map[string]string {
	t.Helper()
	for _, objs := range calls {
		for _, o := range objs {
			if o.GetKind() != "ConfigMap" || o.GetName() != name {
				continue
			}
			data, _, err := unstructured.NestedStringMap(o.Object, "data")
			if err != nil {
				t.Fatalf("ConfigMap %s data: %v", name, err)
			}
			return data
		}
	}
	t.Fatalf("no ConfigMap named %s among the applied objects", name)
	return nil
}

// TestUpPreviewsHaruspexFromItsOwnDirectories is the whole point, end to end:
// a preview of haruspex comes up from the tree haruspex really has. Two
// things have to hold, and they fail differently. The base is the loud one --
// without it Render has nothing to build. The staging ConfigMap is the quiet
// one: a tree with no staging/configmap-env.yaml is legitimate (the
// dashboard), so missing haruspex's would have rendered a preview with none
// of staging's config rather than failing.
func TestUpPreviewsHaruspexFromItsOwnDirectories(t *testing.T) {
	d := newHaruspexMemberDeps(t)
	entry := d.orch.Registry["haruspex"]
	if len(entry.Migrate) == 0 {
		t.Fatal("registry.yaml gives haruspex no migrate command; this test expects the one it has had since onboarding")
	}

	if err := d.orch.Up(context.Background(), "voided-props", UpOptions{}); err != nil {
		t.Fatalf("Up failed: %v", err)
	}

	// The base was found and patched: the Deployment is haruspex's own, with
	// the registry's migrate step on it.
	dep := appliedDeploymentObject(t, d.kube.applyCalls, "haruspex")
	initContainers, _, err := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "initContainers")
	if err != nil || len(initContainers) != 1 {
		t.Fatalf("initContainers = %v (err=%v), want exactly the migrate step", initContainers, err)
	}
	migrate, _ := initContainers[0].(map[string]any)
	command, _, _ := unstructured.NestedStringSlice(migrate, "command")
	if !reflect.DeepEqual(command, entry.Migrate) {
		t.Errorf("migrate command = %v, want the registry's %v", command, entry.Migrate)
	}

	// Staging's config was found and carried over: a key the registry says
	// nothing about keeps staging's value, and one it templates is rendered
	// over staging's.
	env := appliedConfigMapData(t, d.kube.applyCalls, previewEnvConfigMapName("haruspex"))
	if got := env["IDP_AUDIENCE"]; got != "ethanswan:haruspex" {
		t.Errorf("IDP_AUDIENCE = %q, want staging's %q: haruspex's staging config did not reach the preview", got, "ethanswan:haruspex")
	}
	wantURL := "https://" + previewHost("haruspex", "voided-props")
	if got := env["APP_BASE_URL"]; got != wantURL {
		t.Errorf("APP_BASE_URL = %q, want the preview's own %q", got, wantURL)
	}
	if strings.Contains(env["APP_BASE_URL"], "staging") {
		t.Errorf("APP_BASE_URL = %q still points at staging", env["APP_BASE_URL"])
	}
}

// haruspexRenderInput is a Render request for haruspex's real tree, with the
// base directory the registry names for it.
func haruspexRenderInput(t *testing.T) RenderInput {
	t.Helper()
	return RenderInput{
		Service:  "haruspex",
		Tag:      "voided-props",
		ShortSHA: "abc1234",
		K8sFiles: loadK8sTreeFixture(t, "haruspex"),
		BaseDir:  "haruspex-base",
	}
}

func TestRenderBuildsFromTheNamedBaseDir(t *testing.T) {
	objs, err := Render(haruspexRenderInput(t))
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	for _, o := range objs {
		if o.GetKind() == "Deployment" && o.GetName() == "haruspex" {
			return
		}
	}
	t.Fatalf("no haruspex Deployment among the %d rendered objects", len(objs))
}

// TestRenderNamesTheBaseDirItCouldNotFind: when the registry names a
// directory the branch does not have, the error has to say which, or the
// reader goes looking in base/.
func TestRenderNamesTheBaseDirItCouldNotFind(t *testing.T) {
	in := haruspexRenderInput(t)
	in.BaseDir = "somewhere-else"
	_, err := Render(in)
	if err == nil {
		t.Fatal("expected an error when the named base directory has no files, got nil")
	}
	if !strings.Contains(err.Error(), "somewhere-else/") {
		t.Errorf("error = %q, want it to name the directory it looked in", err.Error())
	}
}

// TestRenderIgnoresBaseWhenAnotherDirIsNamed: once the base lives elsewhere,
// a directory that happens to be called base/ is one more sibling Render
// never reads, like staging/ or prod/. This one would break the build if it
// were read (it lists a resource that does not exist) and would be refused if
// it were scanned (the resource is remote).
func TestRenderIgnoresBaseWhenAnotherDirIsNamed(t *testing.T) {
	in := haruspexRenderInput(t)
	in.K8sFiles["base/kustomization.yaml"] = []byte(`
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - https://example.invalid/not-read.yaml
`)
	if _, err := Render(in); err != nil {
		t.Fatalf("Render failed on a stray base/ it should not have read: %v", err)
	}
}

// TestRenderRejectsARemoteResourceInTheNamedBaseDir: the guard against a
// hostile kustomization (see validateLocalKustomizeRefs) has to follow the
// base wherever it is. Checking only base/ while building from another
// directory would let a branch's kustomization there fetch whatever it liked.
func TestRenderRejectsARemoteResourceInTheNamedBaseDir(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: hostile\n"))
	}))
	defer srv.Close()

	ref := srv.URL + "/hostile-resource.yaml"
	_, err := Render(RenderInput{
		Service:  "hostile-service",
		Tag:      "test-tag",
		ShortSHA: "abc1234",
		BaseDir:  "hostile-base",
		K8sFiles: map[string][]byte{
			"hostile-base/kustomization.yaml": []byte(fmt.Sprintf(`
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - %s
`, ref)),
		},
	})
	if err == nil {
		t.Fatal("expected an error for a remote kustomize resource in the named base directory, got nil")
	}
	if !strings.Contains(err.Error(), ref) {
		t.Errorf("error = %q, want it to name the offending ref %q", err.Error(), ref)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("hostile server received %d requests, want 0: Render must reject before fetching anything", got)
	}
}

// TestParseStagingEnvReadsTheNamedDir: the staging ConfigMap is read from the
// directory named, and a staging/ beside it is not a fallback.
func TestParseStagingEnvReadsTheNamedDir(t *testing.T) {
	files := loadK8sTreeFixture(t, "haruspex")
	files["staging/configmap-env.yaml"] = []byte("apiVersion: v1\nkind: ConfigMap\ndata:\n  ENV: decoy\n")

	got, err := parseStagingEnv(files, "haruspex-staging")
	if err != nil {
		t.Fatalf("parseStagingEnv failed: %v", err)
	}
	if got["ENV"] != "staging" || got["PUBSUB_TOPIC"] != "haruspex-events-staging" {
		t.Errorf("parseStagingEnv = %v, want haruspex-staging/configmap-env.yaml's data", got)
	}
}
