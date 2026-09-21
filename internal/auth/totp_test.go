package auth

import "testing"

// RFC 6238 test vector: ASCII secret "12345678901234567890" (base32
// GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ), SHA-1, 6 digits. At Unix time 59 the code
// is 287082 (the last 6 digits of the RFC's 94287082).
func TestTOTPKnownVector(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	code, err := totpAt(secret, 59)
	if err != nil {
		t.Fatal(err)
	}
	if code != "287082" {
		t.Errorf("totpAt = %q, want 287082", code)
	}
}

func TestVerifyTOTPAcceptsCurrentAndAdjacentWindows(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	// code for step at t=59 must verify at t=59, and at t within +/-30s.
	for _, now := range []int64{59, 59 + 29, 59 - 29} {
		want, _ := totpAt(secret, 59)
		if !verifyTOTPAt(secret, want, now) {
			t.Errorf("verify failed at now=%d for code from t=59", now)
		}
	}
	// a code two windows away must be rejected.
	old, _ := totpAt(secret, 59)
	if verifyTOTPAt(secret, old, 59+90) {
		t.Error("verify accepted a code that is 3 windows old")
	}
}
