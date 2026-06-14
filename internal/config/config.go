package config

import (
	"fmt"
	"os"
	"strings"
)

type Config struct {
	HTTPAddress        string
	BaseURL            string
	Env                string // "staging" | "prod"
	Services           []string
	AllowedEmail       string
	OIDCIssuerExternal string // browser-facing
	OIDCIssuerInternal string // server-to-server
	OIDCClientID       string
	OIDCClientSecret   string
	SessionSecret      []byte
	ArgoCDNamespace    string
	GitHubOrg          string
	RepoOverrides      map[string]string // service name → repo name, when they differ
	StagingURLs        map[string]string // service name → public staging URL, for "open" links
	ProdURLs           map[string]string // service name → public prod URL, for "open" links
	GCPProject         string            // for Cloud Build status; "" disables it
}

// RepoFor returns the GitHub repo name for a service. Most repos are named
// after the service; REPO_OVERRIDES covers the exceptions
// (e.g. asset-manager → asset_manager).
func (c *Config) RepoFor(svc string) string {
	if repo, ok := c.RepoOverrides[svc]; ok {
		return repo
	}
	return svc
}

func Load() (*Config, error) {
	m := map[string]string{}
	for _, k := range []string{
		"HTTP_ADDRESS", "BASE_URL", "ENV", "SERVICES", "ALLOWED_EMAIL",
		"OIDC_ISSUER_EXTERNAL", "OIDC_ISSUER_INTERNAL",
		"OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET",
		"SESSION_SECRET", "ARGOCD_NAMESPACE",
		"GITHUB_ORG", "REPO_OVERRIDES", "GCP_PROJECT",
		"STAGING_URLS", "PROD_URLS",
	} {
		m[k] = os.Getenv(k)
	}
	return loadFromMap(m)
}

func loadFromMap(m map[string]string) (*Config, error) {
	required := []string{
		"BASE_URL", "ENV", "SERVICES", "ALLOWED_EMAIL",
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

	// Skip empty entries so a trailing comma doesn't yield a service named ""
	// (which would query namespace "-staging").
	var svcs []string
	for _, s := range strings.Split(m["SERVICES"], ",") {
		if s = strings.TrimSpace(s); s != "" {
			svcs = append(svcs, s)
		}
	}
	if len(svcs) == 0 {
		return nil, fmt.Errorf("SERVICES contains no service names")
	}

	org := m["GITHUB_ORG"]
	if org == "" {
		org = "eswan18"
	}
	overrides, err := parsePairs(m["REPO_OVERRIDES"], "REPO_OVERRIDES")
	if err != nil {
		return nil, err
	}
	stagingURLs, err := parsePairs(m["STAGING_URLS"], "STAGING_URLS")
	if err != nil {
		return nil, err
	}
	prodURLs, err := parsePairs(m["PROD_URLS"], "PROD_URLS")
	if err != nil {
		return nil, err
	}

	return &Config{
		HTTPAddress:        addr,
		BaseURL:            strings.TrimRight(m["BASE_URL"], "/"),
		Env:                m["ENV"],
		Services:           svcs,
		AllowedEmail:       m["ALLOWED_EMAIL"],
		OIDCIssuerExternal: strings.TrimRight(m["OIDC_ISSUER_EXTERNAL"], "/"),
		OIDCIssuerInternal: strings.TrimRight(m["OIDC_ISSUER_INTERNAL"], "/"),
		OIDCClientID:       m["OIDC_CLIENT_ID"],
		OIDCClientSecret:   m["OIDC_CLIENT_SECRET"],
		SessionSecret:      []byte(m["SESSION_SECRET"]),
		ArgoCDNamespace:    ns,
		GitHubOrg:          org,
		RepoOverrides:      overrides,
		StagingURLs:        stagingURLs,
		ProdURLs:           prodURLs,
		GCPProject:         m["GCP_PROJECT"],
	}, nil
}

// parsePairs parses a comma-separated list of "key=value" pairs, as used by
// REPO_OVERRIDES and the per-env URL maps. Empty entries are skipped so a
// trailing comma is harmless; label names the source var for error messages.
func parsePairs(s, label string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		if pair = strings.TrimSpace(pair); pair == "" {
			continue
		}
		key, val, ok := strings.Cut(pair, "=")
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if !ok || key == "" || val == "" {
			return nil, fmt.Errorf("%s entry %q is not key=value", label, pair)
		}
		out[key] = val
	}
	return out, nil
}
