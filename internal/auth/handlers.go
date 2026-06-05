package auth

import (
	"context"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	stateCookie = "bifrost_oauth_state"
	nonceCookie = "bifrost_oauth_nonce"
)

type Handlers struct {
	OIDC    *OIDC
	Session *SessionManager
}

func (h *Handlers) Login(w http.ResponseWriter, r *http.Request) {
	state := randString(16)
	nonce := randString(16)
	setShortCookie(w, stateCookie, state)
	setShortCookie(w, nonceCookie, nonce)
	url := h.OIDC.OAuth2.AuthCodeURL(state, oidc.Nonce(nonce))
	http.Redirect(w, r, url, http.StatusSeeOther)
}

func (h *Handlers) Callback(w http.ResponseWriter, r *http.Request) {
	stateC, err := r.Cookie(stateCookie)
	if err != nil || r.URL.Query().Get("state") != stateC.Value {
		http.Error(w, "bad state", http.StatusBadRequest)
		return
	}
	nonceC, err := r.Cookie(nonceCookie)
	if err != nil {
		http.Error(w, "missing nonce", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	tok, err := h.OIDC.OAuth2.Exchange(ctx, r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		http.Error(w, "no id_token", http.StatusBadGateway)
		return
	}
	idTok, err := h.OIDC.Verifier.Verify(ctx, raw)
	if err != nil {
		http.Error(w, "id_token verify failed", http.StatusBadGateway)
		return
	}
	if idTok.Nonce != nonceC.Value {
		http.Error(w, "nonce mismatch", http.StatusBadRequest)
		return
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := idTok.Claims(&claims); err != nil {
		http.Error(w, "claim parse failed", http.StatusBadGateway)
		return
	}
	if claims.Email == "" {
		http.Error(w, "no email in id_token", http.StatusForbidden)
		return
	}

	clearShortCookie(w, stateCookie)
	clearShortCookie(w, nonceCookie)
	h.Session.Set(w, claims.Email)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	h.Session.Clear(w)
	http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
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
