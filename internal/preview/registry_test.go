package preview

import (
	"reflect"
	"testing"
)

func TestLoadRegistry(t *testing.T) {
	reg, err := LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry() error = %v", err)
	}

	wantNames := []string{"footstrike-api", "footstrike-dashboard", "identity"}
	if got := reg.Names(); !reflect.DeepEqual(got, wantNames) {
		t.Errorf("Names() = %v, want %v", got, wantNames)
	}

	api, ok := reg["footstrike-api"]
	if !ok {
		t.Fatal("footstrike-api missing from registry")
	}
	if api.Neon == nil {
		t.Fatal("footstrike-api.Neon = nil, want non-nil")
	}
	wantAPINeon := NeonRef{Project: "aged-river-81935268", Database: "neondb", Role: "neondb_owner"}
	if *api.Neon != wantAPINeon {
		t.Errorf("footstrike-api.Neon = %+v, want %+v", *api.Neon, wantAPINeon)
	}
	wantAPIEnv := map[string]string{
		"ENV":                       "staging",
		"PUBLIC_API_BASE_URL":       "{{ url self }}",
		"PUBLIC_DASHBOARD_BASE_URL": "{{ url footstrike-dashboard }}",
		"IDENTITY_PROVIDER_URL":     "{{ internalUrl identity }}",
		"JWT_ISSUER":                "{{ url identity }}",
	}
	if !reflect.DeepEqual(api.Env, wantAPIEnv) {
		t.Errorf("footstrike-api.Env = %v, want %v", api.Env, wantAPIEnv)
	}
	if len(api.Required) != 0 {
		t.Errorf("footstrike-api.Required = %v, want empty", api.Required)
	}

	dash, ok := reg["footstrike-dashboard"]
	if !ok {
		t.Fatal("footstrike-dashboard missing from registry")
	}
	if dash.Neon != nil {
		t.Errorf("footstrike-dashboard.Neon = %+v, want nil (dashboard has no database)", dash.Neon)
	}
	wantDashEnv := map[string]string{
		"APP_API_URL":         "{{ url footstrike-api }}",
		"APP_IDENTITY_URL":    "{{ url identity }}",
		"APP_OAUTH_CLIENT_ID": "{{ config previewOAuthClientID }}",
	}
	if !reflect.DeepEqual(dash.Env, wantDashEnv) {
		t.Errorf("footstrike-dashboard.Env = %v, want %v", dash.Env, wantDashEnv)
	}
	wantDashRequired := []string{"APP_API_URL", "APP_IDENTITY_URL", "APP_OAUTH_CLIENT_ID"}
	if !reflect.DeepEqual(dash.Required, wantDashRequired) {
		t.Errorf("footstrike-dashboard.Required = %v, want %v", dash.Required, wantDashRequired)
	}

	identity, ok := reg["identity"]
	if !ok {
		t.Fatal("identity missing from registry")
	}
	if identity.Neon == nil {
		t.Fatal("identity.Neon = nil, want non-nil")
	}
	wantIdentityNeon := NeonRef{Project: "plain-heart-27630935", Database: "neondb", Role: "app"}
	if *identity.Neon != wantIdentityNeon {
		t.Errorf("identity.Neon = %+v, want %+v", *identity.Neon, wantIdentityNeon)
	}
	wantIdentityEnv := map[string]string{"JWT_ISSUER": "{{ url self }}"}
	if !reflect.DeepEqual(identity.Env, wantIdentityEnv) {
		t.Errorf("identity.Env = %v, want %v", identity.Env, wantIdentityEnv)
	}

	// Every declared required key must also appear in that service's env —
	// LoadRegistry enforces this, but assert it holds for the real file too.
	for name, svc := range reg {
		for _, key := range svc.Required {
			if _, ok := svc.Env[key]; !ok {
				t.Errorf("%s: required key %q not present in env", name, key)
			}
		}
	}
}

func TestRegistryNames_Sorted(t *testing.T) {
	reg := Registry{
		"identity":             {},
		"footstrike-api":       {},
		"footstrike-dashboard": {},
	}
	got := reg.Names()
	want := []string{"footstrike-api", "footstrike-dashboard", "identity"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
}

func TestRegistryNames_Empty(t *testing.T) {
	reg := Registry{}
	got := reg.Names()
	if len(got) != 0 {
		t.Errorf("Names() = %v, want empty", got)
	}
}

func TestParseRegistry(t *testing.T) {
	t.Run("no neon yields nil", func(t *testing.T) {
		reg, err := parseRegistry([]byte(`
foo:
  env:
    A: b
`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if reg["foo"].Neon != nil {
			t.Errorf("Neon = %+v, want nil", reg["foo"].Neon)
		}
	})

	t.Run("malformed YAML errors", func(t *testing.T) {
		_, err := parseRegistry([]byte("not: [valid: yaml"))
		if err == nil {
			t.Fatal("expected an error for malformed YAML, got nil")
		}
	})

	t.Run("unknown field in a service errors", func(t *testing.T) {
		_, err := parseRegistry([]byte(`
foo:
  env:
    A: b
  bogus: true
`))
		if err == nil {
			t.Fatal("expected an error for an unknown field, got nil")
		}
	})

	t.Run("required key missing from env errors", func(t *testing.T) {
		_, err := parseRegistry([]byte(`
foo:
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
