package config

import (
	"testing"
)

func TestLoadFromEnv(t *testing.T) {
	env := map[string]string{
		"HTTP_ADDRESS":         ":9090",
		"BASE_URL":             "https://bifrost.example.com",
		"ENV":                  "staging",
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
	if cfg.HTTPAddress != ":9090" {
		t.Errorf("HTTPAddress = %q", cfg.HTTPAddress)
	}
	if cfg.BaseURL != "https://bifrost.example.com" {
		t.Errorf("BaseURL = %q", cfg.BaseURL)
	}
	if cfg.ArgoCDNamespace != "argocd" {
		t.Errorf("ArgoCDNamespace = %q", cfg.ArgoCDNamespace)
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

func TestGitHubOrgDefault(t *testing.T) {
	cfg, err := loadFromMap(minimalValidEnv())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cfg.GitHubOrg != "eswan18" {
		t.Errorf("GitHubOrg = %q, want eswan18", cfg.GitHubOrg)
	}
}

func TestGitHubOrgOverride(t *testing.T) {
	env := minimalValidEnv()
	env["GITHUB_ORG"] = "acme"
	cfg, err := loadFromMap(env)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cfg.GitHubOrg != "acme" {
		t.Errorf("GitHubOrg = %q, want acme", cfg.GitHubOrg)
	}
}

func TestPreviewConfigDefaults(t *testing.T) {
	cfg, err := loadFromMap(minimalValidEnv())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GitHubToken != "" || cfg.NeonAPIKey != "" || cfg.PreviewAPIToken != "" {
		t.Errorf("expected empty tokens by default")
	}
	if cfg.PreviewOAuthClientID != "" {
		t.Errorf("expected empty PreviewOAuthClientID by default")
	}
}

func TestPreviewOAuthClientIDParsed(t *testing.T) {
	m := minimalValidEnv()
	m["PREVIEW_OAUTH_CLIENT_ID"] = "preview-client-id"
	cfg, err := loadFromMap(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PreviewOAuthClientID != "preview-client-id" {
		t.Errorf("PreviewOAuthClientID = %q, want preview-client-id", cfg.PreviewOAuthClientID)
	}
}

func minimalValidEnv() map[string]string {
	return map[string]string{
		"BASE_URL":             "https://b",
		"ENV":                  "staging",
		"ALLOWED_EMAIL":        "me@x",
		"OIDC_ISSUER_EXTERNAL": "https://i",
		"OIDC_ISSUER_INTERNAL": "http://i",
		"OIDC_CLIENT_ID":       "id",
		"OIDC_CLIENT_SECRET":   "s",
		"SESSION_SECRET":       "12345678901234567890123456789012",
	}
}
