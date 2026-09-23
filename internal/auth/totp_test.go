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

// An authenticator app shows a secret in groups and without padding. Both forms
// must give the same code.
func TestTOTPAcceptsPaddedAndUnpaddedSecrets(t *testing.T) {
	const (
		padded   = "AEBAGBAFAYDQQCIKBMGA2DQPCA======"
		unpadded = "AEBAGBAFAYDQQCIKBMGA2DQPCA"
		spaced   = "aebagbaf aydqqcik bmga2dqpca"
	)
	want, err := totpAt(padded, 59)
	if err != nil {
		t.Fatalf("padded secret: %v", err)
	}
	for _, form := range []string{unpadded, spaced} {
		got, err := totpAt(form, 59)
		if err != nil {
			t.Fatalf("secret %q: %v", form, err)
		}
		if got != want {
			t.Errorf("secret %q gives %q, want %q", form, got, want)
		}
	}
}

func TestValidateSecret(t *testing.T) {
	for _, ok := range []string{"AEBAGBAFAYDQQCIKBMGA2DQPCA", "AEBAGBAFAYDQQCIKBMGA2DQPCA======", "GEZDGNBVGY3TQOJQ"} {
		if err := ValidateSecret(ok); err != nil {
			t.Errorf("ValidateSecret(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"this is not base32!", "1890", ""} {
		if err := ValidateSecret(bad); err == nil {
			t.Errorf("ValidateSecret(%q) = nil, want an error", bad)
		}
	}
}

// A code matches one step, and the caller needs that number to refuse a replay.
func TestMatchStepAtReturnsTheStep(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	code, _ := totpAt(secret, 59)
	step, ok := matchStepAt(secret, code, 59)
	if !ok || step != 59/totpStep {
		t.Fatalf("matchStepAt = %d, %v, want %d, true", step, ok, 59/totpStep)
	}
	// The same code read one window later still names its own step.
	step, ok = matchStepAt(secret, code, 59+totpStep)
	if !ok || step != 59/totpStep {
		t.Fatalf("one window later: matchStepAt = %d, %v, want %d, true", step, ok, 59/totpStep)
	}
	if _, ok := matchStepAt(secret, "000000", 59); ok {
		t.Error("a wrong code matched a step")
	}
}
