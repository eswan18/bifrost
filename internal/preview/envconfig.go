package preview

import (
	"errors"
	"fmt"
	"slices"

	"github.com/eswan18/bifrost/internal/config"
	sigsyaml "sigs.k8s.io/yaml"
)

// Service names the preview control plane treats specially: footstrike-api's
// and identity's env config get identity-aware URL overrides, and the
// dashboard's env config is synthesized from scratch rather than fetched.
const (
	svcFootstrikeAPI = "footstrike-api"
	svcDashboard     = "footstrike-dashboard"
	svcIdentity      = "identity"
)

// stagingConfigMapPath is where github.FetchK8s's returned map carries the
// service's staging ConfigMap (path relative to k8s/), the baseline every
// preview's env config starts from.
const stagingConfigMapPath = "staging/configmap-env.yaml"

// parseStagingEnv extracts the staging ConfigMap's data map from a member's
// fetched k8s/ tree. A repo with no staging/configmap-env.yaml (e.g. the
// dashboard, which is env-agnostic) yields an empty map, not an error —
// envConfigFor's dashboard branch ignores it anyway.
func parseStagingEnv(k8sFiles map[string][]byte) (map[string]string, error) {
	content, ok := k8sFiles[stagingConfigMapPath]
	if !ok {
		return map[string]string{}, nil
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := sigsyaml.Unmarshal(content, &cm); err != nil {
		return nil, fmt.Errorf("preview: parsing %s: %w", stagingConfigMapPath, err)
	}
	if cm.Data == nil {
		return map[string]string{}, nil
	}
	return cm.Data, nil
}

// envConfigFor computes the final preview ConfigMap data for one member
// service. It is a pure function: stagingData is never mutated, and the same
// inputs always produce the same output — everything the orchestrator needs
// to know (the preview's tag and its full member list) is passed in
// explicitly rather than read from shared state.
//
//   - footstrike-api: staging config plus ENV=preview, PUBLIC_API_BASE_URL /
//     PUBLIC_DASHBOARD_BASE_URL pointed at this preview's own URLs, and
//     IDENTITY_PROVIDER_URL/JWT_ISSUER repointed at the preview's own
//     identity when identity is a member (otherwise left as whatever the
//     branch's staging config already had — normally staging identity).
//   - identity: staging config plus JWT_ISSUER repointed at identity's own
//     preview URL, since identity's issuer claim must describe wherever
//     identity itself is actually running.
//   - footstrike-dashboard: synthesized from scratch (dashboard ships no
//     staging/configmap-env.yaml — its image is env-agnostic), and the
//     mandatory triple: APP_API_URL/APP_IDENTITY_URL derived from membership
//     (preview URL when the service is a member, else its configured staging
//     URL) and APP_OAUTH_CLIENT_ID from cfg.PreviewOAuthClientID. Any of the
//     three being unresolvable is an error — the dashboard preview image
//     carries no baked fallback, so a partial set only fails at runtime in
//     the browser (see the preview design doc).
//   - anything else: staging config passed through unchanged.
func envConfigFor(svc, tag string, members []string, stagingData map[string]string, cfg *config.Config) (map[string]string, error) {
	switch svc {
	case svcDashboard:
		return dashboardEnvConfig(members, tag, cfg)
	case svcFootstrikeAPI:
		return footstrikeAPIEnvConfig(tag, members, stagingData), nil
	case svcIdentity:
		return identityEnvConfig(tag, members, stagingData), nil
	default:
		return copyStringMap(stagingData), nil
	}
}

func footstrikeAPIEnvConfig(tag string, members []string, stagingData map[string]string) map[string]string {
	data := copyStringMap(stagingData)
	data["ENV"] = "preview"
	data["PUBLIC_API_BASE_URL"] = previewURL(svcFootstrikeAPI, tag)
	data["PUBLIC_DASHBOARD_BASE_URL"] = previewURL(svcDashboard, tag)
	if slices.Contains(members, svcIdentity) {
		data["IDENTITY_PROVIDER_URL"] = internalPreviewURL(svcIdentity, tag)
		data["JWT_ISSUER"] = previewURL(svcIdentity, tag)
	}
	return data
}

func identityEnvConfig(tag string, members []string, stagingData map[string]string) map[string]string {
	data := copyStringMap(stagingData)
	if slices.Contains(members, svcIdentity) {
		data["JWT_ISSUER"] = previewURL(svcIdentity, tag)
	}
	return data
}

// dashboardEnvConfig builds the dashboard's mandatory env triple. It is
// called both as Up's stage-1 pre-flight validation (dashboard member
// without all three resolvable must fail before EnsureNamespace ever runs)
// and, later, to compute the dashboard's actual ConfigMap data — the same
// logic must produce the same verdict both times, so there is exactly one
// implementation.
func dashboardEnvConfig(members []string, tag string, cfg *config.Config) (map[string]string, error) {
	apiURL, err := memberOrStagingURL(svcFootstrikeAPI, tag, members, cfg)
	if err != nil {
		return nil, fmt.Errorf("APP_API_URL: %w", err)
	}
	identityURL, err := memberOrStagingURL(svcIdentity, tag, members, cfg)
	if err != nil {
		return nil, fmt.Errorf("APP_IDENTITY_URL: %w", err)
	}
	if cfg.PreviewOAuthClientID == "" {
		return nil, errors.New("APP_OAUTH_CLIENT_ID: PreviewOAuthClientID is not configured")
	}
	return map[string]string{
		"APP_API_URL":         apiURL,
		"APP_IDENTITY_URL":    identityURL,
		"APP_OAUTH_CLIENT_ID": cfg.PreviewOAuthClientID,
	}, nil
}

// memberOrStagingURL resolves the URL a dashboard preview should point at
// for svc: its own preview URL when svc is a member of this preview,
// otherwise its configured staging URL. Neither being available is an error.
func memberOrStagingURL(svc, tag string, members []string, cfg *config.Config) (string, error) {
	if slices.Contains(members, svc) {
		return previewURL(svc, tag), nil
	}
	if url := cfg.StagingURLs[svc]; url != "" {
		return url, nil
	}
	return "", fmt.Errorf("%s is not in this preview and has no staging URL configured", svc)
}

// previewURL is the externally-reachable preview URL for svc (see
// render.go's previewHost — same host, just with a scheme).
func previewURL(svc, tag string) string {
	return "https://" + previewHost(svc, tag)
}

// internalPreviewURL is the in-cluster DNS name for svc within tag's preview
// namespace, matching the plan's "{app}.preview-{tag}.svc.cluster.local"
// convention. Plain http: cross-namespace cluster traffic isn't behind TLS
// (matching every staging *_PROVIDER_URL value already in use).
func internalPreviewURL(svc, tag string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local", svc, previewNamespace(tag))
}

// copyStringMap returns a shallow copy of m (nil-safe), so callers can hand
// out a map derived from stagingData without letting mutations by one
// caller's overrides bleed into another's, or into the caller's own input.
func copyStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
