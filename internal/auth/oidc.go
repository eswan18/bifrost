package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDC holds the configured provider + verifier + oauth2 config. Use NewOIDC
// to build it (handles the discovery-doc URL rewrite).
type OIDC struct {
	Provider *oidc.Provider
	Verifier *oidc.IDTokenVerifier
	OAuth2   *oauth2.Config
}

// NewOIDC fetches the discovery doc from issuerInternal (in-cluster DNS —
// pods can't resolve the external hostname; identity serves the same doc on
// both, built from its configured JWT_ISSUER), rewrites the
// token/userinfo/jwks endpoints to point at issuerInternal, then builds an
// oidc.Provider from the rewritten doc.
//
// authorizationEndpoint and end_session_endpoint are left as-is because
// the browser hits those directly.
func NewOIDC(ctx context.Context, issuerExternal, issuerInternal, clientID, clientSecret, redirectURL string) (*OIDC, error) {
	doc, err := fetchDiscoveryDoc(ctx, issuerInternal)
	if err != nil {
		return nil, fmt.Errorf("fetch discovery doc: %w", err)
	}
	rewriteToInternal(doc, issuerExternal, issuerInternal,
		"token_endpoint", "userinfo_endpoint", "jwks_uri")

	provider, err := buildProviderFromDoc(ctx, doc)
	if err != nil {
		return nil, fmt.Errorf("build provider: %w", err)
	}
	return &OIDC{
		Provider: provider,
		Verifier: provider.Verifier(&oidc.Config{ClientID: clientID}),
		OAuth2: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  redirectURL,
			Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		},
	}, nil
}

// httpTimeout bounds every outbound call to identity (discovery fetch,
// token exchange via the provider client, JWKS) so a hung endpoint can't
// wedge requests forever.
const httpTimeout = 10 * time.Second

func fetchDiscoveryDoc(ctx context.Context, issuer string) (map[string]any, error) {
	url := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("discovery returned %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func rewriteToInternal(doc map[string]any, ext, internal string, fields ...string) {
	for _, f := range fields {
		v, ok := doc[f].(string)
		if !ok {
			continue
		}
		doc[f] = strings.Replace(v, ext, internal, 1)
	}
}

func buildProviderFromDoc(ctx context.Context, doc map[string]any) (*oidc.Provider, error) {
	// go-oidc 3.x exposes NewProvider only via discovery — to use a custom
	// doc we re-serve it from an in-process server and point go-oidc at that.
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	issuer, _ := doc["issuer"].(string)
	// Provide the rewritten doc to go-oidc via a context-bound HTTP client
	// that returns it when go-oidc fetches /.well-known/openid-configuration.
	client := &http.Client{
		Transport: &docTransport{issuer: issuer, body: b},
		Timeout:   httpTimeout,
	}
	ctx = oidc.ClientContext(ctx, client)
	return oidc.NewProvider(ctx, issuer)
}

type docTransport struct {
	issuer string
	body   []byte
}

func (d *docTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/.well-known/openid-configuration") {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(d.body))),
		}, nil
	}
	return http.DefaultTransport.RoundTrip(req)
}
