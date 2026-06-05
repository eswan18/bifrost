package auth

import "crypto/hmac"

func CSRFToken(key []byte, sessionID string) string {
	return sign(key, []byte("csrf|"+sessionID))
}

func VerifyCSRF(key []byte, sessionID, token string) bool {
	want := CSRFToken(key, sessionID)
	return hmac.Equal([]byte(want), []byte(token))
}
