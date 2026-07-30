package registry

import (
	"reflect"
	"testing"
)

func TestLoad(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	wantNames := []string{
		"asset-manager", "bifrost", "comms", "footstrike-api",
		"footstrike-dashboard", "forecasting", "identity",
	}
	if got := reg.Names(); !reflect.DeepEqual(got, wantNames) {
		t.Errorf("Names() = %v, want %v", got, wantNames)
	}

	wantPreviewNames := []string{"footstrike-api", "footstrike-dashboard", "identity"}
	if got := reg.PreviewNames(); !reflect.DeepEqual(got, wantPreviewNames) {
		t.Errorf("PreviewNames() = %v, want %v", got, wantPreviewNames)
	}

	t.Run("asset-manager: repo override, urls, no preview", func(t *testing.T) {
		svc, ok := reg["asset-manager"]
		if !ok {
			t.Fatal("asset-manager missing from registry")
		}
		if svc.Repo != "asset_manager" {
			t.Errorf("Repo = %q, want asset_manager", svc.Repo)
		}
		wantURLs := URLs{Staging: "https://assets-staging.tailc06f30.ts.net", Prod: "https://assets.ethanswan.com"}
		if svc.URLs != wantURLs {
			t.Errorf("URLs = %+v, want %+v", svc.URLs, wantURLs)
		}
		if svc.Preview != nil {
			t.Errorf("Preview = %+v, want nil", svc.Preview)
		}
	})

	t.Run("bifrost: no repo override, urls, no preview", func(t *testing.T) {
		svc, ok := reg["bifrost"]
		if !ok {
			t.Fatal("bifrost missing from registry")
		}
		if svc.Repo != "" {
			t.Errorf("Repo = %q, want empty (no override)", svc.Repo)
		}
		wantURLs := URLs{Staging: "https://bifrost-staging.tailc06f30.ts.net", Prod: "https://bifrost.ethanswan.com"}
		if svc.URLs != wantURLs {
			t.Errorf("URLs = %+v, want %+v", svc.URLs, wantURLs)
		}
	})

	t.Run("comms: no urls, no preview -- background worker, no ingress", func(t *testing.T) {
		svc, ok := reg["comms"]
		if !ok {
			t.Fatal("comms missing from registry")
		}
		if svc.URLs != (URLs{}) {
			t.Errorf("URLs = %+v, want zero value", svc.URLs)
		}
		if svc.URLs.Staging != "" {
			t.Errorf("URLs.Staging = %q, want empty string, not a placeholder", svc.URLs.Staging)
		}
		if svc.URLs.Prod != "" {
			t.Errorf("URLs.Prod = %q, want empty string, not a placeholder", svc.URLs.Prod)
		}
		if svc.Preview != nil {
			t.Errorf("Preview = %+v, want nil", svc.Preview)
		}
	})

	t.Run("forecasting: urls, no preview", func(t *testing.T) {
		svc, ok := reg["forecasting"]
		if !ok {
			t.Fatal("forecasting missing from registry")
		}
		wantURLs := URLs{Staging: "https://forecasting-staging.tailc06f30.ts.net", Prod: "https://forecasting.ethanswan.com"}
		if svc.URLs != wantURLs {
			t.Errorf("URLs = %+v, want %+v", svc.URLs, wantURLs)
		}
		if svc.Preview != nil {
			t.Errorf("Preview = %+v, want nil", svc.Preview)
		}
	})

	t.Run("footstrike-api: urls and preview (neon + env)", func(t *testing.T) {
		svc, ok := reg["footstrike-api"]
		if !ok {
			t.Fatal("footstrike-api missing from registry")
		}
		wantURLs := URLs{Staging: "https://api.staging.footstrike.run", Prod: "https://api.footstrike.run"}
		if svc.URLs != wantURLs {
			t.Errorf("URLs = %+v, want %+v", svc.URLs, wantURLs)
		}
		if svc.Preview == nil {
			t.Fatal("Preview = nil, want non-nil")
		}
		if svc.Preview.Neon == nil {
			t.Fatal("Preview.Neon = nil, want non-nil")
		}
		wantNeon := NeonRef{Project: "aged-river-81935268", Database: "neondb", Role: "neondb_owner"}
		if *svc.Preview.Neon != wantNeon {
			t.Errorf("Preview.Neon = %+v, want %+v", *svc.Preview.Neon, wantNeon)
		}
		wantEnv := map[string]string{
			"ENV":                       "staging",
			"PUBLIC_API_BASE_URL":       "{{ url self }}",
			"PUBLIC_DASHBOARD_BASE_URL": "{{ url footstrike-dashboard }}",
			"IDENTITY_PROVIDER_URL":     "{{ internalUrl identity }}",
			"JWT_ISSUER":                "{{ url identity }}",
		}
		if !reflect.DeepEqual(svc.Preview.Env, wantEnv) {
			t.Errorf("Preview.Env = %v, want %v", svc.Preview.Env, wantEnv)
		}
		if len(svc.Preview.Required) != 0 {
			t.Errorf("Preview.Required = %v, want empty", svc.Preview.Required)
		}
		wantMigrate := []string{"alembic", "upgrade", "head"}
		if !reflect.DeepEqual(svc.Preview.Migrate, wantMigrate) {
			t.Errorf("Preview.Migrate = %v, want %v", svc.Preview.Migrate, wantMigrate)
		}
	})

	t.Run("footstrike-dashboard: urls, preview with no neon, required keys", func(t *testing.T) {
		svc, ok := reg["footstrike-dashboard"]
		if !ok {
			t.Fatal("footstrike-dashboard missing from registry")
		}
		wantURLs := URLs{Staging: "https://staging.footstrike.run", Prod: "https://footstrike.run"}
		if svc.URLs != wantURLs {
			t.Errorf("URLs = %+v, want %+v", svc.URLs, wantURLs)
		}
		if svc.Preview == nil {
			t.Fatal("Preview = nil, want non-nil")
		}
		if svc.Preview.Neon != nil {
			t.Errorf("Preview.Neon = %+v, want nil (dashboard has no database)", svc.Preview.Neon)
		}
		wantEnv := map[string]string{
			"APP_API_URL":         "{{ url footstrike-api }}",
			"APP_IDENTITY_URL":    "{{ url identity }}",
			"APP_OAUTH_CLIENT_ID": "{{ config previewOAuthClientID }}",
		}
		if !reflect.DeepEqual(svc.Preview.Env, wantEnv) {
			t.Errorf("Preview.Env = %v, want %v", svc.Preview.Env, wantEnv)
		}
		wantRequired := []string{"APP_API_URL", "APP_IDENTITY_URL", "APP_OAUTH_CLIENT_ID"}
		if !reflect.DeepEqual(svc.Preview.Required, wantRequired) {
			t.Errorf("Preview.Required = %v, want %v", svc.Preview.Required, wantRequired)
		}
		if svc.Preview.Migrate != nil {
			t.Errorf("Preview.Migrate = %v, want nil (dashboard has no migrate step)", svc.Preview.Migrate)
		}
	})

	t.Run("identity: urls and preview (neon + env)", func(t *testing.T) {
		svc, ok := reg["identity"]
		if !ok {
			t.Fatal("identity missing from registry")
		}
		wantURLs := URLs{Staging: "https://identity-staging.tailc06f30.ts.net", Prod: "https://identity.ethanswan.com"}
		if svc.URLs != wantURLs {
			t.Errorf("URLs = %+v, want %+v", svc.URLs, wantURLs)
		}
		if svc.Preview == nil {
			t.Fatal("Preview = nil, want non-nil")
		}
		wantNeon := NeonRef{Project: "plain-heart-27630935", Database: "neondb", Role: "app"}
		if *svc.Preview.Neon != wantNeon {
			t.Errorf("Preview.Neon = %+v, want %+v", *svc.Preview.Neon, wantNeon)
		}
		wantEnv := map[string]string{"JWT_ISSUER": "{{ url self }}"}
		if !reflect.DeepEqual(svc.Preview.Env, wantEnv) {
			t.Errorf("Preview.Env = %v, want %v", svc.Preview.Env, wantEnv)
		}
		// identity gained a `migrate` subcommand (identity#127): the
		// absolute path is deliberate, not a copy-paste slip from
		// footstrike-api's bare "alembic" -- see the comment beside this key
		// in registry.yaml for why identity's image needs it and
		// footstrike-api's doesn't.
		wantMigrate := []string{"/app/auth-service", "migrate"}
		if !reflect.DeepEqual(svc.Preview.Migrate, wantMigrate) {
			t.Errorf("Preview.Migrate = %v, want %v", svc.Preview.Migrate, wantMigrate)
		}
	})

	// Every declared required key must also appear in that service's preview
	// env -- parseRegistry enforces this, but assert it holds for the real
	// file too.
	for name, svc := range reg {
		if svc.Preview == nil {
			continue
		}
		for _, key := range svc.Preview.Required {
			if _, ok := svc.Preview.Env[key]; !ok {
				t.Errorf("%s: required key %q not present in preview env", name, key)
			}
		}
	}
}

func TestRegistryNames_Sorted(t *testing.T) {
	reg := Registry{
		"identity":       {},
		"asset-manager":  {},
		"footstrike-api": {},
	}
	got := reg.Names()
	want := []string{"asset-manager", "footstrike-api", "identity"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
}

func TestRegistryNames_Empty(t *testing.T) {
	reg := Registry{}
	if got := reg.Names(); len(got) != 0 {
		t.Errorf("Names() = %v, want empty", got)
	}
}

func TestPreviewNames(t *testing.T) {
	reg := Registry{
		"asset-manager":  {}, // no preview block
		"bifrost":        {}, // no preview block
		"footstrike-api": {Preview: &Preview{Env: map[string]string{"A": "b"}}},
		"identity":       {Preview: &Preview{Env: map[string]string{"A": "b"}}},
	}
	got := reg.PreviewNames()
	want := []string{"footstrike-api", "identity"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PreviewNames() = %v, want %v", got, want)
	}
}

func TestPreviewNames_Empty(t *testing.T) {
	reg := Registry{"asset-manager": {}, "bifrost": {}}
	if got := reg.PreviewNames(); len(got) != 0 {
		t.Errorf("PreviewNames() = %v, want empty", got)
	}
}

func TestRepoFor(t *testing.T) {
	reg := Registry{
		"asset-manager": {Repo: "asset_manager"},
		"bifrost":       {},
	}

	t.Run("override configured", func(t *testing.T) {
		if got := reg.RepoFor("asset-manager"); got != "asset_manager" {
			t.Errorf("RepoFor(asset-manager) = %q, want asset_manager", got)
		}
	})
	t.Run("no override: identity mapping", func(t *testing.T) {
		if got := reg.RepoFor("bifrost"); got != "bifrost" {
			t.Errorf("RepoFor(bifrost) = %q, want bifrost", got)
		}
	})
	t.Run("service absent from registry: identity mapping", func(t *testing.T) {
		if got := reg.RepoFor("unknown-service"); got != "unknown-service" {
			t.Errorf("RepoFor(unknown-service) = %q, want unknown-service", got)
		}
	})
}

func TestParseRegistry(t *testing.T) {
	t.Run("service with no preview block yields nil", func(t *testing.T) {
		reg, err := parseRegistry([]byte(`
foo:
  urls:
    staging: https://foo-staging.example.com
`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if reg["foo"].Preview != nil {
			t.Errorf("Preview = %+v, want nil", reg["foo"].Preview)
		}
	})

	t.Run("service with no neon in preview yields nil neon", func(t *testing.T) {
		reg, err := parseRegistry([]byte(`
foo:
  preview:
    env:
      A: b
`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if reg["foo"].Preview == nil {
			t.Fatal("Preview = nil, want non-nil")
		}
		if reg["foo"].Preview.Neon != nil {
			t.Errorf("Preview.Neon = %+v, want nil", reg["foo"].Preview.Neon)
		}
		if reg["foo"].Preview.Migrate != nil {
			t.Errorf("Preview.Migrate = %v, want nil (not declared)", reg["foo"].Preview.Migrate)
		}
	})

	t.Run("migrate field parses into an ordered command", func(t *testing.T) {
		reg, err := parseRegistry([]byte(`
foo:
  preview:
    env:
      A: b
    migrate: ["alembic", "upgrade", "head"]
`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"alembic", "upgrade", "head"}
		if !reflect.DeepEqual(reg["foo"].Preview.Migrate, want) {
			t.Errorf("Preview.Migrate = %v, want %v", reg["foo"].Preview.Migrate, want)
		}
	})

	t.Run("malformed YAML errors", func(t *testing.T) {
		_, err := parseRegistry([]byte("not: [valid: yaml"))
		if err == nil {
			t.Fatal("expected an error for malformed YAML, got nil")
		}
	})

	t.Run("unknown top-level field in a service errors", func(t *testing.T) {
		_, err := parseRegistry([]byte(`
foo:
  urls:
    staging: https://foo.example.com
  bogus: true
`))
		if err == nil {
			t.Fatal("expected an error for an unknown field, got nil")
		}
	})

	t.Run("unknown field nested inside preview: errors", func(t *testing.T) {
		_, err := parseRegistry([]byte(`
foo:
  preview:
    env:
      A: b
    bogus: true
`))
		if err == nil {
			t.Fatal("expected an error for an unknown field nested inside preview:, got nil")
		}
	})

	t.Run("unknown field nested inside urls: errors", func(t *testing.T) {
		_, err := parseRegistry([]byte(`
foo:
  urls:
    staging: https://foo.example.com
    bogus: true
`))
		if err == nil {
			t.Fatal("expected an error for an unknown field nested inside urls:, got nil")
		}
	})

	t.Run("required key missing from preview env errors", func(t *testing.T) {
		_, err := parseRegistry([]byte(`
foo:
  preview:
    env:
      A: b
    required: [A, MISSING]
`))
		if err == nil {
			t.Fatal("expected an error for a required key absent from env, got nil")
		}
	})

	t.Run("valid minimal registry parses", func(t *testing.T) {
		reg, err := parseRegistry([]byte(`
foo:
  preview:
    env:
      A: b
    required: [A]
`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(reg) != 1 {
			t.Fatalf("len(reg) = %d, want 1", len(reg))
		}
	})
}
