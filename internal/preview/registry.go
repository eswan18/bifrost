package preview

import (
	_ "embed"
	"fmt"
	"sort"

	sigsyaml "sigs.k8s.io/yaml"
)

//go:embed registry.yaml
var registryYAML []byte

// NeonRef locates one service's Neon database for preview branching. It
// lives in the registry — never in the branch under test — so a preview
// can never point at a database other than the one bifrost was configured
// to trust.
type NeonRef struct {
	Project  string `json:"project"`
	Database string `json:"database"`
	Role     string `json:"role"`
}

// Service is one previewable app's declaration: its Neon reference (nil if
// it has no database), its env wiring as unevaluated templates (see
// template.go for the primitives), and which of those env keys must resolve
// to a non-empty value.
type Service struct {
	Neon     *NeonRef          `json:"neon,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Required []string          `json:"required,omitempty"`
}

// Registry is every previewable service, keyed by name.
type Registry map[string]Service

// LoadRegistry parses the embedded registry.yaml — the single source of
// truth for which services are previewable, their Neon references, and
// their env wiring.
func LoadRegistry() (Registry, error) {
	return parseRegistry(registryYAML)
}

// parseRegistry parses raw registry YAML. It is split out from LoadRegistry
// so tests can exercise malformed and invalid input without touching the
// embedded file.
//
// Unknown fields (typos in a service's neon/env/required keys) are rejected
// so they surface here, at load time, rather than silently doing nothing at
// preview-creation time. Every required key must also appear in that
// service's env — a required key with no template to resolve is a registry
// bug, not a runtime error to discover later.
func parseRegistry(data []byte) (Registry, error) {
	var reg Registry
	if err := sigsyaml.UnmarshalStrict(data, &reg); err != nil {
		return nil, fmt.Errorf("preview: parsing registry: %w", err)
	}
	for name, svc := range reg {
		for _, key := range svc.Required {
			if _, ok := svc.Env[key]; !ok {
				return nil, fmt.Errorf("preview: registry: %s: required key %q has no matching env template", name, key)
			}
		}
	}
	return reg, nil
}

// Names returns the previewable service names, sorted.
func (r Registry) Names() []string {
	names := make([]string, 0, len(r))
	for name := range r {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
