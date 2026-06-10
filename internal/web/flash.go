package web

import (
	"net/http"
	"net/url"
)

const flashCookie = "bifrost_flash"

type FlashKind string

const (
	FlashSuccess FlashKind = "success"
	FlashError   FlashKind = "error"
)

type Flash struct {
	Kind FlashKind
	Msg  string
}

func SetFlash(w http.ResponseWriter, kind FlashKind, msg string) {
	// URL-encode the message: cookie values can't carry spaces, non-ASCII
	// (the "→" in promote messages), or arbitrary bytes from kube error
	// strings — net/http would silently strip them.
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: string(kind) + "|" + url.QueryEscape(msg),
		Path: "/", MaxAge: 60, HttpOnly: true, Secure: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// TakeFlash returns the flash (if any) and clears the cookie.
func TakeFlash(w http.ResponseWriter, r *http.Request) *Flash {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return nil
	}
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	for i := 0; i < len(c.Value); i++ {
		if c.Value[i] == '|' {
			msg, err := url.QueryUnescape(c.Value[i+1:])
			if err != nil {
				msg = c.Value[i+1:]
			}
			return &Flash{Kind: FlashKind(c.Value[:i]), Msg: msg}
		}
	}
	return nil
}
