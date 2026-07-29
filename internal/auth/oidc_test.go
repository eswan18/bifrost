package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// testRetry is a retryConfig with the same shape (attempts, doubling
// backoff up to a cap) as defaultDiscoveryRetry but scaled down to
// milliseconds, so retry tests exercise the real backoff/give-up logic
// without sleeping for anywhere near the real ~46s startup budget.
var testRetry = retryConfig{
	attempts:  5,
	baseDelay: 2 * time.Millisecond,
	maxDelay:  10 * time.Millisecond,
}

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

// newFlakyDiscoveryServer answers the first failUntil requests to the
// discovery endpoint with failStatus, then behaves like newDiscoveryServer.
// It mimics identity mid co-restart: refusing/500ing briefly before it's
// ready, exactly the case the retry loop exists for.
func newFlakyDiscoveryServer(t *testing.T, issuerExternal string, failUntil int, failStatus int) (*httptest.Server, *int32) {
	t.Helper()
	var requests int32
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requests, 1)
		if int(n) <= failUntil {
			w.WriteHeader(failStatus)
			return
		}
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
	return srv, &requests
}

// TestNewOIDCRetriesTransientFailures covers the actual incident: identity
// answering 500 (stand-in for connection-refused/timeout, which httptest
// can't easily simulate against an already-listening server) for the first
// few attempts, as it would while still starting up after a co-restart,
// then succeeding once it's ready.
func TestNewOIDCRetriesTransientFailures(t *testing.T) {
	issuerExternal := "https://identity.invalid"
	srv, requests := newFlakyDiscoveryServer(t, issuerExternal, 2, http.StatusInternalServerError)

	o, err := newOIDC(context.Background(), issuerExternal, srv.URL,
		"cid", "csecret", "https://bifrost.invalid/auth/callback", testRetry)
	if err != nil {
		t.Fatalf("newOIDC: %v", err)
	}
	if o == nil {
		t.Fatal("newOIDC: got nil OIDC with no error")
	}
	if got, want := atomic.LoadInt32(requests), int32(3); got != want {
		t.Errorf("requests = %d, want %d (2 failures + 1 success)", got, want)
	}
}

// TestNewOIDCFailsFastOnClientError covers the "bad OIDC_ISSUER_INTERNAL"
// case: a 404 means identity is up and answering deliberately, so it's a
// real misconfiguration, not a transient blip. NewOIDC must report it
// immediately rather than working through the whole retry/backoff budget.
func TestNewOIDCFailsFastOnClientError(t *testing.T) {
	var requests int32
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusNotFound)
	})

	start := time.Now()
	_, err := newOIDC(context.Background(), "https://identity.invalid", srv.URL,
		"cid", "csecret", "https://bifrost.invalid/auth/callback", testRetry)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("newOIDC: got nil error, want failure on 404")
	}
	if got, want := atomic.LoadInt32(&requests), int32(1); got != want {
		t.Errorf("requests = %d, want %d (must not exhaust the retry budget on a permanent error)", got, want)
	}
	// A generous ceiling: well under even one retry delay (testRetry's
	// baseDelay), let alone the full 5-attempt budget.
	if elapsed > time.Second {
		t.Errorf("newOIDC took %s to fail fast on a 404", elapsed)
	}
}

// TestNewOIDCContextCancellationMidRetry ensures a canceled/expired context
// interrupts the retry loop's sleep instead of running out the backoff
// schedule — startup shouldn't be able to outlive its caller's context.
func TestNewOIDCContextCancellationMidRetry(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	// A generous retry budget on paper (10 attempts, up to ~a couple of
	// seconds of backoff) that context cancellation should cut short long
	// before it naturally exhausts.
	longRetry := retryConfig{attempts: 10, baseDelay: 200 * time.Millisecond, maxDelay: 500 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := newOIDC(ctx, "https://identity.invalid", srv.URL,
		"cid", "csecret", "https://bifrost.invalid/auth/callback", longRetry)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("newOIDC: got nil error, want context deadline failure")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("newOIDC error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	// Must return promptly once the context expires, not after the full
	// backoff schedule (which alone would take ~1.8s+ here).
	if elapsed > 500*time.Millisecond {
		t.Errorf("newOIDC took %s to return after context cancellation, want well under 500ms", elapsed)
	}
}
