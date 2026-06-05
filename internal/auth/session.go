package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const sessionCookie = "bifrost_session"

var cryptoReader = rand.Reader

type Session struct {
	Email    string    `json:"email"`
	IssuedAt time.Time `json:"iat"`
	ID       string    `json:"sid"` // random per-session id; used to derive CSRF
}

type SessionManager struct {
	key []byte
	ttl time.Duration
}

func NewSessionManager(key []byte, ttl time.Duration) *SessionManager {
	return &SessionManager{key: key, ttl: ttl}
}

func (m *SessionManager) Set(w http.ResponseWriter, email string) {
	sess := Session{
		Email:    email,
		IssuedAt: time.Now(),
		ID:       randString(16),
	}
	payload, _ := json.Marshal(sess)
	value := encode(payload) + "." + sign(m.key, payload)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(m.ttl.Seconds()),
	})
}

func (m *SessionManager) Get(r *http.Request) (*Session, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(c.Value, ".", 2)
	if len(parts) != 2 {
		return nil, errors.New("malformed cookie")
	}
	payload, err := decode(parts[0])
	if err != nil {
		return nil, err
	}
	want := sign(m.key, payload)
	if !hmac.Equal([]byte(want), []byte(parts[1])) {
		return nil, errors.New("bad signature")
	}
	var s Session
	if err := json.Unmarshal(payload, &s); err != nil {
		return nil, err
	}
	if time.Since(s.IssuedAt) > m.ttl {
		return nil, errors.New("session expired")
	}
	return &s, nil
}

func (m *SessionManager) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

func sign(key, msg []byte) string {
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	return encode(h.Sum(nil))
}

func encode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func decode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

func randString(n int) string {
	b := make([]byte, n)
	_, _ = cryptoReader.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
