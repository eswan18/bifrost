package kube

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/eswan18/bifrost/internal/oracle"
)

func TestAppStatusFrom(t *testing.T) {
	cases := []struct {
		name string
		obj  map[string]any
		want AppStatus
	}{
		{
			"full status",
			map[string]any{"status": map[string]any{
				"sync":   map[string]any{"status": "Synced"},
				"health": map[string]any{"status": "Healthy"},
			}},
			AppStatus{SyncStatus: "Synced", HealthStatus: "Healthy"},
		},
		{
			"missing health",
			map[string]any{"status": map[string]any{
				"sync": map[string]any{"status": "OutOfSync"},
			}},
			AppStatus{SyncStatus: "OutOfSync"},
		},
		{"no status at all", map[string]any{"spec": map[string]any{}}, AppStatus{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := appStatusFrom(tc.obj); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestAppStatusDeployedAt: the deploy time is the newest entry in the
// Application's sync history (when the running revision actually went live),
// falling back to the last sync operation's finish time for apps that have
// synced but recorded no history yet, and the zero time when neither exists.
func TestAppStatusDeployedAt(t *testing.T) {
	rfc := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return ts
	}
	cases := []struct {
		name string
		obj  map[string]any
		want time.Time
	}{
		{
			"latest history entry wins",
			map[string]any{"status": map[string]any{
				"history": []any{
					map[string]any{"deployedAt": "2026-06-14T21:42:50Z"},
					map[string]any{"deployedAt": "2026-06-14T21:47:29Z"},
				},
			}},
			rfc("2026-06-14T21:47:29Z"),
		},
		{
			"newest is chosen regardless of array order",
			map[string]any{"status": map[string]any{
				"history": []any{
					map[string]any{"deployedAt": "2026-06-14T21:47:29Z"},
					map[string]any{"deployedAt": "2026-06-14T21:42:50Z"},
				},
			}},
			rfc("2026-06-14T21:47:29Z"),
		},
		{
			"falls back to operationState when history is empty",
			map[string]any{"status": map[string]any{
				"operationState": map[string]any{"finishedAt": "2026-06-10T08:00:00Z"},
			}},
			rfc("2026-06-10T08:00:00Z"),
		},
		{
			"history beats the operationState fallback",
			map[string]any{"status": map[string]any{
				"history":        []any{map[string]any{"deployedAt": "2026-06-14T21:47:29Z"}},
				"operationState": map[string]any{"finishedAt": "2026-06-10T08:00:00Z"},
			}},
			rfc("2026-06-14T21:47:29Z"),
		},
		{
			"unparseable history entries are skipped, fallback used",
			map[string]any{"status": map[string]any{
				"history":        []any{map[string]any{"deployedAt": "not-a-time"}},
				"operationState": map[string]any{"finishedAt": "2026-06-10T08:00:00Z"},
			}},
			rfc("2026-06-10T08:00:00Z"),
		},
		{"no timestamps at all", map[string]any{"status": map[string]any{}}, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := appStatusFrom(tc.obj).DeployedAt; !got.Equal(tc.want) {
				t.Errorf("DeployedAt = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListArgoApps(t *testing.T) {
	gvr := schema.GroupVersionResource{
		Group: "argoproj.io", Version: "v1alpha1", Resource: "applications",
	}
	app := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"sync":   map[string]any{"status": "Synced"},
			"health": map[string]any{"status": "Progressing"},
		},
	}}
	app.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "argoproj.io", Version: "v1alpha1", Kind: "Application",
	})
	app.SetNamespace("argocd")
	app.SetName("foo-staging")

	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "ApplicationList"},
		app,
	)
	c := &client{dyn: dyn, argoNS: "argocd"}

	got, err := c.ListArgoApps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := AppStatus{SyncStatus: "Synced", HealthStatus: "Progressing"}
	if got["foo-staging"] != want {
		t.Errorf("foo-staging = %+v, want %+v", got["foo-staging"], want)
	}
}

func TestPatchAppImage(t *testing.T) {
	gvr := schema.GroupVersionResource{
		Group: "argoproj.io", Version: "v1alpha1", Resource: "applications",
	}
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "argoproj.io", Version: "v1alpha1", Kind: "Application",
	})
	app.SetNamespace("argocd")
	app.SetName("foo-prod")

	scheme := runtime.NewScheme()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{gvr: "ApplicationList"},
		app,
	)
	c := &client{dyn: dyn, argoNS: "argocd"}

	err := c.PatchAppImage(context.Background(), "foo", "prod",
		"reg/foo:abc1234-prod")
	if err != nil {
		t.Fatal(err)
	}

	got, err := dyn.Resource(gvr).Namespace("argocd").
		Get(context.Background(), "foo-prod", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	imgs, _, err := unstructured.NestedStringSlice(
		got.Object, "spec", "source", "kustomize", "images")
	if err != nil {
		t.Fatal(err)
	}
	want := "reg/foo=reg/foo:abc1234-prod"
	if len(imgs) != 1 || imgs[0] != want {
		out, _ := json.Marshal(got.Object)
		t.Errorf("images = %v, want [%q]; full obj = %s", imgs, want, out)
	}
}

// TestPatchAppImageMatchesOracle asserts the write itself against ib.py.
//
// ib.py shells out to `kubectl patch application <app>-prod -n argocd
// --type=merge -p '<json>'`, and testdata/oracle/promote_decision.json holds
// the exact argv and body it would have sent. PatchAppImage does the same
// thing through client-go, so this reconstructs the kubectl invocation the
// dynamic call amounts to — object name, namespace, patch type, body — and
// compares it to the captured one.
//
// The body is compared SEMANTICALLY, by decoding both sides. Python's
// json.dumps separates with ": " and ", " and encoding/json emits neither, so
// the two bodies are not byte-identical and never will be. That is not a
// difference the API server can observe: a merge patch is parsed, not
// compared. Asserting bytes here would fail on whitespace while proving
// nothing about what prod ends up running — and this is the assertion that
// has to be right, because it is the one that decides that.
func TestPatchAppImageMatchesOracle(t *testing.T) {
	type row struct {
		App            string   `json:"app"`
		Promoted       bool     `json:"promoted"`
		KubectlArgv    []string `json:"kubectlArgv"`
		Patch          *string  `json:"patch"`
		KustomizeImage *string  `json:"kustomizeImage"`
	}
	rows, err := oracle.Load[row]("promote_decision.json")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}

	gvr := schema.GroupVersionResource{
		Group: "argoproj.io", Version: "v1alpha1", Resource: "applications",
	}

	checked := 0
	for _, r := range rows {
		if !r.Promoted {
			continue
		}
		t.Run(r.App+" "+oracle.Str(r.KustomizeImage), func(t *testing.T) {
			// The image to patch comes from the fixture ("<base>=<image>"),
			// not from anything computed here: a test that derived it with
			// the same Go code it is checking would agree with any behaviour.
			ki := oracle.Str(r.KustomizeImage)
			eq := strings.Index(ki, "=")
			if eq < 0 {
				t.Fatalf("fixture kustomizeImage %q is not <base>=<image>", ki)
			}
			newImage := ki[eq+1:]

			app := &unstructured.Unstructured{}
			app.SetGroupVersionKind(schema.GroupVersionKind{
				Group: "argoproj.io", Version: "v1alpha1", Kind: "Application",
			})
			app.SetNamespace("argocd")
			app.SetName(r.App + "-prod")

			dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(
				runtime.NewScheme(),
				map[schema.GroupVersionResource]string{gvr: "ApplicationList"},
				app,
			)
			c := &client{dyn: dyn, argoNS: "argocd"}
			if err := c.PatchAppImage(context.Background(), r.App, "prod", newImage); err != nil {
				t.Fatalf("PatchAppImage: %v", err)
			}

			patch := onlyPatchAction(t, dyn.Actions())
			gotArgv := []string{
				"kubectl", "patch", "application", patch.GetName(),
				"-n", patch.GetNamespace(),
				kubectlPatchTypeFlag(patch.GetPatchType()),
				"-p", string(patch.GetPatch()),
			}
			if len(gotArgv) != len(r.KubectlArgv) {
				t.Fatalf("argv shape differs\n got %q\nib.py %q", gotArgv, r.KubectlArgv)
			}
			// Every element is compared literally except the last, the patch
			// body, which is compared after decoding — see the doc comment.
			body := len(gotArgv) - 1
			for i := range body {
				if gotArgv[i] != r.KubectlArgv[i] {
					t.Errorf("argv[%d] = %q, ib.py used %q", i, gotArgv[i], r.KubectlArgv[i])
				}
			}
			assertJSONEqual(t, patch.GetPatch(), []byte(oracle.Str(r.Patch)))
			if gotArgv[body] != r.KubectlArgv[body] {
				t.Logf("patch bodies differ byte-for-byte, as expected (json.dumps spacing):\n go    %s\n ib.py %s",
					gotArgv[body], r.KubectlArgv[body])
			}

			// And the patch has to land: after the merge, the Application
			// carries exactly the override ib.py would have set.
			stored, err := dyn.Resource(gvr).Namespace("argocd").
				Get(context.Background(), r.App+"-prod", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			imgs, _, err := unstructured.NestedStringSlice(
				stored.Object, "spec", "source", "kustomize", "images")
			if err != nil {
				t.Fatal(err)
			}
			if len(imgs) != 1 || imgs[0] != ki {
				t.Errorf("kustomize images = %v, ib.py set [%q]", imgs, ki)
			}
			checked++
		})
	}
	if checked == 0 {
		t.Fatal("no promoted fixture rows; this test asserted nothing")
	}
}

// onlyPatchAction returns the single patch the client issued, failing if there
// was not exactly one — a promote that patched twice, or not at all, is a bug
// this test must not step over.
func onlyPatchAction(t *testing.T, actions []k8stesting.Action) k8stesting.PatchAction {
	t.Helper()
	var found []k8stesting.PatchAction
	for _, a := range actions {
		if p, ok := a.(k8stesting.PatchAction); ok && a.GetVerb() == "patch" {
			found = append(found, p)
		}
	}
	if len(found) != 1 {
		t.Fatalf("recorded %d patch actions, want exactly 1 (all actions: %+v)", len(found), actions)
	}
	return found[0]
}

// kubectlPatchTypeFlag renders a client-go patch type as the kubectl flag that
// means the same thing, so the type is compared against the oracle's argv
// rather than assumed. ib.py sends --type=merge; a Go patch that quietly
// became strategic or apply would change how the override merges.
func kubectlPatchTypeFlag(pt types.PatchType) string {
	switch pt {
	case types.MergePatchType:
		return "--type=merge"
	case types.StrategicMergePatchType:
		return "--type=strategic"
	case types.JSONPatchType:
		return "--type=json"
	case types.ApplyPatchType:
		return "--type=apply"
	default:
		return "--type=" + string(pt)
	}
}

// assertJSONEqual compares two JSON documents by value.
func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("decode Go patch %q: %v", got, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("decode ib.py patch %q: %v", want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("patch body differs after decoding\n go    %s\n ib.py %s", got, want)
	}
}
