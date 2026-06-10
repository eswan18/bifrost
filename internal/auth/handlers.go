package auth

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	stateCookie    = "bifrost_oauth_state"
	nonceCookie    = "bifrost_oauth_nonce"
	verifierCookie = "bifrost_oauth_verifier"
)

type Handlers struct {
	OIDC    *OIDC
	Session *SessionManager
	// RenderError renders a user-facing error page. Injected from main so the
	// auth package stays decoupled from the web/template layer. If nil, errors
	// fall back to plain http.Error.
	RenderError func(w http.ResponseWriter, status int, message string)
}

func (h *Handlers) Login(w http.ResponseWriter, r *http.Request) {
	state := randString(16)
	nonce := randString(16)
	// identity requires PKCE (S256). Generate a verifier, send its S256
	// challenge on the authorize request, and stash the verifier in a cookie
	// so the callback can present it at the token endpoint.
	verifier := oauth2.GenerateVerifier()
	setShortCookie(w, stateCookie, state)
	setShortCookie(w, nonceCookie, nonce)
	setShortCookie(w, verifierCookie, verifier)
	url := h.OIDC.OAuth2.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	)
	http.Redirect(w, r, url, http.StatusSeeOther)
}

func (h *Handlers) Callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// The IdP signals a rejected request by redirecting back with
	// ?error=...&error_description=... and no code. Surface that to the user
	// instead of falling through to a token exchange that 502s (which
	// Cloudflare masks behind its own "Bad Gateway" page).
	if e := q.Get("error"); e != "" {
		desc := q.Get("error_description")
		slog.Warn("oidc callback returned error", "error", e, "description", desc)
		msg := e
		if desc != "" {
			msg = desc
		}
		h.fail(w, http.StatusBadRequest, "Sign-in failed: "+msg)
		return
	}

	stateC, err := r.Cookie(stateCookie)
	if err != nil || q.Get("state") != stateC.Value {
		h.fail(w, http.StatusBadRequest, "Sign-in failed: invalid state — please try again.")
		return
	}
	nonceC, err := r.Cookie(nonceCookie)
	if err != nil {
		h.fail(w, http.StatusBadRequest, "Sign-in failed: missing nonce — please try again.")
		return
	}
	verifierC, err := r.Cookie(verifierCookie)
	if err != nil {
		h.fail(w, http.StatusBadRequest, "Sign-in failed: missing PKCE verifier — please try again.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	tok, err := h.OIDC.OAuth2.Exchange(ctx, q.Get("code"),
		oauth2.VerifierOption(verifierC.Value))
	if err != nil {
		slog.Warn("oidc token exchange failed", "error", err)
		h.fail(w, http.StatusBadRequest, "Sign-in failed: token exchange error.")
		return
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		h.fail(w, http.StatusBadRequest, "Sign-in failed: no id_token in token response.")
		return
	}
	idTok, err := h.OIDC.Verifier.Verify(ctx, raw)
	if err != nil {
		slog.Warn("oidc id_token verification failed", "error", err)
		h.fail(w, http.StatusBadRequest, "Sign-in failed: id_token verification error.")
		return
	}
	if idTok.Nonce != nonceC.Value {
		h.fail(w, http.StatusBadRequest, "Sign-in failed: nonce mismatch — please try again.")
		return
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := idTok.Claims(&claims); err != nil {
		h.fail(w, http.StatusBadRequest, "Sign-in failed: could not parse token claims.")
		return
	}
	if claims.Email == "" {
		h.fail(w, http.StatusForbidden, "Sign-in failed: no email in token.")
		return
	}

	clearShortCookie(w, stateCookie)
	clearShortCookie(w, nonceCookie)
	clearShortCookie(w, verifierCookie)
	h.Session.Set(w, claims.Email)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	h.Session.Clear(w)
	http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
}

// fail renders a user-facing error page via the injected renderer, or falls
// back to plain text if none was wired.
func (h *Handlers) fail(w http.ResponseWriter, status int, message string) {
	if h.RenderError != nil {
		h.RenderError(w, status, message)
		return
	}
	http.Error(w, message, status)
}

func setShortCookie(w http.ResponseWriter, name, val string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: val, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
		MaxAge: 300,
	})
}

func clearShortCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}
