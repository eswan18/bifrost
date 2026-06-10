package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func testOIDC() *OIDC {
	return &OIDC{
		OAuth2: &oauth2.Config{
			ClientID:    "bifrost",
			RedirectURL: "https://bifrost.example/auth/callback",
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://idp.example/authorize",
				TokenURL: "https://idp.example/token",
			},
			Scopes: []string{"openid", "email", "profile"},
		},
	}
}

// Login must send a PKCE S256 challenge (identity rejects the authorize
// request without it) and persist the verifier for the callback.
func TestLoginSendsPKCEChallenge(t *testing.T) {
	h := &Handlers{OIDC: testOIDC()}
	rec := httptest.NewRecorder()
	h.Login(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	q := loc.Query()
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", got)
	}
	if q.Get("code_challenge") == "" {
		t.Error("code_challenge is missing")
	}

	cookies := map[string]string{}
	for _, c := range rec.Result().Cookies() {
		cookies[c.Name] = c.Value
	}
	for _, name := range []string{stateCookie, nonceCookie, verifierCookie} {
		if cookies[name] == "" {
			t.Errorf("cookie %q not set", name)
		}
	}
}

// A token exchange must carry the PKCE verifier so identity's token endpoint
// accepts the authorization_code grant.
func TestLoginVerifierMatchesChallenge(t *testing.T) {
	h := &Handlers{OIDC: testOIDC()}
	rec := httptest.NewRecorder()
	h.Login(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))

	loc, _ := url.Parse(rec.Header().Get("Location"))
	challenge := loc.Query().Get("code_challenge")

	var verifier string
	for _, c := range rec.Result().Cookies() {
		if c.Name == verifierCookie {
			verifier = c.Value
		}
	}
	if verifier == "" {
		t.Fatal("verifier cookie not set")
	}
	if got := oauth2.S256ChallengeFromVerifier(verifier); got != challenge {
		t.Errorf("challenge %q does not match S256(verifier) %q", challenge, got)
	}
}

// When the IdP redirects back with ?error=..., the callback must render a
// user-facing error (not a 502) and must not attempt a token exchange.
func TestCallbackRendersIdPError(t *testing.T) {
	var gotStatus int
	var gotMsg string
	h := &Handlers{
		OIDC: testOIDC(),
		RenderError: func(w http.ResponseWriter, status int, message string) {
			gotStatus, gotMsg = status, message
			w.WriteHeader(status)
		},
	}

	desc := "Code challenge and code challenge method are required"
	target := "/auth/callback?error=invalid_request&error_description=" +
		url.QueryEscape(desc) + "&state=abc"
	rec := httptest.NewRecorder()
	h.Callback(rec, httptest.NewRequest(http.MethodGet, target, nil))

	if gotStatus != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (Cloudflare masks 5xx)", gotStatus, http.StatusBadRequest)
	}
	if !strings.Contains(gotMsg, desc) {
		t.Errorf("message = %q, want it to include the IdP error_description", gotMsg)
	}
}

// Without an injected renderer, errors still degrade to plain http.Error
// rather than panicking.
func TestCallbackErrorFallsBackToPlainError(t *testing.T) {
	h := &Handlers{OIDC: testOIDC()} // no RenderError
	rec := httptest.NewRecorder()
	h.Callback(rec, httptest.NewRequest(http.MethodGet,
		"/auth/callback?error=access_denied", nil))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "access_denied") {
		t.Errorf("body = %q, want it to mention the error", rec.Body.String())
	}
}
