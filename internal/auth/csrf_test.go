package auth

import "testing"

func TestCSRFTokenStableAndVerifies(t *testing.T) {
	key := []byte("12345678901234567890123456789012")
	tok := CSRFToken(key, "sid-abc")
	if tok == "" {
		t.Fatal("empty token")
	}
	if !VerifyCSRF(key, "sid-abc", tok) {
		t.Fatal("verify failed")
	}
	if VerifyCSRF(key, "different-sid", tok) {
		t.Fatal("verify accepted token for different sid")
	}
}
