package preview

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/kustomize/api/konfig"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/resource"
	ktypes "sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/kustomize/kyaml/resid"
	sigsyaml "sigs.k8s.io/yaml"
)

// imageRepoBase is the Artifact Registry path every member service's image
// lives under; only the trailing service-name segment varies.
const imageRepoBase = "us-central1-docker.pkg.dev/ethans-services/containers"

// previewDomain is the wildcard host suffix every preview Ingress is issued
// under, and previewTLSSecret is the wildcard cert bifrost provisions once
// for the whole preview domain (see the 3c plan).
const (
	previewDomain    = "preview.footstrike.run"
	previewTLSSecret = "preview-footstrike-run-tls"
)

// RenderInput is one member service's render request: its fetched k8s/ base
// plus the environment-specific values the generated overlay needs to wire
// up.
type RenderInput struct {
	Service  string // e.g. "footstrike-api"; also the Deployment/container/Service name by convention
	Tag      string // preview slug, e.g. from preview.TagForBranch; drives the namespace and Ingress host
	ShortSHA string // preview image tag suffix

	// K8sFiles is the service's whole k8s/ subtree as returned by
	// github.FetchK8s (path relative to k8s/ -> content). Only the base/
	// subtree is used; other directories (staging/, prod/, argocd/, ...)
	// are ignored.
	K8sFiles map[string][]byte

	// EnvConfig is the final preview ConfigMap data, computed by the
	// caller (Task 4) and carried into the generated <svc>-preview-env
	// ConfigMap verbatim.
	EnvConfig map[string]string

	// SecretName is the preview Secret to wire into envFrom, or "" if the
	// service has no secret (e.g. the dashboard).
	SecretName string

	// Migrate, if non-empty, is run as a "migrate" initContainer before the
	// app container starts: same image (including the preview tag
	// override) and same envFrom as the app container, so it sees
	// DATABASE_URL pointing at this preview's fresh Neon branch. Nil/empty
	// means no migration step, and the rendered Deployment gets no
	// initContainers at all — this is the common case (see
	// registry.Preview.Migrate for which services set it and why).
	Migrate []string
}

// Render builds a generated kustomize overlay atop the service's fetched
// k8s/base and returns the fully-resolved objects, ready to apply into the
// preview namespace.
func Render(in RenderInput) ([]*unstructured.Unstructured, error) {
	if in.Service == "" {
		return nil, errors.New("preview: render: Service is required")
	}
	if in.Tag == "" {
		return nil, errors.New("preview: render: Tag is required")
	}
	if in.ShortSHA == "" {
		return nil, errors.New("preview: render: ShortSHA is required")
	}

	fs := filesys.MakeFsInMemory()
	if err := writeBase(fs, in.K8sFiles); err != nil {
		return nil, err
	}
	if err := validateLocalKustomizeRefs(in.K8sFiles); err != nil {
		return nil, fmt.Errorf("preview: render %s: %w", in.Service, err)
	}

	base, err := kustomizeBuild(fs, "/base")
	if err != nil {
		return nil, fmt.Errorf("preview: render %s: building fetched base: %w", in.Service, err)
	}
	facts, err := detectBase(base, in.Service)
	if err != nil {
		return nil, fmt.Errorf("preview: render %s: inspecting fetched base: %w", in.Service, err)
	}

	if err := writeOverlay(fs, in, facts); err != nil {
		return nil, fmt.Errorf("preview: render %s: generating overlay: %w", in.Service, err)
	}

	overlay, err := kustomizeBuild(fs, "/overlay")
	if err != nil {
		return nil, fmt.Errorf("preview: render %s: building generated overlay: %w", in.Service, err)
	}

	resources := overlay.Resources()
	objs := make([]*unstructured.Unstructured, 0, len(resources))
	for _, res := range resources {
		u, err := toUnstructured(res)
		if err != nil {
			return nil, fmt.Errorf("preview: render %s: converting %s: %w", in.Service, res.CurId(), err)
		}
		objs = append(objs, u)
	}
	return objs, nil
}

// writeBase writes every k8sFiles entry rooted under "base/" into fs at
// /base/<relative path>, stripping the "base/" prefix. Everything else
// (staging/, prod/, argocd/, ...) is ignored — the generated overlay only
// ever builds atop the fetched base.
func writeBase(fs filesys.FileSystem, k8sFiles map[string][]byte) error {
	const prefix = "base/"
	wrote := false
	for p, content := range k8sFiles {
		rel, ok := strings.CutPrefix(p, prefix)
		if !ok || rel == "" {
			continue
		}
		dest := path.Join("/base", rel)
		if err := fs.MkdirAll(path.Dir(dest)); err != nil {
			return fmt.Errorf("preview: render: creating directory for %s: %w", dest, err)
		}
		if err := fs.WriteFile(dest, content); err != nil {
			return fmt.Errorf("preview: render: writing %s: %w", dest, err)
		}
		wrote = true
	}
	if !wrote {
		return errors.New("preview: render: no base/ files found in K8sFiles")
	}
	return nil
}

// minimalKustomizationRefs is the subset of a kustomization file's fields
// that can name another location to load: the current resources/bases/
// components/patches, plus the two deprecated-but-still-recognized legacy
// patch lists (patchesStrategicMerge, patchesJson6902). It deliberately
// doesn't cover the rest of ktypes.Kustomization (generators, replacements,
// etc.) — sigs.k8s.io/yaml ignores unknown fields, so this parses cleanly
// against any real kustomization.yaml while surfacing only the fields
// validateLocalKustomizeRefs needs to inspect.
type minimalKustomizationRefs struct {
	Resources  []string `json:"resources,omitempty"`
	Bases      []string `json:"bases,omitempty"`
	Components []string `json:"components,omitempty"`
	Patches    []struct {
		Path string `json:"path,omitempty"`
	} `json:"patches,omitempty"`
	PatchesStrategicMerge []string `json:"patchesStrategicMerge,omitempty"`
	PatchesJson6902       []struct {
		Path string `json:"path,omitempty"`
	} `json:"patchesJson6902,omitempty"`
}

// validateLocalKustomizeRefs rejects any fetched base/ kustomization file —
// any of konfig.RecognizedKustomizationFileNames() (currently
// "kustomization.yaml", "kustomization.yml", and the extensionless
// "Kustomization" — driven off krusty's own list rather than a hardcoded
// subset of it, so this can never silently drift out of sync again; an
// earlier version of this guard hardcoded just the first two and missed
// "Kustomization", letting a hostile base/Kustomization file restore the
// SSRF below) — that references anything other than a plain, relative,
// local path from its resources, bases, components, or patches.
//
// This exists because krusty's default options (kustomizeBuild) permit two
// classes of remote reference, and Render has no context to bound either
// one: an http(s) URL (resources/patches path, or a legacy
// patchesStrategicMerge/patchesJson6902 path) is fetched via a plain
// &http.Client{} with no timeout, and a base/component path can be a
// scheme-less "github.com/..." shorthand or an explicit scheme URL
// (ssh://, https://, file://, ...), both of which kustomize's git loader
// clones. A hostile fetched kustomization.yaml — e.g. from an attacker's
// branch — can use either to make bifrost's orchestrator issue a
// server-side request to an address of the attacker's choosing (verified
// SSRF), or hang the goroutine indefinitely, which never releases the run's
// busy-tag claim (see Orchestrator.acquire) since Render never returns.
//
// k8sFiles is the whole fetched k8s/ tree (RenderInput.K8sFiles); only
// entries under the "base/" prefix are checked, matching the only subtree
// writeBase copies into the build filesystem — kustomization files
// elsewhere (staging/, prod/, argocd/) are never read by kustomizeBuild, so
// scanning them would only produce irrelevant false rejections.
func validateLocalKustomizeRefs(k8sFiles map[string][]byte) error {
	const prefix = "base/"
	for p, content := range k8sFiles {
		rel, ok := strings.CutPrefix(p, prefix)
		if !ok || rel == "" {
			continue
		}
		name := path.Base(rel)
		if !slices.Contains(konfig.RecognizedKustomizationFileNames(), name) {
			continue
		}
		var k minimalKustomizationRefs
		if err := sigsyaml.Unmarshal(content, &k); err != nil {
			return fmt.Errorf("parsing %s: %w", p, err)
		}
		var refs []string
		refs = append(refs, k.Resources...)
		refs = append(refs, k.Bases...)
		refs = append(refs, k.Components...)
		refs = append(refs, k.PatchesStrategicMerge...)
		for _, patch := range k.Patches {
			refs = append(refs, patch.Path)
		}
		for _, patch := range k.PatchesJson6902 {
			refs = append(refs, patch.Path)
		}
		for _, ref := range refs {
			if unsafeKustomizeRef(ref) {
				return fmt.Errorf("%s: refuses non-local kustomize reference %q", p, ref)
			}
		}
	}
	return nil
}

// unsafeKustomizeRef reports whether ref is anything other than a plain
// relative local path: an explicit-scheme URL (http://, https://, ssh://,
// file://, ...), a protocol-relative "//host/path", kustomize's one
// recognized scheme-less git shorthand ("github.com/..." or "github.com:...",
// case-insensitively — see internal/git.isStandardGithubHost in
// sigs.k8s.io/kustomize/api; every other git host requires an explicit
// scheme or user@host: syntax, which the "://" check already catches), an
// absolute path, or a path that escapes its starting directory via "..".
// The escape check runs path.Clean first, the same way github.FetchK8s's
// tarball-entry guard does, so "a/../../b"-style decoys are caught too, not
// just a literal leading "../".
func unsafeKustomizeRef(ref string) bool {
	if ref == "" {
		return false
	}
	lower := strings.ToLower(ref)
	if strings.Contains(ref, "://") ||
		strings.HasPrefix(ref, "//") ||
		strings.HasPrefix(lower, "github.com/") ||
		strings.HasPrefix(lower, "github.com:") {
		return true
	}
	if path.IsAbs(ref) {
		return true
	}
	cleaned := path.Clean(ref)
	return cleaned == ".." || strings.HasPrefix(cleaned, "../")
}

// baseFacts records what the fetched base declares, so the generated
// overlay only patches around things that actually exist.
type baseFacts struct {
	hasCronJob    bool
	cronJobName   string
	configMapName string // "" if the base has no ConfigMap
	hasVolumes    bool   // the Deployment's pod spec declares any volumes
	// appImage is the base Deployment's app container's raw image string
	// (already confirmed by detectBase to start with imageRepoBase + "/" +
	// service + ":"). deploymentPatch stamps this onto the generated
	// migrate initContainer so it starts from the exact same repository
	// path as the app container — there's no base initContainer for a
	// strategic-merge patch to inherit an image from, unlike the app
	// container itself, so it has to be given explicitly. Because it's the
	// same repository path, the overlay's images: transformer (matched by
	// repository name only) retags it to the same preview-<sha> tag as the
	// app container.
	appImage string
}

// detectBase builds base (without the generated overlay) and scans its
// resources for the facts the overlay's shape depends on: whether a CronJob
// exists (and its name, which isn't derivable from the service name alone),
// whether a base ConfigMap exists (and its name, wired first into envFrom),
// and whether the Deployment declares volumes (the incomplete CSI
// secrets-store mount every previews must strip).
//
// It also guards two failure modes that do NOT surface as build errors.
//
// First: the generated deployment-patch's implicit target matches the base
// Deployment by resource name (service), which kustomize enforces (a
// missing Deployment name errors loudly at Run time) — but the patch's
// container list is merged into the base's *containers* by container name,
// which kustomize does not cross-check against anything. If the base
// Deployment's container isn't actually named service, the patch's {name,
// envFrom}-only container silently lands as a second, incomplete container
// alongside the untouched original, instead of patching it.
//
// Second: the overlay's `images:` transformer (see writeOverlay) retags
// whichever container image exactly matches imageRepoBase + "/" + service —
// matched by repository name only, ignoring whatever tag the base pins. If
// the base Deployment's image name doesn't match that (a typo, a renamed
// service, a copy-pasted image from a different member), the transformer
// silently no-ops instead of erroring, and the preview runs the base's own
// pinned tag (":prod" or ":latest") under the impression it's running the
// branch's freshly-built image.
//
// detectBase checks both explicitly and fails fast with a clear error
// rather than letting either render silently wrong.
func detectBase(base resmap.ResMap, service string) (baseFacts, error) {
	var f baseFacts
	for _, res := range base.Resources() {
		id := res.CurId()
		switch id.Kind {
		case "ConfigMap":
			if f.configMapName == "" {
				f.configMapName = id.Name
			}
		case "CronJob":
			f.hasCronJob = true
			f.cronJobName = id.Name
		case "Deployment":
			u, err := toUnstructured(res)
			if err != nil {
				return baseFacts{}, err
			}
			volumes, found, err := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "volumes")
			if err != nil {
				return baseFacts{}, err
			}
			f.hasVolumes = found && len(volumes) > 0

			containers, _, err := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "containers")
			if err != nil {
				return baseFacts{}, err
			}
			container, found := findContainerNamed(containers, service)
			if !found {
				return baseFacts{}, fmt.Errorf(
					"preview: render: base Deployment %s has no container named %q; "+
						"the generated deployment-patch merges its container by that name, "+
						"so a mismatch would silently add a second, incomplete container "+
						"instead of patching the real one",
					id.Name, service,
				)
			}
			wantImagePrefix := imageRepoBase + "/" + service + ":"
			image, _ := container["image"].(string)
			if !strings.HasPrefix(image, wantImagePrefix) {
				return baseFacts{}, fmt.Errorf(
					"preview: render: base Deployment %s container %q has image %q, "+
						"want it to start with %q; the overlay's images: transformer matches "+
						"by repository name only, so a mismatch would silently no-op and the "+
						"preview would run the base's own pinned tag instead of the built image",
					id.Name, service, image, wantImagePrefix,
				)
			}
			f.appImage = image
		}
	}
	return f, nil
}

// findContainerNamed returns the container in containers (a
// spec.template.spec.containers slice from an unstructured object) named
// name, or (nil, false) if none matches.
func findContainerNamed(containers []any, name string) (map[string]any, bool) {
	for _, c := range containers {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if n, _ := m["name"].(string); n == name {
			return m, true
		}
	}
	return nil, false
}

// writeOverlay generates /overlay: the kustomization.yaml tying the fetched
// base to the preview-specific patches and generated resources, the
// strategic-merge deployment patch (replicas, envFrom), the conditional
// cronjob suspend patch, the conditional JSON6902 volume-deletion patch, the
// generated preview-env ConfigMap, and the preview Ingress.
func writeOverlay(fs filesys.FileSystem, in RenderInput, facts baseFacts) error {
	if err := fs.MkdirAll("/overlay"); err != nil {
		return err
	}

	patches := []ktypes.Patch{{Path: "deployment-patch.yaml"}}
	if facts.hasCronJob {
		patches = append(patches, ktypes.Patch{Path: "cronjob-patch.yaml"})
	}
	if facts.hasVolumes {
		patches = append(patches, ktypes.Patch{
			Path: "volumes-patch.yaml",
			Target: &ktypes.Selector{
				ResId: resid.ResId{Gvk: resid.Gvk{Kind: "Deployment"}, Name: in.Service},
			},
		})
	}

	kustomization := ktypes.Kustomization{
		TypeMeta:  ktypes.TypeMeta{APIVersion: "kustomize.config.k8s.io/v1beta1", Kind: "Kustomization"},
		Namespace: previewNamespace(in.Tag),
		Resources: []string{"../base", "configmap.yaml", "ingress.yaml"},
		Images: []ktypes.Image{{
			Name:   imageRepoBase + "/" + in.Service,
			NewTag: "preview-" + in.ShortSHA,
		}},
		Patches: patches,
	}
	if err := writeYAML(fs, "/overlay/kustomization.yaml", kustomization); err != nil {
		return err
	}
	if err := writeYAML(fs, "/overlay/deployment-patch.yaml", deploymentPatch(in, facts)); err != nil {
		return err
	}
	if facts.hasCronJob {
		if err := writeYAML(fs, "/overlay/cronjob-patch.yaml", cronJobPatch(facts.cronJobName)); err != nil {
			return err
		}
	}
	if facts.hasVolumes {
		if err := writeYAML(fs, "/overlay/volumes-patch.yaml", volumesDeletePatch()); err != nil {
			return err
		}
	}
	if err := writeYAML(fs, "/overlay/configmap.yaml", previewConfigMap(in)); err != nil {
		return err
	}
	return writeYAML(fs, "/overlay/ingress.yaml", previewIngress(in))
}

// deploymentPatch is the strategic-merge patch pinning the Deployment to
// one preview replica and replacing its envFrom wholesale (envFrom has no
// strategic-merge key, so a submitted list replaces the base's rather than
// appending to it) with, in order: the base ConfigMap (if the base has
// one), the generated preview-env ConfigMap, and the preview Secret (if the
// service has one). No explicit target is needed: kustomize infers it from
// this patch's own apiVersion/kind/metadata.name.
//
// When in.Migrate is set, it also adds a single "migrate" initContainer
// sharing the app container's envFrom (identical list, same object) and
// starting from the app container's own base image (facts.appImage) so the
// shared images: transformer retags both to the same preview-<sha> tag —
// see baseFacts.appImage. It deliberately does not set restartPolicy (which
// would make it a Kubernetes "sidecar" native init container that runs
// alongside the app instead of to completion before it) or any
// volumeMounts (previews strip the base's CSI volume machinery entirely;
// see volumesDeletePatch).
func deploymentPatch(in RenderInput, facts baseFacts) map[string]any {
	var envFrom []map[string]any
	if facts.configMapName != "" {
		envFrom = append(envFrom, map[string]any{
			"configMapRef": map[string]any{"name": facts.configMapName},
		})
	}
	envFrom = append(envFrom, map[string]any{
		"configMapRef": map[string]any{"name": previewEnvConfigMapName(in.Service)},
	})
	if in.SecretName != "" {
		envFrom = append(envFrom, map[string]any{
			"secretRef": map[string]any{"name": in.SecretName},
		})
	}

	podSpec := map[string]any{
		"containers": []map[string]any{
			{"name": in.Service, "envFrom": envFrom},
		},
	}
	if len(in.Migrate) > 0 {
		podSpec["initContainers"] = []map[string]any{
			{
				"name":    "migrate",
				"image":   facts.appImage,
				"command": []string(in.Migrate),
				"envFrom": envFrom,
			},
		}
	}

	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": in.Service},
		"spec": map[string]any{
			"replicas": 1,
			"template": map[string]any{
				"spec": podSpec,
			},
		},
	}
}

// cronJobPatch is the strategic-merge patch suspending the preview's copy
// of the CronJob; previews don't run scheduled jobs.
func cronJobPatch(name string) map[string]any {
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "CronJob",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"suspend": true},
	}
}

// volumesDeletePatch is the JSON6902 patch stripping the base Deployment's
// incomplete CSI secrets-store volume (previews have no SecretProviderClass
// to satisfy it) and its matching volumeMount on the first container.
// Hardcoding containers/0 is safe because it always runs after
// deployment-patch.yaml (both are ordered entries in the same overlay
// kustomization.yaml): strategic-merge patching a containers list keyed by
// name promotes the patched/matched container to the front of the merged
// list regardless of its original position in the base, so the container
// deployment-patch.yaml targets (in.Service, guarded by detectBase) is
// always at index 0 by the time this JSON6902 patch runs — verified against
// a synthetic multi-container base, not assumed.
func volumesDeletePatch() []map[string]any {
	return []map[string]any{
		{"op": "remove", "path": "/spec/template/spec/volumes"},
		{"op": "remove", "path": "/spec/template/spec/containers/0/volumeMounts"},
	}
}

// previewConfigMap is the generated <svc>-preview-env ConfigMap carrying
// EnvConfig verbatim.
func previewConfigMap(in RenderInput) map[string]any {
	m := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": previewEnvConfigMapName(in.Service)},
	}
	if in.EnvConfig != nil {
		m["data"] = in.EnvConfig
	}
	return m
}

// previewIngress is the generated Ingress routing the preview host to the
// service's ClusterIP Service on port 80.
func previewIngress(in RenderInput) map[string]any {
	host := previewHost(in.Service, in.Tag)
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "Ingress",
		"metadata":   map[string]any{"name": in.Service + "-preview"},
		"spec": map[string]any{
			"ingressClassName": "nginx",
			"tls": []map[string]any{
				{"hosts": []string{host}, "secretName": previewTLSSecret},
			},
			"rules": []map[string]any{
				{
					"host": host,
					"http": map[string]any{
						"paths": []map[string]any{
							{
								"path":     "/",
								"pathType": "Prefix",
								"backend": map[string]any{
									"service": map[string]any{
										"name": in.Service,
										"port": map[string]any{"number": 80},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func previewNamespace(tag string) string            { return previewNSPrefix + tag }
func previewEnvConfigMapName(service string) string { return service + "-preview-env" }
func previewHost(service, tag string) string {
	return fmt.Sprintf("%s-%s.%s", service, tag, previewDomain)
}

// kustomizeBuild runs the kustomizer with default options against dir in fs.
func kustomizeBuild(fs filesys.FileSystem, dir string) (resmap.ResMap, error) {
	return krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(fs, dir)
}

// writeYAML marshals v to YAML and writes it to dest in fs.
func writeYAML(fs filesys.FileSystem, dest string, v any) error {
	b, err := sigsyaml.Marshal(v)
	if err != nil {
		return fmt.Errorf("preview: render: marshaling %s: %w", dest, err)
	}
	return fs.WriteFile(dest, b)
}

// toUnstructured converts a built kustomize resource to an
// unstructured.Unstructured via its YAML form — resource.Resource has no
// direct Map()-style accessor, only AsYAML/String (see task report for the
// API-shape delta from the plan's sketch). It goes through
// Unstructured.UnmarshalJSON rather than a plain sigsyaml.Unmarshal into a
// map so whole numbers land as int64 (matching what every other Kubernetes
// client produces) instead of encoding/json's default float64 — callers
// using unstructured.NestedInt64 on the result (replicas, port numbers, ...)
// depend on this.
func toUnstructured(res *resource.Resource) (*unstructured.Unstructured, error) {
	b, err := res.AsYAML()
	if err != nil {
		return nil, err
	}
	j, err := sigsyaml.YAMLToJSON(b)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(j); err != nil {
		return nil, err
	}
	return u, nil
}
