// Package registry is the single source of truth for the bifrost fleet:
// every service bifrost operates on, its repo (when it differs from the
// service name), its public staging/prod URLs, and — for the services
// onboarded to preview environments — its preview wiring. It grew out of
// internal/preview's original registry (plan
// 2026-07-29-fleet-registry-unification, task 1): that one only ever
// described the three previewable services; this one describes the whole
// fleet, with preview data demoted to an optional per-service sub-block so
// adding a new app is one YAML entry instead of a Go change plus a
// still-separate service list.
package registry

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
	// Parent is the NAME of the branch a preview's branch is cut from —
	// "development", not "br-solitary-sun-ad74wwhf". This file is hand-edited
	// and reviewed in a diff, where an opaque branch ID would be
	// unverifiable; the name is resolved to its ID at branch-creation time
	// (internal/preview.ensureNeonBranch) against the ListBranches call it
	// already makes.
	//
	// Empty means "let Neon pick" — the project's DEFAULT branch — which in
	// every project here is production. See registry.yaml's per-service
	// comments for why that default is the wrong one for previews.
	Parent string `json:"parent,omitempty"`
}

// Preview is a previewable service's preview wiring: its Neon reference
// (nil if it has no database), its env wiring as unevaluated templates (see
// internal/preview/template.go for the primitives), and which of those env
// keys must resolve to a non-empty value. A Service with a nil Preview is
// simply not onboarded to preview environments.
type Preview struct {
	Neon     *NeonRef          `json:"neon,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Required []string          `json:"required,omitempty"`
	// Migrate is run as an initContainer before the app starts, with the
	// same image and env as the app container — so it sees DATABASE_URL
	// pointing at this preview's fresh Neon branch. Empty = no migration
	// step. Kept beside Neon deliberately: branch the database, then bring
	// it up to the branch's schema.
	Migrate []string `json:"migrate,omitempty"`
}

// URLs is a service's public endpoints, used for the "open ↗" links on its
// service card. Either field may be empty — e.g. comms, a background worker
// with no ingress, has neither.
type URLs struct {
	Staging string `json:"staging,omitempty"`
	Prod    string `json:"prod,omitempty"`
}

// Service is one app in the fleet.
type Service struct {
	Repo    string   `json:"repo,omitempty"` // defaults to the service name; see RepoFor
	URLs    URLs     `json:"urls,omitempty"`
	Preview *Preview `json:"preview,omitempty"` // nil = not onboarded to preview environments
}

// Registry is the whole fleet, keyed by service name.
type Registry map[string]Service

// Load parses the embedded registry.yaml — the single source of truth for
// the fleet: every service bifrost operates on, its repo/URLs, and which
// services are previewable.
func Load() (Registry, error) {
	return parseRegistry(registryYAML)
}

// parseRegistry parses raw registry YAML. It is split out from Load so
// tests can exercise malformed and invalid input without touching the
// embedded file.
//
// Unknown fields (typos in a service's repo/urls/preview keys, or in a
// preview block's own neon/env/required keys) are rejected so they surface
// here, at load time, rather than silently doing nothing later. Every
// preview block's required key must also appear in that same block's env —
// a required key with no template to resolve is a registry bug, not a
// runtime error to discover later.
func parseRegistry(data []byte) (Registry, error) {
	var reg Registry
	if err := sigsyaml.UnmarshalStrict(data, &reg); err != nil {
		return nil, fmt.Errorf("registry: parsing: %w", err)
	}
	for name, svc := range reg {
		if svc.Preview == nil {
			continue
		}
		for _, key := range svc.Preview.Required {
			if _, ok := svc.Preview.Env[key]; !ok {
				return nil, fmt.Errorf("registry: %s: required key %q has no matching env template", name, key)
			}
		}
	}
	return reg, nil
}

// Names returns every fleet service name, sorted.
func (r Registry) Names() []string {
	names := make([]string, 0, len(r))
	for name := range r {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// PreviewNames returns the names of services with a preview block, sorted.
func (r Registry) PreviewNames() []string {
	names := make([]string, 0, len(r))
	for name, svc := range r {
		if svc.Preview != nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// RepoFor returns svc's repo name: its registry override when one is
// configured (e.g. asset-manager -> asset_manager), or svc itself
// otherwise — "repos are named after the service, except when they're not."
// An svc absent from the registry entirely still returns svc unchanged,
// matching that same fallback.
func (r Registry) RepoFor(svc string) string {
	if s, ok := r[svc]; ok && s.Repo != "" {
		return s.Repo
	}
	return svc
}
