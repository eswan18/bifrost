package preview

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eswan18/bifrost/internal/config"
	"github.com/eswan18/bifrost/internal/gcb"
	"github.com/eswan18/bifrost/internal/github"
	"github.com/eswan18/bifrost/internal/kube"
	"github.com/eswan18/bifrost/internal/neon"
)

// ErrBusy reports that an Up or Down for this tag is already in flight. The
// API layer maps this to 409.
var ErrBusy = errors.New("preview: orchestration already in progress for this tag")

// buildPollInterval is how often Up polls Cloud Build for a preview build's
// status. A var (not a const) so tests can shrink it instead of waiting on
// the real interval.
var buildPollInterval = 10 * time.Second

// Orchestrator composes the preview control plane's clients into the full
// creation (Up) and teardown (Down) flows. It has no constructor: every
// field is a plain dependency the caller wires up (main.go), so a zero-value
// Orchestrator{Cfg: ..., Kube: ..., ...} literal is exactly how it's built —
// the busy-mutex map lazily initializes itself on first use.
type Orchestrator struct {
	Cfg        *config.Config
	Kube       kube.Client
	GitHub     github.Client
	Neon       neon.Client
	Builds     gcb.Client
	TriggerIDs map[string]string // {svc}-preview-build → Cloud Build trigger ID
	// Registry is every previewable service's Neon reference and env
	// wiring (see registry.yaml); Up and renderAndApply both thread it
	// into envConfigFor. Normally populated once at startup via
	// LoadRegistry() (main.go) rather than reloaded per-request.
	Registry Registry

	mu   sync.Mutex
	busy map[string]bool
}

// acquire claims tag for the caller, returning false if it's already
// claimed. Up and Down share the same claim space: a tag mid-creation can't
// be torn down (or re-created) concurrently.
func (o *Orchestrator) acquire(tag string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.busy == nil {
		o.busy = map[string]bool{}
	}
	if o.busy[tag] {
		return false
	}
	o.busy[tag] = true
	return true
}

func (o *Orchestrator) release(tag string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.busy, tag)
}

// Busy reports whether an Up or Down for tag is currently in flight.
func (o *Orchestrator) Busy(tag string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.busy[tag]
}

// Up runs the full preview creation flow for branch to completion: resolve
// membership, stand up (or update) the namespace, build+branch+secret+render
// each member, and mark the namespace ready. Every stage past EnsureNamespace
// that fails marks the namespace bifrost/phase=failed with a sanitized
// bifrost/error annotation before returning; stage-1 validation failures
// (bad membership, an unresolvable dashboard triple) return before the
// namespace is ever touched, so they never leave a zombie behind.
//
// Idempotent re-`Up` (e.g. after a bifrost restart mid-creation, or a
// deliberate re-POST once the branch has new commits) is the recovery path:
// EnsureNamespace, CopySecret, and ApplyObjects all merge/replace rather than
// erroring on "already exists", and Neon branch creation is scan-then-create.
func (o *Orchestrator) Up(ctx context.Context, branch string) error {
	if strings.TrimSpace(branch) == "" {
		return errors.New("preview: Up: branch is required")
	}
	tag := TagForBranch(branch)
	if tag == "" {
		return fmt.Errorf("preview: Up: branch %q slugs to an empty tag", branch)
	}
	if !o.acquire(tag) {
		return ErrBusy
	}
	defer o.release(tag)

	members, err := o.resolveMembers(ctx, branch)
	if err != nil {
		return fmt.Errorf("preview: Up: %w", err)
	}
	if len(members) == 0 {
		return fmt.Errorf("preview: Up: branch %q matches no preview-eligible service", branch)
	}
	// Mandatory required-key pre-flight: any member whose registry entry
	// declares Required env keys (today, only footstrike-dashboard's
	// mandatory APP_API_URL/APP_IDENTITY_URL/APP_OAUTH_CLIENT_ID triple) must
	// have every one of them resolve to a non-empty value before we touch
	// the cluster. envConfigFor is the same function stage 6 uses to build
	// that service's actual ConfigMap data, so this pre-check and the real
	// computation can never disagree. stagingData is passed empty here
	// because it hasn't been fetched yet at this point in Up -- true for
	// every Required-key service today anyway, since footstrike-dashboard
	// (the only one) ships no staging/configmap-env.yaml of its own.
	for _, svc := range members {
		if len(o.Registry[svc].Required) == 0 {
			continue
		}
		if _, err := envConfigFor(svc, tag, members, map[string]string{}, o.Cfg, o.Registry); err != nil {
			return fmt.Errorf("preview: Up: env config for %s: %w", svc, err)
		}
	}

	ns := previewNamespace(tag)
	if err := o.Kube.EnsureNamespace(ctx, ns,
		map[string]string{"bifrost/preview": "true"},
		map[string]string{
			"bifrost/branch": branch,
			"bifrost/apps":   strings.Join(members, ","),
			"bifrost/phase":  "creating",
		},
	); err != nil {
		return fmt.Errorf("preview: Up: ensure namespace: %w", err)
	}

	shortSHAs, err := o.buildMembers(ctx, branch, members)
	if err != nil {
		return o.fail(ctx, ns, err)
	}
	dbURIs, err := o.branchNeonDatabases(ctx, tag, members)
	if err != nil {
		return o.fail(ctx, ns, err)
	}
	if err := o.copySecrets(ctx, ns, members, dbURIs); err != nil {
		return o.fail(ctx, ns, err)
	}
	if err := o.renderAndApply(ctx, ns, tag, branch, members, shortSHAs); err != nil {
		return o.fail(ctx, ns, err)
	}

	if err := o.Kube.AnnotateNamespace(ctx, ns, map[string]string{
		"bifrost/phase": "ready",
		"bifrost/error": "",
	}); err != nil {
		return fmt.Errorf("preview: Up: mark ready: %w", err)
	}
	return nil
}

// failAnnotateTimeout bounds fail's detached compensating write — long
// enough for a real AnnotateNamespace call, short enough not to hang a
// failure path indefinitely if the API server is unreachable.
const failAnnotateTimeout = 10 * time.Second

// fail marks ns bifrost/phase=failed with a sanitized bifrost/error
// annotation (cause's message only — never a secret value; callers are
// responsible for never constructing cause with one embedded, see the Neon
// helpers below) and returns cause. If the annotate call itself fails, that
// failure is joined in rather than swallowed, so an operator sees both.
//
// The annotate call deliberately runs on a context detached from ctx's
// cancellation (context.WithoutCancel), with its own short timeout: cause
// itself is very often ctx dying (a build-poll wait hitting the caller's
// deadline, e.g. the API layer's 30-minute goroutine budget), and every
// other stage's failure races the same deadline. Annotating on the
// already-dead ctx would make this compensating write fail right along
// with it, leaving the namespace stuck at bifrost/phase=creating with no
// bifrost/error — silently violating the "every post-namespace failure
// lands on failed" contract this whole function exists to uphold.
func (o *Orchestrator) fail(ctx context.Context, ns string, cause error) error {
	annotateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failAnnotateTimeout)
	defer cancel()
	if annErr := o.Kube.AnnotateNamespace(annotateCtx, ns, map[string]string{
		"bifrost/phase": "failed",
		"bifrost/error": cause.Error(),
	}); annErr != nil {
		return fmt.Errorf("%w (additionally failed to annotate failure: %v)", cause, annErr)
	}
	return cause
}

// resolveMembers determines which of Cfg.PreviewServices have branch pushed
// to their repo: ErrNoBranch means "not a member", any other error aborts
// the whole run (we can't tell membership, so we can't safely proceed).
func (o *Orchestrator) resolveMembers(ctx context.Context, branch string) ([]string, error) {
	var members []string
	for _, svc := range o.Cfg.PreviewServices {
		_, err := o.GitHub.BranchSHA(ctx, o.Cfg.RepoFor(svc), branch)
		switch {
		case errors.Is(err, github.ErrNoBranch):
			continue
		case err != nil:
			return nil, fmt.Errorf("checking branch for %s: %w", svc, err)
		default:
			members = append(members, svc)
		}
	}
	return members, nil
}

// buildMembers runs (and awaits) each member's preview build trigger,
// returning the resulting short SHA per service.
func (o *Orchestrator) buildMembers(ctx context.Context, branch string, members []string) (map[string]string, error) {
	shortSHAs := make(map[string]string, len(members))
	for _, svc := range members {
		triggerID := o.TriggerIDs[svc+"-preview-build"]
		if triggerID == "" {
			return nil, fmt.Errorf("no preview build trigger configured for %s", svc)
		}
		buildID, err := o.Builds.RunTrigger(ctx, triggerID, branch)
		if err != nil {
			return nil, fmt.Errorf("starting build for %s: %w", svc, err)
		}
		sha, err := o.awaitBuild(ctx, buildID)
		if err != nil {
			return nil, fmt.Errorf("build for %s: %w", svc, err)
		}
		shortSHAs[svc] = sha
	}
	return shortSHAs, nil
}

// awaitBuild polls buildID every buildPollInterval until it reaches a
// terminal status, returning its short SHA on success. It respects ctx
// cancellation while waiting between polls.
func (o *Orchestrator) awaitBuild(ctx context.Context, buildID string) (string, error) {
	for {
		status, err := o.Builds.GetBuild(ctx, buildID)
		if err != nil {
			return "", fmt.Errorf("checking build status: %w", err)
		}
		if status.InProgress() {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(buildPollInterval):
			}
			continue
		}
		// Cloud Build's terminal statuses are SUCCESS, and the failure-ish
		// set status.Failed() names (FAILURE/INTERNAL_ERROR/TIMEOUT/EXPIRED),
		// plus CANCELLED (deliberate, not a Failed()). Anything other than a
		// clean SUCCESS means the run doesn't get a usable image.
		if status.Status != "SUCCESS" {
			return "", fmt.Errorf("build ended with status %s", status.Status)
		}
		return status.SHA, nil
	}
}

// branchNeonDatabases ensures a preview-<tag> Neon branch exists for every
// member with a configured NeonProjectRef, returning each one's connection
// URI. Errors are wrapped without ever including the URI itself (it's a
// secret) — only service/project identifiers appear in the returned error.
func (o *Orchestrator) branchNeonDatabases(ctx context.Context, tag string, members []string) (map[string]string, error) {
	dbURIs := make(map[string]string, len(members))
	for _, svc := range members {
		ref, ok := o.Cfg.NeonProjects[svc]
		if !ok {
			continue
		}
		uri, err := o.ensureNeonBranch(ctx, ref, tag)
		if err != nil {
			return nil, fmt.Errorf("neon branch for %s: %w", svc, err)
		}
		dbURIs[svc] = uri
	}
	return dbURIs, nil
}

// ensureNeonBranch finds (or creates) projectID's preview-<tag> branch and
// returns its connection URI for ref's database/role.
func (o *Orchestrator) ensureNeonBranch(ctx context.Context, ref config.NeonProjectRef, tag string) (string, error) {
	branchName := "preview-" + tag
	branches, err := o.Neon.ListBranches(ctx, ref.ProjectID)
	if err != nil {
		return "", fmt.Errorf("listing branches: %w", err)
	}
	var branchID string
	for _, b := range branches {
		if b.Name == branchName {
			branchID = b.ID
			break
		}
	}
	if branchID == "" {
		created, err := o.Neon.CreateBranch(ctx, ref.ProjectID, branchName, "")
		if err != nil {
			return "", fmt.Errorf("creating branch: %w", err)
		}
		branchID = created.ID
	}
	uri, err := o.Neon.ConnectionURI(ctx, ref.ProjectID, branchID, ref.Database, ref.Role)
	if err != nil {
		return "", fmt.Errorf("fetching connection uri: %w", err)
	}
	return uri, nil
}

// copySecrets copies each Neon-backed member's staging secret into the
// preview namespace (overriding DATABASE_URL with its preview branch's URI),
// plus the shared wildcard TLS cert every preview namespace needs.
func (o *Orchestrator) copySecrets(ctx context.Context, ns string, members []string, dbURIs map[string]string) error {
	for _, svc := range members {
		if _, ok := o.Cfg.NeonProjects[svc]; !ok {
			continue
		}
		overrides := map[string][]byte{"DATABASE_URL": []byte(dbURIs[svc])}
		if err := o.Kube.CopySecret(ctx, svc+"-staging", svc+"-staging-secrets", ns, previewSecretName(svc), overrides); err != nil {
			return fmt.Errorf("copy secret for %s: %w", svc, err)
		}
	}
	if err := o.Kube.CopySecret(ctx, "previews", previewTLSSecret, ns, previewTLSSecret, nil); err != nil {
		return fmt.Errorf("copy wildcard tls secret: %w", err)
	}
	return nil
}

// renderAndApply fetches each member's k8s/ tree, computes its env config,
// renders its manifests, and applies them into ns.
func (o *Orchestrator) renderAndApply(ctx context.Context, ns, tag, branch string, members []string, shortSHAs map[string]string) error {
	for _, svc := range members {
		k8sFiles, err := o.GitHub.FetchK8s(ctx, o.Cfg.RepoFor(svc), branch)
		if err != nil {
			return fmt.Errorf("fetch k8s files for %s: %w", svc, err)
		}
		stagingData, err := parseStagingEnv(k8sFiles)
		if err != nil {
			return fmt.Errorf("parse staging env for %s: %w", svc, err)
		}
		envConfig, err := envConfigFor(svc, tag, members, stagingData, o.Cfg, o.Registry)
		if err != nil {
			return fmt.Errorf("env config for %s: %w", svc, err)
		}
		secretName := ""
		if _, ok := o.Cfg.NeonProjects[svc]; ok {
			secretName = previewSecretName(svc)
		}
		objs, err := Render(RenderInput{
			Service:    svc,
			Tag:        tag,
			ShortSHA:   shortSHAs[svc],
			K8sFiles:   k8sFiles,
			EnvConfig:  envConfig,
			SecretName: secretName,
		})
		if err != nil {
			return fmt.Errorf("render %s: %w", svc, err)
		}
		if err := o.Kube.ApplyObjects(ctx, ns, objs); err != nil {
			return fmt.Errorf("apply %s: %w", svc, err)
		}
	}
	return nil
}

func previewSecretName(svc string) string { return svc + "-preview-secrets" }

// Down tears tag's preview down: delete its namespace, then best-effort
// delete a preview-<tag> Neon branch from every *configured* NeonProjectRef —
// not just the tag's current members, since a re-created preview may have
// changed its membership since the branch was created, and Down has no other
// way to know which projects to check. Every step is attempted regardless of
// earlier failures; all errors are joined and returned.
func (o *Orchestrator) Down(ctx context.Context, tag string) error {
	if !o.acquire(tag) {
		return ErrBusy
	}
	defer o.release(tag)

	var errs []error
	if err := o.Kube.DeleteNamespace(ctx, previewNamespace(tag)); err != nil {
		errs = append(errs, fmt.Errorf("delete namespace: %w", err))
	}

	svcs := make([]string, 0, len(o.Cfg.NeonProjects))
	for svc := range o.Cfg.NeonProjects {
		svcs = append(svcs, svc)
	}
	sort.Strings(svcs) // deterministic order for logs/tests; map order is not

	branchName := "preview-" + tag
	for _, svc := range svcs {
		ref := o.Cfg.NeonProjects[svc]
		branches, err := o.Neon.ListBranches(ctx, ref.ProjectID)
		if err != nil {
			errs = append(errs, fmt.Errorf("neon list branches for %s: %w", svc, err))
			continue
		}
		for _, b := range branches {
			if b.Name != branchName {
				continue
			}
			if err := o.Neon.DeleteBranch(ctx, ref.ProjectID, b.ID); err != nil {
				errs = append(errs, fmt.Errorf("neon delete branch for %s: %w", svc, err))
			}
			break
		}
	}
	return errors.Join(errs...)
}
