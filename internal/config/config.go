package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	HTTPAddress        string
	BaseURL            string
	Env                string // "staging" | "prod"
	AllowedEmail       string
	OIDCIssuerExternal string // browser-facing
	OIDCIssuerInternal string // server-to-server
	OIDCClientID       string
	OIDCClientSecret   string
	SessionSecret      []byte
	ArgoCDNamespace    string
	// GitHubOrg is the single org every fleet repo lives under; per-service
	// repo names (when they differ from the service name) live in the
	// registry now (see internal/registry.Registry.RepoFor), not here.
	GitHubOrg  string
	GCPProject string // for Cloud Build status; "" disables it
	// DisplayLocation renders timestamps ("today 09:58", next job runs) in the
	// operator's local time; the cluster and cron schedules run in UTC.
	DisplayLocation *time.Location
	// Preview control plane (all optional; empty disables preview features).
	// Which services are previewable, and their Neon references, live in the
	// registry (see internal/registry/registry.go), not here — these are
	// just the secrets/tokens the orchestrator needs to act on it.
	GitHubToken     string // PAT for branch lookups on private repos
	NeonAPIKey      string // Neon API key for branch create/delete
	PreviewAPIToken string // static bearer token for the preview API (CLI use)
	// PreviewOAuthClientID is the OAuth client the preview dashboard's
	// APP_OAUTH_CLIENT_ID resolves to (the wildcard-registered staging
	// dashboard client — see the 3c orchestration plan). Optional; empty
	// fails dashboard preview creation rather than disabling anything at
	// load time.
	PreviewOAuthClientID string
}

func Load() (*Config, error) {
	m := map[string]string{}
	for _, k := range []string{
		"HTTP_ADDRESS", "BASE_URL", "ENV", "ALLOWED_EMAIL",
		"OIDC_ISSUER_EXTERNAL", "OIDC_ISSUER_INTERNAL",
		"OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET",
		"SESSION_SECRET", "ARGOCD_NAMESPACE",
		"GITHUB_ORG", "GCP_PROJECT", "DISPLAY_TIMEZONE",
		"GITHUB_TOKEN", "NEON_API_KEY", "PREVIEW_API_TOKEN",
		"PREVIEW_OAUTH_CLIENT_ID",
	} {
		m[k] = os.Getenv(k)
	}
	return loadFromMap(m)
}

func loadFromMap(m map[string]string) (*Config, error) {
	required := []string{
		"BASE_URL", "ENV", "ALLOWED_EMAIL",
		"OIDC_ISSUER_EXTERNAL", "OIDC_ISSUER_INTERNAL",
		"OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "SESSION_SECRET",
	}
	var missing []string
	for _, k := range required {
		if m[k] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	if len(m["SESSION_SECRET"]) < 32 {
		return nil, fmt.Errorf("SESSION_SECRET must be at least 32 bytes")
	}

	addr := m["HTTP_ADDRESS"]
	if addr == "" {
		addr = ":8080"
	}
	ns := m["ARGOCD_NAMESPACE"]
	if ns == "" {
		ns = "argocd"
	}

	org := m["GITHUB_ORG"]
	if org == "" {
		org = "eswan18"
	}

	tz := m["DISPLAY_TIMEZONE"]
	if tz == "" {
		tz = "America/New_York"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("DISPLAY_TIMEZONE %q: %w", tz, err)
	}

	return &Config{
		HTTPAddress:          addr,
		BaseURL:              strings.TrimRight(m["BASE_URL"], "/"),
		Env:                  m["ENV"],
		AllowedEmail:         m["ALLOWED_EMAIL"],
		OIDCIssuerExternal:   strings.TrimRight(m["OIDC_ISSUER_EXTERNAL"], "/"),
		OIDCIssuerInternal:   strings.TrimRight(m["OIDC_ISSUER_INTERNAL"], "/"),
		OIDCClientID:         m["OIDC_CLIENT_ID"],
		OIDCClientSecret:     m["OIDC_CLIENT_SECRET"],
		SessionSecret:        []byte(m["SESSION_SECRET"]),
		ArgoCDNamespace:      ns,
		GitHubOrg:            org,
		GCPProject:           m["GCP_PROJECT"],
		DisplayLocation:      loc,
		GitHubToken:          m["GITHUB_TOKEN"],
		NeonAPIKey:           m["NEON_API_KEY"],
		PreviewAPIToken:      m["PREVIEW_API_TOKEN"],
		PreviewOAuthClientID: m["PREVIEW_OAUTH_CLIENT_ID"],
	}, nil
}
