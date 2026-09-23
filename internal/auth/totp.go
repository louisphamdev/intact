// Package auth gates the intact server: a TOTP code and nothing else for a
// person, and a bearer token for a machine. It uses only the standard library.
package auth

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

const totpStep = 30 // seconds per TOTP window (RFC 6238 default)

// decodeSecret reads a base32 secret. An authenticator app shows the secret in
// groups and without padding, so spaces and missing "=" must both be accepted.
func decodeSecret(base32Secret string) ([]byte, error) {
	s := strings.ToUpper(strings.Join(strings.Fields(base32Secret), ""))
	s = strings.TrimRight(s, "=")
	return base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
}

// ValidateSecret reports whether a secret can be used. Call it at startup: a
// secret that does not decode refuses every code, and the operator must learn
// that when the process starts, not at the first sign-in.
func ValidateSecret(base32Secret string) error {
	key, err := decodeSecret(base32Secret)
	if err != nil {
		return fmt.Errorf("decode base32: %w", err)
	}
	if len(key) == 0 {
		return errors.New("the secret is empty")
	}
	return nil
}

// totpAt returns the 6-digit TOTP for a base32 secret at a Unix time.
func totpAt(base32Secret string, unix int64) (string, error) {
	key, err := decodeSecret(base32Secret)
	if err != nil {
		return "", fmt.Errorf("decode secret: %w", err)
	}
	return codeAt(key, unix), nil
}

func codeAt(key []byte, unix int64) string {
	counter := uint64(unix / totpStep)
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[offset]&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])) % 1000000
	return fmt.Sprintf("%06d", code)
}

// matchStepAt returns the time step of a code that matches the secret within one
// window either side of now, which absorbs clock skew and a code typed near a
// boundary. The step lets the caller refuse a code that was already used.
func matchStepAt(secret, code string, now int64) (int64, bool) {
	code = strings.TrimSpace(code)
	key, err := decodeSecret(secret)
	if err != nil {
		return 0, false
	}
	for _, skew := range []int64{0, -totpStep, totpStep} {
		at := now + skew
		if hmac.Equal([]byte(codeAt(key, at)), []byte(code)) {
			return at / totpStep, true
		}
	}
	return 0, false
}

// verifyTOTPAt reports whether code matches the secret at now.
func verifyTOTPAt(secret, code string, now int64) bool {
	_, ok := matchStepAt(secret, code, now)
	return ok
}
