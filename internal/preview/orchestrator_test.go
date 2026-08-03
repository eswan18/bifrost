package preview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	// shas overrides what BranchSHA reports for a member repo; anything
	// unset reports defaultBranchSHA. This is how an auto-update test moves
	// a branch forward — the whole feature is a comparison against this
	// value, so a fake that always answered the same thing would make every
	// one of those tests inert.
	shas map[string]string

	// hook, if set, runs at the top of every BranchSHA call — used by the
	// busy-mutex test to pause an in-flight Up long enough to observe it.
	hook func(repo string)
}

// defaultBranchSHA is what a member repo's branch points at unless a test
// says otherwise.
const defaultBranchSHA = "deadbeef0123456789"

func (f *fakeGitHub) BranchSHA(_ context.Context, repo, _ string) (string, error) {
	if f.hook != nil {
		f.hook(repo)
	}
	if err, ok := f.branchErr[repo]; ok {
		return "", err
	}
	if f.members[repo] {
		if sha, ok := f.shas[repo]; ok {
			return sha, nil
		}
		return defaultBranchSHA, nil
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
	mu        sync.Mutex
	branches  map[string][]neon.Branch
	listErr   map[string]error
	createErr map[string]error
	deleteErr map[string]error
	// deleteErrOnce fails the FIRST DeleteBranch for a project and then clears
	// itself, leaving later deletes working. It models the failure the orphan
	// sweep exists for: a teardown whose Neon half didn't land, leaving a
	// branch behind that a subsequent pass has to reclaim.
	deleteErrOnce map[string]error
	connErr       map[string]error
	connURI       map[string]string // projectID -> uri to return; default is a synthesized fake uri
	nextBranchID  int
	// listCalls counts ListBranches calls per project — how a test asserts the
	// orphan sweep lists a project ONCE even when two registry services share
	// it, which no assertion on the resulting deletions can distinguish (a
	// second pass over the same project deletes nothing either way).
	listCalls map[string]int
	// createCalls records every CreateBranch, in order, WITH the parentID it
	// was given. The resulting branch list can't stand in for this: a branch
	// records no parent, so "cut from staging" and "cut from the project
	// default" are indistinguishable after the fact — which is exactly the
	// bug preview.neon.parent exists to fix.
	createCalls []neonCreateCall
}

// neonCreateCall is one recorded CreateBranch, parentID included.
type neonCreateCall struct {
	project  string
	name     string
	parentID string
}

func (f *fakeNeon) ListBranches(_ context.Context, projectID string) ([]neon.Branch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listCalls == nil {
		f.listCalls = map[string]int{}
	}
	f.listCalls[projectID]++
	if err, ok := f.listErr[projectID]; ok {
		return nil, err
	}
	out := make([]neon.Branch, len(f.branches[projectID]))
	copy(out, f.branches[projectID])
	return out, nil
}

// hasBranch reports whether project still holds a branch named name, under the
// fake's lock — safe to call while a sweep goroutine is running.
func (f *fakeNeon) hasBranch(projectID, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.branches[projectID], func(b neon.Branch) bool { return b.Name == name })
}

// listCallsFor reads the ListBranches count for a project under the lock.
func (f *fakeNeon) listCallsFor(projectID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls[projectID]
}

// previewBranchesFor returns only the preview-* branches in a project — what
// a run of Up is responsible for creating. The seeded long-lived branches
// (see seedRegistryNeonBranches) are deliberately excluded: those model the
// project's pre-existing `development`/`production` and are not this
// package's to create, count, or delete.
func (f *fakeNeon) previewBranchesFor(projectID string) []neon.Branch {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []neon.Branch
	for _, b := range f.branches[projectID] {
		if strings.HasPrefix(b.Name, "preview-") {
			out = append(out, b)
		}
	}
	return out
}

// createCallsFor returns the recorded CreateBranch calls for a project.
func (f *fakeNeon) createCallsFor(projectID string) []neonCreateCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []neonCreateCall
	for _, c := range f.createCalls {
		if c.project == projectID {
			out = append(out, c)
		}
	}
	return out
}

// seededBranchID is the ID the fake gives a seeded long-lived branch. It
// embeds the project as well as the branch name so that two projects whose
// parent branches share a NAME — footstrike-api and identity both use
// `development` — still get distinct IDs. Without that, a resolver that
// looked the parent up in the wrong project's branch list would return the
// right-looking ID and no assertion could tell.
func seededBranchID(projectID, branch string) string {
	return "br-" + projectID + "-" + branch
}

// seededDefaultBranch is the name of the decoy branch seedRegistryNeonBranches
// puts in every project ALONGSIDE the registry's declared parent. It stands in
// for the project's Neon default branch — `production` in both projects that
// declare a parent today — which is precisely what a preview lands on when
// CreateBranch is passed an empty parentID. Its presence is what makes a
// parent-resolution assertion discriminate: with only one branch seeded, "we
// resolved the configured name" is indistinguishable from "we grabbed
// whatever branch was there."
const seededDefaultBranch = "production"

// seedRegistryNeonBranches puts every Neon project named by the real embedded
// registry.yaml into the state the live project is in: the branch that
// entry's preview.neon.parent names, plus the default branch it is NOT
// supposed to use. The decoy is listed FIRST, so an implementation that took
// the head of the branch list rather than matching by name would pick the
// wrong one and fail.
//
// Without this, every Up in this file fails Up's parent pre-flight — correct
// behavior, but not what most of these tests are about.
func (f *fakeNeon) seedRegistryNeonBranches(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.branches == nil {
		f.branches = map[string][]neon.Branch{}
	}
	for _, svc := range testRegistry(t) {
		ref := svc.Neon
		if ref == nil || ref.Parent == "" {
			continue
		}
		f.branches[ref.Project] = []neon.Branch{
			{ID: seededBranchID(ref.Project, seededDefaultBranch), Name: seededDefaultBranch},
			{ID: seededBranchID(ref.Project, ref.Parent), Name: ref.Parent},
		}
	}
}

func (f *fakeNeon) CreateBranch(_ context.Context, projectID, name, parentID string) (neon.Branch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls = append(f.createCalls, neonCreateCall{project: projectID, name: name, parentID: parentID})
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
	if err, ok := f.deleteErrOnce[projectID]; ok {
		delete(f.deleteErrOnce, projectID)
		return err
	}
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
	// runCalls counts RunTrigger calls per trigger — the witness that an Up
	// really started a build. The auto-update tests need it because a run
	// that fails in the build stage leaves no other trace an assertion can
	// count (nothing is applied, and the annotations a failed retry writes
	// are identical to the ones already there).
	runCalls map[string]int
	// onGetBuild, if set, runs at the top of every GetBuild call, OUTSIDE the
	// fake's lock (like fakeGitHub.hook, and unlike everything else here) so
	// it is safe to block in: that is what lets a test hold one build's poll
	// open while another build's poll arrives, which is the only way to
	// observe that the two are really being awaited concurrently rather than
	// one after the other.
	onGetBuild func(buildID string)

	// priorBuilds is Cloud Build's build HISTORY as FindSuccessfulBuild sees
	// it: triggerID -> commit -> the short SHA that build produced. Empty by
	// default, so an unconfigured fake reports every commit as never built —
	// which is what a first-ever create really faces, and what keeps every
	// pre-existing test building exactly as much as it did before.
	priorBuilds map[string]map[string]string
	// findErr fails FindSuccessfulBuild for a trigger: "bifrost could not
	// tell", which must degrade to building rather than to failing the run.
	findErr map[string]error
	// findCalls counts FindSuccessfulBuild calls per trigger. It is the only
	// witness that the LIVE-preview path never consults Cloud Build at all: a
	// re-run over a healthy preview builds nothing and applies the same images
	// whether or not the lookup happened, so no other assertion can see it.
	findCalls map[string]int
}

func (f *fakeGCB) LatestBuilds(context.Context) (map[string]gcb.BuildStatus, error) { return nil, nil }

func (f *fakeGCB) FindSuccessfulBuild(_ context.Context, triggerID, commit string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.findCalls == nil {
		f.findCalls = map[string]int{}
	}
	f.findCalls[triggerID]++
	if err, ok := f.findErr[triggerID]; ok {
		return "", false, err
	}
	shortSHA, ok := f.priorBuilds[triggerID][commit]
	if !ok {
		return "", false, nil
	}
	return shortSHA, true, nil
}

// findCallsFor reads a trigger's FindSuccessfulBuild count under the fake's
// lock — safe to call while a background watcher goroutine is running.
func (f *fakeGCB) findCallsFor(triggerID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.findCalls[triggerID]
}

func (f *fakeGCB) RunTrigger(_ context.Context, triggerID, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runCalls == nil {
		f.runCalls = map[string]int{}
	}
	f.runCalls[triggerID]++
	if err, ok := f.runErr[triggerID]; ok {
		return "", err
	}
	return triggerID, nil
}

// runCallsFor reads a trigger's RunTrigger count under the fake's lock —
// safe to call while a background watcher goroutine is running.
func (f *fakeGCB) runCallsFor(triggerID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runCalls[triggerID]
}

func (f *fakeGCB) GetBuild(_ context.Context, buildID string) (gcb.BuildStatus, error) {
	f.mu.Lock()
	hook := f.onGetBuild
	f.mu.Unlock()
	if hook != nil {
		hook(buildID)
	}

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
	// phase is the KUBERNETES namespace status phase ("Active" |
	// "Terminating"), not the bifrost/phase annotation — the expiry sweep
	// reads both, and they mean different things. "" lists as "Active", so
	// only a test that cares has to say anything.
	phase string
}

type copySecretCall struct {
	srcNS, srcName, dstNS, dstName string
	overrides                      map[string][]byte
}

// appliedDeployment is one Deployment ApplyObjects saw, reduced to what
// synthesizing its pods needs.
type appliedDeployment struct {
	name  string
	image string
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

	// appliedDeployments[ns] is every Deployment ApplyObjects has applied
	// into ns, with the app-container image it carried. ListPods synthesizes
	// one running, ready pod per entry unless podScript overrides it —
	// modelling what a real cluster converges to, so every test that doesn't
	// care about pod readiness gets the healthy outcome without hand-writing
	// pod fixtures. Carrying the image matters: Up's readiness wait only
	// counts pods running the image it just applied.
	appliedDeployments map[string][]appliedDeployment
	// podScript, when non-nil, replaces that synthesis: each ListPods call
	// for a namespace consumes the next entry in its sequence (the last
	// entry repeats forever once exhausted), so a test can script pods that
	// start unready and become ready — or never do. A namespace with no
	// entry lists no pods at all.
	podScript map[string][][]kube.PodInfo
	// listPodsCalls counts ListPods calls per namespace: the index into
	// podScript, and how a test asserts Up actually polled rather than
	// glancing once.
	listPodsCalls map[string]int

	ensureErr   error
	annotateErr error
	listPodsErr error
	// annotateStepErr, if set, fails only a "pure" step() write — a call
	// whose annotations carry bifrost/step but not bifrost/phase (see
	// isStepOnlyAnnotation) — leaving fail()'s and the final ready call's
	// writes (which always carry bifrost/phase too) governed by annotateErr
	// instead. This is what lets a test isolate "step annotations fail" from
	// "every AnnotateNamespace call fails."
	annotateStepErr error
	applyErr        error
	copySecretErr   error
	deleteErr       error
	// deleteErrByNS fails DeleteNamespace for named namespaces only, leaving
	// every other one working. deleteErr fails them all; a sweep test needs
	// one teardown to fail while the next still succeeds.
	deleteErrByNS map[string]error
	// getErrByNS fails GetNamespace for named namespaces only — the expiry
	// sweep's re-read, whose failure must skip that preview rather than let it
	// be torn down on a stale snapshot.
	getErrByNS map[string]error
	// onEnsureNamespace, if set, runs at the top of every EnsureNamespace call
	// (before the namespace is created/merged). It is how a test reaches a
	// LATER stage of Up with a run context that has since died: cancelling
	// before Up is called no longer gets there, because the still-Terminating
	// pre-check reads the namespace first and a dead context fails that read.
	// Like onDeleteNamespace it runs with f.mu ALREADY HELD, so it must not
	// call back into the fake's own locking methods.
	onEnsureNamespace func(name string)
	// onDeleteNamespace, if set, runs at the top of every DeleteNamespace call
	// (before the delete itself), and is how a sweep test moves the cluster
	// underneath an in-progress sweep: renewing a later namespace's expiry, or
	// deleting it outright, while an earlier one is being torn down. It runs
	// with f.mu ALREADY HELD, so it must touch f's fields directly rather than
	// calling back into the fake's own locking methods.
	onDeleteNamespace func(deleting string)
}

// isStepOnlyAnnotation reports whether annotations is exactly what
// Orchestrator.step writes (bifrost/step + bifrost/step-since, nothing
// else) as opposed to fail()'s or Up's final ready write, both of which
// always include bifrost/phase alongside.
func isStepOnlyAnnotation(annotations map[string]string) bool {
	_, hasStep := annotations["bifrost/step"]
	_, hasPhase := annotations["bifrost/phase"]
	return hasStep && !hasPhase
}

func newFakeKube() *fakeKube {
	return &fakeKube{
		namespaces:         map[string]*fakeNamespace{},
		appliedDeployments: map[string][]appliedDeployment{},
		listPodsCalls:      map[string]int{},
	}
}

func (f *fakeKube) EnsureNamespace(_ context.Context, name string, labels, annotations map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onEnsureNamespace != nil {
		f.onEnsureNamespace(name)
	}
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

func (f *fakeKube) AnnotateNamespace(ctx context.Context, name string, annotations map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Context-respecting, unlike the other fake methods: this is what makes
	// TestUpFailDetachesAnnotateFromADeadRunContext meaningful — a real
	// client-go call against an already-cancelled/expired context fails the
	// same way, and fail()'s compensating write must survive that.
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.annotateStepErr != nil && isStepOnlyAnnotation(annotations) {
		return f.annotateStepErr
	}
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

// annotations returns a snapshot of name's merged annotations, or an empty
// map if the namespace was never created — so a test asserting on a single
// key reads a missing namespace as a missing annotation rather than panicking.
func (f *fakeKube) annotations(name string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ns, ok := f.namespaces[name]
	if !ok {
		return map[string]string{}
	}
	return copyStringMap(ns.annotations)
}

// appliesFor counts the ApplyObjects calls that targeted ns, under the fake's
// lock. It is the witness that an Up really ran (or really didn't) for ONE
// preview: the auto-update tests run several previews through the same fakes,
// where a global call count can't tell whose work it is, and the namespace
// annotations can't distinguish "not re-run" from "re-run and wrote the same
// values back".
func (f *fakeKube) appliesFor(ns string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, objs := range f.applyCalls {
		for _, o := range objs {
			if o.GetNamespace() == ns {
				n++
				break
			}
		}
	}
	return n
}

// hasNamespace reports whether name still exists, under the fake's lock —
// the accessor a test polling against a live background goroutine (the expiry
// sweep) has to use instead of reading the map directly.
func (f *fakeKube) hasNamespace(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.namespaces[name]
	return ok
}

func (f *fakeKube) ApplyObjects(_ context.Context, _ string, objs []*unstructured.Unstructured) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applyCalls = append(f.applyCalls, objs)
	for _, o := range objs {
		if o.GetKind() != "Deployment" {
			continue
		}
		ns := o.GetNamespace()
		dep := appliedDeployment{name: o.GetName(), image: appliedAppImage([]*unstructured.Unstructured{o}, o.GetName())}
		// A re-apply replaces the previous generation outright, exactly as a
		// converged rolling update does — otherwise a rerun test would list
		// pods from both generations forever.
		replaced := false
		for i, existing := range f.appliedDeployments[ns] {
			if existing.name == dep.name {
				f.appliedDeployments[ns][i] = dep
				replaced = true
				break
			}
		}
		if !replaced {
			f.appliedDeployments[ns] = append(f.appliedDeployments[ns], dep)
		}
	}
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
	if f.onDeleteNamespace != nil {
		f.onDeleteNamespace(name)
	}
	if err, ok := f.deleteErrByNS[name]; ok {
		return err
	}
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.namespaces, name)
	f.deletedNamespaces = append(f.deletedNamespaces, name)
	return nil
}

// ListPods serves Up's readiness wait. Default behavior is "the cluster
// converged": one ready pod per Deployment ApplyObjects has seen. podScript
// overrides that with a per-namespace sequence, one entry consumed per call.
func (f *fakeKube) ListPods(_ context.Context, ns string) ([]kube.PodInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := f.listPodsCalls[ns]
	f.listPodsCalls[ns]++
	if f.listPodsErr != nil {
		return nil, f.listPodsErr
	}
	if f.podScript != nil {
		seq := f.podScript[ns]
		if len(seq) == 0 {
			return nil, nil
		}
		if call >= len(seq) {
			call = len(seq) - 1
		}
		return seq[call], nil
	}
	var out []kube.PodInfo
	for _, dep := range f.appliedDeployments[ns] {
		out = append(out, readyPod(dep.name, dep.image))
	}
	return out, nil
}

// apiImage/dashImage are the images a newTwoMemberDeps run really applies:
// fakeGCB returns "<triggerID>-sha" as each build's short SHA (trig-api,
// trig-dash), and render tags the preview image preview-<sha>. A scripted
// pod that doesn't carry these is a *previous* generation's pod as far as
// Up's readiness wait is concerned.
var (
	apiImage  = previewImage("footstrike-api", "trig-api-sha")
	dashImage = previewImage("footstrike-dashboard", "trig-dash-sha")
)

// previewImage is the image a preview's rendered Deployment carries, and
// therefore what its pods run: the fleet's Artifact Registry path, tagged
// preview-<short sha>. Tests that script pods have to match it, because Up's
// wait only counts pods running the image it just applied.
func previewImage(svc, shortSHA string) string {
	return "us-central1-docker.pkg.dev/ethans-services/containers/" + svc + ":preview-" + shortSHA
}

// readyPod is one running, fully-ready Deployment pod for deployment,
// shaped the way a real one is: owned by a ReplicaSet named
// "<deployment>-<template hash>", with a container named after the service
// (the convention render.go's detectBase enforces).
func readyPod(deployment, image string) kube.PodInfo {
	return kube.PodInfo{
		Name:       deployment + "-7d9f6b8c4d-nx2kp",
		OwnerKind:  "ReplicaSet",
		OwnerName:  deployment + "-7d9f6b8c4d",
		Phase:      "Running",
		Containers: []kube.ContainerInfo{{Name: deployment, Image: image, Ready: true}},
	}
}

// initializingPod is a pod whose init containers are still running: the app
// container isn't ready and reports the contentless "PodInitializing", which
// is exactly what a pod mid-migration looks like. The init containers are
// given the app's image, as the rendered migrate initContainer really is.
func initializingPod(deployment, image string, init ...kube.ContainerInfo) kube.PodInfo {
	p := readyPod(deployment, image)
	p.Phase = "Pending"
	p.Containers = []kube.ContainerInfo{{Name: deployment, Image: image, WaitingReason: "PodInitializing"}}
	for i := range init {
		init[i].Image = image
	}
	p.InitContainers = init
	return p
}

// ListNamespaces serves the expiry sweep. selector is understood only in the
// single "key=value" form the preview control plane actually uses ("" matches
// everything) — enough to prove the sweep filters on bifrost/preview rather
// than reaching for every namespace in the cluster. Results are sorted by
// name so a multi-namespace sweep processes them in a fixed order.
//
// Context-respecting, like AnnotateNamespace and unlike the rest of the fake,
// and for the same kind of reason: a real List against a dead context fails,
// which is what makes a test of the sweep's detachment from shutdown
// cancellation mean anything.
func (f *fakeKube) ListNamespaces(ctx context.Context, selector string) ([]kube.NamespaceInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, value, hasSelector := strings.Cut(selector, "=")
	var out []kube.NamespaceInfo
	for name, ns := range f.namespaces {
		if hasSelector && ns.labels[key] != value {
			continue
		}
		out = append(out, namespaceInfo(name, ns))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// GetNamespace serves the expiry sweep's re-read: the fresh copy it re-decides
// against immediately before tearing a preview down. Shares namespaceInfo with
// ListNamespaces so a test can't have the two disagree about a namespace the
// real cluster would describe identically either way.
//
// Context-respecting for the same reason ListNamespaces is, and absent means
// found=false with a nil error — the contract kube.Client documents, and the
// case where something else has already deleted the namespace mid-sweep.
func (f *fakeKube) GetNamespace(ctx context.Context, name string) (kube.NamespaceInfo, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return kube.NamespaceInfo{}, false, err
	}
	if err, ok := f.getErrByNS[name]; ok {
		return kube.NamespaceInfo{}, false, err
	}
	ns, ok := f.namespaces[name]
	if !ok {
		return kube.NamespaceInfo{}, false, nil
	}
	return namespaceInfo(name, ns), true, nil
}

// namespaceInfo renders one fake namespace as the kube.Client type. An empty
// fake phase lists as "Active", so only a test that cares has to say anything.
// Callers must hold f.mu.
func namespaceInfo(name string, ns *fakeNamespace) kube.NamespaceInfo {
	phase := ns.phase
	if phase == "" {
		phase = "Active"
	}
	return kube.NamespaceInfo{
		Name:        name,
		Labels:      copyStringMap(ns.labels),
		Annotations: copyStringMap(ns.annotations),
		Phase:       phase,
	}
}

// Unused kube.Client methods — plain stubs to satisfy the interface.
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
// membership (preview URL) and identity's staging URL from the fleet
// registry (fallback).
func newTwoMemberDeps(t *testing.T) *testDeps {
	t.Helper()
	cfg := &config.Config{PreviewOAuthClientID: "preview-client-id"}
	gh := &fakeGitHub{
		members: map[string]bool{"footstrike-api": true, "footstrike-dashboard": true},
		k8sFiles: map[string]map[string][]byte{
			"footstrike-api":       loadFullK8sFixture(t, "footstrike-api"),
			"footstrike-dashboard": loadFullK8sFixture(t, "footstrike-dashboard"),
		},
	}
	nc := &fakeNeon{}
	nc.seedRegistryNeonBranches(t)
	gc := &fakeGCB{}
	kc := newFakeKube()
	o := &Orchestrator{
		Cfg:      cfg,
		Kube:     kc,
		GitHub:   gh,
		Neon:     nc,
		Builds:   gc,
		Registry: testRegistry(t),
		Fleet:    testFleet(t),
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

	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err != nil {
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

	// footstrike-api (has a registry Neon ref) gets its secret copied with a
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
	// footstrike-api's Neon project/database in the real registry: see registry.yaml.
	wantURI := "postgres://preview:fakesecret@aged-river-81935268/neondb"
	if string(apiCall.overrides["DATABASE_URL"]) != wantURI {
		t.Errorf("DATABASE_URL override = %q, want %q", apiCall.overrides["DATABASE_URL"], wantURI)
	}
	tlsCall := d.kube.copySecretCalls[1]
	if tlsCall.srcNS != "previews" || tlsCall.srcName != previewTLSSecret ||
		tlsCall.dstNS != ns || tlsCall.dstName != previewTLSSecret || tlsCall.overrides != nil {
		t.Errorf("tls secret copy = %+v, unexpected shape", tlsCall)
	}

	// The Neon branch was actually created for footstrike-api's project.
	branches := d.neon.previewBranchesFor("aged-river-81935268")
	if len(branches) != 1 || branches[0].Name != "preview-hae-cadence" {
		t.Errorf("aged-river-81935268 preview branches = %+v, want exactly one named preview-hae-cadence", branches)
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
	if err := d.orch.Up(ctx, "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("first Up failed: %v", err)
	}
	if err := d.orch.Up(ctx, "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("second Up (rerun) failed: %v", err)
	}
	ns := d.kube.namespaces["preview-hae-cadence"]
	if ns.annotations["bifrost/phase"] != "ready" {
		t.Errorf("bifrost/phase after rerun = %q, want ready", ns.annotations["bifrost/phase"])
	}
	if len(d.neon.previewBranchesFor("aged-river-81935268")) != 1 {
		t.Errorf("aged-river-81935268 preview branches after rerun = %+v, want still exactly one (scan-then-create, no duplicate)", d.neon.previewBranchesFor("aged-river-81935268"))
	}
}

// ---- Up: build reuse and parallelism -------------------------------------------

// appliedImages returns the app-container image of every Deployment currently
// applied into ns, keyed by Deployment name. A re-apply replaces the previous
// entry (see fakeKube.ApplyObjects), so this is "what the most recent run
// actually deployed" — which is the question a reuse test is asking, and which
// a scan over the whole applyCalls history cannot answer without also matching
// the previous run's identical images.
func (f *fakeKube) appliedImages(ns string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for _, dep := range f.appliedDeployments[ns] {
		out[dep.name] = dep.image
	}
	return out
}

// TestUpFirstCreateBuildsEveryMemberAndRecordsWhatItBuilt is the baseline the
// skip must never break: with nothing recorded from a previous run, every
// member is built. It also pins bifrost/built-images' format — one
// `svc=<full commit>:<short sha>` entry per member, comma-joined in
// bifrost/apps order — since everything downstream (the skip decision, and the
// image tag rendered without a rebuild) is parsed back out of exactly this
// string.
func TestUpFirstCreateBuildsEveryMemberAndRecordsWhatItBuilt(t *testing.T) {
	d := newTwoMemberDeps(t)
	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("Up failed: %v", err)
	}

	for _, trigger := range []string{"trig-api", "trig-dash"} {
		if got := d.gcb.runCallsFor(trigger); got != 1 {
			t.Errorf("%s ran %d times on a first create, want 1 — a create with nothing recorded must build every member", trigger, got)
		}
	}
	// The full commit each member resolved to (fakeGitHub's defaultBranchSHA)
	// paired with the short SHA its build reported (fakeGCB's "<id>-sha") —
	// deliberately unrelated strings here, so an implementation that derived
	// the tag from the commit instead of recording it cannot pass.
	want := "footstrike-api=" + defaultBranchSHA + ":trig-api-sha," +
		"footstrike-dashboard=" + defaultBranchSHA + ":trig-dash-sha"
	if got := d.kube.annotations(previewNamespace("hae-cadence"))["bifrost/built-images"]; got != want {
		t.Errorf("bifrost/built-images = %q, want %q", got, want)
	}
}

// TestUpRerunWithNoNewCommitsBuildsNothing is the headline case: re-running
// `bif preview up` on a branch nobody has pushed to — the documented recovery
// path, and what the auto-update watcher walks — must not rebuild anything,
// and must still render and apply a working preview off the images the
// previous run produced.
func TestUpRerunWithNoNewCommitsBuildsNothing(t *testing.T) {
	d := newTwoMemberDeps(t)
	ctx := context.Background()
	const ns = "preview-hae-cadence"

	if err := d.orch.Up(ctx, "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("first Up failed: %v", err)
	}
	appliesAfterFirst := d.kube.appliesFor(ns)

	if err := d.orch.Up(ctx, "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("re-run Up failed: %v", err)
	}

	for _, trigger := range []string{"trig-api", "trig-dash"} {
		if got := d.gcb.runCallsFor(trigger); got != 1 {
			t.Errorf("%s ran %d times across two Ups, want 1 — a re-run with no new commits must rebuild nothing", trigger, got)
		}
	}
	// Not rebuilding must not mean not deploying: the re-run still renders and
	// applies, using the tags the first run's builds produced.
	if got := d.kube.appliesFor(ns); got <= appliesAfterFirst {
		t.Errorf("ApplyObjects calls for %s went %d -> %d, want the re-run to have applied its manifests", ns, appliesAfterFirst, got)
	}
	want := map[string]string{"footstrike-api": apiImage, "footstrike-dashboard": dashImage}
	for svc, image := range want {
		if got := d.kube.appliedImages(ns)[svc]; got != image {
			t.Errorf("%s deployed image = %q, want the reused %q", svc, got, image)
		}
	}
	if got := d.kube.annotations(ns)["bifrost/phase"]; got != "ready" {
		t.Errorf("bifrost/phase after a build-free re-run = %q, want ready", got)
	}
}

// TestUpRerunBuildsOnlyTheMemberWhoseCommitMoved is the point of the whole
// feature, and the case auto-update hits constantly: someone pushes to ONE of
// a preview's repos, and only that repo is rebuilt. The load-bearing assertion
// is the negative one — the untouched member's trigger must never run a second
// time — because everything else about the run looks identical either way.
func TestUpRerunBuildsOnlyTheMemberWhoseCommitMoved(t *testing.T) {
	d := newTwoMemberDeps(t)
	ctx := context.Background()
	const ns = "preview-hae-cadence"

	if err := d.orch.Up(ctx, "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("first Up failed: %v", err)
	}

	// Only the dashboard's branch moves; the api's stays exactly where it was.
	d.github.shas = map[string]string{"footstrike-dashboard": "beef99887766554433"}
	// ...and its rebuild produces a visibly different image tag, so "reused"
	// and "rebuilt" can't be confused for one another in the applied manifests.
	d.gcb.statuses = map[string][]gcb.BuildStatus{"trig-dash": {{Status: "SUCCESS", SHA: "newdash"}}}

	if err := d.orch.Up(ctx, "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("re-run Up failed: %v", err)
	}

	if got := d.gcb.runCallsFor("trig-api"); got != 1 {
		t.Errorf("footstrike-api's trigger ran %d times, want 1 — its commit never moved, so the re-run must not have started a build for it at all", got)
	}
	if got := d.gcb.runCallsFor("trig-dash"); got != 2 {
		t.Errorf("footstrike-dashboard's trigger ran %d times, want 2 — its commit moved, so it must be rebuilt", got)
	}

	images := d.kube.appliedImages(ns)
	if got := images["footstrike-api"]; got != apiImage {
		t.Errorf("footstrike-api deployed image = %q, want the reused %q", got, apiImage)
	}
	if want := previewImage("footstrike-dashboard", "newdash"); images["footstrike-dashboard"] != want {
		t.Errorf("footstrike-dashboard deployed image = %q, want the freshly built %q", images["footstrike-dashboard"], want)
	}
	// The record advances for the rebuilt member and stands still for the
	// reused one, so the NEXT re-run can skip both.
	want := "footstrike-api=" + defaultBranchSHA + ":trig-api-sha," +
		"footstrike-dashboard=beef99887766554433:newdash"
	if got := d.kube.annotations(ns)["bifrost/built-images"]; got != want {
		t.Errorf("bifrost/built-images = %q, want %q", got, want)
	}
}

// TestUpRerunAfterAFailedBuildStillRebuilds is the trap this feature is built
// around. bifrost/source-shas is written BEFORE the builds run (deliberately —
// it's what stops the auto-update watcher hot-looping on a doomed commit), so
// after a FAILED build the namespace records the commit while no image was
// ever pushed. Keying the skip off it would send the next run straight to
// applying a Deployment whose `preview-<sha>` tag does not exist in Artifact
// Registry.
//
// The preconditions are asserted, not assumed: source-shas really does carry
// the failed member's commit, and the branch really has not moved, so the only
// thing that can make the retry rebuild is that the skip consults a record
// written after success.
func TestUpRerunAfterAFailedBuildStillRebuilds(t *testing.T) {
	d := newTwoMemberDeps(t)
	ctx := context.Background()
	const ns = "preview-hae-cadence"
	d.gcb.statuses = map[string][]gcb.BuildStatus{"trig-api": {{Status: "FAILURE"}}}

	if err := d.orch.Up(ctx, "hae-cadence", UpOptions{}); err == nil {
		t.Fatal("first Up succeeded, want the scripted build failure")
	}
	ann := d.kube.annotations(ns)
	if !strings.Contains(ann["bifrost/source-shas"], "footstrike-api="+defaultBranchSHA) {
		t.Fatalf("precondition: bifrost/source-shas = %q, want the failed run's attempted commit recorded", ann["bifrost/source-shas"])
	}
	if got := ann["bifrost/built-images"]; got != "" {
		t.Fatalf("bifrost/built-images = %q after a failed build, want nothing recorded — no image was produced", got)
	}

	// The branch has NOT moved (fakeGitHub still reports defaultBranchSHA);
	// only the build now succeeds.
	d.gcb.statuses = nil
	if err := d.orch.Up(ctx, "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("retry Up failed: %v", err)
	}
	if got := d.gcb.runCallsFor("trig-api"); got != 2 {
		t.Errorf("footstrike-api's trigger ran %d times, want 2 — the retry must rebuild a commit whose previous build FAILED, however unchanged bifrost/source-shas is", got)
	}
	if got := d.kube.annotations(ns)["bifrost/phase"]; got != "ready" {
		t.Errorf("bifrost/phase after the retry = %q, want ready", got)
	}
}

// ---- Up: reuse that survives a teardown -------------------------------------
//
// bifrost/built-images lives on the preview's namespace and Down deletes the
// namespace, so an up -> down -> up at the same commit has nothing recorded to
// reuse. These cover the fallback that closes that gap: when — and only when —
// the annotation has no entry for a member, Cloud Build is asked whether the
// commit was already built successfully.
//
// The fake answers at the interface boundary (found / not-found / error),
// which is where the orchestrator's own behavior lives. What counts as a
// findable build — SUCCESS only, newest first, a usable SHORT_SHA — is the
// gcb package's contract and is tested against the real implementation there
// (TestSuccessfulBuildFor, TestFindSuccessfulBuild*).

// priorBuiltImages is a Cloud Build history in which BOTH members' current
// commit has already been built successfully, under image tags that are
// visibly different from the ones a fresh build would produce ("<trigger>-sha")
// — so an image reused from the lookup can never be mistaken for one this run
// built.
func priorBuiltImages() map[string]map[string]string {
	return map[string]map[string]string{
		"trig-api":  {defaultBranchSHA: "coldapi"},
		"trig-dash": {defaultBranchSHA: "colddash"},
	}
}

// TestUpAfterDownReusesImagesWithoutBuilding is the entire point of the
// feature: tear a preview down and bring it straight back up at the same
// commit, and nothing is rebuilt.
//
// The load-bearing assertion is the negative one — RunTrigger is never called
// — because a run that rebuilt would look identical in every other respect: it
// would reach ready, apply both members, and record built-images too. Only the
// image tags and the absence of builds tell the two apart.
func TestUpAfterDownReusesImagesWithoutBuilding(t *testing.T) {
	d := newTwoMemberDeps(t)
	ctx := context.Background()
	const ns = "preview-hae-cadence"
	d.gcb.priorBuilds = priorBuiltImages()

	if err := d.orch.Down(ctx, "hae-cadence"); err != nil {
		t.Fatalf("Down failed: %v", err)
	}
	if _, exists := d.kube.namespaces[ns]; exists {
		t.Fatalf("precondition: %s still exists after Down, so its bifrost/built-images was never lost", ns)
	}

	if err := d.orch.Up(ctx, "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("Up after Down failed: %v", err)
	}

	for _, trigger := range []string{"trig-api", "trig-dash"} {
		if got := d.gcb.runCallsFor(trigger); got != 0 {
			t.Errorf("%s ran %d builds, want 0 — the images for this commit already exist and the run must reuse them", trigger, got)
		}
	}
	images := d.kube.appliedImages(ns)
	want := map[string]string{
		"footstrike-api":       previewImage("footstrike-api", "coldapi"),
		"footstrike-dashboard": previewImage("footstrike-dashboard", "colddash"),
	}
	for svc, image := range want {
		if images[svc] != image {
			t.Errorf("%s deployed image = %q, want the already-built %q", svc, images[svc], image)
		}
	}
	if got := d.kube.annotations(ns)["bifrost/phase"]; got != "ready" {
		t.Errorf("bifrost/phase = %q, want ready — reusing must still produce a working preview", got)
	}
}

// TestUpAfterDownRecordsWhatItReusedSoTheNextRunNeedsNoLookup: a member reused
// from the build lookup has to be written into bifrost/built-images exactly as
// a real build would be. Otherwise every subsequent re-run of a recreated
// preview — including every auto-update tick — would fall through to the
// lookup again forever, paying an API call per member per tick for an answer
// the namespace could have held.
func TestUpAfterDownRecordsWhatItReusedSoTheNextRunNeedsNoLookup(t *testing.T) {
	d := newTwoMemberDeps(t)
	ctx := context.Background()
	const ns = "preview-hae-cadence"
	d.gcb.priorBuilds = priorBuiltImages()

	if err := d.orch.Down(ctx, "hae-cadence"); err != nil {
		t.Fatalf("Down failed: %v", err)
	}
	if err := d.orch.Up(ctx, "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("Up after Down failed: %v", err)
	}

	want := "footstrike-api=" + defaultBranchSHA + ":coldapi," +
		"footstrike-dashboard=" + defaultBranchSHA + ":colddash"
	if got := d.kube.annotations(ns)["bifrost/built-images"]; got != want {
		t.Fatalf("bifrost/built-images = %q, want %q — a reused image must be recorded exactly as a built one is", got, want)
	}

	lookupsAfterRecreate := map[string]int{
		"trig-api": d.gcb.findCallsFor("trig-api"), "trig-dash": d.gcb.findCallsFor("trig-dash"),
	}
	if err := d.orch.Up(ctx, "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("second Up failed: %v", err)
	}
	for trigger, before := range lookupsAfterRecreate {
		if before == 0 {
			t.Fatalf("precondition: %s was never looked up during the recreate", trigger)
		}
		if got := d.gcb.findCallsFor(trigger); got != before {
			t.Errorf("%s build lookups went %d -> %d across the follow-up run, want no change: the recorded annotation must answer it", trigger, before, got)
		}
	}
}

// TestUpLivePreviewRerunNeverConsultsCloudBuild guards the common path this
// feature must not tax. A re-run over a live preview — the documented recovery
// path, and what the auto-update watcher walks every two minutes — has an
// annotation entry for every member, which is authoritative and free. Nothing
// else in the run can witness a stray lookup: it would return the same images,
// apply the same manifests and reach ready either way, just having spent an
// API call per member per tick to learn what the namespace already said.
func TestUpLivePreviewRerunNeverConsultsCloudBuild(t *testing.T) {
	d := newTwoMemberDeps(t)
	ctx := context.Background()
	d.gcb.priorBuilds = priorBuiltImages()

	if err := d.orch.Up(ctx, "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("first Up failed: %v", err)
	}
	// The first create legitimately looks (it has nothing recorded); only what
	// happens once the namespace holds a record is under test here.
	before := map[string]int{
		"trig-api": d.gcb.findCallsFor("trig-api"), "trig-dash": d.gcb.findCallsFor("trig-dash"),
	}

	// Re-run with nothing moved: both members are covered by the annotation.
	if err := d.orch.Up(ctx, "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("re-run Up failed: %v", err)
	}
	// ...and again with ONE member's commit moved, which is what an
	// auto-update tick looks like. An entry that DISAGREES is still an answer:
	// the member moved, so it is built, with no lookup.
	d.github.shas = map[string]string{"footstrike-dashboard": "beef99887766554433"}
	if err := d.orch.Up(ctx, "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("moved-commit Up failed: %v", err)
	}

	for trigger, n := range before {
		if got := d.gcb.findCallsFor(trigger); got != n {
			t.Errorf("%s build lookups went %d -> %d over two re-runs of a live preview, want no change — bifrost/built-images already covers every member", trigger, n, got)
		}
	}
	// The moved member is still rebuilt (the annotation said it moved), and
	// the untouched one still rides its recorded entry.
	if got := d.gcb.runCallsFor("trig-dash"); got != 1 {
		t.Errorf("trig-dash ran %d builds, want 1 — the member whose commit moved must still be rebuilt", got)
	}
	if got := d.gcb.runCallsFor("trig-api"); got != 0 {
		t.Errorf("trig-api ran %d builds, want 0 — its commit never moved", got)
	}
	images := d.kube.appliedImages("preview-hae-cadence")
	if want := previewImage("footstrike-api", "coldapi"); images["footstrike-api"] != want {
		t.Errorf("footstrike-api deployed image = %q, want %q carried through by the annotation", images["footstrike-api"], want)
	}
	if want := previewImage("footstrike-dashboard", "trig-dash-sha"); images["footstrike-dashboard"] != want {
		t.Errorf("footstrike-dashboard deployed image = %q, want the freshly built %q", images["footstrike-dashboard"], want)
	}
}

// TestUpAfterDownStillBuildsWhenTheCommitWasNeverBuiltSuccessfully covers both
// halves of "not found": a commit Cloud Build has no record of, and one whose
// only build FAILED. The gcb client reports them identically — a failed build
// produced no image, so it is not a findable build (see
// TestSuccessfulBuildFor's "only a failed build at this commit") — and the
// orchestrator must build in both cases rather than reaching for a tag that
// does not exist in Artifact Registry.
func TestUpAfterDownStillBuildsWhenTheCommitWasNeverBuiltSuccessfully(t *testing.T) {
	d := newTwoMemberDeps(t)
	ctx := context.Background()
	const ns = "preview-hae-cadence"
	// Cloud Build's history holds nothing usable for this commit: for the api
	// because the only build of it failed, for the dashboard because it has
	// never been built at all. Both surface here as not-found.
	d.gcb.priorBuilds = map[string]map[string]string{"trig-api": {}, "trig-dash": {}}

	if err := d.orch.Down(ctx, "hae-cadence"); err != nil {
		t.Fatalf("Down failed: %v", err)
	}
	if err := d.orch.Up(ctx, "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("Up after Down failed: %v", err)
	}

	for _, trigger := range []string{"trig-api", "trig-dash"} {
		if got := d.gcb.findCallsFor(trigger); got != 1 {
			t.Errorf("%s was looked up %d times, want 1 — a recreate has no annotation and must ask", trigger, got)
		}
		if got := d.gcb.runCallsFor(trigger); got != 1 {
			t.Errorf("%s ran %d builds, want 1 — no successful build exists for this commit, so it must be built", trigger, got)
		}
	}
	images := d.kube.appliedImages(ns)
	if images["footstrike-api"] != apiImage || images["footstrike-dashboard"] != dashImage {
		t.Errorf("deployed images = %v, want the freshly built %q and %q", images, apiImage, dashImage)
	}
}

// TestUpFirstCreateStillBuildsEverythingDespiteTheLookup is the other common
// path that must not regress: a brand-new preview for a brand-new branch. It
// now consults Cloud Build (it has no annotation to consult instead), and the
// lookup finding nothing must leave the run building exactly as much as it
// always did. The lookup-call assertion is what stops this from passing
// vacuously — without it, an implementation that never asked would look the
// same.
func TestUpFirstCreateStillBuildsEverythingDespiteTheLookup(t *testing.T) {
	d := newTwoMemberDeps(t)
	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("Up failed: %v", err)
	}
	for _, trigger := range []string{"trig-api", "trig-dash"} {
		if got := d.gcb.findCallsFor(trigger); got != 1 {
			t.Errorf("%s was looked up %d times, want 1", trigger, got)
		}
		if got := d.gcb.runCallsFor(trigger); got != 1 {
			t.Errorf("%s ran %d builds, want 1 — a first create with nothing built anywhere must build every member", trigger, got)
		}
	}
	images := d.kube.appliedImages("preview-hae-cadence")
	if images["footstrike-api"] != apiImage || images["footstrike-dashboard"] != dashImage {
		t.Errorf("deployed images = %v, want %q and %q", images, apiImage, dashImage)
	}
}

// TestUpBuildLookupFailureBuildsRatherThanFailingTheRun: the lookup is an
// optimization, so "bifrost could not tell" has to mean the same thing as "no
// such build" — build it. A lookup error that propagated would turn a Cloud
// Build blip into a failed preview, which is strictly worse than the rebuild
// this feature exists to avoid.
func TestUpBuildLookupFailureBuildsRatherThanFailingTheRun(t *testing.T) {
	d := newTwoMemberDeps(t)
	ctx := context.Background()
	const ns = "preview-hae-cadence"
	d.gcb.priorBuilds = priorBuiltImages()
	// Only the api's lookup fails, so the same run also proves the failure is
	// scoped to its own member rather than poisoning the whole stage.
	d.gcb.findErr = map[string]error{"trig-api": errors.New("cloudbuild: 503 backend error")}

	if err := d.orch.Down(ctx, "hae-cadence"); err != nil {
		t.Fatalf("Down failed: %v", err)
	}
	if err := d.orch.Up(ctx, "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("Up failed despite the build lookup being best-effort: %v", err)
	}

	if got := d.gcb.runCallsFor("trig-api"); got != 1 {
		t.Errorf("trig-api ran %d builds, want 1 — an unanswerable lookup must fall back to building", got)
	}
	if got := d.gcb.runCallsFor("trig-dash"); got != 0 {
		t.Errorf("trig-dash ran %d builds, want 0 — one member's lookup failing must not stop another's from being used", got)
	}
	if got := d.kube.annotations(ns)["bifrost/phase"]; got != "ready" {
		t.Errorf("bifrost/phase = %q, want ready", got)
	}
	images := d.kube.appliedImages(ns)
	if images["footstrike-api"] != apiImage {
		t.Errorf("footstrike-api deployed image = %q, want the freshly built %q", images["footstrike-api"], apiImage)
	}
	if want := previewImage("footstrike-dashboard", "colddash"); images["footstrike-dashboard"] != want {
		t.Errorf("footstrike-dashboard deployed image = %q, want the reused %q", images["footstrike-dashboard"], want)
	}
}

// TestUpAwaitsBuildsConcurrently proves the builds overlap rather than merely
// both happening. Each build's first status poll parks until the OTHER build's
// poll has arrived: awaited one after the other, the first one waits alone
// until it gives up, which is what the timedOut flag records. Nothing else
// about the run distinguishes concurrent from sequential — both orders start
// both triggers and finish with both images.
func TestUpAwaitsBuildsConcurrently(t *testing.T) {
	d := newTwoMemberDeps(t)

	var (
		mu       sync.Mutex
		seen     = map[string]bool{}
		both     = make(chan struct{})
		closeAll sync.Once
		timedOut atomic.Bool
	)
	d.gcb.onGetBuild = func(buildID string) {
		mu.Lock()
		seen[buildID] = true
		arrived := len(seen)
		mu.Unlock()
		if arrived == 2 {
			closeAll.Do(func() { close(both) })
		}
		select {
		case <-both:
		case <-time.After(2 * time.Second):
			timedOut.Store(true)
		}
	}

	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("Up failed: %v", err)
	}
	if timedOut.Load() {
		t.Error("a build's status poll waited out the bound for the other build's poll to arrive: the two are being awaited one after the other, not concurrently")
	}
	images := d.kube.appliedImages("preview-hae-cadence")
	if images["footstrike-api"] != apiImage || images["footstrike-dashboard"] != dashImage {
		t.Errorf("deployed images = %v, want both concurrent builds to have completed (%q, %q)", images, apiImage, dashImage)
	}
}

// TestUpBuildFailureNamesTheFailingMember: with the builds running
// concurrently, "a build failed" is no longer self-evidently about the first
// member — so the failure has to name the one that actually failed, and must
// not blame a sibling whose build was perfectly fine.
//
// It also pins the other half of the "record only what really exists" rule:
// footstrike-api's build SUCCEEDS here, and nothing is recorded for it either.
// A run that failed is not evidence about which images survived it, and a
// half-written record is one more thing a later skip could get wrong; the cost
// is one redundant rebuild on the retry.
func TestUpBuildFailureNamesTheFailingMember(t *testing.T) {
	d := newTwoMemberDeps(t)
	d.gcb.statuses = map[string][]gcb.BuildStatus{"trig-dash": {{Status: "FAILURE"}}}

	err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{})
	if err == nil {
		t.Fatal("expected Up to fail when a member's build fails, got nil")
	}
	if !strings.Contains(err.Error(), "footstrike-dashboard") {
		t.Errorf("error = %q, want it to name footstrike-dashboard, whose build failed", err.Error())
	}
	if strings.Contains(err.Error(), "footstrike-api:") || strings.Contains(err.Error(), "for footstrike-api") {
		t.Errorf("error = %q, want it not to blame footstrike-api, whose build succeeded", err.Error())
	}
	ann := d.kube.annotations("preview-hae-cadence")
	if ann["bifrost/phase"] != "failed" {
		t.Errorf("bifrost/phase = %q, want failed", ann["bifrost/phase"])
	}
	if got := ann["bifrost/built-images"]; got != "" {
		t.Errorf("bifrost/built-images = %q, want nothing recorded for a run whose build stage failed", got)
	}
}

// TestBuiltImagesAnnotationRoundTrips pins the format/parse pair directly,
// including the drops: every malformed entry has to land on "nothing recorded
// for this member", which buildMembers reads as "must build". The failure
// direction matters — a lenient parse that invented a member's entry would
// skip a build and deploy an image tag that was never pushed.
func TestBuiltImagesAnnotationRoundTrips(t *testing.T) {
	members := []string{"footstrike-api", "footstrike-dashboard"}
	built := map[string]builtImage{
		"footstrike-api":       {Commit: "deadbeef0123456789", ShortSHA: "deadbee"},
		"footstrike-dashboard": {Commit: "c0ffee1122334455", ShortSHA: "c0ffee1"},
		"identity":             {Commit: "aaaa1111", ShortSHA: "aaaa111"}, // not a member: not rendered
	}
	raw := builtImagesAnnotation(members, built)
	want := "footstrike-api=deadbeef0123456789:deadbee,footstrike-dashboard=c0ffee1122334455:c0ffee1"
	if raw != want {
		t.Fatalf("builtImagesAnnotation = %q, want %q", raw, want)
	}
	got := parseBuiltImages(raw)
	for _, svc := range members {
		if got[svc] != built[svc] {
			t.Errorf("parseBuiltImages()[%q] = %+v, want %+v", svc, got[svc], built[svc])
		}
	}

	// A member with only half a record renders nothing rather than a
	// half-entry: an image tag with no commit could never match, and a commit
	// with no tag would skip the build and then render an empty tag.
	half := builtImagesAnnotation([]string{"footstrike-api"}, map[string]builtImage{
		"footstrike-api": {Commit: "deadbeef0123456789"},
	})
	if half != "" {
		t.Errorf("builtImagesAnnotation with no short SHA = %q, want it omitted", half)
	}

	for _, raw := range []string{
		"",
		"footstrike-api",                     // no "="
		"footstrike-api=deadbeef0123456789",  // no ":"
		"footstrike-api=:deadbee",            // no commit
		"footstrike-api=deadbeef0123456789:", // no short sha
		"=deadbeef0123456789:deadbee",        // no service
	} {
		if parsed := parseBuiltImages(raw); len(parsed) != 0 {
			t.Errorf("parseBuiltImages(%q) = %+v, want nothing recorded", raw, parsed)
		}
	}
}

// ---- Up: expiry ---------------------------------------------------------------

// TestUpRecordsExpiresAtVerBATIM pins the half of UpOptions that stops the
// auto-updater from renewing an expiry it was only supposed to preserve: the
// instant the caller passes is what lands in bifrost/expires-at, unchanged.
// Up must not re-derive it from a duration and "now" — an assertion that
// merely checked "roughly 8h from now" would pass for a recomputing Up, which
// is exactly the bug.
func TestUpRecordsExpiresAtVerbatim(t *testing.T) {
	d := newTwoMemberDeps(t)
	// Deliberately NOT now+8h: a fixed instant unrelated to the wall clock
	// can only match if Up wrote through what it was given.
	want := time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{ExpiresAt: want}); err != nil {
		t.Fatalf("Up failed: %v", err)
	}
	got := d.kube.annotations(previewNamespace("hae-cadence"))["bifrost/expires-at"]
	if got != want.Format(time.RFC3339) {
		t.Errorf("bifrost/expires-at = %q, want it recorded verbatim as %q", got, want.Format(time.RFC3339))
	}
}

func TestUpWithoutExpiryClearsAnyPriorExpiry(t *testing.T) {
	d := newTwoMemberDeps(t)
	expiry := time.Now().UTC().Add(8 * time.Hour)
	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{ExpiresAt: expiry}); err != nil {
		t.Fatalf("first Up failed: %v", err)
	}
	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("second Up failed: %v", err)
	}
	if got := d.kube.annotations(previewNamespace("hae-cadence"))["bifrost/expires-at"]; got != "" {
		t.Errorf("bifrost/expires-at = %q after a no-expiry re-run, want cleared", got)
	}
}

// ---- Up: auto-update opt-in ---------------------------------------------------

// TestUpRecordsAutoUpdateOptIn covers both directions of the annotation in
// one place, since the second is only interesting relative to the first: an
// Up that asks for auto-update writes "true", and an Up that doesn't CLEARS
// it rather than inheriting the previous run's value through the merge.
func TestUpRecordsAutoUpdateOptIn(t *testing.T) {
	d := newTwoMemberDeps(t)
	ns := previewNamespace("hae-cadence")

	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{AutoUpdate: true}); err != nil {
		t.Fatalf("Up failed: %v", err)
	}
	if got := d.kube.annotations(ns)["bifrost/auto-update"]; got != "true" {
		t.Fatalf("bifrost/auto-update = %q, want true", got)
	}

	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("second Up failed: %v", err)
	}
	if got := d.kube.annotations(ns)["bifrost/auto-update"]; got != "" {
		t.Errorf("bifrost/auto-update = %q after a re-run that didn't ask for it, want cleared", got)
	}
}

// TestUpRecordsSourceSHAs pins the annotation the watcher compares against:
// full SHAs (what BranchSHA returns), one service=sha pair per member, in
// bifrost/apps order, and no entry for a non-member.
func TestUpRecordsSourceSHAs(t *testing.T) {
	d := newTwoMemberDeps(t)
	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("Up failed: %v", err)
	}
	// fakeGitHub returns this SHA for every repo that has the branch; the
	// builds' own short SHAs (trig-api-sha) must NOT appear here.
	want := "footstrike-api=deadbeef0123456789,footstrike-dashboard=deadbeef0123456789"
	if got := d.kube.annotations(previewNamespace("hae-cadence"))["bifrost/source-shas"]; got != want {
		t.Errorf("bifrost/source-shas = %q, want %q", got, want)
	}
}

// TestUpRecordsSourceSHAsBeforeBuilding is the anti-hot-loop property stated
// as a property of Up alone: the SHA a run attempted is on the namespace even
// when that run FAILS, because it is written in the same EnsureNamespace call
// as the phase, before any build runs. Without it the auto-update watcher
// would find the branch still ahead of the (never-recorded) deployed SHA and
// re-run the same doomed build every two minutes forever.
func TestUpRecordsSourceSHAsBeforeBuilding(t *testing.T) {
	d := newTwoMemberDeps(t)
	d.gcb.statuses = map[string][]gcb.BuildStatus{"trig-api": {{Status: "FAILURE"}}}

	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{AutoUpdate: true}); err == nil {
		t.Fatal("Up succeeded, want the scripted build failure")
	}
	ann := d.kube.annotations(previewNamespace("hae-cadence"))
	if ann["bifrost/phase"] != "failed" {
		t.Fatalf("bifrost/phase = %q, want failed", ann["bifrost/phase"])
	}
	if got := ann["bifrost/source-shas"]; !strings.Contains(got, "footstrike-api=deadbeef0123456789") {
		t.Errorf("bifrost/source-shas = %q, want the attempted SHA recorded despite the failure", got)
	}
}

// ---- Up: the Neon parent branch ------------------------------------------------

// setNeonParent replaces svc's registry Neon parent, copying the NeonRef
// rather than writing through the pointer so one test can't disturb another
// (testRegistry re-parses the YAML per call, but the copy makes that
// independence explicit rather than incidental).
func setNeonParent(t *testing.T, reg Registry, svc, parent string) {
	t.Helper()
	entry, ok := reg[svc]
	if !ok || entry.Neon == nil {
		t.Fatalf("registry entry for %s has no neon block to reparent", svc)
	}
	ref := *entry.Neon
	ref.Parent = parent
	entry.Neon = &ref
	reg[svc] = entry
}

// TestUpCutsTheNeonBranchFromTheRegistryParent is the whole point of
// preview.neon.parent: a preview's database branch must be cut from the
// branch staging runs on, not from the project's Neon default (production).
//
// It asserts on the parentID CreateBranch RECEIVED, because that is the only
// place the distinction is observable — the created branch object records no
// parent, so "cut from staging" and "cut from production" produce identical
// state in the fake and in Neon's own branch list.
func TestUpCutsTheNeonBranchFromTheRegistryParent(t *testing.T) {
	d := newTwoMemberDeps(t)
	const project = "aged-river-81935268"
	ref := d.orch.Registry["footstrike-api"].Neon
	if ref.Parent == "" {
		t.Fatal("registry.yaml declares no preview.neon.parent for footstrike-api; this test exists to prove that parent is honored")
	}

	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("Up failed: %v", err)
	}

	calls := d.neon.createCallsFor(project)
	if len(calls) != 1 {
		t.Fatalf("CreateBranch calls for %s = %+v, want exactly one", project, calls)
	}
	got := calls[0].parentID
	// Spelled out as three checks rather than one equality, so a failure says
	// WHICH way it went wrong. The two named wrong answers are the two real
	// regressions: "" is the pre-parent behavior (Neon silently picks the
	// default branch, which is production), and the default branch's own ID
	// is what a resolver matching the wrong entry would hand back.
	if got == "" {
		t.Error("CreateBranch parentID is empty: the branch was cut from the project's DEFAULT branch (production) — exactly the bug preview.neon.parent exists to fix")
	}
	if got == seededBranchID(project, seededDefaultBranch) {
		t.Errorf("CreateBranch parentID = %q, the %q branch — the configured parent name resolved to the wrong branch", got, seededDefaultBranch)
	}
	if want := seededBranchID(project, ref.Parent); got != want {
		t.Errorf("CreateBranch parentID = %q, want %q (the ID of branch %q, resolved from its name)", got, want, ref.Parent)
	}
}

// TestUpWithoutARegistryParentBranchesFromTheNeonDefault pins the
// backwards-compatible half of the contract: an app whose neon: block omits
// parent: must behave exactly as it did before the key existed — an empty
// parentID, which Neon reads as "use the project's default branch". Every
// previewable app was in this state until this commit, and any future one
// that skips the key stays in it.
func TestUpWithoutARegistryParentBranchesFromTheNeonDefault(t *testing.T) {
	d := newTwoMemberDeps(t)
	const project = "aged-river-81935268"
	setNeonParent(t, d.orch.Registry, "footstrike-api", "")

	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("Up failed: %v", err)
	}

	calls := d.neon.createCallsFor(project)
	if len(calls) != 1 {
		t.Fatalf("CreateBranch calls for %s = %+v, want exactly one", project, calls)
	}
	if calls[0].parentID != "" {
		t.Errorf("CreateBranch parentID = %q, want \"\": an omitted parent: must pass the empty parent Neon reads as 'the project default', not resolve to some branch of its own choosing", calls[0].parentID)
	}
}

// TestUpUnknownNeonParentFailsBeforeTheNamespaceExists covers the failure
// mode a silent fallback would hide. A parent: naming a branch the project
// doesn't have is a typo in a hand-edited registry, and the only safe
// response is to refuse: branching from the default instead would put the
// preview back on production data with nothing in the diff to show it.
//
// It must also fail EARLY. The check ensureNeonBranch does at creation time
// is the real gate, but it runs at stage 5 — after EnsureNamespace and after
// every member has been built — so it would leave a phase=failed namespace to
// clean up by hand plus a round of wasted builds, for a static config error
// knowable before the run started. Hence Up's pre-flight, and hence the
// no-namespace/no-build assertions below.
func TestUpUnknownNeonParentFailsBeforeTheNamespaceExists(t *testing.T) {
	d := newTwoMemberDeps(t)
	const project = "aged-river-81935268"
	setNeonParent(t, d.orch.Registry, "footstrike-api", "staging-typo")

	err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{})
	if err == nil {
		t.Fatal("expected an error for a parent: naming a branch that does not exist, got nil")
	}
	// Asserted verbatim, not by substring: this message IS the deliverable —
	// it has to name the service, the project, and the missing branch, and
	// say which registry key to go fix, since none of those is recoverable
	// from the others when staring at a failed `bif preview up`.
	const wantErr = `preview: Up: pre-flight: neon parent branch for footstrike-api: ` +
		`neon project aged-river-81935268 has no branch named "staging-typo" (registry preview.neon.parent)`
	if err.Error() != wantErr {
		t.Errorf("error =\n  %q\nwant\n  %q", err.Error(), wantErr)
	}
	if len(d.kube.namespaces) != 0 {
		t.Errorf("namespaces = %v, want none: an unknown parent must be caught before the namespace exists, or it leaves a zombie failed preview behind", d.kube.namespaces)
	}
	if len(d.gcb.runCalls) != 0 {
		t.Errorf("RunTrigger calls = %v, want none: the pre-flight must run before any build minutes are spent", d.gcb.runCalls)
	}
	if got := d.neon.createCallsFor(project); len(got) != 0 {
		t.Errorf("CreateBranch calls = %+v, want none — under no circumstances may an unresolvable parent fall back to the default branch", got)
	}
}

// TestEnsureNeonBranchRejectsAParentMissingAtCreationTime exercises the
// creation-time gate on its own, without Up's pre-flight in front of it. That
// gate is the one that actually governs what CreateBranch is passed: the
// pre-flight is an early-warning copy, and a branch can be deleted in the
// minutes between the two (a preview's builds are not instant). Neither check
// may fall back to the default branch.
func TestEnsureNeonBranchRejectsAParentMissingAtCreationTime(t *testing.T) {
	nc := &fakeNeon{branches: map[string][]neon.Branch{
		// Only the default branch is present — the parent is gone, which is
		// what the pre-flight would have seen a moment earlier had it run.
		"proj-api": {{ID: "br-default", Name: "production"}},
	}}
	o := &Orchestrator{Neon: nc}
	ref := NeonRef{Project: "proj-api", Database: "fitnessdb", Role: "owner", Parent: "development"}

	_, err := o.ensureNeonBranch(context.Background(), ref, "hae-cadence")
	if err == nil {
		t.Fatal("expected an error when the configured parent branch is absent at creation time, got nil")
	}
	const wantErr = `neon project proj-api has no branch named "development" (registry preview.neon.parent)`
	if err.Error() != wantErr {
		t.Errorf("error = %q, want %q", err.Error(), wantErr)
	}
	if got := nc.createCallsFor("proj-api"); len(got) != 0 {
		t.Errorf("CreateBranch calls = %+v, want none: a missing parent must abort the create, not fall back to the project default", got)
	}
}

// TestEnsureNeonBranchReusesAnExistingPreviewBranchWithoutResolvingTheParent
// pins that parent resolution happens only on the create path. A re-run over
// a live preview finds its branch by name and must not fail merely because
// the parent has since been renamed or removed — the branch it would have
// been the parent of already exists.
func TestEnsureNeonBranchReusesAnExistingPreviewBranchWithoutResolvingTheParent(t *testing.T) {
	nc := &fakeNeon{branches: map[string][]neon.Branch{
		"proj-api": {{ID: "br-existing", Name: previewBranchName("hae-cadence")}},
	}}
	o := &Orchestrator{Neon: nc}
	ref := NeonRef{Project: "proj-api", Database: "fitnessdb", Role: "owner", Parent: "long-gone"}

	if _, err := o.ensureNeonBranch(context.Background(), ref, "hae-cadence"); err != nil {
		t.Fatalf("ensureNeonBranch failed: %v", err)
	}
	if got := nc.createCallsFor("proj-api"); len(got) != 0 {
		t.Errorf("CreateBranch calls = %+v, want none (the preview branch already existed)", got)
	}
}

// ---- Up: failure paths --------------------------------------------------------

func TestUpRejectsEmptyBranch(t *testing.T) {
	d := newTwoMemberDeps(t)
	for _, branch := range []string{"", "   "} {
		if err := d.orch.Up(context.Background(), branch, UpOptions{}); err == nil {
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
	if err := d.orch.Up(context.Background(), "!!!", UpOptions{}); err == nil {
		t.Fatal("expected an error for a branch that slugs to an empty tag, got nil")
	}
}

func TestUpNeonCreateBranchFailureFailsTheRun(t *testing.T) {
	d := newTwoMemberDeps(t)
	d.neon.createErr = map[string]error{"aged-river-81935268": errors.New("neon: quota exceeded")}

	err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{})
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

	err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{})
	if err == nil {
		t.Fatal("expected an error when the final ready AnnotateNamespace call fails, got nil")
	}
	if !strings.Contains(err.Error(), "mark ready") {
		t.Errorf("error = %q, want it to mention marking the namespace ready", err.Error())
	}
}

// TestUpFailDetachesAnnotateFromADeadRunContext proves the fix for a
// Critical review finding: fail()'s compensating AnnotateNamespace write
// must land even when the run's own context is already
// cancelled/expired by the time a post-namespace stage fails (the
// realistic trigger is the API layer's future 30-minute goroutine deadline
// firing right as, say, a build-poll wait gives up) — otherwise the
// namespace is stuck at bifrost/phase=creating with no bifrost/error,
// silently violating the "every post-namespace failure lands on failed"
// contract. fakeKube.AnnotateNamespace is context-respecting specifically
// so this test can catch a regression to the old "reuse ctx directly"
// behavior.
func TestUpFailDetachesAnnotateFromADeadRunContext(t *testing.T) {
	d := newTwoMemberDeps(t)
	d.gcb.statuses = map[string][]gcb.BuildStatus{"trig-api": {{Status: "FAILURE"}}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The run context dies the instant the namespace exists — i.e. after the
	// still-Terminating pre-check's read (which a dead context would fail,
	// aborting the run before any of this) and before the build failure this
	// test is actually about. What's under test is that fail()'s compensating
	// annotate write lands anyway, on a context detached from this one.
	d.kube.onEnsureNamespace = func(string) { cancel() }

	err := d.orch.Up(ctx, "hae-cadence", UpOptions{})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if strings.Contains(err.Error(), "annotate failure") {
		t.Errorf("error = %q, want the compensating annotate write to have succeeded despite the dead context", err.Error())
	}

	ns, ok := d.kube.namespaces["preview-hae-cadence"]
	if !ok {
		t.Fatal("namespace should exist (EnsureNamespace ran before the failure)")
	}
	if ns.annotations["bifrost/phase"] != "failed" {
		t.Errorf("bifrost/phase = %q, want failed even though the run context was already cancelled", ns.annotations["bifrost/phase"])
	}
	if ns.annotations["bifrost/error"] == "" {
		t.Error("bifrost/error is empty, want the build-failure message preserved despite the dead run context")
	}
}

func TestUpBuildFailureSetsPhaseFailed(t *testing.T) {
	d := newTwoMemberDeps(t)
	d.gcb.statuses = map[string][]gcb.BuildStatus{
		"trig-api": {{Status: "FAILURE"}},
	}

	err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{})
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
	if len(d.neon.previewBranchesFor("aged-river-81935268")) != 0 {
		t.Errorf("aged-river-81935268 preview branches = %+v, want none created (build failed first)", d.neon.previewBranchesFor("aged-river-81935268"))
	}
	if len(d.kube.copySecretCalls) != 0 {
		t.Errorf("CopySecret called %d times, want 0 (build failed first)", len(d.kube.copySecretCalls))
	}
}

// A failed build's message must point at the build's log, not merely name a
// status: the URL is what turns bifrost/error from a fact into a next step,
// and it has to survive all the way into the annotation the UI and `ib
// preview up` read, not just into the error Up returns.
func TestUpBuildFailureErrorCarriesLogURL(t *testing.T) {
	const logURL = "https://console.cloud.google.com/cloud-build/builds/b1?project=1234"
	d := newTwoMemberDeps(t)
	d.gcb.statuses = map[string][]gcb.BuildStatus{
		"trig-api": {{Status: "FAILURE", LogURL: logURL}},
	}

	err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{})
	if err == nil {
		t.Fatal("expected Up to fail when a build fails, got nil")
	}
	if !strings.Contains(err.Error(), logURL) {
		t.Errorf("error = %q, want it to carry the build's log URL %q", err.Error(), logURL)
	}

	got := d.kube.namespaces["preview-hae-cadence"].annotations["bifrost/error"]
	if !strings.Contains(got, logURL) {
		t.Errorf("bifrost/error = %q, want it to carry the build's log URL %q", got, logURL)
	}
	// Single line, and short enough to read in a table cell: this string is
	// served over the API and rendered into a grid row.
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("bifrost/error = %q, want a single line", got)
	}
	if len(got) > 200 {
		t.Errorf("bifrost/error is %d chars (%q), want something a table cell can show", len(got), got)
	}
}

// With no LogURL to link — an API response that omitted it — the message must
// degrade to something still actionable (the build ID, enough for `gcloud
// builds log`) rather than a dangling pointer to nowhere.
func TestUpBuildFailureWithoutLogURLDegradesCleanly(t *testing.T) {
	d := newTwoMemberDeps(t)
	d.gcb.statuses = map[string][]gcb.BuildStatus{
		"trig-api": {{Status: "FAILURE"}}, // LogURL deliberately empty
	}

	err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{})
	if err == nil {
		t.Fatal("expected Up to fail when a build fails, got nil")
	}
	got := d.kube.namespaces["preview-hae-cadence"].annotations["bifrost/error"]
	if strings.Contains(got, "http") {
		t.Errorf("bifrost/error = %q, want no link at all when the build reported no LogURL", got)
	}
	// A trailing separator with nothing after it is exactly the dangling
	// "see: " this guards against.
	if strings.HasSuffix(strings.TrimSpace(got), ":") {
		t.Errorf("bifrost/error = %q, want no dangling trailing separator", got)
	}
	// fakeGCB returns the triggerID as the build ID.
	if !strings.Contains(got, "trig-api") {
		t.Errorf("bifrost/error = %q, want the build ID as the fallback handle", got)
	}
	if !strings.Contains(got, "FAILURE") {
		t.Errorf("bifrost/error = %q, want it to still report the build status", got)
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
	err := d.orch.Up(ctx, "hae-cadence", UpOptions{})
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

	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("Up failed: %v", err)
	}
	if !anyObjectHasImage(d.kube.applyCalls, "footstrike-api:preview-realsha7") {
		t.Error("rendered footstrike-api image doesn't carry the SHA from the final poll, want realsha7")
	}
}

func TestUpDashboardWithoutClientIDErrors(t *testing.T) {
	cfg := &config.Config{
		// PreviewOAuthClientID intentionally left empty.
	}
	gh := &fakeGitHub{members: map[string]bool{"footstrike-dashboard": true}}
	kc := newFakeKube()
	// Fleet supplies footstrike-api's and identity's staging URLs (both are
	// non-members here), matching what this test used to hardcode into
	// cfg.StagingURLs -- both resolve fine, isolating the failure to the
	// missing PreviewOAuthClientID.
	o := &Orchestrator{Cfg: cfg, Kube: kc, GitHub: gh, Neon: &fakeNeon{}, Builds: &fakeGCB{}, Registry: testRegistry(t), Fleet: testFleet(t)}

	err := o.Up(context.Background(), "dash-only-branch", UpOptions{})
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

	err := d.orch.Up(context.Background(), "nonexistent-branch", UpOptions{})
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

	err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{})
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
	const secretURI = "postgres://realuser:hunter2@aged-river-81935268.neon.tech/fitnessdb"
	d.neon.connURI = map[string]string{"aged-river-81935268": secretURI}
	d.kube.copySecretErr = errors.New("secret copy: connection refused")

	err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{})
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

	err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{})
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

	err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{})
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

// ---- Up: the pre-flight refusals (terminating, tag collision) -----------------

// assertNothingWasStarted asserts that a run bailed out before it did any of
// the expensive, externally-visible work: no build triggered, no Neon branch
// created, no secret copied, nothing applied — and that the namespace was
// never even annotated, which is the witness that EnsureNamespace itself
// wasn't reached.
//
// The build assertions are the point of both refusals, not garnish. A run
// against a namespace Kubernetes is deleting cannot possibly succeed, and it
// used to discover that only at copy-secrets, minutes and two Cloud Builds
// later. A run against another branch's namespace succeeded, which is worse:
// it adopted that branch's Neon branch and its migrations. Asserting only that
// Up returned an error would pass just as happily against either version.
//
// It is written against the hae-cadence fixture's tag, which every caller
// shares — including the collision tests, whose two colliding branch names
// both derive exactly that tag.
func (d *testDeps) assertNothingWasStarted(t *testing.T) {
	t.Helper()
	for _, trigger := range []string{"trig-api", "trig-dash"} {
		if n := d.gcb.runCallsFor(trigger); n != 0 {
			t.Errorf("RunTrigger(%s) called %d times, want 0 — the run must fail before any build starts", trigger, n)
		}
	}
	if d.neon.hasBranch("aged-river-81935268", previewBranchName("hae-cadence")) {
		t.Error("a Neon branch was created, want none — the run must fail before it branches a database")
	}
	if n := len(d.kube.copySecretCalls); n != 0 {
		t.Errorf("CopySecret called %d times, want 0", n)
	}
	if n := len(d.kube.applyCalls); n != 0 {
		t.Errorf("ApplyObjects called %d times, want 0", n)
	}
	if n := len(d.kube.annotationHistory); n != 0 {
		t.Errorf("namespace was annotated %d times, want 0 — EnsureNamespace must never be reached", n)
	}
}

// TestUpRefusesATerminatingNamespace is the production bug: the owner tore a
// preview down and immediately recreated it, and Up ran for two and a half
// minutes against a namespace that no longer existed — completing both Cloud
// Builds and branching Neon — before dying at copy-secrets with `namespaces
// "preview-training-plans" not found`. Kubernetes accepts an annotation
// update on a Terminating namespace, so EnsureNamespace SUCCEEDS and there is
// no error for Up to catch.
func TestUpRefusesATerminatingNamespace(t *testing.T) {
	d := newTwoMemberDeps(t)
	// A namespace mid-delete: still present, still carrying the last run's
	// annotations, but on its way out.
	d.kube.namespaces["preview-hae-cadence"] = &fakeNamespace{
		labels:      map[string]string{"bifrost/preview": "true"},
		annotations: map[string]string{"bifrost/phase": "ready"},
		phase:       "Terminating",
	}

	err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{})
	if !errors.Is(err, ErrTerminating) {
		t.Fatalf("Up = %v, want ErrTerminating so the API layer and CLI can report it precisely", err)
	}
	if !strings.Contains(err.Error(), "preview-hae-cadence") {
		t.Errorf("error = %q, want it to name the namespace", err.Error())
	}
	d.assertNothingWasStarted(t)

	// The dying namespace's own annotations are left exactly as they were:
	// a refused run must not scribble bifrost/phase=creating onto a preview
	// that is being deleted.
	if got := d.kube.annotations("preview-hae-cadence")["bifrost/phase"]; got != "ready" {
		t.Errorf("bifrost/phase = %q, want the terminating namespace's annotations untouched", got)
	}
}

// TestUpCreatesAFreshNamespaceOnceTheOldOneIsGone is the other half of the
// same flow, and the one the refusal must not break: recreate-after-teardown
// is the ordinary path, and once Kubernetes has actually finished the delete
// there is nothing left to refuse. A pre-check that treated "absent" as
// anything other than "go ahead" would make every second `bif preview up`
// fail forever.
func TestUpCreatesAFreshNamespaceOnceTheOldOneIsGone(t *testing.T) {
	d := newTwoMemberDeps(t)
	if _, present := d.kube.namespaces["preview-hae-cadence"]; present {
		t.Fatal("fixture already has the namespace; this test is about its total absence")
	}

	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("Up over a fully-absent namespace failed: %v", err)
	}
	if got := d.kube.annotations("preview-hae-cadence")["bifrost/phase"]; got != "ready" {
		t.Errorf("bifrost/phase = %q, want ready — the namespace must be created fresh and run through", got)
	}
	for _, trigger := range []string{"trig-api", "trig-dash"} {
		if n := d.gcb.runCallsFor(trigger); n != 1 {
			t.Errorf("RunTrigger(%s) called %d times, want 1", trigger, n)
		}
	}
}

// TestUpAbortsWhenTheNamespaceCannotBeRead covers the pre-check's own failure
// mode. A namespace bifrost can't read is not evidence that it is safe to
// build into — the same reading PurgeExpired applies to its re-read — and
// proceeding on an unknown is exactly the bug the pre-check exists to stop.
// It is deliberately NOT ErrTerminating: nothing was determined, so nothing
// should be claimed.
func TestUpAbortsWhenTheNamespaceCannotBeRead(t *testing.T) {
	d := newTwoMemberDeps(t)
	d.kube.getErrByNS = map[string]error{"preview-hae-cadence": errors.New("apiserver: etcdserver: request timed out")}

	err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{})
	if err == nil {
		t.Fatal("Up = nil, want an error when the namespace read fails")
	}
	if errors.Is(err, ErrTerminating) {
		t.Errorf("error = %v, want a plain read failure rather than a terminating verdict", err)
	}
	if !strings.Contains(err.Error(), "preview-hae-cadence") {
		t.Errorf("error = %q, want it to name the namespace", err.Error())
	}
	d.assertNothingWasStarted(t)
}

// collidingBranches are two DIFFERENT branch names that derive the same tag,
// "hae-cadence" — the fixture tag every test in this file uses. They collide
// by character folding alone ('/' and '_' both become '-'), so neither is long
// enough to truncate: the collision the guard exists for needs no long branch
// name at all. TestTagForBranchIsManyToOne pins the property itself, including
// the truncation path.
const (
	collidingBranchA = "hae/cadence"
	collidingBranchB = "hae_cadence"
)

// TestUpRefusesACollidingTagFromADifferentBranch is the silent-corruption
// path this guard closes. Two branches deriving one tag used to share one
// preview with no warning at any layer: EnsureNamespace merges (overwriting
// bifrost/branch with the newcomer), ensureNeonBranch finds the existing
// preview-<tag> Neon branch by name and reuses it — inheriting whatever
// migrations the first branch applied — and one `bif preview down` on that tag
// then destroys both.
//
// The refusal has to land BEFORE any of that, which is what
// assertNothingWasStarted is checking: a guard that fired after the builds
// would still have branched the database and burned two Cloud Builds, and one
// that fired after EnsureNamespace would already have stolen the first
// branch's namespace record.
func TestUpRefusesACollidingTagFromADifferentBranch(t *testing.T) {
	d := newTwoMemberDeps(t)
	if TagForBranch(collidingBranchA) != TagForBranch(collidingBranchB) {
		t.Fatalf("fixture branches %q and %q no longer collide; this test is about the collision",
			collidingBranchA, collidingBranchB)
	}
	// The first branch's finished preview, exactly as its own Up left it.
	d.kube.namespaces["preview-hae-cadence"] = &fakeNamespace{
		labels: map[string]string{"bifrost/preview": "true"},
		annotations: map[string]string{
			"bifrost/branch": collidingBranchA,
			"bifrost/phase":  "ready",
		},
	}

	err := d.orch.Up(context.Background(), collidingBranchB, UpOptions{})
	if !errors.Is(err, ErrTagCollision) {
		t.Fatalf("Up = %v, want ErrTagCollision so the API layer and CLI can report it precisely", err)
	}
	// Both branches by name: an operator reading this has to know which
	// preview is in the way, not merely that something is.
	for _, want := range []string{collidingBranchA, collidingBranchB, "hae-cadence"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err.Error(), want)
		}
	}
	d.assertNothingWasStarted(t)

	// The incumbent's record is left exactly as it was: a refused run must not
	// rewrite bifrost/branch to the newcomer, which is the very overwrite that
	// made the collision silent.
	if got := d.kube.annotations("preview-hae-cadence")["bifrost/branch"]; got != collidingBranchA {
		t.Errorf("bifrost/branch = %q, want the incumbent branch %q untouched", got, collidingBranchA)
	}
	if got := d.kube.annotations("preview-hae-cadence")["bifrost/phase"]; got != "ready" {
		t.Errorf("bifrost/phase = %q, want the incumbent preview left ready", got)
	}
}

// TestUpProceedsOverItsOwnNamespace is the case the guard must NOT catch, and
// the one with the most to lose: re-running `bif preview up` on the same branch
// is the documented recovery path for a stuck or failed preview, and the
// auto-update watcher calls Up on the recorded branch every time a member's
// SHA moves. A guard that refused on the mere presence of a namespace would
// break both, permanently — every preview would be creatable exactly once.
func TestUpProceedsOverItsOwnNamespace(t *testing.T) {
	d := newTwoMemberDeps(t)
	// A previous run of the SAME branch that failed partway: the state the
	// recovery re-run is aimed at.
	d.kube.namespaces["preview-hae-cadence"] = &fakeNamespace{
		labels: map[string]string{"bifrost/preview": "true"},
		annotations: map[string]string{
			"bifrost/branch": "hae-cadence",
			"bifrost/phase":  "failed",
			"bifrost/error":  "build ended with status FAILURE",
		},
	}

	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("re-Up on the same branch failed: %v", err)
	}
	if got := d.kube.annotations("preview-hae-cadence")["bifrost/phase"]; got != "ready" {
		t.Errorf("bifrost/phase = %q, want ready — the retry must run all the way through", got)
	}
	for _, trigger := range []string{"trig-api", "trig-dash"} {
		if n := d.gcb.runCallsFor(trigger); n != 1 {
			t.Errorf("RunTrigger(%s) called %d times, want 1 — the retry must really rebuild", trigger, n)
		}
	}
}

// TestUpProceedsOverANamespaceWithNoBranchAnnotation covers the third state
// the read can find: a namespace that records no branch at all — a preview
// predating the annotation, or one left by a partial or out-of-band run.
//
// It proceeds, deliberately. Absence is not evidence of a collision, and
// refusing on it would strand the namespace forever: no `bif preview up` could
// ever get past the guard again, including the re-run that would repair it.
// The EnsureNamespace that follows writes the annotation, so the state is
// self-healing rather than permanent.
func TestUpProceedsOverANamespaceWithNoBranchAnnotation(t *testing.T) {
	d := newTwoMemberDeps(t)
	d.kube.namespaces["preview-hae-cadence"] = &fakeNamespace{
		labels:      map[string]string{"bifrost/preview": "true"},
		annotations: map[string]string{"bifrost/phase": "ready"},
	}

	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("Up over a namespace with no bifrost/branch annotation failed: %v", err)
	}
	if got := d.kube.annotations("preview-hae-cadence")["bifrost/phase"]; got != "ready" {
		t.Errorf("bifrost/phase = %q, want ready", got)
	}
	// And the run repaired what it found: the next Up has the evidence this
	// one didn't.
	if got := d.kube.annotations("preview-hae-cadence")["bifrost/branch"]; got != "hae-cadence" {
		t.Errorf("bifrost/branch = %q, want the run to have recorded the branch it ran for", got)
	}
}

// ---- Up: waiting for pods -----------------------------------------------------

// shrinkPodWait shrinks the readiness wait's bound and poll interval for the
// duration of a test, so a test that exercises the timeout path costs
// milliseconds instead of five real minutes. The bound stays generously
// larger than the interval so a test can still distinguish "gave up at the
// bound" from "failed early".
func shrinkPodWait(t *testing.T, timeout, interval time.Duration) {
	t.Helper()
	oldTimeout, oldInterval := podReadyTimeout, podPollInterval
	podReadyTimeout, podPollInterval = timeout, interval
	t.Cleanup(func() { podReadyTimeout, podPollInterval = oldTimeout, oldInterval })
}

// TestUpWaitsForPodsBeforeMarkingReady is the core of this behavior: the
// preview only reaches ready after the pods actually report ready, not when
// ApplyObjects returns. The scripted namespace starts with a pod still
// running its migrate initContainer and only converges on the third poll —
// so an Up that declared ready off the apply alone would finish having
// listed pods at most once.
func TestUpWaitsForPodsBeforeMarkingReady(t *testing.T) {
	shrinkPodWait(t, 5*time.Second, time.Millisecond)
	d := newTwoMemberDeps(t)
	const ns = "preview-hae-cadence"

	migrating := []kube.PodInfo{
		initializingPod("footstrike-api", apiImage, kube.ContainerInfo{Name: "migrate"}),
		readyPod("footstrike-dashboard", dashImage),
	}
	converged := []kube.PodInfo{
		readyPod("footstrike-api", apiImage),
		readyPod("footstrike-dashboard", dashImage),
	}
	d.kube.podScript = map[string][][]kube.PodInfo{ns: {migrating, migrating, converged}}

	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("Up failed: %v", err)
	}
	if got := d.kube.namespaces[ns].annotations["bifrost/phase"]; got != "ready" {
		t.Errorf("bifrost/phase = %q, want ready", got)
	}
	if got := d.kube.listPodsCalls[ns]; got != 3 {
		t.Errorf("ListPods called %d times, want exactly 3 (Up must keep polling until the pods converge)", got)
	}

	// "waiting for pods" must be narrated before ready, not after: it's the
	// stage a user watches the CLI sit on.
	var sawWaiting bool
	for _, snap := range d.kube.annotationHistory {
		if snap["bifrost/step"] == "waiting for pods" {
			sawWaiting = true
		}
		if snap["bifrost/phase"] == "ready" && !sawWaiting {
			t.Error("preview reached ready without ever narrating the pod wait")
		}
	}
	if !sawWaiting {
		t.Error("no bifrost/step write said \"waiting for pods\"")
	}
}

// TestUpFailsWhenMigrateInitContainerCrashLoops is the failure mode Task 1
// created: a branch whose alembic migration fails leaves the migrate
// initContainer in CrashLoopBackOff and the app container never starts.
// The preview must land on failed, naming both the member and the migrate
// step specifically — "footstrike-api not ready: PodInitializing" would tell
// an operator nothing about where to look.
func TestUpFailsWhenMigrateInitContainerCrashLoops(t *testing.T) {
	// A long bound (relative to the poll interval) that this test must NOT
	// spend: a crash-looping container is terminal, so the wait has to give
	// up immediately rather than burn the whole timeout.
	shrinkPodWait(t, 30*time.Second, time.Millisecond)
	d := newTwoMemberDeps(t)
	const ns = "preview-hae-cadence"

	exitCode := int32(1)
	d.kube.podScript = map[string][][]kube.PodInfo{ns: {{
		initializingPod("footstrike-api", apiImage, kube.ContainerInfo{
			Name:             "migrate",
			WaitingReason:    "CrashLoopBackOff",
			RestartCount:     3,
			ExitCode:         &exitCode,
			TerminatedReason: "Error",
		}),
		readyPod("footstrike-dashboard", dashImage),
	}}}

	start := time.Now()
	err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected Up to fail when the migrate initContainer crash-loops, got nil")
	}
	if elapsed > 5*time.Second {
		t.Errorf("Up took %v to fail, want it to give up as soon as it sees CrashLoopBackOff rather than waiting out the %v bound", elapsed, podReadyTimeout)
	}

	want := "footstrike-api not ready: migrate initContainer CrashLoopBackOff"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	nsRec := d.kube.namespaces[ns]
	if got := nsRec.annotations["bifrost/phase"]; got != "failed" {
		t.Errorf("bifrost/phase = %q, want failed (a readiness failure is a failed preview, not a new phase)", got)
	}
	if got := nsRec.annotations["bifrost/error"]; got != want {
		t.Errorf("bifrost/error = %q, want %q", got, want)
	}
	// The retained step is the diagnostic: which stage died.
	if got := nsRec.annotations["bifrost/step"]; got != "waiting for pods" {
		t.Errorf("bifrost/step on the failed preview = %q, want the pod wait retained", got)
	}
	// Sanitized: the annotation is served over the JSON API, and this
	// preview's namespace holds a real DATABASE_URL.
	if strings.Contains(nsRec.annotations["bifrost/error"], "postgres://") ||
		strings.Contains(nsRec.annotations["bifrost/error"], "fakesecret") {
		t.Errorf("bifrost/error leaks connection details: %q", nsRec.annotations["bifrost/error"])
	}
}

// TestUpTimesOutWhenPodsNeverBecomeReady covers the bound itself: a member
// stuck starting forever (no terminal reason to short-circuit on) fails the
// preview once the wait expires, naming the member.
func TestUpTimesOutWhenPodsNeverBecomeReady(t *testing.T) {
	shrinkPodWait(t, 50*time.Millisecond, time.Millisecond)
	d := newTwoMemberDeps(t)
	const ns = "preview-hae-cadence"

	d.kube.podScript = map[string][][]kube.PodInfo{ns: {{
		// Still pulling its image: not terminal, so the wait keeps trying
		// until the bound rather than failing on the first sighting.
		initializingPod("footstrike-api", apiImage, kube.ContainerInfo{Name: "migrate", WaitingReason: "ImagePullBackOff"}),
		readyPod("footstrike-dashboard", dashImage),
	}}}

	err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{})
	if err == nil {
		t.Fatal("expected Up to fail when a member's pods never become ready, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want it to say the wait timed out", err.Error())
	}
	if !strings.Contains(err.Error(), "footstrike-api") {
		t.Errorf("error = %q, want it to name the member that never came up", err.Error())
	}
	if !strings.Contains(err.Error(), "migrate initContainer ImagePullBackOff") {
		t.Errorf("error = %q, want the pod's own reason carried through", err.Error())
	}
	if strings.Contains(err.Error(), "footstrike-dashboard") {
		t.Errorf("error = %q, want only the member that isn't ready named", err.Error())
	}
	if d.kube.listPodsCalls[ns] < 2 {
		t.Errorf("ListPods called %d times, want repeated polling across the bound", d.kube.listPodsCalls[ns])
	}
	nsRec := d.kube.namespaces[ns]
	if got := nsRec.annotations["bifrost/phase"]; got != "failed" {
		t.Errorf("bifrost/phase = %q, want failed", got)
	}
	if got := nsRec.annotations["bifrost/error"]; !strings.Contains(got, "timed out") {
		t.Errorf("bifrost/error = %q, want the timeout recorded", got)
	}
}

// TestUpTimesOutWhenAMemberHasNoPods is the "nothing to wait for" care
// point: a member whose manifests produced no Deployment (so no pods ever
// appear) must hit the bound with a clear message, not spin forever.
func TestUpTimesOutWhenAMemberHasNoPods(t *testing.T) {
	shrinkPodWait(t, 50*time.Millisecond, time.Millisecond)
	d := newTwoMemberDeps(t)
	const ns = "preview-hae-cadence"

	// footstrike-dashboard's pods show up; footstrike-api's never do.
	d.kube.podScript = map[string][][]kube.PodInfo{ns: {{readyPod("footstrike-dashboard", dashImage)}}}

	err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{})
	if err == nil {
		t.Fatal("expected Up to fail when a member never gets any pods, got nil")
	}
	want := "footstrike-api not ready: no pods found"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), want)
	}
	if got := d.kube.namespaces[ns].annotations["bifrost/phase"]; got != "failed" {
		t.Errorf("bifrost/phase = %q, want failed", got)
	}
}

// TestUpPodWaitRetriesListFailures: one ListPods blip must not
// destroy a preview whose builds took ten minutes. (The fake fails every
// call while listPodsErr is set, so this also pins that a *persistent*
// failure still ends in a timeout naming the cause rather than hanging.)
func TestUpPodWaitRetriesListFailures(t *testing.T) {
	shrinkPodWait(t, 50*time.Millisecond, time.Millisecond)
	d := newTwoMemberDeps(t)
	d.kube.listPodsErr = errors.New("pods list: connection reset by peer")

	err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{})
	if err == nil {
		t.Fatal("expected Up to fail when pods can never be listed, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("error = %q, want a timeout carrying the underlying list failure", err.Error())
	}
	if d.kube.listPodsCalls["preview-hae-cadence"] < 2 {
		t.Errorf("ListPods called %d times, want the list error retried rather than failing the run on the first blip", d.kube.listPodsCalls["preview-hae-cadence"])
	}
}

// TestUpPodWaitRespectsContextCancellation: the wait must not outlive the
// run's own context (the API layer's 30-minute goroutine budget), and the
// namespace must still land on failed via fail()'s detached write.
func TestUpPodWaitRespectsContextCancellation(t *testing.T) {
	// Leave the real 5-minute bound in place: the point is that ctx, not the
	// bound, is what ends this wait.
	shrinkPodWait(t, 5*time.Minute, 10*time.Millisecond)
	d := newTwoMemberDeps(t)
	const ns = "preview-hae-cadence"
	d.kube.podScript = map[string][][]kube.PodInfo{ns: {{
		initializingPod("footstrike-api", apiImage, kube.ContainerInfo{Name: "migrate"}),
	}}}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := d.orch.Up(ctx, "hae-cadence", UpOptions{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected Up to fail when the context dies mid-wait, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("Up took %v to return after context cancellation, want it to abandon the 5-minute bound immediately", elapsed)
	}
	if got := d.kube.namespaces[ns].annotations["bifrost/phase"]; got != "failed" {
		t.Errorf("bifrost/phase = %q, want failed", got)
	}
}

// TestDownWorksAfterAReadinessFailure: teardown of a preview that failed the
// pod wait is unchanged — the namespace exists and its Neon branch was
// created before the failure, so both must still be cleaned up. A readiness
// failure must not strand cluster or Neon resources.
func TestDownWorksAfterAReadinessFailure(t *testing.T) {
	shrinkPodWait(t, 30*time.Second, time.Millisecond)
	d := newTwoMemberDeps(t)
	const ns = "preview-hae-cadence"
	d.kube.podScript = map[string][][]kube.PodInfo{ns: {{
		initializingPod("footstrike-api", apiImage, kube.ContainerInfo{Name: "migrate", WaitingReason: "CrashLoopBackOff"}),
	}}}

	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err == nil {
		t.Fatal("expected the Up to fail its readiness wait, got nil")
	}
	if len(d.neon.previewBranchesFor("aged-river-81935268")) != 1 {
		t.Fatalf("expected the Neon preview branch to exist after the failed Up, got %+v", d.neon.previewBranchesFor("aged-river-81935268"))
	}

	if err := d.orch.Down(context.Background(), "hae-cadence"); err != nil {
		t.Fatalf("Down after a readiness failure: %v", err)
	}
	if _, stillPresent := d.kube.namespaces[ns]; stillPresent {
		t.Error("namespace still present after Down")
	}
	if len(d.neon.previewBranchesFor("aged-river-81935268")) != 0 {
		t.Errorf("aged-river-81935268 preview branches = %+v, want the preview branch deleted", d.neon.previewBranchesFor("aged-river-81935268"))
	}
}

// ---- readiness helpers --------------------------------------------------------

// TestPodNotReadyReason pins the diagnosis rules the failure message is
// built from — in particular that an init container's own reason beats the
// app container's contentless "PodInitializing", and that only
// CrashLoopBackOff is terminal.
func TestPodNotReadyReason(t *testing.T) {
	exit1 := int32(1)
	exit0 := int32(0)
	tests := []struct {
		name       string
		pod        kube.PodInfo
		wantReason string
		wantFatal  bool
	}{
		{
			name:       "ready pod",
			pod:        readyPod("footstrike-api", apiImage),
			wantReason: "",
		},
		{
			name:       "migrate crashlooping is terminal and named",
			pod:        initializingPod("footstrike-api", apiImage, kube.ContainerInfo{Name: "migrate", WaitingReason: crashLoopBackOff}),
			wantReason: "migrate initContainer CrashLoopBackOff",
			wantFatal:  true,
		},
		{
			name: "migrate that has failed once but isn't backing off yet",
			pod: initializingPod("footstrike-api", apiImage, kube.ContainerInfo{
				Name: "migrate", ExitCode: &exit1, TerminatedReason: "Error",
			}),
			wantReason: "migrate initContainer exited 1",
		},
		{
			name: "migrate OOM-killed names the reason",
			pod: initializingPod("footstrike-api", apiImage, kube.ContainerInfo{
				Name: "migrate", ExitCode: &exit1, TerminatedReason: "OOMKilled",
			}),
			wantReason: "migrate initContainer OOMKilled (exit 1)",
		},
		{
			name:       "migrate still running falls through to the app container",
			pod:        initializingPod("footstrike-api", apiImage, kube.ContainerInfo{Name: "migrate"}),
			wantReason: "PodInitializing",
		},
		{
			name: "a completed migrate is not a problem",
			pod: func() kube.PodInfo {
				p := initializingPod("footstrike-api", apiImage, kube.ContainerInfo{Name: "migrate", ExitCode: &exit0})
				p.Containers = []kube.ContainerInfo{{Name: "footstrike-api", WaitingReason: crashLoopBackOff}}
				return p
			}(),
			wantReason: "CrashLoopBackOff",
			wantFatal:  true,
		},
		{
			name: "no waiting reason at all still reads as not ready",
			pod: kube.PodInfo{
				Name: "footstrike-api-7d9f6b8c4d-nx2kp", OwnerKind: "ReplicaSet",
				OwnerName: "footstrike-api-7d9f6b8c4d", Phase: "Running",
				Containers: []kube.ContainerInfo{{Name: "footstrike-api"}},
			},
			wantReason: "containers not ready",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reason, fatal := podNotReadyReason(tc.pod)
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if fatal != tc.wantFatal {
				t.Errorf("fatal = %v, want %v", fatal, tc.wantFatal)
			}
		})
	}
}

// TestPodsForMemberIgnoresJobAndStaleGenerationPods: a member's CronJob is
// suspended in a preview, but a leftover "<svc>-purge-..." job pod must
// never be mistaken for one of the member's Deployment pods (it would hold
// the preview un-ready, or once Succeeded be waited on forever) — and
// neither must a pod left over from the *previous* Up, which is what a
// rolling update keeps around while the new one starts.
func TestPodsForMemberIgnoresJobAndStaleGenerationPods(t *testing.T) {
	jobPod := kube.PodInfo{
		Name: "footstrike-api-purge-29735100-8wrsp", OwnerKind: "Job",
		OwnerName: "footstrike-api-purge-29735100", Phase: "Failed",
		Containers: []kube.ContainerInfo{{Name: "footstrike-api-purge", Image: apiImage}},
	}
	succeeded := readyPod("footstrike-api", apiImage)
	succeeded.Name, succeeded.Phase = "footstrike-api-oldrs-abcde", "Succeeded"
	stale := readyPod("footstrike-api", previewImage("footstrike-api", "oldsha"))
	stale.Name, stale.OwnerName = "footstrike-api-1a2b3c4d5e-qqqqq", "footstrike-api-1a2b3c4d5e"
	pods := []kube.PodInfo{
		jobPod, succeeded, stale,
		readyPod("footstrike-api", apiImage),
		readyPod("footstrike-dashboard", dashImage),
	}

	got, anyGeneration := podsForMember(pods, "footstrike-api", apiImage)
	if len(got) != 1 || got[0].Name != "footstrike-api-7d9f6b8c4d-nx2kp" {
		t.Errorf("podsForMember = %+v, want only this generation's running ReplicaSet-owned pod", got)
	}
	if !anyGeneration {
		t.Error("anyGeneration = false, want true — the member does have (older) pods")
	}

	// With no image to key on, the filter has to fall back to matching every
	// generation rather than matching nothing.
	got, _ = podsForMember(pods, "footstrike-api", "")
	if len(got) != 2 {
		t.Errorf("podsForMember with no wantImage = %+v, want both running pods", got)
	}
}

// TestUpToleratesThePreviousRunsCrashLoopingPod is the regression guard on
// the documented recovery path: re-running Up is how a developer fixes a
// preview that failed (say, on a bad migration). A rolling update keeps the
// broken generation's pod around until the new one is ready, so the wait
// must judge only the pods this run applied — otherwise the fix always fails
// on the strength of the bug it fixes.
func TestUpToleratesThePreviousRunsCrashLoopingPod(t *testing.T) {
	shrinkPodWait(t, 30*time.Second, time.Millisecond)
	d := newTwoMemberDeps(t)
	const ns = "preview-hae-cadence"

	// The previous Up's pod, still crash-looping on its failed migration.
	broken := initializingPod("footstrike-api", previewImage("footstrike-api", "brokensha"),
		kube.ContainerInfo{Name: "migrate", WaitingReason: crashLoopBackOff})
	broken.Name, broken.OwnerName = "footstrike-api-1a2b3c4d5e-qqqqq", "footstrike-api-1a2b3c4d5e"

	starting := []kube.PodInfo{broken,
		initializingPod("footstrike-api", apiImage, kube.ContainerInfo{Name: "migrate"}),
		readyPod("footstrike-dashboard", dashImage)}
	converged := []kube.PodInfo{
		readyPod("footstrike-api", apiImage),
		readyPod("footstrike-dashboard", dashImage)}
	d.kube.podScript = map[string][][]kube.PodInfo{ns: {starting, converged}}

	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("Up failed on the previous run's leftover pod: %v", err)
	}
	if got := d.kube.namespaces[ns].annotations["bifrost/phase"]; got != "ready" {
		t.Errorf("bifrost/phase = %q, want ready", got)
	}
}

// TestSanitizeReasonBoundsFreeFormText: container reasons are short tokens,
// but a client-go error is free-form and lands in an annotation served over
// the JSON API — it must be flattened and capped.
func TestSanitizeReasonBoundsFreeFormText(t *testing.T) {
	if got := sanitizeReason("pods list:\n  connection   reset\n"); got != "pods list: connection reset" {
		t.Errorf("sanitizeReason collapsed to %q", got)
	}
	long := sanitizeReason(strings.Repeat("x", 500))
	if len([]rune(long)) > 170 {
		t.Errorf("sanitizeReason returned %d runes, want it capped", len([]rune(long)))
	}
}

// ---- Down -----------------------------------------------------------------

func TestDownDeletesNamespaceAndBestEffortDeletesNeonBranches(t *testing.T) {
	reg := Registry{
		"footstrike-api": {Neon: &NeonRef{Project: "proj-api", Database: "fitnessdb", Role: "fitness_owner"}},
		"identity":       {Neon: &NeonRef{Project: "proj-identity", Database: "identitydb", Role: "identity_owner"}},
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
	o := &Orchestrator{Kube: kc, Neon: nc, Registry: reg}

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
	reg := Registry{
		"identity": {Neon: &NeonRef{Project: "proj-identity", Database: "identitydb", Role: "identity_owner"}},
	}
	kc := newFakeKube()
	nc := &fakeNeon{branches: map[string][]neon.Branch{
		"proj-identity": {{ID: "br-1", Name: "preview-orphan"}},
	}}
	o := &Orchestrator{Kube: kc, Neon: nc, Registry: reg}

	if err := o.Down(context.Background(), "orphan"); err != nil {
		t.Fatalf("Down failed: %v", err)
	}
	if len(nc.branches["proj-identity"]) != 0 {
		t.Errorf("proj-identity branches = %v, want the orphaned branch deleted even though identity was never listed as a member anywhere", nc.branches["proj-identity"])
	}
}

func TestDownDeleteNamespaceFailureStillAttemptsNeonCleanup(t *testing.T) {
	reg := Registry{
		"footstrike-api": {Neon: &NeonRef{Project: "proj-api", Database: "fitnessdb", Role: "fitness_owner"}},
	}
	kc := newFakeKube()
	kc.deleteErr = errors.New("namespace delete: forbidden")
	nc := &fakeNeon{branches: map[string][]neon.Branch{"proj-api": {{ID: "br-1", Name: "preview-hae-cadence"}}}}
	o := &Orchestrator{Kube: kc, Neon: nc, Registry: reg}

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
	reg := Registry{
		"footstrike-api": {Neon: &NeonRef{Project: "proj-api", Database: "fitnessdb", Role: "fitness_owner"}},
	}
	kc := newFakeKube()
	nc := &fakeNeon{branches: map[string][]neon.Branch{"proj-api": {{ID: "br-1", Name: "some-other-branch"}}}}
	o := &Orchestrator{Kube: kc, Neon: nc, Registry: reg}

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
	go func() { done <- d.orch.Up(context.Background(), "busy-branch", UpOptions{}) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Up to reach the blocking hook")
	}

	if !d.orch.Busy(tag) {
		t.Error("Busy(tag) = false while Up is in flight, want true")
	}
	if err := d.orch.Up(context.Background(), "busy-branch", UpOptions{}); !errors.Is(err, ErrBusy) {
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

// ---- step progress reporting -------------------------------------------------

// distinctSteps extracts the bifrost/step values a run wrote, in order and
// with consecutive duplicates collapsed. Read from annotationHistory rather
// than the final state because every earlier step's annotation is overwritten
// by the next.
func (f *fakeKube) distinctSteps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var steps []string
	for _, snap := range f.annotationHistory {
		step, ok := snap["bifrost/step"]
		if !ok {
			continue
		}
		if len(steps) == 0 || steps[len(steps)-1] != step {
			steps = append(steps, step)
		}
	}
	return steps
}

// TestUpReportsStepsInOrder pins the sequence and wording of every
// bifrost/step write across a full two-member happy-path Up: the build stage,
// Neon branching, secret copying, manifest application, and the wait for those
// manifests' pods — in that order, and nothing else in between.
//
// The build stage is ONE step naming every member being built, deliberately
// replacing the old per-member "building footstrike-api (1/2)" sequence. Both
// halves of that wording had stopped being true: the builds now run
// concurrently (so there is no first-then-second to count off), and a member
// whose commit already has an image isn't built at all (so n is not the
// member count). See TestUpStepNarratesReusedAndBuiltMembers for the other two
// shapes this step takes.
//
// Membership resolution is deliberately NOT narrated: it finishes before the
// namespace exists (so there's nowhere to write it), and a step written after
// its own work is done, then overwritten by the first build step
// milliseconds later, is unobservable by any poller.
func TestUpReportsStepsInOrder(t *testing.T) {
	d := newTwoMemberDeps(t)

	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("Up failed: %v", err)
	}

	steps := d.kube.distinctSteps()
	want := []string{
		"", // cleared on entry, by EnsureNamespace's own write
		"building footstrike-api, footstrike-dashboard",
		"branching databases",
		"copying secrets",
		"applying manifests",
		"waiting for pods",
		"", // cleared on ready
	}
	if len(steps) != len(want) {
		t.Fatalf("distinct step sequence = %#v, want %#v", steps, want)
	}
	for i, w := range want {
		if steps[i] != w {
			t.Errorf("step[%d] = %q, want %q", i, steps[i], w)
		}
	}
}

// TestUpStepNarratesReusedAndBuiltMembers covers the two step wordings a
// re-run produces, which are the whole reason the old "(i/n)" numbering had to
// go: an operator watching a re-run has to be able to tell what is actually
// being rebuilt, and "building footstrike-api (1/2)" for a run that rebuilt
// nothing would be a lie in both halves.
func TestUpStepNarratesReusedAndBuiltMembers(t *testing.T) {
	d := newTwoMemberDeps(t)
	ctx := context.Background()
	if err := d.orch.Up(ctx, "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("first Up failed: %v", err)
	}

	// Nothing moved: every member is reused, and the step says so rather than
	// claiming a build.
	if err := d.orch.Up(ctx, "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("second Up failed: %v", err)
	}
	want := "reusing images for footstrike-api, footstrike-dashboard"
	if steps := d.kube.distinctSteps(); !slices.Contains(steps, want) {
		t.Errorf("step sequence = %#v, want it to contain %q", steps, want)
	}

	// One member moved: the step names the one being built AND the one being
	// reused, so neither is a surprise.
	d.github.shas = map[string]string{"footstrike-dashboard": "beef99887766554433"}
	if err := d.orch.Up(ctx, "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("third Up failed: %v", err)
	}
	want = "building footstrike-dashboard (reusing footstrike-api)"
	if steps := d.kube.distinctSteps(); !slices.Contains(steps, want) {
		t.Errorf("step sequence = %#v, want it to contain %q", steps, want)
	}
}

// TestUpStepSinceIsAnRFC3339Timestamp checks that every bifrost/step-since
// write is a real RFC3339 timestamp (the CLI parses it to compute elapsed
// time locally, per the plan) rather than, say, a duration string.
func TestUpStepSinceIsAnRFC3339Timestamp(t *testing.T) {
	d := newTwoMemberDeps(t)
	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("Up failed: %v", err)
	}
	ns := d.kube.namespaces["preview-hae-cadence"]
	// Ready clears step-since; the final annotate call happens after every
	// step write, so re-derive what was set for the last real step from
	// history instead of the cleared final state.
	var lastSince string
	for _, snap := range d.kube.annotationHistory {
		if since, ok := snap["bifrost/step-since"]; ok && since != "" {
			lastSince = since
		}
	}
	if lastSince == "" {
		t.Fatal("no non-empty bifrost/step-since was ever written")
	}
	if _, err := time.Parse(time.RFC3339, lastSince); err != nil {
		t.Errorf("bifrost/step-since = %q, not RFC3339: %v", lastSince, err)
	}
	if ns.annotations["bifrost/step-since"] != "" {
		t.Errorf("bifrost/step-since after ready = %q, want cleared", ns.annotations["bifrost/step-since"])
	}
}

// TestUpReadyClearsStep is the direct assertion behind "reaching ready
// clears the step": after a successful Up, the final namespace state must
// carry no step text, so a finished preview doesn't display a stale one.
func TestUpReadyClearsStep(t *testing.T) {
	d := newTwoMemberDeps(t)
	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("Up failed: %v", err)
	}
	ns := d.kube.namespaces["preview-hae-cadence"]
	if ns.annotations["bifrost/step"] != "" {
		t.Errorf("bifrost/step after ready = %q, want cleared", ns.annotations["bifrost/step"])
	}
}

// TestUpFailedPreviewRetainsLastStep is the direct assertion behind "a
// failed preview keeps its last step — that's the diagnostic": fail() must
// not clear (or overwrite) whatever step was last recorded, so an operator
// sees e.g. "failed while building footstrike-api" rather than a blank.
func TestUpFailedPreviewRetainsLastStep(t *testing.T) {
	d := newTwoMemberDeps(t)
	d.gcb.statuses = map[string][]gcb.BuildStatus{"trig-api": {{Status: "FAILURE"}}}

	err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{})
	if err == nil {
		t.Fatal("expected an error when a build fails, got nil")
	}
	ns := d.kube.namespaces["preview-hae-cadence"]
	if ns.annotations["bifrost/phase"] != "failed" {
		t.Fatalf("bifrost/phase = %q, want failed", ns.annotations["bifrost/phase"])
	}
	if ns.annotations["bifrost/step"] != "building footstrike-api, footstrike-dashboard" {
		t.Errorf("bifrost/step on a failed preview = %q, want the last step retained", ns.annotations["bifrost/step"])
	}
	if ns.annotations["bifrost/step-since"] == "" {
		t.Error("bifrost/step-since on a failed preview is empty, want the last step's timestamp retained")
	}
}

// TestUpRetryDoesNotInheritPreviousRunsStepOrError covers the recovery path
// the docs point operators at: fail a preview, then re-run `bif preview up`.
// EnsureNamespace MERGES annotations, so unless Up's entry write clears them
// explicitly, the previous run's bifrost/error and bifrost/step survive into
// the retry — and since the retry's own builds take minutes, the UI and the
// CLI would both show the OLD failure ("creating · building X — build ended
// with status FAILURE") for that entire window, which reads as if the new run
// had already failed.
//
// The assertion targets the retry's very first annotation snapshot (the
// EnsureNamespace write itself), because that's the write responsible: later
// snapshots legitimately carry this run's own fresh step text, which for an
// identical failure would be indistinguishable from the stale one.
func TestUpRetryDoesNotInheritPreviousRunsStepOrError(t *testing.T) {
	d := newTwoMemberDeps(t)
	d.gcb.statuses = map[string][]gcb.BuildStatus{"trig-api": {{Status: "FAILURE"}}}

	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err == nil {
		t.Fatal("expected the first Up to fail (its build fails), got nil")
	}
	ns := d.kube.namespaces["preview-hae-cadence"]
	staleStep, staleErr := ns.annotations["bifrost/step"], ns.annotations["bifrost/error"]
	if staleStep == "" || staleErr == "" {
		t.Fatalf("precondition: a failed preview should retain a step (%q) and an error (%q)", staleStep, staleErr)
	}
	firstRunWrites := len(d.kube.annotationHistory)

	// Retry: same branch, builds now succeed (an unconfigured build in
	// fakeGCB succeeds immediately).
	d.gcb.statuses = nil
	if err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{}); err != nil {
		t.Fatalf("retry Up failed: %v", err)
	}
	if len(d.kube.annotationHistory) <= firstRunWrites {
		t.Fatalf("retry recorded no annotation writes (history stayed at %d)", firstRunWrites)
	}

	entry := d.kube.annotationHistory[firstRunWrites]
	if entry["bifrost/phase"] != "creating" {
		t.Fatalf("retry's first annotation write has phase %q, want creating — this test is looking at the wrong snapshot", entry["bifrost/phase"])
	}
	if got := entry["bifrost/error"]; got != "" {
		t.Errorf("bifrost/error at the start of the retry = %q, want cleared (it's the previous run's failure)", got)
	}
	if got := entry["bifrost/step"]; got != "" {
		t.Errorf("bifrost/step at the start of the retry = %q, want cleared (it's the previous run's last step)", got)
	}
	if got := entry["bifrost/step-since"]; got != "" {
		t.Errorf("bifrost/step-since at the start of the retry = %q, want cleared (it would date the previous run's step)", got)
	}
}

// TestUpStepAnnotationFailureDoesNotFailTheRun is the constraint from the
// task brief made explicit: step() is best-effort. fakeKube.annotateStepErr
// fails every pure step() write (a call carrying only bifrost/step and
// bifrost/step-since — see isStepOnlyAnnotation) while leaving the final
// ready write (which also carries bifrost/phase) untouched, so this proves
// specifically that step-annotation failures mid-run are swallowed rather
// than propagated: Up must still reach ready.
func TestUpStepAnnotationFailureDoesNotFailTheRun(t *testing.T) {
	d := newTwoMemberDeps(t)
	kc := d.kube
	kc.annotateStepErr = errors.New("annotate: connection reset")

	err := d.orch.Up(context.Background(), "hae-cadence", UpOptions{})
	if err != nil {
		t.Fatalf("Up failed despite step-annotation errors being best-effort: %v", err)
	}
	ns := d.kube.namespaces["preview-hae-cadence"]
	if ns.annotations["bifrost/phase"] != "ready" {
		t.Errorf("bifrost/phase = %q, want ready even though every step annotation failed", ns.annotations["bifrost/phase"])
	}
	// Every step() write failed, so none of them should have landed at all.
	if ns.annotations["bifrost/step"] != "" {
		t.Errorf("bifrost/step = %q, want empty — every step write failed and only the final ready write (unaffected by annotateStepErr) should have landed", ns.annotations["bifrost/step"])
	}
}
