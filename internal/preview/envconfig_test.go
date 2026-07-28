package preview

import (
	"reflect"
	"testing"

	"github.com/eswan18/bifrost/internal/config"
)

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
		got, err := envConfigFor("footstrike-api", "hae-cadence", []string{"footstrike-api"}, staging, &config.Config{})
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
		if got["PUBLIC_DASHBOARD_BASE_URL"] != "https://footstrike-dashboard-hae-cadence.preview.footstrike.run" {
			t.Errorf("PUBLIC_DASHBOARD_BASE_URL = %q", got["PUBLIC_DASHBOARD_BASE_URL"])
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
		got, err := envConfigFor("footstrike-api", "hae-cadence", []string{"footstrike-api", "identity"}, staging, &config.Config{})
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
		if _, err := envConfigFor("footstrike-api", "t", []string{"footstrike-api", "identity"}, staging, &config.Config{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !reflect.DeepEqual(staging, before) {
			t.Errorf("envConfigFor mutated its stagingData input: got %v, want %v", staging, before)
		}
	})

	t.Run("absent staging file (empty map) still yields the mandatory overrides", func(t *testing.T) {
		got, err := envConfigFor("footstrike-api", "t", []string{"footstrike-api"}, map[string]string{}, &config.Config{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["ENV"] != "staging" || got["PUBLIC_API_BASE_URL"] == "" || got["PUBLIC_DASHBOARD_BASE_URL"] == "" {
			t.Errorf("envConfigFor with empty staging data = %v, missing mandatory overrides", got)
		}
	})
}

func TestEnvConfigForIdentity(t *testing.T) {
	t.Run("identity in members: JWT_ISSUER repoints at its own preview URL", func(t *testing.T) {
		staging := stagingFixture()
		got, err := envConfigFor("identity", "hae-cadence", []string{"footstrike-api", "identity"}, staging, &config.Config{})
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

	t.Run("identity not in members: JWT_ISSUER passes through unchanged", func(t *testing.T) {
		staging := stagingFixture()
		got, err := envConfigFor("identity", "hae-cadence", []string{"footstrike-api"}, staging, &config.Config{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["JWT_ISSUER"] != staging["JWT_ISSUER"] {
			t.Errorf("JWT_ISSUER = %q, want passthrough %q", got["JWT_ISSUER"], staging["JWT_ISSUER"])
		}
	})
}

func TestEnvConfigForDashboardSynthesis(t *testing.T) {
	baseCfg := &config.Config{PreviewOAuthClientID: "preview-client-id"}

	t.Run("both api and identity members: both URLs are preview URLs", func(t *testing.T) {
		got, err := envConfigFor("footstrike-dashboard", "hae-cadence",
			[]string{"footstrike-api", "footstrike-dashboard", "identity"}, nil, baseCfg)
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

	t.Run("neither member: falls back to configured staging URLs", func(t *testing.T) {
		cfg := &config.Config{
			PreviewOAuthClientID: "preview-client-id",
			StagingURLs: map[string]string{
				"footstrike-api": "https://api.staging.footstrike.run",
				"identity":       "https://identity-staging.tailc06f30.ts.net",
			},
		}
		got, err := envConfigFor("footstrike-dashboard", "hae-cadence", []string{"footstrike-dashboard"}, nil, cfg)
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
		_, err := envConfigFor("footstrike-dashboard", "t", []string{"footstrike-dashboard"}, nil, baseCfg)
		if err == nil {
			t.Fatal("expected an error when APP_API_URL is unresolvable, got nil")
		}
	})

	t.Run("identity unresolvable (not a member, no staging URL) errors", func(t *testing.T) {
		cfg := &config.Config{
			PreviewOAuthClientID: "preview-client-id",
			StagingURLs:          map[string]string{"footstrike-api": "https://api.staging.footstrike.run"},
		}
		_, err := envConfigFor("footstrike-dashboard", "t", []string{"footstrike-dashboard", "footstrike-api"}, nil, cfg)
		if err == nil {
			t.Fatal("expected an error when APP_IDENTITY_URL is unresolvable, got nil")
		}
	})

	t.Run("missing PreviewOAuthClientID errors even when both URLs resolve", func(t *testing.T) {
		cfg := &config.Config{} // PreviewOAuthClientID empty
		_, err := envConfigFor("footstrike-dashboard", "t",
			[]string{"footstrike-api", "footstrike-dashboard", "identity"}, nil, cfg)
		if err == nil {
			t.Fatal("expected an error for missing PreviewOAuthClientID, got nil")
		}
	})

	t.Run("ignores any stagingData passed in (dashboard is synthesized, not fetched)", func(t *testing.T) {
		staging := map[string]string{"SOME_STALE_KEY": "should not appear"}
		got, err := envConfigFor("footstrike-dashboard", "t",
			[]string{"footstrike-api", "footstrike-dashboard", "identity"}, staging, baseCfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := got["SOME_STALE_KEY"]; ok {
			t.Errorf("dashboard env config = %v, want no trace of stagingData", got)
		}
	})
}

func TestEnvConfigForUnknownServicePassesThroughStagingData(t *testing.T) {
	staging := map[string]string{"SOME_KEY": "some-value", "OTHER_KEY": "other-value"}
	got, err := envConfigFor("some-other-service", "t", []string{"some-other-service"}, staging, &config.Config{})
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
