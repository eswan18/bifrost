package preview

import (
	"sort"

	"github.com/eswan18/bifrost/internal/registry"
)

// This package's registry types are now thin aliases onto internal/registry,
// the fleet-wide source of truth (plan 2026-07-29-fleet-registry-unification,
// task 1). Service aliases registry.Preview -- not registry.Service, whose
// preview fields live one level deeper under .Preview -- so Registry keeps
// exactly the narrow "previewable services only, preview fields promoted to
// the top level" shape this package's orchestration logic (envConfigFor,
// Orchestrator) has always operated on. That keeps every existing call site
// (o.Registry[svc].Neon, entry.Env, entry.Required, and test literals like
// Registry{"svc": {Neon: &NeonRef{...}}}) unchanged, so this move carries no
// risk of altering preview behavior. See FromFleet for how the two shapes
// connect.

// NeonRef locates one service's Neon database for preview branching (see
// registry.NeonRef's doc comment for the full rationale).
type NeonRef = registry.NeonRef

// Service is one previewable app's declaration: its Neon reference (nil if
// it has no database), its env wiring as unevaluated templates (see
// template.go for the primitives), and which of those env keys must resolve
// to a non-empty value. It aliases registry.Preview, the fleet registry's
// optional per-service preview block.
type Service = registry.Preview

// Registry is every previewable service, keyed by name.
type Registry map[string]Service

// LoadRegistry loads the fleet registry and narrows it to the preview view
// (see FromFleet). It's the preview control plane's sole entry point onto
// the registry; registry.Load is the one that actually parses and embeds
// the YAML.
func LoadRegistry() (Registry, error) {
	fleet, err := registry.Load()
	if err != nil {
		return nil, err
	}
	return FromFleet(fleet), nil
}

// FromFleet narrows a fleet registry down to the previewable subset,
// promoting each service's Preview block to the map value -- exactly the
// shape this package's orchestration logic has always operated on, now
// sourced from the fleet-wide registry.yaml instead of one of its own.
// Services with no preview block are dropped, matching the old registry's
// contents exactly (it only ever held the three previewable services).
// PreviewNames is the single source of truth for "previewable" -- this
// reuses it rather than re-testing svc.Preview != nil itself.
func FromFleet(fleet registry.Registry) Registry {
	names := fleet.PreviewNames()
	reg := make(Registry, len(names))
	for _, name := range names {
		reg[name] = *fleet[name].Preview
	}
	return reg
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
