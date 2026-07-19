---
name: verify
description: Run bifrost locally against the real cluster and drive the web UI to verify changes end-to-end
---

# Verifying bifrost changes

Bifrost can run locally against the real cluster (kubeconfig fallback) with
dummy OIDC credentials — the login flow needs real client credentials, but a
session cookie can be minted directly since it's just HMAC-signed JSON with
the locally-chosen `SESSION_SECRET`.

## Launch

```bash
HTTP_ADDRESS=:8091 BASE_URL=http://localhost:8091 ENV=local \
SERVICES=asset-manager,bifrost,comms,footstrike-api,footstrike-dashboard,forecasting,identity \
ALLOWED_EMAIL=<the-allowed-email> \
OIDC_ISSUER_EXTERNAL=https://identity.ethanswan.com \
OIDC_ISSUER_INTERNAL=https://identity.ethanswan.com \
OIDC_CLIENT_ID=dummy OIDC_CLIENT_SECRET=dummy \
SESSION_SECRET=local-verify-secret-0123456789abcdef \
go run ./cmd/bifrost
```

- OIDC discovery is fetched at startup and must succeed — the public prod
  issuer works; the client id/secret are only used in the login flow.
- `ALLOWED_EMAIL` must match the email in the minted cookie.
- Reads are safe; the only mutating routes are POST `/services/{name}/promote`
  and `/rollback`, which patch real ArgoCD Applications — never POST them
  with a *correct* `expected_sha` during verification. A stale/wrong
  `expected_sha` is refused server-side before patching, which makes it a
  safe probe of the promote path.

## Mint a session cookie + CSRF token

Cookie format (`internal/auth/session.go`): `b64url(json)  "."  b64url(HMAC-SHA256(secret, json))`,
cookie name `bifrost_session`, session JSON `{"email":..., "iat":"2026-01-02T15:04:05Z", "sid":...}`.
CSRF (`internal/auth/csrf.go`): `b64url(HMAC-SHA256(secret, "csrf|"+sid))`.

```python
import base64, hashlib, hmac, json, time
secret = b"local-verify-secret-0123456789abcdef"
b64 = lambda b: base64.urlsafe_b64encode(b).rstrip(b"=").decode()
sess = {"email": "<the-allowed-email>",
        "iat": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "sid": "verify-1"}
payload = json.dumps(sess, separators=(",", ":")).encode()
cookie = f"bifrost_session={b64(payload)}.{b64(hmac.new(secret, payload, hashlib.sha256).digest())}"
csrf = b64(hmac.new(secret, b"csrf|" + sess["sid"].encode(), hashlib.sha256).digest())
```

## Drive

```bash
curl -s -b "$COOKIE" http://localhost:8091/apps            # full page
curl -s -b "$COOKIE" http://localhost:8091/services/<app>/status   # per-app JSON state
curl -s -b "$COOKIE" -H "Accept: application/json" \
  -d "csrf=$CSRF&expected_sha=fff0000" http://localhost:8091/services/<app>/promote
  # safe promote probe: 409 "staging changed" if promotable, without patching
```

Promote forms in `/apps` HTML carry `name="expected_sha" value="<target-tag>"` —
grep for `expected_sha` to see what a promote would deploy.
