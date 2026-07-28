package preview

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	sigsyaml "sigs.k8s.io/yaml"
)

// loadK8sFixture reads every file under testdata/<repo>/base into a map
// keyed "base/<name>", matching the shape github.FetchK8s returns.
func loadK8sFixture(t *testing.T, repo string) map[string][]byte {
	t.Helper()
	dir := filepath.Join("testdata", repo, "base")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading fixture dir %s: %v", dir, err)
	}
	files := make(map[string][]byte)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading fixture file %s: %v", e.Name(), err)
		}
		files["base/"+e.Name()] = content
	}
	return files
}

// loadEnvConfigFixture parses testdata/<repo>/staging/configmap-env.yaml's
// data map, standing in for the final preview EnvConfig Task 4 computes —
// real staging values make for a realistic golden assertion.
func loadEnvConfigFixture(t *testing.T, repo string) map[string]string {
	t.Helper()
	path := filepath.Join("testdata", repo, "staging", "configmap-env.yaml")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fixture file %s: %v", path, err)
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := sigsyaml.Unmarshal(content, &cm); err != nil {
		t.Fatalf("parsing fixture file %s: %v", path, err)
	}
	return cm.Data
}

// findObject returns the single rendered object matching kind and name, or
// fails the test.
func findObject(t *testing.T, objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	var matches []*unstructured.Unstructured
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			matches = append(matches, o)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("findObject(%s, %s): got %d matches, want 1 (all objects: %v)", kind, name, len(matches), describeAll(objs))
	}
	return matches[0]
}

func describeAll(objs []*unstructured.Unstructured) []string {
	var out []string
	for _, o := range objs {
		out = append(out, o.GetKind()+"/"+o.GetName())
	}
	return out
}

func TestRenderFootstrikeAPI(t *testing.T) {
	envConfig := loadEnvConfigFixture(t, "footstrike-api")
	in := RenderInput{
		Service:    "footstrike-api",
		Tag:        "hae-cadence",
		ShortSHA:   "abc1234",
		K8sFiles:   loadK8sFixture(t, "footstrike-api"),
		EnvConfig:  envConfig,
		SecretName: "footstrike-api-preview-secrets",
	}

	objs, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(objs) == 0 {
		t.Fatal("Render returned no objects")
	}

	const wantNamespace = "preview-hae-cadence"
	for _, o := range objs {
		if got := o.GetNamespace(); got != wantNamespace {
			t.Errorf("%s/%s: namespace = %q, want %q", o.GetKind(), o.GetName(), got, wantNamespace)
		}
	}

	dep := findObject(t, objs, "Deployment", "footstrike-api")

	replicas, found, err := unstructured.NestedInt64(dep.Object, "spec", "replicas")
	if err != nil || !found || replicas != 1 {
		t.Errorf("Deployment replicas = %v (found=%v, err=%v), want 1", replicas, found, err)
	}

	if _, found, _ := unstructured.NestedString(dep.Object, "spec", "template", "spec", "serviceAccountName"); found {
		t.Errorf("Deployment has serviceAccountName set, want absent")
	}

	if _, found, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "volumes"); found {
		t.Errorf("Deployment still has spec.template.spec.volumes, want removed")
	}

	containers, found, err := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
	if err != nil || !found || len(containers) != 1 {
		t.Fatalf("Deployment containers = %v (found=%v, err=%v), want exactly 1", containers, found, err)
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("container[0] is not a map: %T", containers[0])
	}
	if _, hasMounts := container["volumeMounts"]; hasMounts {
		t.Errorf("container still has volumeMounts, want removed")
	}

	wantImage := imageRepoBase + "/footstrike-api:preview-abc1234"
	if gotImage, _, _ := unstructured.NestedString(container, "image"); gotImage != wantImage {
		t.Errorf("container image = %q, want %q", gotImage, wantImage)
	}

	envFrom, found, err := unstructured.NestedSlice(container, "envFrom")
	if err != nil || !found {
		t.Fatalf("container envFrom missing (err=%v)", err)
	}
	wantEnvFromNames := []string{"footstrike-api-config", "footstrike-api-preview-env", "footstrike-api-preview-secrets"}
	if len(envFrom) != len(wantEnvFromNames) {
		t.Fatalf("envFrom has %d entries, want %d: %v", len(envFrom), len(wantEnvFromNames), envFrom)
	}
	for i, entry := range envFrom {
		m, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("envFrom[%d] is not a map: %T", i, entry)
		}
		var name string
		if i < 2 { // the two configMapRef entries
			ref, ok := m["configMapRef"].(map[string]any)
			if !ok {
				t.Fatalf("envFrom[%d] missing configMapRef: %v", i, m)
			}
			name, _ = ref["name"].(string)
		} else { // the trailing secretRef
			ref, ok := m["secretRef"].(map[string]any)
			if !ok {
				t.Fatalf("envFrom[%d] missing secretRef: %v", i, m)
			}
			name, _ = ref["name"].(string)
		}
		if name != wantEnvFromNames[i] {
			t.Errorf("envFrom[%d] name = %q, want %q", i, name, wantEnvFromNames[i])
		}
	}

	// The staging deployment-patch (with its per-var secretKeyRef env
	// entries) is never fetched, and base declares no `env` — so no env
	// entries should appear at all.
	if _, found, _ := unstructured.NestedSlice(container, "env"); found {
		t.Errorf("container has env entries, want none (staging patch not fetched)")
	}

	cron := findObject(t, objs, "CronJob", "footstrike-api-purge")
	suspend, found, err := unstructured.NestedBool(cron.Object, "spec", "suspend")
	if err != nil || !found || !suspend {
		t.Errorf("CronJob spec.suspend = %v (found=%v, err=%v), want true", suspend, found, err)
	}
	// NestedString can't index arrays, so walk to the container manually.
	cronContainers, _, _ := unstructured.NestedSlice(cron.Object, "spec", "jobTemplate", "spec", "template", "spec", "containers")
	if len(cronContainers) != 1 {
		t.Fatalf("CronJob containers = %v, want exactly 1", cronContainers)
	}
	var cronImage string
	if cc, ok := cronContainers[0].(map[string]any); ok {
		cronImage, _ = cc["image"].(string)
	}
	if cronImage != wantImage {
		t.Errorf("CronJob container image = %q, want %q", cronImage, wantImage)
	}

	envCM := findObject(t, objs, "ConfigMap", "footstrike-api-preview-env")
	data, found, err := unstructured.NestedStringMap(envCM.Object, "data")
	if err != nil || !found {
		t.Fatalf("preview-env ConfigMap data missing (err=%v)", err)
	}
	if len(data) != len(envConfig) {
		t.Fatalf("preview-env ConfigMap data has %d entries, want %d", len(data), len(envConfig))
	}
	for k, v := range envConfig {
		if data[k] != v {
			t.Errorf("preview-env ConfigMap data[%q] = %q, want %q", k, data[k], v)
		}
	}

	ing := findObject(t, objs, "Ingress", "footstrike-api-preview")
	wantHost := "footstrike-api-hae-cadence.preview.footstrike.run"
	if class, _, _ := unstructured.NestedString(ing.Object, "spec", "ingressClassName"); class != "nginx" {
		t.Errorf("Ingress ingressClassName = %q, want nginx", class)
	}
	tls, found, err := unstructured.NestedSlice(ing.Object, "spec", "tls")
	if err != nil || !found || len(tls) != 1 {
		t.Fatalf("Ingress tls = %v (found=%v, err=%v), want exactly 1 entry", tls, found, err)
	}
	tlsEntry, ok := tls[0].(map[string]any)
	if !ok {
		t.Fatalf("tls[0] is not a map: %T", tls[0])
	}
	if secretName, _ := tlsEntry["secretName"].(string); secretName != previewTLSSecret {
		t.Errorf("Ingress tls secretName = %q, want %q", secretName, previewTLSSecret)
	}
	hosts, _ := tlsEntry["hosts"].([]any)
	if len(hosts) != 1 || hosts[0] != wantHost {
		t.Errorf("Ingress tls hosts = %v, want [%q]", hosts, wantHost)
	}
	rules, found, err := unstructured.NestedSlice(ing.Object, "spec", "rules")
	if err != nil || !found || len(rules) != 1 {
		t.Fatalf("Ingress rules = %v (found=%v, err=%v), want exactly 1 entry", rules, found, err)
	}
	rule, ok := rules[0].(map[string]any)
	if !ok {
		t.Fatalf("rules[0] is not a map: %T", rules[0])
	}
	if host, _ := rule["host"].(string); host != wantHost {
		t.Errorf("Ingress rule host = %q, want %q", host, wantHost)
	}
	backendName, backendPort := ingressBackend(t, rule)
	if backendName != "footstrike-api" || backendPort != 80 {
		t.Errorf("Ingress backend = %s:%d, want footstrike-api:80", backendName, backendPort)
	}
}

// ingressBackend digs the backend service name/port out of a single
// Ingress rule's first HTTP path.
func ingressBackend(t *testing.T, rule map[string]any) (string, int64) {
	t.Helper()
	httpBlock, ok := rule["http"].(map[string]any)
	if !ok {
		t.Fatalf("rule missing http: %v", rule)
	}
	paths, ok := httpBlock["paths"].([]any)
	if !ok || len(paths) != 1 {
		t.Fatalf("http.paths = %v, want exactly 1 entry", httpBlock["paths"])
	}
	p, ok := paths[0].(map[string]any)
	if !ok {
		t.Fatalf("paths[0] is not a map: %T", paths[0])
	}
	backend, ok := p["backend"].(map[string]any)
	if !ok {
		t.Fatalf("path missing backend: %v", p)
	}
	svc, ok := backend["service"].(map[string]any)
	if !ok {
		t.Fatalf("backend missing service: %v", backend)
	}
	name, _ := svc["name"].(string)
	port, ok := svc["port"].(map[string]any)
	if !ok {
		t.Fatalf("backend service missing port: %v", svc)
	}
	number, _ := port["number"].(int64)
	return name, number
}

func TestRenderDashboard(t *testing.T) {
	in := RenderInput{
		Service:  "footstrike-dashboard",
		Tag:      "hae-cadence",
		ShortSHA: "def5678",
		K8sFiles: loadK8sFixture(t, "footstrike-dashboard"),
		EnvConfig: map[string]string{
			"PUBLIC_API_BASE_URL": "https://footstrike-api-hae-cadence.preview.footstrike.run",
		},
		SecretName: "", // dashboard has no secret
	}

	objs, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	const wantNamespace = "preview-hae-cadence"
	for _, o := range objs {
		if got := o.GetNamespace(); got != wantNamespace {
			t.Errorf("%s/%s: namespace = %q, want %q", o.GetKind(), o.GetName(), got, wantNamespace)
		}
		if o.GetKind() == "CronJob" {
			t.Errorf("dashboard render produced a CronJob, want none")
		}
	}

	dep := findObject(t, objs, "Deployment", "footstrike-dashboard")
	replicas, found, err := unstructured.NestedInt64(dep.Object, "spec", "replicas")
	if err != nil || !found || replicas != 1 {
		t.Errorf("Deployment replicas = %v (found=%v, err=%v), want 1", replicas, found, err)
	}
	if _, found, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "volumes"); found {
		t.Errorf("dashboard Deployment has volumes, want none (base declares none)")
	}

	containers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
	if len(containers) != 1 {
		t.Fatalf("Deployment containers = %v, want exactly 1", containers)
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("container[0] is not a map: %T", containers[0])
	}

	wantImage := imageRepoBase + "/footstrike-dashboard:preview-def5678"
	if gotImage, _, _ := unstructured.NestedString(container, "image"); gotImage != wantImage {
		t.Errorf("container image = %q, want %q", gotImage, wantImage)
	}

	envFrom, found, err := unstructured.NestedSlice(container, "envFrom")
	if err != nil || !found || len(envFrom) != 1 {
		t.Fatalf("envFrom = %v (found=%v, err=%v), want exactly 1 entry (no base configmap, no secret)", envFrom, found, err)
	}
	entry, ok := envFrom[0].(map[string]any)
	if !ok {
		t.Fatalf("envFrom[0] is not a map: %T", envFrom[0])
	}
	if _, hasSecret := entry["secretRef"]; hasSecret {
		t.Errorf("dashboard envFrom has a secretRef, want none")
	}
	ref, ok := entry["configMapRef"].(map[string]any)
	if !ok {
		t.Fatalf("envFrom[0] missing configMapRef: %v", entry)
	}
	if name, _ := ref["name"].(string); name != "footstrike-dashboard-preview-env" {
		t.Errorf("envFrom[0] configMapRef name = %q, want footstrike-dashboard-preview-env", name)
	}

	ing := findObject(t, objs, "Ingress", "footstrike-dashboard-preview")
	wantHost := "footstrike-dashboard-hae-cadence.preview.footstrike.run"
	rules, _, _ := unstructured.NestedSlice(ing.Object, "spec", "rules")
	if len(rules) != 1 {
		t.Fatalf("Ingress rules = %v, want exactly 1", rules)
	}
	rule, ok := rules[0].(map[string]any)
	if !ok {
		t.Fatalf("rules[0] is not a map: %T", rules[0])
	}
	if host, _ := rule["host"].(string); host != wantHost {
		t.Errorf("Ingress rule host = %q, want %q", host, wantHost)
	}
}

func TestRenderRejectsMissingService(t *testing.T) {
	_, err := Render(RenderInput{Tag: "x", ShortSHA: "abc1234", K8sFiles: map[string][]byte{"base/x.yaml": nil}})
	if err == nil {
		t.Fatal("expected an error for missing Service, got nil")
	}
}

func TestRenderRejectsMissingBaseFiles(t *testing.T) {
	_, err := Render(RenderInput{Service: "footstrike-api", Tag: "x", ShortSHA: "abc1234", K8sFiles: map[string][]byte{
		"staging/deployment-patch.yaml": []byte("irrelevant"),
	}})
	if err == nil {
		t.Fatal("expected an error when K8sFiles has no base/ entries, got nil")
	}
}

// TestRenderRejectsContainerNameMismatch covers a failure mode that does NOT
// surface as a kustomize build error: a base Deployment named exactly
// Service (so the deployment-patch's implicit resource-level target matches
// fine) whose pod spec's container is named something else entirely. Without
// the detectBase guard, kustomize's strategic merge would silently add a
// second, incomplete {name, envFrom}-only container alongside the untouched
// original instead of patching it — Render must reject this up front with a
// clear error instead of returning a broken Deployment.
func TestRenderRejectsContainerNameMismatch(t *testing.T) {
	const service = "mismatched-service"
	k8sFiles := map[string][]byte{
		"base/kustomization.yaml": []byte(`
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
  - service.yaml
`),
		"base/deployment.yaml": []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mismatched-service
spec:
  replicas: 2
  selector:
    matchLabels:
      app: mismatched-service
  template:
    metadata:
      labels:
        app: mismatched-service
    spec:
      containers:
        - name: totally-different-container-name
          image: example.com/mismatched-service:latest
`),
		"base/service.yaml": []byte(`
apiVersion: v1
kind: Service
metadata:
  name: mismatched-service
spec:
  selector:
    app: mismatched-service
  ports:
    - port: 80
      targetPort: 8080
`),
	}

	_, err := Render(RenderInput{
		Service:  service,
		Tag:      "test-tag",
		ShortSHA: "abc1234",
		K8sFiles: k8sFiles,
	})
	if err == nil {
		t.Fatal("expected an error for a Deployment whose container name doesn't match Service, got nil")
	}
	if !strings.Contains(err.Error(), service) {
		t.Errorf("error = %q, want it to mention the mismatched service/container name %q", err.Error(), service)
	}
}

// TestRenderRejectsImageNameMismatch covers detectBase's other pre-flight
// guard: a base Deployment whose container is correctly named service (so
// the container-name guard above passes) but whose image doesn't start with
// imageRepoBase+"/"+service+":" must be rejected before rendering. Without
// this guard, the overlay's images: transformer (which matches by
// repository name only, see writeOverlay) would silently no-op for this
// container, and the preview would run under whatever tag the base has
// pinned instead of the branch's freshly built image.
func TestRenderRejectsImageNameMismatch(t *testing.T) {
	const service = "mismatched-image-service"
	k8sFiles := map[string][]byte{
		"base/kustomization.yaml": []byte(`
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
  - service.yaml
`),
		"base/deployment.yaml": []byte(fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
spec:
  replicas: 2
  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
    spec:
      containers:
        - name: %s
          image: us-central1-docker.pkg.dev/ethans-services/containers/some-other-service:latest
`, service, service, service, service)),
		"base/service.yaml": []byte(fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata:
  name: %s
spec:
  selector:
    app: %s
  ports:
    - port: 80
      targetPort: 8080
`, service, service)),
	}

	_, err := Render(RenderInput{
		Service:  service,
		Tag:      "test-tag",
		ShortSHA: "abc1234",
		K8sFiles: k8sFiles,
	})
	if err == nil {
		t.Fatal("expected an error for a base Deployment image that doesn't match the service, got nil")
	}
	wantPrefix := imageRepoBase + "/" + service + ":"
	if !strings.Contains(err.Error(), wantPrefix) {
		t.Errorf("error = %q, want it to mention the expected image prefix %q", err.Error(), wantPrefix)
	}
}

// --- F1: local-only kustomize refs --------------------------------------
//
// The four tests below each plant one hostile reference in a fetched
// base/kustomization.yaml and assert Render rejects it, naming the
// offending ref in the error. TestRenderRejectsRemoteHTTPResource is the
// discriminating one: it proves the guard fires *before* krusty gets a
// chance to dial out, not just that Render happens to return some error.

// TestRenderRejectsRemoteHTTPResource is the discriminating revert-check
// for the SSRF this guard exists to close: without validateLocalKustomizeRefs,
// krusty's fileloader.Load fetches an http(s) resources: entry for real, via
// a plain &http.Client{} with no timeout and no context (Render has none to
// give it) — this exact fixture makes the httptest server *actually receive
// a request* on an unpatched Render. The zero-hits assertion below is what
// makes this test meaningfully different from "Render returned an error for
// some reason": it fails (hits > 0) if the guard is removed, proving it's
// the guard — not a coincidental build error — that stops the fetch.
func TestRenderRejectsRemoteHTTPResource(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: hostile\n"))
	}))
	defer srv.Close()

	ref := srv.URL + "/hostile-resource.yaml"
	k8sFiles := map[string][]byte{
		"base/kustomization.yaml": []byte(fmt.Sprintf(`
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - %s
`, ref)),
	}

	_, err := Render(RenderInput{
		Service:  "hostile-service",
		Tag:      "test-tag",
		ShortSHA: "abc1234",
		K8sFiles: k8sFiles,
	})
	if err == nil {
		t.Fatal("expected an error for a remote http kustomize resource, got nil")
	}
	if !strings.Contains(err.Error(), ref) {
		t.Errorf("error = %q, want it to name the offending ref %q", err.Error(), ref)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("hostile server received %d requests, want 0 — Render must reject before attempting any fetch", got)
	}
}

// TestRenderRejectsGitHubStyleBase covers kustomize's one scheme-less
// remote shorthand (see unsafeKustomizeRef): a bare "github.com/..." base,
// which kustomize's git loader clones without needing an explicit scheme.
func TestRenderRejectsGitHubStyleBase(t *testing.T) {
	const ref = "github.com/eswan18/some-other-repo/base"
	k8sFiles := map[string][]byte{
		"base/kustomization.yaml": []byte(fmt.Sprintf(`
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
bases:
  - %s
`, ref)),
	}

	_, err := Render(RenderInput{
		Service:  "hostile-service",
		Tag:      "test-tag",
		ShortSHA: "abc1234",
		K8sFiles: k8sFiles,
	})
	if err == nil {
		t.Fatal("expected an error for a git-style github.com base, got nil")
	}
	if !strings.Contains(err.Error(), ref) {
		t.Errorf("error = %q, want it to name the offending ref %q", err.Error(), ref)
	}
}

// TestRenderRejectsAbsolutePathComponent covers the absolute-path rejection,
// via the components: field (resources/bases/patches are covered by the
// other tests in this group).
func TestRenderRejectsAbsolutePathComponent(t *testing.T) {
	const ref = "/etc/passwd"
	k8sFiles := map[string][]byte{
		"base/kustomization.yaml": []byte(fmt.Sprintf(`
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
components:
  - %s
`, ref)),
	}

	_, err := Render(RenderInput{
		Service:  "hostile-service",
		Tag:      "test-tag",
		ShortSHA: "abc1234",
		K8sFiles: k8sFiles,
	})
	if err == nil {
		t.Fatal("expected an error for an absolute-path component, got nil")
	}
	if !strings.Contains(err.Error(), ref) {
		t.Errorf("error = %q, want it to name the offending ref %q", err.Error(), ref)
	}
}

// TestRenderRejectsPathTraversalPatch covers the ".." traversal rejection,
// via patches:'s object-with-a-path-field shape (as opposed to
// resources/bases/components' bare-string shape) — pinning that the guard
// reaches into Patch.Path, not just the plain string lists.
func TestRenderRejectsPathTraversalPatch(t *testing.T) {
	const ref = "../../../etc/passwd"
	k8sFiles := map[string][]byte{
		"base/kustomization.yaml": []byte(fmt.Sprintf(`
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
patches:
  - path: %s
`, ref)),
	}

	_, err := Render(RenderInput{
		Service:  "hostile-service",
		Tag:      "test-tag",
		ShortSHA: "abc1234",
		K8sFiles: k8sFiles,
	})
	if err == nil {
		t.Fatal("expected an error for a path-traversal patch, got nil")
	}
	if !strings.Contains(err.Error(), ref) {
		t.Errorf("error = %q, want it to name the offending ref %q", err.Error(), ref)
	}
}

// TestRenderRejectsRemoteHTTPPatchesStrategicMerge covers the legacy
// patchesStrategicMerge field specifically (a bare string list, not
// patches:'s {path: ...} object shape) with a remote http(s) ref, pinning
// that the guard's legacy-field coverage isn't just patchesJson6902.
func TestRenderRejectsRemoteHTTPPatchesStrategicMerge(t *testing.T) {
	const ref = "https://attacker.example/patch.yaml"
	k8sFiles := map[string][]byte{
		"base/kustomization.yaml": []byte(fmt.Sprintf(`
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
patchesStrategicMerge:
  - %s
`, ref)),
	}

	_, err := Render(RenderInput{
		Service:  "hostile-service",
		Tag:      "test-tag",
		ShortSHA: "abc1234",
		K8sFiles: k8sFiles,
	})
	if err == nil {
		t.Fatal("expected an error for a remote patchesStrategicMerge entry, got nil")
	}
	if !strings.Contains(err.Error(), ref) {
		t.Errorf("error = %q, want it to name the offending ref %q", err.Error(), ref)
	}
}
