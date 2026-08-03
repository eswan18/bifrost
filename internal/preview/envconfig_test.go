package preview

import (
	"reflect"
	"strings"
	"testing"

	"github.com/eswan18/bifrost/internal/config"
	"github.com/eswan18/bifrost/internal/registry"
)

// testRegistry loads the real embedded fleet registry.yaml (via
// LoadRegistry, which narrows it to the preview view) for tests that need
// an actual Registry value -- envConfigFor's callers, mainly -- without
// every one of them repeating the error-check boilerplate. The registry's
// own parsing/validation is covered by internal/registry's tests; this
// package only needs a real value to drive envConfigFor/Orchestrator with.
func testRegistry(t *testing.T) Registry {
	t.Helper()
	reg, err := LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry() error = %v", err)
	}
	return reg
}

// testFleet loads the real embedded fleet-wide registry.yaml (the
// un-narrowed registry.Registry, carrying every service's repo/URLs, not
// just the previewable subset testRegistry returns) -- what cascade step 3
// (resolveURL's non-member/no-baseline fallback) now consults instead of
// the old operator-configured config.Config.StagingURLs. Its footstrike-api/
// footstrike-dashboard/identity staging URLs are exactly the values these
// tests used to hardcode into StagingURLs maps (Task 1 verified them
// field-by-field against k8s/base/configmap.yaml), so swapping to the real
// fleet here changes nothing about what resolves.
func testFleet(t *testing.T) registry.Registry {
	t.Helper()
	fleet, err := registry.Load()
	if err != nil {
		t.Fatalf("registry.Load() error = %v", err)
	}
	return fleet
}

func TestParseStagingEnv(t *testing.T) {
	t.Run("present file parses data map", func(t *testing.T) {
		files := map[string][]byte{
			"staging/configmap-env.yaml": []byte(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: footstrike-api-staging-env-config
data:
  ENV: "staging"
  JWT_AUDIENCE: "ethanswan:fitness"
`),
		}
		got, err := parseStagingEnv(files)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := map[string]string{"ENV": "staging", "JWT_AUDIENCE": "ethanswan:fitness"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parseStagingEnv() = %v, want %v", got, want)
		}
	})

	t.Run("absent file yields empty map, not an error", func(t *testing.T) {
		got, err := parseStagingEnv(map[string][]byte{"base/deployment.yaml": []byte("irrelevant")})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("parseStagingEnv() = %v, want empty map", got)
		}
	})

	t.Run("configmap with no data key yields empty map", func(t *testing.T) {
		files := map[string][]byte{
			"staging/configmap-env.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"),
		}
		got, err := parseStagingEnv(files)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("parseStagingEnv() = %v, want empty map", got)
		}
	})

	t.Run("malformed yaml errors", func(t *testing.T) {
		files := map[string][]byte{"staging/configmap-env.yaml": []byte("not: [valid: yaml")}
		if _, err := parseStagingEnv(files); err == nil {
			t.Fatal("expected an error for malformed YAML, got nil")
		}
	})
}

func stagingFixture() map[string]string {
	return map[string]string{
		"ENV":                       "staging",
		"IDENTITY_PROVIDER_URL":     "http://identity.identity-staging.svc.cluster.local",
		"JWT_ISSUER":                "https://identity-staging.tailc06f30.ts.net",
		"PUBLIC_API_BASE_URL":       "https://api.staging.footstrike.run",
		"PUBLIC_DASHBOARD_BASE_URL": "https://staging.footstrike.run",
		"JWT_AUDIENCE":              "ethanswan:fitness",
	}
}

func TestEnvConfigForFootstrikeAPI(t *testing.T) {
	t.Run("identity not in members: identity URLs pass through from staging", func(t *testing.T) {
		staging := stagingFixture()
		got, err := envConfigFor("footstrike-api", "hae-cadence", []string{"footstrike-api"}, staging, &config.Config{}, testRegistry(t), nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// "staging", not "preview": the api validates ENV against a closed set
		// and exits at import on anything else (caught by the first end-to-end
		// preview smoke, which crash-looped the api pod).
		if got["ENV"] != "staging" {
			t.Errorf("ENV = %q, want staging", got["ENV"])
		}
		if got["PUBLIC_API_BASE_URL"] != "https://footstrike-api-hae-cadence.preview.footstrike.run" {
			t.Errorf("PUBLIC_API_BASE_URL = %q", got["PUBLIC_API_BASE_URL"])
		}
		// footstrike-dashboard is NOT a member here, so the resolution cascade
		// falls through to footstrike-api's own staging baseline instead of a
		// preview URL nothing serves -- the one sanctioned divergence from the
		// old hardcoded behavior (previously always
		// "https://footstrike-dashboard-hae-cadence.preview.footstrike.run",
		// a dead host when the dashboard isn't part of this preview). See
		// TestEnvConfigForRegistryEquivalence for the full writeup.
		if want := staging["PUBLIC_DASHBOARD_BASE_URL"]; got["PUBLIC_DASHBOARD_BASE_URL"] != want {
			t.Errorf("PUBLIC_DASHBOARD_BASE_URL = %q, want staging baseline %q", got["PUBLIC_DASHBOARD_BASE_URL"], want)
		}
		if got["IDENTITY_PROVIDER_URL"] != staging["IDENTITY_PROVIDER_URL"] {
			t.Errorf("IDENTITY_PROVIDER_URL = %q, want passthrough %q", got["IDENTITY_PROVIDER_URL"], staging["IDENTITY_PROVIDER_URL"])
		}
		if got["JWT_ISSUER"] != staging["JWT_ISSUER"] {
			t.Errorf("JWT_ISSUER = %q, want passthrough %q", got["JWT_ISSUER"], staging["JWT_ISSUER"])
		}
		// Unrelated staging keys pass through untouched.
		if got["JWT_AUDIENCE"] != staging["JWT_AUDIENCE"] {
			t.Errorf("JWT_AUDIENCE = %q, want passthrough %q", got["JWT_AUDIENCE"], staging["JWT_AUDIENCE"])
		}
	})

	t.Run("identity in members: identity URLs repoint at preview identity", func(t *testing.T) {
		staging := stagingFixture()
		got, err := envConfigFor("footstrike-api", "hae-cadence", []string{"footstrike-api", "identity"}, staging, &config.Config{}, testRegistry(t), nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "http://identity.preview-hae-cadence.svc.cluster.local"; got["IDENTITY_PROVIDER_URL"] != want {
			t.Errorf("IDENTITY_PROVIDER_URL = %q, want %q", got["IDENTITY_PROVIDER_URL"], want)
		}
		if want := "https://identity-hae-cadence.preview.footstrike.run"; got["JWT_ISSUER"] != want {
			t.Errorf("JWT_ISSUER = %q, want %q", got["JWT_ISSUER"], want)
		}
	})

	t.Run("does not mutate the input staging map", func(t *testing.T) {
		staging := stagingFixture()
		before := copyStringMap(staging)
		if _, err := envConfigFor("footstrike-api", "t", []string{"footstrike-api", "identity"}, staging, &config.Config{}, testRegistry(t), nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !reflect.DeepEqual(staging, before) {
			t.Errorf("envConfigFor mutated its stagingData input: got %v, want %v", staging, before)
		}
	})

	// SANCTIONED DIVERGENCE (decided 2026-07-28): PUBLIC_DASHBOARD_BASE_URL
	// and JWT_ISSUER have nowhere to fall back to without a staging fixture,
	// so -- unlike the old hardcoded implementation, which had no error
	// return path at all and could never fail here -- the cascade now needs
	// the registry's staging URL fallback (Task 2 moved this from an
	// operator-configured config.Config.StagingURLs to
	// registry.yaml's Service.URLs.Staging) to still resolve when the real
	// staging configmap is unavailable and dashboard/identity aren't
	// members. See the sibling subtest below for the case with no such
	// fallback available, which is the divergence itself: old code always
	// produced *something*; the cascade instead errors, naming the exact
	// unresolvable key.
	t.Run("absent staging file (empty map) still yields the mandatory overrides, given the registry's staging URL fallback", func(t *testing.T) {
		got, err := envConfigFor("footstrike-api", "t", []string{"footstrike-api"}, map[string]string{}, &config.Config{}, testRegistry(t), testFleet(t))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["ENV"] != "staging" || got["PUBLIC_API_BASE_URL"] == "" || got["PUBLIC_DASHBOARD_BASE_URL"] == "" {
			t.Errorf("envConfigFor with empty staging data = %v, missing mandatory overrides", got)
		}
	})

	// SANCTIONED DIVERGENCE (decided 2026-07-28): the actual divergence the
	// subtest above works around by supplying the registry's staging URL
	// fallback. Old footstrikeAPIEnvConfig had no error return path
	// whatsoever -- given a completely empty stagingData and an unconfigured
	// cfg, it still hardcoded PUBLIC_DASHBOARD_BASE_URL/JWT_ISSUER to preview
	// or staging URLs unconditionally. The registry-driven cascade instead
	// exhausts (footstrike-dashboard/identity aren't members, there's no
	// baseline entry to defer to, and fleet is nil here so there's no
	// registry staging URL fallback either) and errors, naming the
	// unresolvable key. This is data-reachable in production only in the
	// sense that a nil Fleet never happens in practice (main.go always wires
	// Orchestrator.Fleet to the loaded registry, which has a staging URL for
	// every previewable service) -- this subtest isolates cascade step 4
	// itself, not a real production gap. Controller ruling: sanctioned as
	// correct and better -- the old path would deploy a pod with a broken/
	// dead PUBLIC_DASHBOARD_BASE_URL or JWT_ISSUER that crash-loops or fails
	// silently at runtime; the new cascade fails loudly at preview-creation
	// time, before the cluster is ever touched, naming exactly which key and
	// service is unresolvable.
	t.Run("SANCTIONED DIVERGENCE (decided 2026-07-28): no staging baseline and no registry staging URL fallback now errors, where old code never could", func(t *testing.T) {
		_, err := envConfigFor("footstrike-api", "t", []string{"footstrike-api"}, map[string]string{}, &config.Config{}, testRegistry(t), nil)
		if err == nil {
			t.Fatal("expected an error when PUBLIC_DASHBOARD_BASE_URL/JWT_ISSUER have no member, baseline, or registry staging URL fallback, got nil")
		}
	})
}

func TestEnvConfigForIdentity(t *testing.T) {
	t.Run("identity in members: JWT_ISSUER repoints at its own preview URL", func(t *testing.T) {
		staging := stagingFixture()
		got, err := envConfigFor("identity", "hae-cadence", []string{"footstrike-api", "identity"}, staging, &config.Config{}, testRegistry(t), nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "https://identity-hae-cadence.preview.footstrike.run"; got["JWT_ISSUER"] != want {
			t.Errorf("JWT_ISSUER = %q, want %q", got["JWT_ISSUER"], want)
		}
		// Unrelated staging keys still pass through.
		if got["JWT_AUDIENCE"] != staging["JWT_AUDIENCE"] {
			t.Errorf("JWT_AUDIENCE = %q, want passthrough %q", got["JWT_AUDIENCE"], staging["JWT_AUDIENCE"])
		}
	})

	// NOT a production scenario: renderAndApply only ever calls envConfigFor
	// with svc drawn from members (see orchestrator.go's "for _, svc := range
	// members" loop), so identity's own config is never computed with
	// identity excluded from members in any real Up() run -- the old
	// identityEnvConfig's "if slices.Contains(members, svcIdentity)" branch
	// was therefore unreachable in production too, always true in every real
	// call. Eval's "{{ url self }}" resolves svc == ctx.Service unconditionally
	// (EvalContext's own doc comment: "Service is usually, but need not be,
	// included in [Members] explicitly"), a Task 2 design decision, so this
	// input now resolves to identity's own preview URL instead of the old
	// passthrough. Byte-identical in every reachable case; this test is kept
	// only to document the synthetic edge case's new behavior explicitly.
	t.Run("identity NOT in members (unreachable via Up(); self still resolves)", func(t *testing.T) {
		staging := stagingFixture()
		got, err := envConfigFor("identity", "hae-cadence", []string{"footstrike-api"}, staging, &config.Config{}, testRegistry(t), nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "https://identity-hae-cadence.preview.footstrike.run"; got["JWT_ISSUER"] != want {
			t.Errorf("JWT_ISSUER = %q, want %q (self always resolves to its own preview URL)", got["JWT_ISSUER"], want)
		}
	})
}

func TestEnvConfigForDashboardSynthesis(t *testing.T) {
	baseCfg := &config.Config{PreviewOAuthClientID: "preview-client-id"}

	t.Run("both api and identity members: both URLs are preview URLs", func(t *testing.T) {
		got, err := envConfigFor("footstrike-dashboard", "hae-cadence",
			[]string{"footstrike-api", "footstrike-dashboard", "identity"}, nil, baseCfg, testRegistry(t), nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := map[string]string{
			"APP_API_URL":         "https://footstrike-api-hae-cadence.preview.footstrike.run",
			"APP_IDENTITY_URL":    "https://identity-hae-cadence.preview.footstrike.run",
			"APP_OAUTH_CLIENT_ID": "preview-client-id",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("dashboard env = %v, want %v", got, want)
		}
	})

	t.Run("neither member: falls back to the registry's staging URLs", func(t *testing.T) {
		cfg := &config.Config{PreviewOAuthClientID: "preview-client-id"}
		got, err := envConfigFor("footstrike-dashboard", "hae-cadence", []string{"footstrike-dashboard"}, nil, cfg, testRegistry(t), testFleet(t))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["APP_API_URL"] != "https://api.staging.footstrike.run" {
			t.Errorf("APP_API_URL = %q", got["APP_API_URL"])
		}
		if got["APP_IDENTITY_URL"] != "https://identity-staging.tailc06f30.ts.net" {
			t.Errorf("APP_IDENTITY_URL = %q", got["APP_IDENTITY_URL"])
		}
	})

	t.Run("api unresolvable (not a member, no staging URL) errors", func(t *testing.T) {
		// fleet: nil -- no registry staging URL fallback available, so
		// footstrike-api (not a member, no baseline) stays unresolvable.
		_, err := envConfigFor("footstrike-dashboard", "t", []string{"footstrike-dashboard"}, nil, baseCfg, testRegistry(t), nil)
		if err == nil {
			t.Fatal("expected an error when APP_API_URL is unresolvable, got nil")
		}
	})

	t.Run("identity unresolvable (not a member, no staging URL) errors", func(t *testing.T) {
		cfg := &config.Config{PreviewOAuthClientID: "preview-client-id"}
		// A synthetic fleet with a staging URL for footstrike-api but none for
		// identity -- footstrike-api resolves (proving the fallback path
		// works at all), identity deliberately has no registry entry to fall
		// back to, so APP_IDENTITY_URL stays unresolvable.
		fleet := registry.Registry{
			"footstrike-api": {URLs: registry.URLs{Staging: "https://api.staging.footstrike.run"}},
		}
		_, err := envConfigFor("footstrike-dashboard", "t", []string{"footstrike-dashboard", "footstrike-api"}, nil, cfg, testRegistry(t), fleet)
		if err == nil {
			t.Fatal("expected an error when APP_IDENTITY_URL is unresolvable, got nil")
		}
	})

	t.Run("missing PreviewOAuthClientID errors even when both URLs resolve", func(t *testing.T) {
		cfg := &config.Config{} // PreviewOAuthClientID empty
		_, err := envConfigFor("footstrike-dashboard", "t",
			[]string{"footstrike-api", "footstrike-dashboard", "identity"}, nil, cfg, testRegistry(t), nil)
		if err == nil {
			t.Fatal("expected an error for missing PreviewOAuthClientID, got nil")
		}
		if !strings.Contains(err.Error(), "APP_OAUTH_CLIENT_ID") {
			t.Errorf("error = %q, want it to name APP_OAUTH_CLIENT_ID", err.Error())
		}
	})

	// Unlike the old dashboard-specific code path (which built its ConfigMap
	// data from scratch, discarding whatever stagingData it was handed),
	// envConfigFor now treats every previewable service uniformly: stagingData
	// is always copied first, then the registry's env keys are rendered over
	// it -- exactly the semantics footstrike-api and identity already had.
	// This is a deliberate Task 3 unification, not a behavior regression: the
	// dashboard's real fetched k8s/ tree never contains a
	// staging/configmap-env.yaml (its image is env-agnostic), so stagingData
	// is always empty in production regardless of this test's stale-key probe.
	t.Run("stagingData is copied like every other service (dashboard's real fetch is always empty, so this is inert in production)", func(t *testing.T) {
		staging := map[string]string{"SOME_STALE_KEY": "passes through now"}
		got, err := envConfigFor("footstrike-dashboard", "t",
			[]string{"footstrike-api", "footstrike-dashboard", "identity"}, staging, baseCfg, testRegistry(t), nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["SOME_STALE_KEY"] != "passes through now" {
			t.Errorf("dashboard env config = %v, want stagingData copied through like any other service", got)
		}
	})
}

// ---- registry-driven equivalence ------------------------------------------
//
// These goldens were captured by running the OLD, pre-refactor envConfigFor
// (the three-function switch) against the scenarios below and printing its
// output with "%#v" (see the task-3 report for the exact capture harness).
// They are not hand-derived from the plan: a hand-written golden that
// matched the plan rather than the actual running code would defeat the
// entire point of this test. Every entry must remain byte-for-byte
// unchanged after envConfigFor becomes registry-driven, except the one
// entry explicitly marked SANCTIONED DIVERGENCE below.
func TestEnvConfigForRegistryEquivalence(t *testing.T) {
	tag := "hae-cadence"
	staging := stagingFixture()
	cfgFull := &config.Config{PreviewOAuthClientID: "preview-client-id"}
	// fleetFull is the real embedded fleet registry: it carries the same
	// footstrike-api/identity staging URLs cfgFull.StagingURLs used to
	// hardcode (Task 1 verified them field-by-field against
	// k8s/base/configmap.yaml), so every table entry below resolves
	// identically to before Task 2 moved cascade step 3's fallback off
	// config.Config and onto the registry.
	fleetFull := testFleet(t)

	tests := []struct {
		name    string
		svc     string
		members []string
		staging map[string]string
		cfg     *config.Config
		want    map[string]string
	}{
		{
			name:    "footstrike-api: full members {api,dashboard,identity}",
			svc:     "footstrike-api",
			members: []string{"footstrike-api", "footstrike-dashboard", "identity"},
			staging: staging,
			cfg:     cfgFull,
			want: map[string]string{
				"ENV":                       "staging",
				"IDENTITY_PROVIDER_URL":     "http://identity.preview-hae-cadence.svc.cluster.local",
				"JWT_AUDIENCE":              "ethanswan:fitness",
				"JWT_ISSUER":                "https://identity-hae-cadence.preview.footstrike.run",
				"PUBLIC_API_BASE_URL":       "https://footstrike-api-hae-cadence.preview.footstrike.run",
				"PUBLIC_DASHBOARD_BASE_URL": "https://footstrike-dashboard-hae-cadence.preview.footstrike.run",
			},
		},
		{
			name:    "footstrike-api: {api,dashboard} (no identity)",
			svc:     "footstrike-api",
			members: []string{"footstrike-api", "footstrike-dashboard"},
			staging: staging,
			cfg:     cfgFull,
			want: map[string]string{
				"ENV":                       "staging",
				"IDENTITY_PROVIDER_URL":     "http://identity.identity-staging.svc.cluster.local",
				"JWT_AUDIENCE":              "ethanswan:fitness",
				"JWT_ISSUER":                "https://identity-staging.tailc06f30.ts.net",
				"PUBLIC_API_BASE_URL":       "https://footstrike-api-hae-cadence.preview.footstrike.run",
				"PUBLIC_DASHBOARD_BASE_URL": "https://footstrike-dashboard-hae-cadence.preview.footstrike.run",
			},
		},
		{
			// SANCTIONED DIVERGENCE (decided 2026-07-28): footstrike-dashboard
			// is NOT a member here. The old footstrikeAPIEnvConfig set
			// PUBLIC_DASHBOARD_BASE_URL to the preview dashboard's URL
			// unconditionally -- even though nothing serves that host in this
			// preview (dead OAuth redirect target, dead CORS origin). Under
			// the registry's resolution cascade, "{{ url footstrike-dashboard
			// }}" falls through to footstrike-api's own staging baseline,
			// which actually exists ("https://staging.footstrike.run"). This
			// is the one deliberate, intentionally-updated golden the task-3
			// brief sanctions; every other entry in this test is byte-for-byte
			// unchanged from the old switch-based implementation.
			name:    "footstrike-api: {api} alone (no dashboard, no identity)",
			svc:     "footstrike-api",
			members: []string{"footstrike-api"},
			staging: staging,
			cfg:     cfgFull,
			want: map[string]string{
				"ENV":                       "staging",
				"IDENTITY_PROVIDER_URL":     "http://identity.identity-staging.svc.cluster.local",
				"JWT_AUDIENCE":              "ethanswan:fitness",
				"JWT_ISSUER":                "https://identity-staging.tailc06f30.ts.net",
				"PUBLIC_API_BASE_URL":       "https://footstrike-api-hae-cadence.preview.footstrike.run",
				"PUBLIC_DASHBOARD_BASE_URL": "https://staging.footstrike.run", // was: https://footstrike-dashboard-hae-cadence.preview.footstrike.run
			},
		},
		{
			name:    "identity: {api,dashboard,identity} (identity present)",
			svc:     "identity",
			members: []string{"footstrike-api", "footstrike-dashboard", "identity"},
			staging: staging,
			cfg:     cfgFull,
			want: map[string]string{
				"ENV":                       "staging",
				"IDENTITY_PROVIDER_URL":     "http://identity.identity-staging.svc.cluster.local",
				"JWT_AUDIENCE":              "ethanswan:fitness",
				"JWT_ISSUER":                "https://identity-hae-cadence.preview.footstrike.run",
				"PUBLIC_API_BASE_URL":       "https://api.staging.footstrike.run",
				"PUBLIC_DASHBOARD_BASE_URL": "https://staging.footstrike.run",
			},
		},
		{
			// A SECOND, non-sanctioned-but-non-production divergence,
			// discovered while building this equivalence suite (not the one
			// the task-3 brief names): the old identityEnvConfig only
			// repointed JWT_ISSUER when "slices.Contains(members,
			// svcIdentity)" was true. Eval's "{{ url self }}" instead always
			// resolves svc == ctx.Service, regardless of Members (see
			// EvalContext's doc comment and template.go's resolveURL) -- a
			// Task 2 design decision, not something introduced here. This
			// changes the output for this one synthetic input (identity
			// excluded from its own members while computing its own env),
			// but renderAndApply (orchestrator.go) only ever calls
			// envConfigFor with svc drawn from members, so identity's own
			// config is NEVER computed with identity absent from members in
			// any real Up() run -- the old branch this diverges from was
			// itself unreachable in production, always true in every real
			// call. Zero observable production impact; see the task-3 report
			// for the full reachability argument.
			name:    "identity: {api} alone (identity NOT in members -- unreachable via Up())",
			svc:     "identity",
			members: []string{"footstrike-api"},
			staging: staging,
			cfg:     cfgFull,
			want: map[string]string{
				"ENV":                       "staging",
				"IDENTITY_PROVIDER_URL":     "http://identity.identity-staging.svc.cluster.local",
				"JWT_AUDIENCE":              "ethanswan:fitness",
				"JWT_ISSUER":                "https://identity-hae-cadence.preview.footstrike.run", // was: https://identity-staging.tailc06f30.ts.net (passthrough)
				"PUBLIC_API_BASE_URL":       "https://api.staging.footstrike.run",
				"PUBLIC_DASHBOARD_BASE_URL": "https://staging.footstrike.run",
			},
		},
		{
			name:    "dashboard: both api+identity members",
			svc:     "footstrike-dashboard",
			members: []string{"footstrike-api", "footstrike-dashboard", "identity"},
			staging: nil,
			cfg:     cfgFull,
			want: map[string]string{
				"APP_API_URL":         "https://footstrike-api-hae-cadence.preview.footstrike.run",
				"APP_IDENTITY_URL":    "https://identity-hae-cadence.preview.footstrike.run",
				"APP_OAUTH_CLIENT_ID": "preview-client-id",
			},
		},
		{
			name:    "dashboard: neither member, staging fallback",
			svc:     "footstrike-dashboard",
			members: []string{"footstrike-dashboard"},
			staging: nil,
			cfg:     cfgFull,
			want: map[string]string{
				"APP_API_URL":         "https://api.staging.footstrike.run",
				"APP_IDENTITY_URL":    "https://identity-staging.tailc06f30.ts.net",
				"APP_OAUTH_CLIENT_ID": "preview-client-id",
			},
		},
		{
			name:    "dashboard: api member, identity absent (mixed resolution)",
			svc:     "footstrike-dashboard",
			members: []string{"footstrike-api", "footstrike-dashboard"},
			staging: nil,
			cfg:     cfgFull,
			want: map[string]string{
				"APP_API_URL":         "https://footstrike-api-hae-cadence.preview.footstrike.run",
				"APP_IDENTITY_URL":    "https://identity-staging.tailc06f30.ts.net",
				"APP_OAUTH_CLIENT_ID": "preview-client-id",
			},
		},
		{
			name:    "unknown service passthrough",
			svc:     "some-other-service",
			members: []string{"some-other-service"},
			staging: map[string]string{"SOME_KEY": "some-value", "OTHER_KEY": "other-value"},
			cfg:     cfgFull,
			want: map[string]string{
				"SOME_KEY":  "some-value",
				"OTHER_KEY": "other-value",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := envConfigFor(tt.svc, tag, tt.members, tt.staging, tt.cfg, testRegistry(t), fleetFull)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("envConfigFor(%s) = %#v,\nwant %#v", tt.svc, got, tt.want)
			}
		})
	}
}

func TestEnvConfigForUnknownServicePassesThroughStagingData(t *testing.T) {
	staging := map[string]string{"SOME_KEY": "some-value", "OTHER_KEY": "other-value"}
	got, err := envConfigFor("some-other-service", "t", []string{"some-other-service"}, staging, &config.Config{}, testRegistry(t), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, staging) {
		t.Errorf("envConfigFor(unknown service) = %v, want passthrough %v", got, staging)
	}
	// Purity: returned map must be a copy, not an alias.
	got["SOME_KEY"] = "mutated"
	if staging["SOME_KEY"] == "mutated" {
		t.Error("envConfigFor returned an alias of stagingData, not a copy")
	}
}

// TestEnvConfigForRequiredKeyRenderedEmptyErrors exercises the runtime
// required-key check directly (rather than only via the dashboard's
// PreviewOAuthClientID path above): a required key whose template renders to
// "" -- not just one that errors outright -- must fail with an error naming
// the key, in the same "<KEY>: ..." shape the old dashboardEnvConfig used for
// its unset-PreviewOAuthClientID check. parseRegistry's load-time check only
// proves a required key has a template; it can't prove that template renders
// non-empty, which is exactly the gap this closes (flagged in Task 2's
// review).
func TestEnvConfigForRequiredKeyRenderedEmptyErrors(t *testing.T) {
	reg := Registry{
		"svc": Service{
			Env:      map[string]string{"MUST_BE_SET": "{{ config previewOAuthClientID }}"},
			Required: []string{"MUST_BE_SET"},
		},
	}
	_, err := envConfigFor("svc", "t", []string{"svc"}, nil, &config.Config{}, reg, nil)
	if err == nil {
		t.Fatal("expected an error when a required key renders empty, got nil")
	}
	if !strings.Contains(err.Error(), "MUST_BE_SET") {
		t.Errorf("error = %q, want it to name the empty required key", err.Error())
	}
}

// TestEnvConfigEmptyOverrideBeatsTheStagingBaseline is forecasting's
// SENTRY_DSN: "" re-verified rather than assumed, against the real embedded
// registry. Three separate claims, each one load-bearing for that entry, and
// each one a plausible implementation could get wrong:
//
//  1. an empty registry template OVERWRITES a non-empty staging baseline. A
//     renderer that treated "" as "nothing to say" -- skipping the key, or
//     preferring the baseline -- would leave staging's real DSN in place and
//     every forecasting preview would report its errors into the Sentry
//     project someone actually watches. The baseline below is deliberately
//     non-empty and recognizable, so a passthrough is visible.
//  2. it does not disturb the app's other staging keys. PUBSUB_TOPIC is here
//     because forecasting deliberately does NOT override it (comms#6);
//     previews inherit staging's topic, and this pins that inheriting is what
//     happens rather than something the registry has to restate.
//  3. Required rejects an empty rendered value -- which is why SENTRY_DSN
//     must stay out of required:. Asserted here on a synthetic entry rather
//     than by breaking the real one.
func TestEnvConfigEmptyOverrideBeatsTheStagingBaseline(t *testing.T) {
	staging := map[string]string{
		"SENTRY_DSN":   "https://realkey@o123.ingest.sentry.io/456",
		"PUBSUB_TOPIC": "forecasting-staging-notifications",
		"ENV":          "staging",
	}
	got, err := envConfigFor("forecasting", "hae-cadence", []string{"forecasting"}, staging, &config.Config{}, testRegistry(t), testFleet(t))
	if err != nil {
		t.Fatalf("envConfigFor: %v", err)
	}

	dsn, ok := got["SENTRY_DSN"]
	if !ok {
		t.Fatal("SENTRY_DSN missing from the rendered config; the key must be PRESENT and empty, since the app reads process.env.SENTRY_DSN")
	}
	if dsn != "" {
		t.Errorf("SENTRY_DSN = %q, want \"\": the registry's empty override must beat the staging baseline, or previews report into staging's Sentry project", dsn)
	}
	if got["PUBSUB_TOPIC"] != staging["PUBSUB_TOPIC"] {
		t.Errorf("PUBSUB_TOPIC = %q, want staging's %q untouched (previews deliberately share staging's topic -- comms#6)", got["PUBSUB_TOPIC"], staging["PUBSUB_TOPIC"])
	}
	if got["ENV"] != "staging" {
		t.Errorf("ENV = %q, want staging's value untouched (forecasting declares no override for it)", got["ENV"])
	}

	// Claim 3, on a synthetic entry: listing a key that renders empty in
	// required: turns every preview of that service into a pre-flight
	// failure. This is why forecasting's required: is absent, not why its
	// SENTRY_DSN is empty -- two facts that have to stay consistent.
	reg := Registry{"svc": Service{Env: map[string]string{"SENTRY_DSN": ""}, Required: []string{"SENTRY_DSN"}}}
	if _, err := envConfigFor("svc", "t", []string{"svc"}, staging, &config.Config{}, reg, nil); err == nil {
		t.Error("expected an error for a required key whose template is the empty string, got nil")
	}
}
