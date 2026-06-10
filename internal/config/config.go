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
}

func Load() (*Config, error) {
	m := map[string]string{}
	for _, k := range []string{
		"HTTP_ADDRESS", "BASE_URL", "ENV", "SERVICES", "ALLOWED_EMAIL",
		"OIDC_ISSUER_EXTERNAL", "OIDC_ISSUER_INTERNAL",
		"OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET",
		"SESSION_SECRET", "ARGOCD_NAMESPACE",
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
	}, nil
}
