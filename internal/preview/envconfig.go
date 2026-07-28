package preview

import (
	"fmt"
	"sort"

	"github.com/eswan18/bifrost/internal/config"
	sigsyaml "sigs.k8s.io/yaml"
)

// stagingConfigMapPath is where github.FetchK8s's returned map carries the
// service's staging ConfigMap (path relative to k8s/), the baseline every
// preview's env config starts from.
const stagingConfigMapPath = "staging/configmap-env.yaml"

// parseStagingEnv extracts the staging ConfigMap's data map from a member's
// fetched k8s/ tree. A repo with no staging/configmap-env.yaml (e.g. the
// dashboard, which is env-agnostic) yields an empty map, not an error.
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
// inputs always produce the same output — everything needed (the preview's
// tag, its full member list, and the registry describing every previewable
// service's env wiring) is passed in explicitly rather than read from shared
// state.
//
// The result is stagingData copied verbatim, then reg[svc]'s env templates
// (registry.yaml's Service.Env) rendered over it one key at a time via
// Eval/EvalContext — see template.go for the resolution cascade each
// template goes through. A service with no registry entry (there is none
// today, but the registry doesn't guarantee full coverage) passes staging
// data through unchanged, matching previewable services with no special env
// wiring.
//
// Once every key has rendered, every key named in reg[svc]'s Required list
// must have rendered to a non-empty value — a required key that resolves to
// "" (e.g. an unset PreviewOAuthClientID rendering "{{ config
// previewOAuthClientID }}" to "") is an error, not a silently broken preview
// deploy. This complements parseRegistry's load-time check: that only proves
// a required key has *a* template to render; this proves the rendered value
// is actually usable.
//
// Callers must draw svc from members (as every real call site does — see
// orchestrator.go's "for _, svc := range members" loops): Eval's "{{ url
// self }}" resolves svc == ctx.Service to its own preview URL
// unconditionally, regardless of whether svc actually appears in members
// (see EvalContext.Members and resolveURL in template.go). Calling this with
// svc absent from members would silently change that service's own "self"
// URL semantics rather than erroring.
func envConfigFor(svc, tag string, members []string, stagingData map[string]string, cfg *config.Config, reg Registry) (map[string]string, error) {
	data := copyStringMap(stagingData)

	entry, ok := reg[svc]
	if !ok {
		return data, nil
	}

	ctx := EvalContext{
		Service:  svc,
		Tag:      tag,
		Members:  members,
		Cfg:      cfg,
		Baseline: stagingData,
	}

	// Sorted, not range-order: map iteration order is randomized, and a
	// registry with more than one unresolvable key would otherwise report a
	// different offender on every run.
	keys := make([]string, 0, len(entry.Env))
	for key := range entry.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		ctx.Key = key
		val, err := Eval(entry.Env[key], ctx)
		if err != nil {
			return nil, fmt.Errorf("preview: env config for %s: %w", svc, err)
		}
		data[key] = val
	}

	for _, key := range entry.Required {
		if data[key] == "" {
			return nil, fmt.Errorf("preview: env config for %s: %s: required but rendered empty", svc, key)
		}
	}

	return data, nil
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
