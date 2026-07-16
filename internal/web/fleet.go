package web

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eswan18/bifrost/internal/gcb"
	"github.com/eswan18/bifrost/internal/kube"
	"github.com/eswan18/bifrost/internal/promote"
)

// fleet is the fully-derived view of every service, assembled once per request
// and shared by the overview, apps, and jobs pages (they all need the tab
// counts, and the modals need per-app promote/rollback data on any page).
type fleet struct {
	Apps     []appView
	Jobs     []jobView // every job, flattened across apps and environments
	Overview overviewData

	// Tab-header counts.
	AppCount  int // number of services
	AppIssues int // crashed + drifted, the red badge on the Apps tab
	JobCount  int // total jobs
	JobIssues int // failed jobs, the red badge on the Jobs tab
}

// envView is one environment (staging or prod) of one service.
type envView struct {
	Env       string // "staging" | "prod"
	Tag       string // short image tag/hash; "" → render "—"
	SHA       string // commit SHA extracted from the tag; "" → no commit link
	CommitURL string
	AppURL    string // public app URL; "" → no "open" affordance
	Image     string // full current image ref

	Status     string // "ok" | "deploying" | "crash" | "unknown"
	Label      string // "healthy" | "deploying 2/3" | "CrashLoopBackOff ×7" | "unknown"
	LabelClass string
	Bold       bool
	HashClass  string // "c-red" when this env crashes, else ""
	Restarts   int32
	Progress   string // "N/M" while deploying
	Detail     string // health detail, for a tooltip

	PrevImage string // rollback target image; "" → none known
	PrevSHA   string
}

// buildView is a service's most recent CI build.
type buildView struct {
	State string // "ok" | "failed" | "building" | "none"
	Label string // "✓ today 09:58" | "✗ today 09:14" | "◌ running 2m"
	Class string
	Bold  bool
	URL   string
	SHA   string
}

// jobView is one CronJob and the state of its most recent run.
type jobView struct {
	App       string
	Env       string // "staging" | "prod"
	EnvLabel  string // "stg" | "prod" micro-label
	Name      string
	Image     string
	SHA       string
	CommitURL string

	State      string // "failed" | "running" | "ok" | "neutral"
	StateLabel string // "✗ Failed" | "● Running 4m" | "✓ Succeeded" | "—"
	StateClass string
	Suspended  bool

	LastRunTime  time.Time
	LastRunLabel string // day-relative, e.g. "today 03:12"
	Detail       string // "1m 12s" | "exit 137" | ""
	LastLabel    string // composed "today 03:12 · exit 137"
	RunningFor   string

	Next     string // day-relative next run, "suspended", or "—"
	NextTime time.Time
}

type appView struct {
	Name    string
	RepoURL string
	Staging envView
	Prod    envView

	Overall      string // "crash" | "deploying" | "drift" | "sync" | "unknown"
	PromoteState promote.State
	Badge        string // CRASHLOOP | DRIFT | DEPLOYING | IN SYNC | UNKNOWN
	BadgeClass   string

	Build buildView

	Jobs      []jobView
	HasJobs   bool
	JobsLabel string // "3 jobs"
	JobsClass string
	JobsBold  bool

	// Action affordances (design: drift→Promote, crash→Roll back,
	// deploying→locked, sync→in sync + ghost ↺).
	NewProdTag          string
	ShowPromote         bool
	ShowRollbackPrimary bool
	ShowRollbackGhost   bool
	SyncText            string

	// Modal copy.
	PromoteFrom string
	PromoteTo   string
	PromoteNote string
}

// Active reports whether the app is in flight — rolling out or building — so
// the client polls it on the fast cadence until it settles.
func (a appView) Active() bool {
	return a.Overall == "deploying" || a.Build.State == "building"
}

// anyActive reports whether anything in the fleet warrants the fast poll
// cadence: an app deploying or building, or a job running right now.
func (f *fleet) anyActive() bool {
	for _, a := range f.Apps {
		if a.Active() {
			return true
		}
	}
	for _, j := range f.Jobs {
		if j.State == "running" {
			return true
		}
	}
	return false
}

type attentionItem struct {
	Class  string // "c-red" | "c-amb"
	Title  string
	Sub    string
	Action string // "Roll back" | "View" | "Promote"
	Href   string // "#modal-…" for a modal, "/jobs?app=…" for View
}

type overviewJob struct {
	Name     string
	EnvLabel string
	Meta     string
}

type overviewData struct {
	Attention []attentionItem
	AttnHead  string
	AttnCount int

	Sync, Drift, Deploying, Crash int

	JobsTotal, JobsFailed, JobsRunning int

	Running  []overviewJob
	Failed   []overviewJob
	NextRuns []overviewJob
}

// envRaw is the raw cluster reads for one namespace. Each field is written by
// its own goroutine; the struct is only read after that goroutine's WaitGroup
// has drained.
type envRaw struct {
	pods     []kube.PodInfo
	rsets    []kube.ReplicaSetInfo
	cronjobs []kube.CronJobInfo
	jobs     []kube.JobInfo
}

// assembleFleet fans out staging+prod × (pods, replicasets, cronjobs, jobs)
// for every service, plus one bulk ArgoCD list and one Cloud Build list, then
// derives the whole UI model. Per-namespace errors degrade that service/env to
// "unknown" rather than failing the page.
func (h *Handlers) assembleFleet(ctx context.Context) *fleet {
	now := time.Now()

	// Two bulk calls shared by every service; run them concurrently and wait,
	// since per-env deploy detection consults ArgoCD health.
	var argo map[string]kube.AppStatus
	var builds map[string]gcb.BuildStatus
	var bulkWG sync.WaitGroup
	bulkWG.Add(2)
	go func() {
		defer bulkWG.Done()
		apps, err := h.Kube.ListArgoApps(ctx)
		if err != nil {
			slog.Warn("list argocd applications failed", "error", err)
		}
		argo = apps
	}()
	go func() {
		defer bulkWG.Done()
		if h.Builds == nil {
			return
		}
		b, err := h.Builds.LatestBuilds(ctx)
		if err != nil {
			slog.Warn("list cloud builds failed", "error", err)
		}
		builds = b
	}()
	bulkWG.Wait()

	type result struct {
		idx int
		app appView
	}
	results := make(chan result, len(h.Cfg.Services))
	var wg sync.WaitGroup
	for i, svc := range h.Cfg.Services {
		wg.Add(1)
		go func(i int, svc string) {
			defer wg.Done()
			results <- result{i, h.assembleApp(ctx, svc, now, argo, builds)}
		}(i, svc)
	}
	go func() { wg.Wait(); close(results) }()

	apps := make([]appView, len(h.Cfg.Services))
	for r := range results {
		apps[r.idx] = r.app
	}

	f := &fleet{Apps: apps, AppCount: len(apps)}
	for _, a := range apps {
		f.Jobs = append(f.Jobs, a.Jobs...)
	}
	f.deriveOverview(now)
	return f
}

// assembleApp reads both environments of one service concurrently and derives
// its full view.
func (h *Handlers) assembleApp(ctx context.Context, svc string, now time.Time, argo map[string]kube.AppStatus, builds map[string]gcb.BuildStatus) appView {
	org := h.Cfg.GitHubOrg
	repo := h.Cfg.RepoFor(svc)
	loc := h.Cfg.DisplayLocation

	var sRaw, pRaw envRaw
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); h.readEnv(ctx, svc+"-staging", &sRaw) }()
	go func() { defer wg.Done(); h.readEnv(ctx, svc+"-prod", &pRaw) }()
	wg.Wait()

	staging := deriveEnv("staging", sRaw, argo[svc+"-staging"], org, repo, h.Cfg.StagingURLs[svc], loc)
	prod := deriveEnv("prod", pRaw, argo[svc+"-prod"], org, repo, h.Cfg.ProdURLs[svc], loc)

	ps := promote.StatusOf(kube.Images(sRaw.pods), kube.Images(pRaw.pods))
	overall := deriveOverall(staging, prod, ps)

	a := appView{
		Name:         svc,
		RepoURL:      repoURL(org, repo),
		Staging:      staging,
		Prod:         prod,
		Overall:      overall,
		PromoteState: ps.State,
		NewProdTag:   ps.NewProdTag,
	}
	a.Badge, a.BadgeClass = badgeFor(overall)

	// Actions.
	switch overall {
	case "drift":
		a.ShowPromote = true
	case "crash":
		a.ShowRollbackPrimary = true
	case "deploying":
		a.SyncText = "locked"
	case "sync":
		a.SyncText = "in sync"
		a.ShowRollbackGhost = true
	}

	// Promote modal copy (aligned with the promote endpoint's tags).
	a.PromoteFrom = ps.ProdTag
	a.PromoteTo = ps.NewProdTag
	a.PromoteNote = "prod is " + prod.Label + " · staging is " + staging.Label

	// Build. When no build is tracked (no LogURL), fall back to the pipeline
	// history so the BUILD cell still links somewhere useful.
	a.Build = buildViewFor(builds[repo], now, loc)
	if a.Build.URL == "" {
		a.Build.URL = buildPipelineURL(h.Cfg.GCPProject, h.TriggerIDs[svc])
	}

	// Jobs across both environments.
	a.Jobs = append(a.Jobs, buildJobs(svc, "staging", sRaw, org, repo, now, loc)...)
	a.Jobs = append(a.Jobs, buildJobs(svc, "prod", pRaw, org, repo, now, loc)...)
	sortJobs(a.Jobs)
	a.HasJobs = len(a.Jobs) > 0
	a.JobsLabel, a.JobsClass, a.JobsBold = jobsSummary(a.Jobs)

	return a
}

// readEnv issues the four per-namespace list calls concurrently. Each writes a
// distinct field of raw, so there's no shared-memory race; errors are logged
// and leave that field nil (the derivation degrades to unknown).
func (h *Handlers) readEnv(ctx context.Context, ns string, raw *envRaw) {
	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		pods, err := h.Kube.ListPods(ctx, ns)
		if err != nil {
			slog.Warn("list pods failed", "namespace", ns, "error", err)
		}
		raw.pods = pods
	}()
	go func() {
		defer wg.Done()
		rs, err := h.Kube.ListReplicaSets(ctx, ns)
		if err != nil {
			slog.Warn("list replicasets failed", "namespace", ns, "error", err)
		}
		raw.rsets = rs
	}()
	go func() {
		defer wg.Done()
		cj, err := h.Kube.ListCronJobs(ctx, ns)
		if err != nil {
			slog.Warn("list cronjobs failed", "namespace", ns, "error", err)
		}
		raw.cronjobs = cj
	}()
	go func() {
		defer wg.Done()
		j, err := h.Kube.ListJobs(ctx, ns)
		if err != nil {
			slog.Warn("list jobs failed", "namespace", ns, "error", err)
		}
		raw.jobs = j
	}()
	wg.Wait()
}

func deriveEnv(env string, raw envRaw, argo kube.AppStatus, org, repo, appURL string, loc *time.Location) envView {
	image := currentImage(raw.pods, raw.rsets)
	ev := envView{Env: env, Image: image, AppURL: appURL}
	if image != "" {
		ev.Tag = promote.ExtractTag(image)
		ev.SHA = promote.ExtractSHA(ev.Tag)
		ev.CommitURL = commitURL(org, repo, ev.Tag)
	}

	// Health reflects the deployment, so exclude Job-owned pods — a lingering
	// failed/OOM job pod (Phase Failed) would otherwise mark the whole env
	// degraded. kube.Images already filters them the same way.
	deployPods := longRunningPods(raw.pods)
	health := kube.SummarizeHealth(deployPods)
	ev.Detail = health.Detail
	switch {
	case len(kube.Images(raw.pods)) == 0:
		ev.Status = "unknown"
	case health.State == kube.Crashlooping:
		ev.Status = "crash"
		ev.Restarts = crashRestarts(deployPods)
	case health.State == kube.Degraded || deployingEnv(raw, argo):
		// The design's env vocabulary is crash / deploying / healthy; a degraded
		// env (partial readiness, ImagePullBackOff) has no state of its own, so
		// it reads as "deploying N/M" — the closest not-yet-healthy signal.
		ev.Status = "deploying"
		if rs, ok := kube.NewestReplicaSet(raw.rsets, nil); ok {
			ev.Progress = fmt.Sprintf("%d/%d", readyPodsOnImage(raw.pods, rs.Image), rs.Replicas)
		}
	default:
		ev.Status = "ok"
	}

	ev.Label = envLabel(ev)
	ev.LabelClass = statusClass(ev.Status)
	ev.Bold = ev.Status == "crash" || ev.Status == "deploying"
	if ev.Status == "crash" {
		ev.HashClass = "c-red"
	}

	// ArgoCD visibility (label-only): an env that reads healthy from pod state
	// alone still hides an Application ArgoCD reports as Degraded/Missing or
	// OutOfSync. Surface argo's own view as an amber label without touching the
	// env's status — overall precedence, promote/rollback gating, and fleet
	// counts all key off ev.Status, which is deliberately left unchanged.
	if ev.Status == "ok" {
		if label, detail := argoAttention(argo); label != "" {
			ev.Label = label
			ev.LabelClass = "c-amb"
			ev.Bold = true
			if ev.Detail != "" {
				ev.Detail += " · " + detail
			} else {
				ev.Detail = detail
			}
		}
	}

	if ev.PrevImage = kube.PreviousImage(raw.rsets, image); ev.PrevImage != "" {
		ev.PrevSHA = promote.ExtractSHA(promote.ExtractTag(ev.PrevImage))
	}
	return ev
}

func envLabel(ev envView) string {
	switch ev.Status {
	case "crash":
		return fmt.Sprintf("CrashLoopBackOff ×%d", ev.Restarts)
	case "deploying":
		if ev.Progress != "" {
			return "deploying " + ev.Progress
		}
		return "deploying"
	case "unknown":
		return "unknown"
	default:
		return "healthy"
	}
}

// deployingEnv reports whether an environment is mid-rollout: old and new
// images coexisting among its long-running pods, or ArgoCD reporting the
// Application as Progressing.
func deployingEnv(raw envRaw, argo kube.AppStatus) bool {
	if len(kube.Images(raw.pods)) > 1 {
		return true
	}
	return argo.HealthStatus == "Progressing"
}

// argoAttention surfaces an ArgoCD problem that an otherwise-healthy env would
// hide, returning an amber status label and a tooltip fragment (both "" when
// argo looks fine). A Degraded/Missing health takes precedence over an
// OutOfSync sync, matching how a human would triage the Application.
func argoAttention(argo kube.AppStatus) (label, detail string) {
	switch argo.HealthStatus {
	case "Degraded":
		return "argo degraded", "ArgoCD health: Degraded"
	case "Missing":
		return "argo missing", "ArgoCD health: Missing"
	}
	if argo.SyncStatus == "OutOfSync" {
		return "argo out of sync", "ArgoCD sync: OutOfSync"
	}
	return "", ""
}

func deriveOverall(s, p envView, ps promote.Status) string {
	switch {
	case s.Status == "crash" || p.Status == "crash":
		return "crash"
	case s.Status == "deploying" || p.Status == "deploying" || ps.State == promote.MidDeploy:
		return "deploying"
	case ps.State == promote.OutOfSync:
		return "drift"
	case s.Status == "unknown" || p.Status == "unknown" || ps.State == promote.Unknown:
		return "unknown"
	default:
		return "sync"
	}
}

func badgeFor(overall string) (label, class string) {
	switch overall {
	case "crash":
		return "CRASHLOOP", "c-red"
	case "drift":
		return "DRIFT", "c-amb"
	case "deploying":
		return "DEPLOYING", "c-blu"
	case "sync":
		return "IN SYNC", "c-grn"
	default:
		return "UNKNOWN", "c-faint"
	}
}

func statusClass(status string) string {
	switch status {
	case "crash":
		return "c-red"
	case "deploying":
		return "c-blu"
	case "unknown":
		return "c-faint"
	default:
		return "c-mut"
	}
}

func buildViewFor(b gcb.BuildStatus, now time.Time, loc *time.Location) buildView {
	bv := buildView{State: "none", SHA: b.SHA, URL: b.LogURL}
	switch {
	case b.Status == "":
		return bv
	case b.InProgress():
		bv.State = "building"
		bv.Class = "c-blu"
		bv.Bold = true
		if b.StartTime.IsZero() {
			bv.Label = "◌ queued"
		} else {
			bv.Label = "◌ running " + runningFor(now.Sub(b.StartTime))
		}
	case b.Failed():
		bv.State = "failed"
		bv.Class = "c-red"
		bv.Bold = true
		bv.Label = "✗ " + dayRelative(b.FinishTime, now, loc)
	case b.Status == "SUCCESS":
		bv.State = "ok"
		bv.Class = "c-mut"
		bv.Label = "✓ " + dayRelative(b.FinishTime, now, loc)
	}
	return bv
}

// buildJobs joins each CronJob in an environment with its retained Jobs and
// derives the latest run's state, last-run label, exit detail, and next run.
func buildJobs(app, env string, raw envRaw, org, repo string, now time.Time, loc *time.Location) []jobView {
	out := make([]jobView, 0, len(raw.cronjobs))
	for _, cj := range raw.cronjobs {
		jv := jobView{
			App:       app,
			Env:       env,
			EnvLabel:  envMicro(env),
			Name:      cj.Name,
			Suspended: cj.Suspended,
		}

		latest := latestJobFor(cj.Name, raw.jobs)

		image := cj.Image
		if latest != nil && latest.Image != "" {
			image = latest.Image
		}
		jv.Image = image
		if image != "" {
			tag := promote.ExtractTag(image)
			jv.SHA = promote.ExtractSHA(tag)
			jv.CommitURL = commitURL(org, repo, tag)
		}

		switch {
		case latest == nil:
			jv.State = "neutral"
			jv.StateLabel = "—"
			jv.StateClass = "c-faint"
			if !cj.LastScheduleTime.IsZero() {
				jv.LastRunTime = cj.LastScheduleTime
				jv.LastRunLabel = dayRelative(cj.LastScheduleTime, now, loc)
			} else {
				jv.LastRunLabel = "never"
			}
		case latest.Failed:
			jv.State = "failed"
			jv.StateLabel = "✗ Failed"
			jv.StateClass = "c-red"
			jv.Detail = exitDetail(latest, raw.pods)
			jv.LastRunTime = jobTime(latest)
			jv.LastRunLabel = dayRelative(jv.LastRunTime, now, loc)
		case latest.Active:
			jv.State = "running"
			jv.RunningFor = runningFor(now.Sub(latest.StartTime))
			jv.StateLabel = "● Running " + jv.RunningFor
			jv.StateClass = "c-blu"
			jv.LastRunTime = jobTime(latest)
			jv.LastRunLabel = dayRelative(jv.LastRunTime, now, loc)
		case latest.Succeeded:
			jv.State = "ok"
			jv.StateLabel = "✓ Succeeded"
			jv.StateClass = "c-mut"
			if !latest.CompletionTime.IsZero() && !latest.StartTime.IsZero() {
				jv.Detail = humanDuration(latest.CompletionTime.Sub(latest.StartTime))
			}
			jv.LastRunTime = jobTime(latest)
			jv.LastRunLabel = dayRelative(jv.LastRunTime, now, loc)
		default:
			jv.State = "neutral"
			jv.StateLabel = "—"
			jv.StateClass = "c-faint"
			jv.LastRunTime = jobTime(latest)
			// A retained Job with no start/completion time has no run to date;
			// dayRelative renders the zero time as "", so fall back to an em dash.
			if jv.LastRunTime.IsZero() {
				jv.LastRunLabel = "—"
			} else {
				jv.LastRunLabel = dayRelative(jv.LastRunTime, now, loc)
			}
		}

		jv.LastLabel = jv.LastRunLabel
		if jv.Detail != "" {
			jv.LastLabel += " · " + jv.Detail
		}

		switch {
		case cj.Suspended:
			jv.Next = "suspended"
		default:
			if next, err := nextRun(cj.Schedule, cj.TimeZone, now); err == nil {
				jv.NextTime = next
				jv.Next = dayRelative(next, now, loc)
			} else {
				jv.Next = "—"
			}
		}

		out = append(out, jv)
	}
	return out
}

// nextRun is a seam over kube.NextRun so tests can supply deterministic next
// run times without depending on wall-clock cron math.
var nextRun = kube.NextRun

func envMicro(env string) string {
	if env == "staging" {
		return "stg"
	}
	return "prod"
}

func latestJobFor(cron string, jobs []kube.JobInfo) *kube.JobInfo {
	var latest *kube.JobInfo
	for i := range jobs {
		j := &jobs[i]
		if j.OwnerCron != cron {
			continue
		}
		if latest == nil || jobTime(j).After(jobTime(latest)) {
			latest = j
		}
	}
	return latest
}

func jobTime(j *kube.JobInfo) time.Time {
	if !j.StartTime.IsZero() {
		return j.StartTime
	}
	return j.CompletionTime
}

// exitDetail surfaces a failed job's exit code from its pods (a non-zero
// container exit), falling back to the Job's Failed-condition reason.
func exitDetail(job *kube.JobInfo, pods []kube.PodInfo) string {
	for _, p := range pods {
		if p.OwnerName != job.Name {
			continue
		}
		for _, c := range p.Containers {
			if c.ExitCode != nil && *c.ExitCode != 0 {
				detail := fmt.Sprintf("exit %d", *c.ExitCode)
				// TerminatedReason names why the kernel/kubelet killed it (e.g.
				// "OOMKilled"); "Error" is just the generic non-zero-exit reason
				// the code already conveys, so only append something informative.
				if c.TerminatedReason != "" && c.TerminatedReason != "Error" {
					detail += fmt.Sprintf(" (%s)", c.TerminatedReason)
				}
				return detail
			}
		}
	}
	return job.FailReason
}

func sortJobs(jobs []jobView) {
	sort.SliceStable(jobs, func(i, j int) bool {
		ri, rj := stateRank(jobs[i].State), stateRank(jobs[j].State)
		if ri != rj {
			return ri < rj
		}
		if jobs[i].App != jobs[j].App {
			return jobs[i].App < jobs[j].App
		}
		return jobs[i].Name < jobs[j].Name
	})
}

func stateRank(s string) int {
	switch s {
	case "failed":
		return 0
	case "running":
		return 1
	case "ok":
		return 2
	default:
		return 3
	}
}

func jobsSummary(jobs []jobView) (label, class string, bold bool) {
	if len(jobs) == 1 {
		label = "1 job"
	} else {
		label = fmt.Sprintf("%d jobs", len(jobs))
	}
	// The summary takes the color of the most severe job state present.
	// stateRank stays the single source of truth for that failed>running>ok
	// precedence: the lowest rank wins.
	best := 3
	for _, j := range jobs {
		if r := stateRank(j.State); r < best {
			best = r
		}
	}
	switch best {
	case 0: // failed
		return label, "c-red", true
	case 1: // running
		return label, "c-blu", true
	default:
		return label, "c-mut", false
	}
}

// --- pod / replicaset helpers -------------------------------------------------

// currentImage is the image an environment is converging on: the newest
// ReplicaSet's image (the rollout target), falling back to any long-running
// pod image when ReplicaSet history is unavailable.
func currentImage(pods []kube.PodInfo, sets []kube.ReplicaSetInfo) string {
	if rs, ok := kube.NewestReplicaSet(sets, nil); ok && rs.Image != "" {
		return rs.Image
	}
	if imgs := kube.Images(pods); len(imgs) > 0 {
		return imgs[0]
	}
	return ""
}

// longRunningPods drops Job-owned pods, leaving only the pods that reflect
// what's deployed (the same exclusion kube.Images makes).
func longRunningPods(pods []kube.PodInfo) []kube.PodInfo {
	out := make([]kube.PodInfo, 0, len(pods))
	for _, p := range pods {
		if p.OwnerKind == "Job" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// readyPodsOnImage counts long-running pods fully ready and running the given
// image — the numerator of the "N/M" deploy progress.
func readyPodsOnImage(pods []kube.PodInfo, image string) int {
	n := 0
	for _, p := range pods {
		if p.OwnerKind == "Job" || len(p.Containers) == 0 {
			continue
		}
		ready, onImage := true, false
		for _, c := range p.Containers {
			if c.Image == image {
				onImage = true
			}
			if !c.Ready {
				ready = false
			}
		}
		if onImage && ready {
			n++
		}
	}
	return n
}

func crashRestarts(pods []kube.PodInfo) int32 {
	var max int32
	for _, p := range pods {
		for _, c := range p.Containers {
			if c.WaitingReason == "CrashLoopBackOff" && c.RestartCount > max {
				max = c.RestartCount
			}
		}
	}
	return max
}

// --- overview derivation ------------------------------------------------------

func (f *fleet) deriveOverview(now time.Time) {
	o := &f.Overview
	for _, a := range f.Apps {
		switch a.Overall {
		case "sync":
			o.Sync++
		case "drift":
			o.Drift++
		case "deploying":
			o.Deploying++
		case "crash":
			o.Crash++
		}

		// Attention: crash, then failed jobs, then drift (design ordering).
		if a.Overall == "crash" {
			env, ev := "prod", a.Prod
			if a.Prod.Status != "crash" {
				env, ev = "staging", a.Staging
			}
			o.Attention = append(o.Attention, attentionItem{
				Class:  "c-red",
				Title:  a.Name + " is crashlooping in " + env,
				Sub:    fmt.Sprintf("— %d restarts · %s", ev.Restarts, ev.Tag),
				Action: "Roll back",
				Href:   "#modal-rollback-" + a.Name,
			})
		}
		for _, j := range a.Jobs {
			if j.State != "failed" {
				continue
			}
			o.Attention = append(o.Attention, attentionItem{
				Class:  "c-red",
				Title:  j.Name + " failed at " + stripToday(j.LastRunLabel),
				Sub:    "— " + a.Name + " " + j.EnvLabel + " · " + orDash(j.Detail),
				Action: "View",
				Href:   "/jobs?app=" + a.Name,
			})
		}
		if a.Overall == "drift" {
			o.Attention = append(o.Attention, attentionItem{
				Class:  "c-amb",
				Title:  a.Name + " is ready to promote",
				Sub:    "— staging " + a.Staging.Tag + " ahead of prod " + a.Prod.Tag,
				Action: "Promote",
				Href:   "#modal-promote-" + a.Name,
			})
		}
	}

	o.AttnCount = len(o.Attention)
	if o.AttnCount > 0 {
		o.AttnHead = fmt.Sprintf("ATTENTION · %d", o.AttnCount)
	} else {
		o.AttnHead = "ATTENTION"
	}

	f.JobCount = len(f.Jobs)
	for _, j := range f.Jobs {
		switch j.State {
		case "failed":
			f.JobIssues++
		case "running":
			o.JobsRunning++
		}
	}
	o.JobsTotal = f.JobCount
	o.JobsFailed = f.JobIssues
	f.AppIssues = o.Crash + o.Drift

	for _, j := range f.Jobs {
		switch j.State {
		case "running":
			o.Running = append(o.Running, overviewJob{
				Name:     j.Name,
				EnvLabel: j.EnvLabel,
				Meta:     j.App + " " + j.EnvLabel + " · running for " + j.RunningFor,
			})
		case "failed":
			// The overview's RECENT FAILURES column is scoped to the last 24h (its
			// empty state says so); fleet counts and attention items stay unbounded.
			if j.LastRunTime.IsZero() || now.Sub(j.LastRunTime) > 24*time.Hour {
				continue
			}
			o.Failed = append(o.Failed, overviewJob{
				Name:     j.Name,
				EnvLabel: j.EnvLabel,
				Meta:     j.App + " " + j.EnvLabel + " · " + j.LastRunLabel + " · " + orDash(j.Detail),
			})
		}
	}

	// Next runs: the soonest three real (non-suspended, scheduled) jobs.
	upcoming := make([]jobView, 0, len(f.Jobs))
	for _, j := range f.Jobs {
		if !j.NextTime.IsZero() {
			upcoming = append(upcoming, j)
		}
	}
	sort.SliceStable(upcoming, func(i, j int) bool {
		return upcoming[i].NextTime.Before(upcoming[j].NextTime)
	})
	for i, j := range upcoming {
		if i == 3 {
			break
		}
		o.NextRuns = append(o.NextRuns, overviewJob{
			Name:     j.Name,
			EnvLabel: j.EnvLabel,
			Meta:     j.App + " · " + stripToday(j.Next),
		})
	}
}

func stripToday(label string) string {
	return strings.TrimPrefix(label, "today ")
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
