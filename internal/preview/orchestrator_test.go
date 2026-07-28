package preview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eswan18/bifrost/internal/config"
	"github.com/eswan18/bifrost/internal/gcb"
	"github.com/eswan18/bifrost/internal/github"
	"github.com/eswan18/bifrost/internal/kube"
	"github.com/eswan18/bifrost/internal/neon"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ---- fakeGitHub -------------------------------------------------------------

// fakeGitHub is an in-package hand-fake for github.Client. members marks
// which repos have branch pushed (BranchSHA succeeds); everything else is
// github.ErrNoBranch unless branchErr overrides it. k8sFiles stands in for
// FetchK8s's tarball extraction, keyed by repo.
type fakeGitHub struct {
	members   map[string]bool
	branchErr map[string]error
	k8sFiles  map[string]map[string][]byte
	fetchErr  map[string]error

	// hook, if set, runs at the top of every BranchSHA call — used by the
	// busy-mutex test to pause an in-flight Up long enough to observe it.
	hook func(repo string)
}

func (f *fakeGitHub) BranchSHA(_ context.Context, repo, _ string) (string, error) {
	if f.hook != nil {
		f.hook(repo)
	}
	if err, ok := f.branchErr[repo]; ok {
		return "", err
	}
	if f.members[repo] {
		return "deadbeef0123456789", nil
	}
	return "", github.ErrNoBranch
}

func (f *fakeGitHub) FetchK8s(_ context.Context, repo, _ string) (map[string][]byte, error) {
	if err, ok := f.fetchErr[repo]; ok {
		return nil, err
	}
	if files, ok := f.k8sFiles[repo]; ok {
		return files, nil
	}
	return map[string][]byte{}, nil
}

// ---- fakeNeon -----------------------------------------------------------------

// fakeNeon is an in-package hand-fake for neon.Client, keyed by projectID.
type fakeNeon struct {
	mu           sync.Mutex
	branches     map[string][]neon.Branch
	listErr      map[string]error
	createErr    map[string]error
	deleteErr    map[string]error
	connErr      map[string]error
	connURI      map[string]string // projectID -> uri to return; default is a synthesized fake uri
	nextBranchID int
}

func (f *fakeNeon) ListBranches(_ context.Context, projectID string) ([]neon.Branch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.listErr[projectID]; ok {
		return nil, err
	}
	out := make([]neon.Branch, len(f.branches[projectID]))
	copy(out, f.branches[projectID])
	return out, nil
}

func (f *fakeNeon) CreateBranch(_ context.Context, projectID, name, _ string) (neon.Branch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.createErr[projectID]; ok {
		return neon.Branch{}, err
	}
	f.nextBranchID++
	b := neon.Branch{ID: fmt.Sprintf("branch-%d", f.nextBranchID), Name: name}
	if f.branches == nil {
		f.branches = map[string][]neon.Branch{}
	}
	f.branches[projectID] = append(f.branches[projectID], b)
	return b, nil
}

func (f *fakeNeon) DeleteBranch(_ context.Context, projectID, branchID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.deleteErr[projectID]; ok {
		return err
	}
	kept := f.branches[projectID][:0]
	for _, b := range f.branches[projectID] {
		if b.ID != branchID {
			kept = append(kept, b)
		}
	}
	f.branches[projectID] = kept
	return nil
}

func (f *fakeNeon) ConnectionURI(_ context.Context, projectID, _, database, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.connErr[projectID]; ok {
		return "", err
	}
	if uri, ok := f.connURI[projectID]; ok {
		return uri, nil
	}
	return "postgres://preview:fakesecret@" + projectID + "/" + database, nil
}

// ---- fakeGCB --------------------------------------------------------------

// fakeGCB is an in-package hand-fake for gcb.Client. RunTrigger's returned
// build ID always equals the triggerID it was given, which keys statuses:
// each GetBuild call for a build pops the next status in its configured
// sequence (the last entry repeats once exhausted); an unconfigured build
// succeeds immediately with a synthesized SHA.
type fakeGCB struct {
	mu        sync.Mutex
	runErr    map[string]error
	getErr    map[string]error
	statuses  map[string][]gcb.BuildStatus
	callCount map[string]int
}

func (f *fakeGCB) LatestBuilds(context.Context) (map[string]gcb.BuildStatus, error) { return nil, nil }

func (f *fakeGCB) RunTrigger(_ context.Context, triggerID, _ string) (string, error) {
	if err, ok := f.runErr[triggerID]; ok {
		return "", err
	}
	return triggerID, nil
}

func (f *fakeGCB) GetBuild(_ context.Context, buildID string) (gcb.BuildStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.getErr[buildID]; ok {
		return gcb.BuildStatus{}, err
	}
	seq, ok := f.statuses[buildID]
	if !ok || len(seq) == 0 {
		return gcb.BuildStatus{Status: "SUCCESS", SHA: buildID + "-sha"}, nil
	}
	if f.callCount == nil {
		f.callCount = map[string]int{}
	}
	idx := f.callCount[buildID]
	if idx >= len(seq) {
		idx = len(seq) - 1
	}
	f.callCount[buildID]++
	return seq[idx], nil
}

// ---- fakeKube -----------------------------------------------------------------

type fakeNamespace struct {
	labels      map[string]string
	annotations map[string]string
}

type copySecretCall struct {
	srcNS, srcName, dstNS, dstName string
	overrides                      map[string][]byte
}

// fakeKube is an in-package hand-fake implementing the full kube.Client
// interface; only the five preview-write methods do anything interesting,
// the rest are unused stubs to satisfy the interface.
type fakeKube struct {
	mu sync.Mutex

	namespaces        map[string]*fakeNamespace
	deletedNamespaces []string
	applyCalls        [][]*unstructured.Unstructured
	copySecretCalls   []copySecretCall
	// annotationHistory records a snapshot of a namespace's merged
	// annotations after every Ensure/AnnotateNamespace call, in order — this
	// is how tests observe bifrost/phase transitions (creating -> ready or
	// creating -> failed), since the final state alone only shows the last one.
	annotationHistory []map[string]string

	ensureErr     error
	annotateErr   error
	applyErr      error
	copySecretErr error
	deleteErr     error
}

func newFakeKube() *fakeKube {
	return &fakeKube{namespaces: map[string]*fakeNamespace{}}
}

func (f *fakeKube) EnsureNamespace(_ context.Context, name string, labels, annotations map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ensureErr != nil {
		return f.ensureErr
	}
	ns, ok := f.namespaces[name]
	if !ok {
		ns = &fakeNamespace{labels: map[string]string{}, annotations: map[string]string{}}
		f.namespaces[name] = ns
	}
	for k, v := range labels {
		ns.labels[k] = v
	}
	for k, v := range annotations {
		ns.annotations[k] = v
	}
	f.annotationHistory = append(f.annotationHistory, copyStringMap(ns.annotations))
	return nil
}

func (f *fakeKube) AnnotateNamespace(_ context.Context, name string, annotations map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.annotateErr != nil {
		return f.annotateErr
	}
	ns, ok := f.namespaces[name]
	if !ok {
		return fmt.Errorf("fakeKube: AnnotateNamespace: namespace %s not found", name)
	}
	for k, v := range annotations {
		ns.annotations[k] = v
	}
	f.annotationHistory = append(f.annotationHistory, copyStringMap(ns.annotations))
	return nil
}

func (f *fakeKube) ApplyObjects(_ context.Context, objs []*unstructured.Unstructured) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applyCalls = append(f.applyCalls, objs)
	return nil
}

func (f *fakeKube) CopySecret(_ context.Context, srcNS, srcName, dstNS, dstName string, overrides map[string][]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.copySecretErr != nil {
		return f.copySecretErr
	}
	f.copySecretCalls = append(f.copySecretCalls, copySecretCall{srcNS, srcName, dstNS, dstName, overrides})
	return nil
}

func (f *fakeKube) DeleteNamespace(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.namespaces, name)
	f.deletedNamespaces = append(f.deletedNamespaces, name)
	return nil
}

// Unused kube.Client methods — plain stubs to satisfy the interface.
func (f *fakeKube) ListPods(context.Context, string) ([]kube.PodInfo, error) { return nil, nil }
func (f *fakeKube) ListArgoApps(context.Context) (map[string]kube.AppStatus, error) {
	return nil, nil
}
func (f *fakeKube) PatchAppImage(context.Context, string, string, string) error { return nil }
func (f *fakeKube) ListCronJobs(context.Context, string) ([]kube.CronJobInfo, error) {
	return nil, nil
}
func (f *fakeKube) ListJobs(context.Context, string) ([]kube.JobInfo, error) { return nil, nil }
func (f *fakeKube) ListReplicaSets(context.Context, string) ([]kube.ReplicaSetInfo, error) {
	return nil, nil
}
func (f *fakeKube) ListNamespaces(context.Context, string) ([]kube.NamespaceInfo, error) {
	return nil, nil
}
func (f *fakeKube) GetNamespace(context.Context, string) (kube.NamespaceInfo, bool, error) {
	return kube.NamespaceInfo{}, false, nil
}

// ---- test fixtures & helpers ------------------------------------------------

// loadFullK8sFixture mirrors what github.FetchK8s really returns for a
// member repo: every base/ file (via render_test.go's loadK8sFixture) plus,
// when present, staging/configmap-env.yaml — the same real fixtures Task 2
// committed, reused here so Render genuinely succeeds end-to-end instead of
// against synthetic YAML.
func loadFullK8sFixture(t *testing.T, repo string) map[string][]byte {
	t.Helper()
	files := loadK8sFixture(t, repo)
	stagingPath := filepath.Join("testdata", repo, "staging", "configmap-env.yaml")
	if content, err := os.ReadFile(stagingPath); err == nil {
		files["staging/configmap-env.yaml"] = content
	}
	return files
}

// testDeps bundles the five fakes plus the Orchestrator wired to them, so
// each test only has to override what it cares about.
type testDeps struct {
	orch   *Orchestrator
	kube   *fakeKube
	github *fakeGitHub
	neon   *fakeNeon
	gcb    *fakeGCB
}

// newTwoMemberDeps sets up a preview whose branch exists for footstrike-api
// and footstrike-dashboard but not identity — a realistic two-repo preview,
// with the dashboard's mandatory triple resolvable via footstrike-api's
// membership (preview URL) and identity's configured staging URL (fallback).
func newTwoMemberDeps(t *testing.T) *testDeps {
	t.Helper()
	cfg := &config.Config{
		PreviewServices: []string{"footstrike-api", "footstrike-dashboard", "identity"},
		NeonProjects: map[string]config.NeonProjectRef{
			"footstrike-api": {ProjectID: "proj-api", Database: "fitnessdb", Role: "fitness_owner"},
		},
		PreviewOAuthClientID: "preview-client-id",
		StagingURLs: map[string]string{
			"identity": "https://identity-staging.tailc06f30.ts.net",
		},
	}
	gh := &fakeGitHub{
		members: map[string]bool{"footstrike-api": true, "footstrike-dashboard": true},
		k8sFiles: map[string]map[string][]byte{
			"footstrike-api":       loadFullK8sFixture(t, "footstrike-api"),
			"footstrike-dashboard": loadFullK8sFixture(t, "footstrike-dashboard"),
		},
	}
	nc := &fakeNeon{}
	gc := &fakeGCB{}
	kc := newFakeKube()
	o := &Orchestrator{
		Cfg:    cfg,
		Kube:   kc,
		GitHub: gh,
		Neon:   nc,
		Builds: gc,
		TriggerIDs: map[string]string{
			"footstrike-api-preview-build":       "trig-api",
			"footstrike-dashboard-preview-build": "trig-dash",
		},
	}
	return &testDeps{orch: o, kube: kc, github: gh, neon: nc, gcb: gc}
}

// ---- Up: happy path -----------------------------------------------------------

func TestUpHappyPathTwoMembers(t *testing.T) {
	d := newTwoMemberDeps(t)

	if err := d.orch.Up(context.Background(), "hae-cadence"); err != nil {
		t.Fatalf("Up failed: %v", err)
	}

	const ns = "preview-hae-cadence"
	got, ok := d.kube.namespaces[ns]
	if !ok {
		t.Fatalf("namespace %s was never created", ns)
	}
	if got.labels["bifrost/preview"] != "true" {
		t.Errorf("label bifrost/preview = %q, want true", got.labels["bifrost/preview"])
	}
	if got.annotations["bifrost/branch"] != "hae-cadence" {
		t.Errorf("bifrost/branch = %q", got.annotations["bifrost/branch"])
	}
	if got.annotations["bifrost/apps"] != "footstrike-api,footstrike-dashboard" {
		t.Errorf("bifrost/apps = %q, want a clean comma join excluding identity", got.annotations["bifrost/apps"])
	}
	if got.annotations["bifrost/phase"] != "ready" {
		t.Errorf("bifrost/phase = %q, want ready", got.annotations["bifrost/phase"])
	}
	if _, hasErr := got.annotations["bifrost/error"]; !hasErr || got.annotations["bifrost/error"] != "" {
		t.Errorf("bifrost/error = %q, want present and empty on success", got.annotations["bifrost/error"])
	}

	// Phase transitions: creating (EnsureNamespace) then ready (final
	// AnnotateNamespace) — asserted via the fake's recorded annotation
	// history, not just the final state.
	if len(d.kube.annotationHistory) < 2 {
		t.Fatalf("annotationHistory has %d entries, want at least 2", len(d.kube.annotationHistory))
	}
	if got := d.kube.annotationHistory[0]["bifrost/phase"]; got != "creating" {
		t.Errorf("first recorded phase = %q, want creating", got)
	}
	if got := d.kube.annotationHistory[len(d.kube.annotationHistory)-1]["bifrost/phase"]; got != "ready" {
		t.Errorf("last recorded phase = %q, want ready", got)
	}

	// Both members applied, each with the right namespace and preview image tag.
	if len(d.kube.applyCalls) != 2 {
		t.Fatalf("ApplyObjects called %d times, want 2", len(d.kube.applyCalls))
	}
	wantImageFragments := []string{"footstrike-api:preview-trig-api-sha", "footstrike-dashboard:preview-trig-dash-sha"}
	for _, frag := range wantImageFragments {
		if !anyObjectHasImage(d.kube.applyCalls, frag) {
			t.Errorf("no applied object carries image fragment %q", frag)
		}
	}
	for _, call := range d.kube.applyCalls {
		for _, o := range call {
			if o.GetNamespace() != ns {
				t.Errorf("%s/%s applied with namespace %q, want %q", o.GetKind(), o.GetName(), o.GetNamespace(), ns)
			}
		}
	}

	// footstrike-api (has a NeonProjectRef) gets its secret copied with a
	// DATABASE_URL override; the dashboard (no ref) does not. Plus one
	// wildcard TLS copy, always.
	if len(d.kube.copySecretCalls) != 2 {
		t.Fatalf("CopySecret called %d times, want 2 (api secret + tls): %+v", len(d.kube.copySecretCalls), d.kube.copySecretCalls)
	}
	apiCall := d.kube.copySecretCalls[0]
	if apiCall.srcNS != "footstrike-api-staging" || apiCall.srcName != "footstrike-api-staging-secrets" ||
		apiCall.dstNS != ns || apiCall.dstName != "footstrike-api-preview-secrets" {
		t.Errorf("api secret copy = %+v, unexpected shape", apiCall)
	}
	wantURI := "postgres://preview:fakesecret@proj-api/fitnessdb"
	if string(apiCall.overrides["DATABASE_URL"]) != wantURI {
		t.Errorf("DATABASE_URL override = %q, want %q", apiCall.overrides["DATABASE_URL"], wantURI)
	}
	tlsCall := d.kube.copySecretCalls[1]
	if tlsCall.srcNS != "previews" || tlsCall.srcName != previewTLSSecret ||
		tlsCall.dstNS != ns || tlsCall.dstName != previewTLSSecret || tlsCall.overrides != nil {
		t.Errorf("tls secret copy = %+v, unexpected shape", tlsCall)
	}

	// The Neon branch was actually created for footstrike-api's project.
	branches := d.neon.branches["proj-api"]
	if len(branches) != 1 || branches[0].Name != "preview-hae-cadence" {
		t.Errorf("proj-api branches = %+v, want exactly one named preview-hae-cadence", branches)
	}
}

func anyObjectHasImage(calls [][]*unstructured.Unstructured, imageFragment string) bool {
	for _, objs := range calls {
		for _, o := range objs {
			if o.GetKind() != "Deployment" {
				continue
			}
			containers, found, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
			if !found {
				continue
			}
			for _, c := range containers {
				m, ok := c.(map[string]any)
				if !ok {
					continue
				}
				image, _ := m["image"].(string)
				if strings.Contains(image, imageFragment) {
					return true
				}
			}
		}
	}
	return false
}

// TestUpIsIdempotentOnRerun exercises "re-running Up updates in place": a
// second Up for the same branch against a fresh set of fakes still primed
// with the first run's state should succeed and leave phase=ready, not
// error out over "already exists" anywhere along the way.
func TestUpIsIdempotentOnRerun(t *testing.T) {
	d := newTwoMemberDeps(t)
	ctx := context.Background()
	if err := d.orch.Up(ctx, "hae-cadence"); err != nil {
		t.Fatalf("first Up failed: %v", err)
	}
	if err := d.orch.Up(ctx, "hae-cadence"); err != nil {
		t.Fatalf("second Up (rerun) failed: %v", err)
	}
	ns := d.kube.namespaces["preview-hae-cadence"]
	if ns.annotations["bifrost/phase"] != "ready" {
		t.Errorf("bifrost/phase after rerun = %q, want ready", ns.annotations["bifrost/phase"])
	}
	if len(d.neon.branches["proj-api"]) != 1 {
		t.Errorf("proj-api branches after rerun = %+v, want still exactly one (scan-then-create, no duplicate)", d.neon.branches["proj-api"])
	}
}

// ---- Up: failure paths --------------------------------------------------------

func TestUpRejectsEmptyBranch(t *testing.T) {
	d := newTwoMemberDeps(t)
	for _, branch := range []string{"", "   "} {
		if err := d.orch.Up(context.Background(), branch); err == nil {
			t.Errorf("Up(%q) = nil, want an error", branch)
		}
	}
	if len(d.kube.namespaces) != 0 {
		t.Errorf("namespaces = %v, want none created", d.kube.namespaces)
	}
}

func TestUpRejectsBranchWithNoUsableTag(t *testing.T) {
	d := newTwoMemberDeps(t)
	// TagForBranch strips every character outside [a-z0-9-]; an all-emoji
	// (or similarly all-invalid) branch name slugs to "".
	if err := d.orch.Up(context.Background(), "!!!"); err == nil {
		t.Fatal("expected an error for a branch that slugs to an empty tag, got nil")
	}
}

func TestUpNeonCreateBranchFailureFailsTheRun(t *testing.T) {
	d := newTwoMemberDeps(t)
	d.neon.createErr = map[string]error{"proj-api": errors.New("neon: quota exceeded")}

	err := d.orch.Up(context.Background(), "hae-cadence")
	if err == nil {
		t.Fatal("expected an error when Neon branch creation fails, got nil")
	}
	if !strings.Contains(err.Error(), "footstrike-api") {
		t.Errorf("error = %q, want it to name the service", err.Error())
	}
	ns := d.kube.namespaces["preview-hae-cadence"]
	if ns.annotations["bifrost/phase"] != "failed" {
		t.Errorf("bifrost/phase = %q, want failed", ns.annotations["bifrost/phase"])
	}
	if len(d.kube.copySecretCalls) != 0 {
		t.Errorf("CopySecret called %d times, want 0 (Neon branch creation failed first)", len(d.kube.copySecretCalls))
	}
}

func TestUpFinalReadyAnnotateFailureIsReturned(t *testing.T) {
	d := newTwoMemberDeps(t)
	kc := d.kube
	// Fail only the very last AnnotateNamespace call (the "ready" one): flip
	// annotateErr on right before Up would issue it isn't feasible without a
	// hook, so instead assert indirectly by making every AnnotateNamespace
	// call fail — the build/render stages never call AnnotateNamespace on
	// the happy path (only fail() and the final ready call do), so this
	// still isolates the final-ready branch specifically.
	kc.annotateErr = errors.New("annotate: etcd unavailable")

	err := d.orch.Up(context.Background(), "hae-cadence")
	if err == nil {
		t.Fatal("expected an error when the final ready AnnotateNamespace call fails, got nil")
	}
	if !strings.Contains(err.Error(), "mark ready") {
		t.Errorf("error = %q, want it to mention marking the namespace ready", err.Error())
	}
}

func TestUpBuildFailureSetsPhaseFailed(t *testing.T) {
	d := newTwoMemberDeps(t)
	d.gcb.statuses = map[string][]gcb.BuildStatus{
		"trig-api": {{Status: "FAILURE"}},
	}

	err := d.orch.Up(context.Background(), "hae-cadence")
	if err == nil {
		t.Fatal("expected Up to fail when a build fails, got nil")
	}
	if !strings.Contains(err.Error(), "footstrike-api") {
		t.Errorf("error = %q, want it to name the failing service", err.Error())
	}

	ns, ok := d.kube.namespaces["preview-hae-cadence"]
	if !ok {
		t.Fatal("namespace should exist (EnsureNamespace ran before the build stage)")
	}
	if ns.annotations["bifrost/phase"] != "failed" {
		t.Errorf("bifrost/phase = %q, want failed", ns.annotations["bifrost/phase"])
	}
	if ns.annotations["bifrost/error"] == "" {
		t.Error("bifrost/error is empty, want a message describing the build failure")
	}
	if strings.Contains(ns.annotations["bifrost/error"], "postgres://") {
		t.Errorf("bifrost/error leaks what looks like a secret URI: %q", ns.annotations["bifrost/error"])
	}

	// Failure happened before the Neon/secrets stages — neither should have run.
	if len(d.neon.branches["proj-api"]) != 0 {
		t.Errorf("proj-api branches = %+v, want none created (build failed first)", d.neon.branches["proj-api"])
	}
	if len(d.kube.copySecretCalls) != 0 {
		t.Errorf("CopySecret called %d times, want 0 (build failed first)", len(d.kube.copySecretCalls))
	}
}

func TestUpBuildPollRespectsContextCancellation(t *testing.T) {
	d := newTwoMemberDeps(t)
	// footstrike-api's build never leaves WORKING; footstrike-dashboard's
	// trigger is never even reached (api is processed first).
	d.gcb.statuses = map[string][]gcb.BuildStatus{
		"trig-api": {{Status: "WORKING"}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := d.orch.Up(ctx, "hae-cadence")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected Up to fail when the context is cancelled mid-poll, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	// buildPollInterval defaults to 10s; if this took anywhere near that,
	// the poll loop isn't actually selecting on ctx.Done().
	if elapsed > 2*time.Second {
		t.Errorf("Up took %v to return after context cancellation, want well under the 10s poll interval", elapsed)
	}

	ns := d.kube.namespaces["preview-hae-cadence"]
	if ns.annotations["bifrost/phase"] != "failed" {
		t.Errorf("bifrost/phase = %q, want failed", ns.annotations["bifrost/phase"])
	}
}

func TestUpPollsMultipleTimesBeforeSucceeding(t *testing.T) {
	old := buildPollInterval
	buildPollInterval = time.Millisecond
	defer func() { buildPollInterval = old }()

	d := newTwoMemberDeps(t)
	d.gcb.statuses = map[string][]gcb.BuildStatus{
		"trig-api": {{Status: "QUEUED"}, {Status: "WORKING"}, {Status: "SUCCESS", SHA: "realsha7"}},
	}

	if err := d.orch.Up(context.Background(), "hae-cadence"); err != nil {
		t.Fatalf("Up failed: %v", err)
	}
	if !anyObjectHasImage(d.kube.applyCalls, "footstrike-api:preview-realsha7") {
		t.Error("rendered footstrike-api image doesn't carry the SHA from the final poll, want realsha7")
	}
}

func TestUpDashboardWithoutClientIDErrors(t *testing.T) {
	cfg := &config.Config{
		PreviewServices: []string{"footstrike-api", "footstrike-dashboard", "identity"},
		StagingURLs: map[string]string{
			"footstrike-api": "https://api.staging.footstrike.run",
			"identity":       "https://identity-staging.tailc06f30.ts.net",
		},
		// PreviewOAuthClientID intentionally left empty.
	}
	gh := &fakeGitHub{members: map[string]bool{"footstrike-dashboard": true}}
	kc := newFakeKube()
	o := &Orchestrator{Cfg: cfg, Kube: kc, GitHub: gh, Neon: &fakeNeon{}, Builds: &fakeGCB{}}

	err := o.Up(context.Background(), "dash-only-branch")
	if err == nil {
		t.Fatal("expected an error for a dashboard preview with no PreviewOAuthClientID, got nil")
	}
	if !strings.Contains(err.Error(), "APP_OAUTH_CLIENT_ID") && !strings.Contains(err.Error(), "PreviewOAuthClientID") {
		t.Errorf("error = %q, want it to mention the unresolved APP_OAUTH_CLIENT_ID", err.Error())
	}
	if len(kc.namespaces) != 0 {
		t.Errorf("namespaces = %v, want none created — this must fail before touching the cluster", kc.namespaces)
	}
}

func TestUpNoMembersErrors(t *testing.T) {
	d := newTwoMemberDeps(t)
	d.github.members = map[string]bool{} // no repo has this branch

	err := d.orch.Up(context.Background(), "nonexistent-branch")
	if err == nil {
		t.Fatal("expected an error when no service has the branch, got nil")
	}
	if len(d.kube.namespaces) != 0 {
		t.Errorf("namespaces = %v, want none created", d.kube.namespaces)
	}
}

func TestUpAbortsOnNonNotFoundMembershipError(t *testing.T) {
	d := newTwoMemberDeps(t)
	d.github.branchErr = map[string]error{"footstrike-api": errors.New("github: rate limited")}

	err := d.orch.Up(context.Background(), "hae-cadence")
	if err == nil {
		t.Fatal("expected Up to abort on a non-ErrNoBranch membership error, got nil")
	}
	if !strings.Contains(err.Error(), "footstrike-api") {
		t.Errorf("error = %q, want it to name the service whose lookup failed", err.Error())
	}
	if len(d.kube.namespaces) != 0 {
		t.Errorf("namespaces = %v, want none created — membership failures abort before the cluster is touched", d.kube.namespaces)
	}
}

// TestUpNeonSecretNeverLeaksIntoErrorAnnotation exercises the "never
// log/annotate secret values" constraint end-to-end: the Neon connection URI
// (a real secret — it embeds DB credentials) successfully resolves, but the
// downstream secret copy then fails; the resulting bifrost/error must not
// contain any trace of it.
func TestUpNeonSecretNeverLeaksIntoErrorAnnotation(t *testing.T) {
	d := newTwoMemberDeps(t)
	const secretURI = "postgres://realuser:hunter2@proj-api.neon.tech/fitnessdb"
	d.neon.connURI = map[string]string{"proj-api": secretURI}
	d.kube.copySecretErr = errors.New("secret copy: connection refused")

	err := d.orch.Up(context.Background(), "hae-cadence")
	if err == nil {
		t.Fatal("expected Up to fail when CopySecret fails, got nil")
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), secretURI) {
		t.Fatalf("returned error leaks the Neon connection URI: %q", err.Error())
	}
	ns := d.kube.namespaces["preview-hae-cadence"]
	if strings.Contains(ns.annotations["bifrost/error"], "hunter2") || strings.Contains(ns.annotations["bifrost/error"], secretURI) {
		t.Fatalf("bifrost/error leaks the Neon connection URI: %q", ns.annotations["bifrost/error"])
	}
}

func TestUpFailureJoinsAnnotateErrorRatherThanSwallowingIt(t *testing.T) {
	d := newTwoMemberDeps(t)
	d.gcb.statuses = map[string][]gcb.BuildStatus{"trig-api": {{Status: "FAILURE"}}}
	d.kube.annotateErr = errors.New("annotate: etcd unavailable")

	err := d.orch.Up(context.Background(), "hae-cadence")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "footstrike-api") {
		t.Errorf("error = %q, want the original build failure preserved", err.Error())
	}
	if !strings.Contains(err.Error(), "etcd unavailable") {
		t.Errorf("error = %q, want the annotate failure joined in, not swallowed", err.Error())
	}
}

func TestUpRenderStageFetchK8sFailureFailsTheRun(t *testing.T) {
	d := newTwoMemberDeps(t)
	d.github.fetchErr = map[string]error{"footstrike-api": errors.New("github: tarball 500")}

	err := d.orch.Up(context.Background(), "hae-cadence")
	if err == nil {
		t.Fatal("expected an error when fetching a member's k8s/ tree fails, got nil")
	}
	if !strings.Contains(err.Error(), "footstrike-api") {
		t.Errorf("error = %q, want it to name the service", err.Error())
	}
	ns := d.kube.namespaces["preview-hae-cadence"]
	if ns.annotations["bifrost/phase"] != "failed" {
		t.Errorf("bifrost/phase = %q, want failed", ns.annotations["bifrost/phase"])
	}
	if len(d.kube.applyCalls) != 0 {
		t.Errorf("ApplyObjects called %d times, want 0", len(d.kube.applyCalls))
	}
}

// ---- Down -----------------------------------------------------------------

func TestDownDeletesNamespaceAndBestEffortDeletesNeonBranches(t *testing.T) {
	cfg := &config.Config{
		NeonProjects: map[string]config.NeonProjectRef{
			"footstrike-api": {ProjectID: "proj-api", Database: "fitnessdb", Role: "fitness_owner"},
			"identity":       {ProjectID: "proj-identity", Database: "identitydb", Role: "identity_owner"},
		},
	}
	kc := newFakeKube()
	kc.namespaces["preview-hae-cadence"] = &fakeNamespace{
		labels:      map[string]string{"bifrost/preview": "true"},
		annotations: map[string]string{"bifrost/phase": "ready"},
	}
	nc := &fakeNeon{
		branches: map[string][]neon.Branch{
			"proj-identity": {{ID: "br-1", Name: "preview-hae-cadence"}},
		},
		// footstrike-api sorts before identity, so this proves a failure on
		// the first-processed project doesn't stop the second from being
		// attempted (best-effort, not fail-fast).
		listErr: map[string]error{"proj-api": errors.New("neon: proj-api unavailable")},
	}
	o := &Orchestrator{Cfg: cfg, Kube: kc, Neon: nc}

	err := o.Down(context.Background(), "hae-cadence")
	if err == nil {
		t.Fatal("expected a joined error from the proj-api Neon failure, got nil")
	}
	if !strings.Contains(err.Error(), "proj-api") {
		t.Errorf("error = %q, want it to mention proj-api", err.Error())
	}

	if len(kc.deletedNamespaces) != 1 || kc.deletedNamespaces[0] != "preview-hae-cadence" {
		t.Errorf("deletedNamespaces = %v, want namespace deleted despite the later Neon error", kc.deletedNamespaces)
	}
	if _, stillPresent := kc.namespaces["preview-hae-cadence"]; stillPresent {
		t.Error("namespace still present after Down")
	}
	if len(nc.branches["proj-identity"]) != 0 {
		t.Errorf("proj-identity branches = %v, want the preview-hae-cadence branch deleted", nc.branches["proj-identity"])
	}
}

func TestDownChecksEveryConfiguredNeonProjectNotJustCurrentMembers(t *testing.T) {
	// Down takes only a tag, never a member list — by construction it can't
	// scope itself to "current members" even if it wanted to, which is
	// exactly the point: a re-created preview may have changed which
	// services it includes since a stray branch was created for one of them.
	cfg := &config.Config{
		NeonProjects: map[string]config.NeonProjectRef{
			"identity": {ProjectID: "proj-identity", Database: "identitydb", Role: "identity_owner"},
		},
	}
	kc := newFakeKube()
	nc := &fakeNeon{branches: map[string][]neon.Branch{
		"proj-identity": {{ID: "br-1", Name: "preview-orphan"}},
	}}
	o := &Orchestrator{Cfg: cfg, Kube: kc, Neon: nc}

	if err := o.Down(context.Background(), "orphan"); err != nil {
		t.Fatalf("Down failed: %v", err)
	}
	if len(nc.branches["proj-identity"]) != 0 {
		t.Errorf("proj-identity branches = %v, want the orphaned branch deleted even though identity was never listed as a member anywhere", nc.branches["proj-identity"])
	}
}

func TestDownDeleteNamespaceFailureStillAttemptsNeonCleanup(t *testing.T) {
	cfg := &config.Config{
		NeonProjects: map[string]config.NeonProjectRef{
			"footstrike-api": {ProjectID: "proj-api", Database: "fitnessdb", Role: "fitness_owner"},
		},
	}
	kc := newFakeKube()
	kc.deleteErr = errors.New("namespace delete: forbidden")
	nc := &fakeNeon{branches: map[string][]neon.Branch{"proj-api": {{ID: "br-1", Name: "preview-hae-cadence"}}}}
	o := &Orchestrator{Cfg: cfg, Kube: kc, Neon: nc}

	err := o.Down(context.Background(), "hae-cadence")
	if err == nil {
		t.Fatal("expected a joined error from the namespace delete failure, got nil")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("error = %q, want it to mention the namespace delete failure", err.Error())
	}
	if len(nc.branches["proj-api"]) != 0 {
		t.Errorf("proj-api branches = %v, want the Neon branch deleted despite the namespace delete failure", nc.branches["proj-api"])
	}
}

func TestDownNoMatchingBranchIsNotAnError(t *testing.T) {
	cfg := &config.Config{
		NeonProjects: map[string]config.NeonProjectRef{
			"footstrike-api": {ProjectID: "proj-api", Database: "fitnessdb", Role: "fitness_owner"},
		},
	}
	kc := newFakeKube()
	nc := &fakeNeon{branches: map[string][]neon.Branch{"proj-api": {{ID: "br-1", Name: "some-other-branch"}}}}
	o := &Orchestrator{Cfg: cfg, Kube: kc, Neon: nc}

	if err := o.Down(context.Background(), "hae-cadence"); err != nil {
		t.Fatalf("Down failed: %v", err)
	}
	if len(nc.branches["proj-api"]) != 1 {
		t.Errorf("proj-api branches = %v, want the unrelated branch left alone", nc.branches["proj-api"])
	}
}

// ---- Busy -------------------------------------------------------------------

func TestBusyMutex(t *testing.T) {
	d := newTwoMemberDeps(t)
	d.github.members = map[string]bool{"footstrike-api": true} // single member, simpler
	d.orch.Cfg.PreviewServices = []string{"footstrike-api"}

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	d.github.hook = func(repo string) {
		if repo != "footstrike-api" {
			return
		}
		once.Do(func() { close(started) })
		<-release
	}

	tag := TagForBranch("busy-branch")
	if d.orch.Busy(tag) {
		t.Fatal("Busy(tag) = true before Up ever ran")
	}

	done := make(chan error, 1)
	go func() { done <- d.orch.Up(context.Background(), "busy-branch") }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Up to reach the blocking hook")
	}

	if !d.orch.Busy(tag) {
		t.Error("Busy(tag) = false while Up is in flight, want true")
	}
	if err := d.orch.Up(context.Background(), "busy-branch"); !errors.Is(err, ErrBusy) {
		t.Errorf("concurrent Up = %v, want ErrBusy", err)
	}
	if err := d.orch.Down(context.Background(), tag); !errors.Is(err, ErrBusy) {
		t.Errorf("concurrent Down = %v, want ErrBusy", err)
	}

	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Up failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Up to finish after release")
	}

	if d.orch.Busy(tag) {
		t.Error("Busy(tag) = true after Up finished, want false")
	}
}

func TestBusyMutexDoesNotBlockUnrelatedTags(t *testing.T) {
	d := newTwoMemberDeps(t)
	if d.orch.Busy("some-other-tag") {
		t.Error("Busy(unrelated tag) = true, want false")
	}
}
