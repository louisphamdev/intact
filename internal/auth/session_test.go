package auth

import (
	"strings"
	"testing"
)

func TestSessionSignAndValidate(t *testing.T) {
	key := []byte("server-secret-key")
	now := int64(1_000_000)
	tok := signSession(key, now+3600)

	if !validSession(key, tok, now) {
		t.Error("a freshly signed session did not validate")
	}
	if validSession(key, tok, now+7200) {
		t.Error("an expired session validated")
	}
	if validSession([]byte("other-key"), tok, now) {
		t.Error("a session validated under the wrong key")
	}
	// tamper with the expiry but keep the old signature.
	parts := strings.SplitN(tok, ".", 2)
	forged := "9999999999." + parts[1]
	if validSession(key, forged, now) {
		t.Error("a tampered session validated")
	}
}
