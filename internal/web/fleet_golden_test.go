package web

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/eswan18/bifrost/internal/config"
	"github.com/eswan18/bifrost/internal/registry"
)

// TestAssembleFleetRegistryEquivalence is the equivalence golden for plan
// 2026-07-29-fleet-registry-unification's Task 2: it pins byte-for-byte what
// assembleFleet rendered for the real seven-service fleet BEFORE fleet.go and
// handlers.go were repointed from config.Config's four maps
// (Services/RepoOverrides/StagingURLs/ProdURLs) at internal/registry.
//
// The golden below was captured by running the OLD (pre-migration)
// assembleFleet — back when Handlers had no Registry field and fleet.go read
// h.Cfg.Services/RepoFor/StagingURLs/ProdURLs directly — against this same
// fixture, and printing its actual output with "%#v" (see the task-2 report
// for the exact capture harness). It is not hand-derived from the plan or
// from registry.yaml. It deliberately covers every one of the four migrated
// fields per service: the repo name (asset-manager's "asset_manager"
// override is the one service where RepoFor/RepoOverrides actually changes
// the answer), the staging URL, the prod URL, and comms' two empty URLs
// (it's a background worker with no ingress). assembleFleet is the seam, not
// the narrower appView-only construction path, because it's what every page
// handler (Overview, Apps, Jobs) actually calls — the tightest seam that is
// still exactly what ships, with no test-only shortcut between the golden
// and production behavior.
//
// Every derived value here is deliberately time-independent (every env is a
// single settled image, no jobs/cronjobs, no tracked build) so the golden
// stays byte-for-byte stable however long it sits unrun — assembleFleet's
// time.Now() calls only affect deploy-progress/job-recency labels, none of
// which this fixture exercises.
//
// Now that fleet.go/handlers.go read h.Registry instead, this test still
// passes byte-for-byte against goldenFleetRegistry below — a registry.Registry
// literal built (not loaded from the embedded registry.yaml) with the exact
// same repo/URL values goldenFleetConfig used to carry, so this test stays a
// self-contained fixture rather than drifting whenever registry.yaml does.
func goldenFleetServices() []string {
	return []string{
		"asset-manager", "bifrost", "comms", "footstrike-api",
		"footstrike-dashboard", "forecasting", "identity",
	}
}

// goldenFleetHandlers builds the Handlers this golden was captured against,
// from an already-constructed *config.Config and registry.Registry (see
// goldenFleetConfig/goldenFleetRegistry below).
func goldenFleetHandlers(t *testing.T, cfg *config.Config, reg registry.Registry) *Handlers {
	t.Helper()
	services := goldenFleetServices()
	imgs := map[string][]string{}
	for _, svc := range services {
		imgs[svc+"-staging"] = []string{"reg/" + svc + ":abc1234"}
		imgs[svc+"-prod"] = []string{"reg/" + svc + ":abc1234"}
	}
	k := &fakeKube{imgs: imgs}
	rend, err := LoadTemplates("../../templates")
	if err != nil {
		t.Fatalf("templates: %v", err)
	}
	return &Handlers{Cfg: cfg, Registry: reg, Kube: k, Renderer: rend}
}

// goldenFleetConfig is what's left of the fixture on config.Config now that
// Services/RepoOverrides/StagingURLs/ProdURLs live in the registry instead.
func goldenFleetConfig() *config.Config {
	return &config.Config{
		SessionSecret:   []byte("12345678901234567890123456789012"),
		ArgoCDNamespace: "argocd",
		GitHubOrg:       "eswan18",
		DisplayLocation: time.UTC,
	}
}

// goldenFleetRegistry is the registry-shaped equivalent of the four maps
// goldenFleetConfig used to carry, populated with the exact values
// k8s/base/configmap.yaml carried at the base commit (SERVICES,
// REPO_OVERRIDES, STAGING_URLS, PROD_URLS) — the same source Task 1 verified
// registry.yaml against field-by-field. A self-contained literal (not
// registry.Load()) so this golden can't silently start passing/failing
// because someone edited the embedded registry.yaml for an unrelated
// reason.
func goldenFleetRegistry() registry.Registry {
	return registry.Registry{
		"asset-manager": {
			Repo: "asset_manager",
			URLs: registry.URLs{
				Staging: "https://assets-staging.tailc06f30.ts.net",
				Prod:    "https://assets.ethanswan.com",
			},
		},
		"bifrost": {
			URLs: registry.URLs{
				Staging: "https://bifrost-staging.tailc06f30.ts.net",
				Prod:    "https://bifrost.ethanswan.com",
			},
		},
		"comms": {}, // background worker, no ingress: no URLs.
		"footstrike-api": {
			URLs: registry.URLs{
				Staging: "https://api.staging.footstrike.run",
				Prod:    "https://api.footstrike.run",
			},
		},
		"footstrike-dashboard": {
			URLs: registry.URLs{
				Staging: "https://staging.footstrike.run",
				Prod:    "https://footstrike.run",
			},
		},
		"forecasting": {
			URLs: registry.URLs{
				Staging: "https://forecasting-staging.tailc06f30.ts.net",
				Prod:    "https://forecasting.ethanswan.com",
			},
		},
		"identity": {
			URLs: registry.URLs{
				Staging: "https://identity-staging.tailc06f30.ts.net",
				Prod:    "https://identity.ethanswan.com",
			},
		},
	}
}

// wantGoldenFleetApps is the captured golden itself: assembleFleet(ctx).Apps
// for goldenFleetConfig()+goldenFleetHandlers()'s fixture, exactly as the OLD
// code produced it. See the package doc comment above for capture method and
// what it guards.
func wantGoldenFleetApps() []appView {
	healthy := func(name, repo, stagingURL, prodURL string) appView {
		env := func(kind, appURL string) envView {
			return envView{
				Env:        kind,
				Tag:        "abc1234",
				SHA:        "abc1234",
				CommitURL:  "https://github.com/eswan18/" + repo + "/commit/abc1234",
				AppURL:     appURL,
				Image:      "reg/" + name + ":abc1234",
				Status:     "ok",
				Label:      "healthy",
				LabelClass: "c-mut",
				Detail:     "1/1 ready",
			}
		}
		return appView{
			Name:         name,
			RepoURL:      "https://github.com/eswan18/" + repo,
			Staging:      env("staging", stagingURL),
			Prod:         env("prod", prodURL),
			Overall:      "sync",
			PromoteState: "in_sync",
			Badge:        "IN SYNC",
			BadgeClass:   "c-grn",
			Build:        buildView{State: "none"},
			JobsLabel:    "0 jobs",
			JobsClass:    "c-mut",

			ShowRollbackGhost: true,
			SyncText:          "in sync",

			PromoteFrom: "abc1234",
			PromoteNote: "prod is healthy · staging is healthy",
		}
	}

	return []appView{
		healthy("asset-manager", "asset_manager",
			"https://assets-staging.tailc06f30.ts.net", "https://assets.ethanswan.com"),
		healthy("bifrost", "bifrost",
			"https://bifrost-staging.tailc06f30.ts.net", "https://bifrost.ethanswan.com"),
		healthy("comms", "comms", "", ""), // background worker, no ingress: no URLs.
		healthy("footstrike-api", "footstrike-api",
			"https://api.staging.footstrike.run", "https://api.footstrike.run"),
		healthy("footstrike-dashboard", "footstrike-dashboard",
			"https://staging.footstrike.run", "https://footstrike.run"),
		healthy("forecasting", "forecasting",
			"https://forecasting-staging.tailc06f30.ts.net", "https://forecasting.ethanswan.com"),
		healthy("identity", "identity",
			"https://identity-staging.tailc06f30.ts.net", "https://identity.ethanswan.com"),
	}
}

func TestAssembleFleetRegistryEquivalence(t *testing.T) {
	h := goldenFleetHandlers(t, goldenFleetConfig(), goldenFleetRegistry())
	f := h.assembleFleet(context.Background())

	want := wantGoldenFleetApps()
	if !reflect.DeepEqual(f.Apps, want) {
		t.Errorf("assembleFleet().Apps diverged from the pre-migration golden.\ngot:  %#v\nwant: %#v", f.Apps, want)
	}

	// Ordering is itself part of the "no behavior change" contract (same
	// seven services, same order) — assert it explicitly, not just via the
	// slice-wide DeepEqual above.
	var gotNames []string
	for _, a := range f.Apps {
		gotNames = append(gotNames, a.Name)
	}
	wantNames := goldenFleetServices()
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Errorf("fleet order = %v, want %v", gotNames, wantNames)
	}
}
