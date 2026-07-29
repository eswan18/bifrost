package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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
	return newOIDC(ctx, issuerExternal, issuerInternal, clientID, clientSecret, redirectURL, defaultDiscoveryRetry)
}

// newOIDC is NewOIDC with the retry schedule as a parameter so tests can
// swap in a short one instead of waiting out the real startup budget.
func newOIDC(ctx context.Context, issuerExternal, issuerInternal, clientID, clientSecret, redirectURL string, retry retryConfig) (*OIDC, error) {
	doc, err := fetchDiscoveryDocWithRetry(ctx, issuerInternal, retry)
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

// permanentErr marks a discovery-doc failure that retrying won't fix: the
// server answered deliberately (a 4xx — most likely a bad
// OIDC_ISSUER_INTERNAL, or a route identity doesn't serve) or answered with
// a body that isn't the JSON we expect. fetchDiscoveryDocWithRetry stops on
// sight of one instead of burning its attempt budget on a real
// misconfiguration.
type permanentErr struct{ err error }

func (p *permanentErr) Error() string { return p.err.Error() }
func (p *permanentErr) Unwrap() error { return p.err }

func fetchDiscoveryDoc(ctx context.Context, issuer string) (map[string]any, error) {
	url := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		// Connection refused, DNS not resolving yet, or a request timeout —
		// exactly the transient co-restart case this retries: identity's
		// Service may simply have no ready endpoint yet.
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		err := fmt.Errorf("discovery returned %d", resp.StatusCode)
		if resp.StatusCode >= 500 {
			// identity is up but erroring — treat like any other transient
			// failure and let the caller retry.
			return nil, err
		}
		// A non-5xx, non-200 response (typically 4xx) means something
		// answered on purpose. Retrying a misconfigured issuer URL just
		// delays reporting it.
		return nil, &permanentErr{err}
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		// Malformed body: identity responded but the discovery doc itself is
		// broken. Also not something a retry will fix.
		return nil, &permanentErr{err}
	}
	return m, nil
}

// retryConfig is an exponential backoff schedule for the discovery-doc
// retry loop: attempts total tries, with delay(n) sleep after each failed
// attempt n (except the last).
type retryConfig struct {
	attempts  int
	baseDelay time.Duration
	maxDelay  time.Duration
}

// delay returns how long to sleep after the given 1-indexed attempt before
// trying again: baseDelay doubled each attempt, capped at maxDelay.
func (c retryConfig) delay(attempt int) time.Duration {
	d := c.baseDelay * time.Duration(uint64(1)<<uint(attempt-1))
	if d <= 0 || d > c.maxDelay { // d<=0 catches overflow from the shift
		d = c.maxDelay
	}
	return d
}

// defaultDiscoveryRetry governs discovery-doc fetches at startup.
//
// Sized for the spot-node-preemption scenario (observed 2026-07-28): a node
// preemption reschedules every pod at once, so bifrost can start before
// identity's Service has any ready endpoint. identity's readiness probe has
// initialDelaySeconds: 5, periodSeconds: 10, so a freshly-rescheduled
// identity pod typically reports ready within ~15-25s of the node coming
// back. Six attempts with backoff 2s, 4s, 8s, 16s, 16s total 46s of sleep —
// comfortably inside the 30-60s target: enough to outlast a routine
// co-restart, but a genuinely-down dependency still gives up (and crash
// loops, surfacing loudly) within about a minute rather than hanging
// indefinitely.
var defaultDiscoveryRetry = retryConfig{
	attempts:  6,
	baseDelay: 2 * time.Second,
	maxDelay:  16 * time.Second,
}

// fetchDiscoveryDocWithRetry retries fetchDiscoveryDoc on transient failures
// (dial errors, timeouts, 5xx) per cfg's backoff schedule, logging each
// failed attempt so the pod's logs explain any startup delay, plus the
// eventual success (if it took more than one try) or final failure. It
// gives up immediately on a permanentErr instead of consuming the rest of
// the attempt budget, and it never sleeps past ctx's deadline/cancellation.
func fetchDiscoveryDocWithRetry(ctx context.Context, issuer string, cfg retryConfig) (map[string]any, error) {
	var lastErr error
	for attempt := 1; attempt <= cfg.attempts; attempt++ {
		doc, err := fetchDiscoveryDoc(ctx, issuer)
		if err == nil {
			if attempt > 1 {
				log.Printf("oidc: discovery doc fetch succeeded on attempt %d/%d", attempt, cfg.attempts)
			}
			return doc, nil
		}

		log.Printf("oidc: discovery doc fetch attempt %d/%d failed: %v", attempt, cfg.attempts, err)
		var perm *permanentErr
		if errors.As(err, &perm) {
			return nil, err
		}
		lastErr = err
		if attempt == cfg.attempts {
			break
		}

		d := cfg.delay(attempt)
		log.Printf("oidc: retrying discovery doc fetch in %s", d)
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("discovery doc fetch canceled after %d attempt(s): %w", attempt, ctx.Err())
		}
	}
	log.Printf("oidc: discovery doc fetch failed after %d attempts, giving up", cfg.attempts)
	return nil, fmt.Errorf("giving up after %d attempts: %w", cfg.attempts, lastErr)
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
