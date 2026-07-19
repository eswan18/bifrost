package config

import (
	"reflect"
	"testing"
)

func TestLoadFromEnv(t *testing.T) {
	env := map[string]string{
		"HTTP_ADDRESS":         ":9090",
		"BASE_URL":             "https://bifrost.example.com",
		"ENV":                  "staging",
		"SERVICES":             "asset-manager,comms,bifrost",
		"ALLOWED_EMAIL":        "me@example.com",
		"OIDC_ISSUER_EXTERNAL": "https://identity.example.com",
		"OIDC_ISSUER_INTERNAL": "http://identity.identity-staging.svc.cluster.local",
		"OIDC_CLIENT_ID":       "cid",
		"OIDC_CLIENT_SECRET":   "csecret",
		"SESSION_SECRET":       "12345678901234567890123456789012",
		"ARGOCD_NAMESPACE":     "argocd",
	}
	cfg, err := loadFromMap(env)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := []string{"asset-manager", "comms", "bifrost"}
	if !reflect.DeepEqual(cfg.Services, want) {
		t.Errorf("Services = %v, want %v", cfg.Services, want)
	}
	if cfg.HTTPAddress != ":9090" {
		t.Errorf("HTTPAddress = %q", cfg.HTTPAddress)
	}
}

func TestLoadSkipsEmptyServiceEntries(t *testing.T) {
	env := minimalValidEnv()
	env["SERVICES"] = " footstrike-api, ,identity,"
	cfg, err := loadFromMap(env)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := []string{"footstrike-api", "identity"}
	if !reflect.DeepEqual(cfg.Services, want) {
		t.Errorf("Services = %v, want %v", cfg.Services, want)
	}
}

func TestLoadRejectsServicesWithNoNames(t *testing.T) {
	env := minimalValidEnv()
	env["SERVICES"] = " , ,"
	if _, err := loadFromMap(env); err == nil {
		t.Fatal("expected error for SERVICES with no service names")
	}
}

func TestLoadMissingRequired(t *testing.T) {
	_, err := loadFromMap(map[string]string{"HTTP_ADDRESS": ":8080"})
	if err == nil {
		t.Fatal("expected error for missing required vars")
	}
}

func TestLoadSessionSecretTooShort(t *testing.T) {
	env := minimalValidEnv()
	env["SESSION_SECRET"] = "short"
	if _, err := loadFromMap(env); err == nil {
		t.Fatal("expected error for short SESSION_SECRET")
	}
}

func TestRepoOverrides(t *testing.T) {
	env := minimalValidEnv()
	env["REPO_OVERRIDES"] = "asset-manager=asset_manager, foo = bar ,"
	cfg, err := loadFromMap(env)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := cfg.RepoFor("asset-manager"); got != "asset_manager" {
		t.Errorf("RepoFor(asset-manager) = %q, want asset_manager", got)
	}
	if got := cfg.RepoFor("foo"); got != "bar" {
		t.Errorf("RepoFor(foo) = %q, want bar", got)
	}
	if got := cfg.RepoFor("identity"); got != "identity" {
		t.Errorf("RepoFor(identity) = %q, want identity (default)", got)
	}
}

func TestRepoOverridesMalformed(t *testing.T) {
	for _, bad := range []string{"asset-manager", "=repo", "svc="} {
		env := minimalValidEnv()
		env["REPO_OVERRIDES"] = bad
		if _, err := loadFromMap(env); err == nil {
			t.Errorf("REPO_OVERRIDES=%q: expected error", bad)
		}
	}
}

func TestPerEnvURLs(t *testing.T) {
	env := minimalValidEnv()
	env["STAGING_URLS"] = "identity=https://identity-staging.example.net, foo = https://foo ,"
	env["PROD_URLS"] = "identity=https://identity.example.com"
	cfg, err := loadFromMap(env)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := cfg.StagingURLs["identity"]; got != "https://identity-staging.example.net" {
		t.Errorf("StagingURLs[identity] = %q", got)
	}
	if got := cfg.StagingURLs["foo"]; got != "https://foo" {
		t.Errorf("StagingURLs[foo] = %q (whitespace not trimmed)", got)
	}
	if got := cfg.ProdURLs["identity"]; got != "https://identity.example.com" {
		t.Errorf("ProdURLs[identity] = %q", got)
	}
	// A service with no URL configured maps to "" (no link rendered).
	if got := cfg.ProdURLs["comms"]; got != "" {
		t.Errorf("ProdURLs[comms] = %q, want empty", got)
	}
}

func TestPerEnvURLsMalformed(t *testing.T) {
	for _, bad := range []string{"identity", "=https://x", "identity="} {
		env := minimalValidEnv()
		env["PROD_URLS"] = bad
		if _, err := loadFromMap(env); err == nil {
			t.Errorf("PROD_URLS=%q: expected error", bad)
		}
	}
}

func TestGitHubOrgDefault(t *testing.T) {
	cfg, err := loadFromMap(minimalValidEnv())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cfg.GitHubOrg != "eswan18" {
		t.Errorf("GitHubOrg = %q, want eswan18", cfg.GitHubOrg)
	}
}

func minimalValidEnv() map[string]string {
	return map[string]string{
		"BASE_URL":             "https://b",
		"ENV":                  "staging",
		"SERVICES":             "a,b",
		"ALLOWED_EMAIL":        "me@x",
		"OIDC_ISSUER_EXTERNAL": "https://i",
		"OIDC_ISSUER_INTERNAL": "http://i",
		"OIDC_CLIENT_ID":       "id",
		"OIDC_CLIENT_SECRET":   "s",
		"SESSION_SECRET":       "12345678901234567890123456789012",
	}
}
