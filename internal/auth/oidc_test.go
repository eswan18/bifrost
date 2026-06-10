package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newDiscoveryServer plays the in-cluster identity service. Like the real
// thing, it serves a discovery document that advertises the EXTERNAL issuer
// and external endpoints — identity builds the doc from its configured
// JWT_ISSUER, so the internal hostname returns the same doc.
func newDiscoveryServer(t *testing.T, issuerExternal string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]any{
			"issuer":                                issuerExternal,
			"authorization_endpoint":                issuerExternal + "/oauth/authorize",
			"token_endpoint":                        issuerExternal + "/oauth/token",
			"userinfo_endpoint":                     issuerExternal + "/oauth/userinfo",
			"jwks_uri":                              issuerExternal + "/.well-known/jwks.json",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"ES256"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})
	return srv
}

func TestNewOIDCFetchesFromInternalIssuer(t *testing.T) {
	// The external issuer is deliberately unresolvable: in-cluster pods can't
	// reach external hostnames, and neither can this test. NewOIDC must only
	// ever talk to the internal server.
	issuerExternal := "https://identity.invalid"
	srv := newDiscoveryServer(t, issuerExternal)
	issuerInternal := srv.URL

	o, err := NewOIDC(context.Background(), issuerExternal, issuerInternal,
		"cid", "csecret", "https://bifrost.invalid/auth/callback")
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}

	// Server-to-server endpoints must be rewritten to the internal URL.
	if got, want := o.OAuth2.Endpoint.TokenURL, issuerInternal+"/oauth/token"; got != want {
		t.Errorf("TokenURL = %q, want %q", got, want)
	}
	if got, want := o.Provider.UserInfoEndpoint(), issuerInternal+"/oauth/userinfo"; got != want {
		t.Errorf("UserInfoEndpoint = %q, want %q", got, want)
	}
	var claims struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := o.Provider.Claims(&claims); err != nil {
		t.Fatalf("Claims: %v", err)
	}
	if got, want := claims.JWKSURI, issuerInternal+"/.well-known/jwks.json"; got != want {
		t.Errorf("jwks_uri = %q, want %q", got, want)
	}

	// The browser hits the authorization endpoint directly — must stay external.
	if got, want := o.OAuth2.Endpoint.AuthURL, issuerExternal+"/oauth/authorize"; got != want {
		t.Errorf("AuthURL = %q, want %q", got, want)
	}
}
