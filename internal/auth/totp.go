// Package auth gates the intact server: a password plus a TOTP second factor for
// a person, and a bearer token for a machine. It uses only the standard library.
package auth

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

const totpStep = 30 // seconds per TOTP window (RFC 6238 default)

// totpAt returns the 6-digit TOTP for a base32 secret at a Unix time.
func totpAt(base32Secret string, unix int64) (string, error) {
	key, err := base32.StdEncoding.DecodeString(strings.ToUpper(strings.TrimSpace(base32Secret)))
	if err != nil {
		return "", fmt.Errorf("decode secret: %w", err)
	}
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
	return fmt.Sprintf("%06d", code), nil
}

// verifyTOTPAt reports whether code matches the secret within one window either
// side of now, which absorbs clock skew and a code typed near a boundary.
func verifyTOTPAt(secret, code string, now int64) bool {
	code = strings.TrimSpace(code)
	for _, skew := range []int64{0, -totpStep, totpStep} {
		want, err := totpAt(secret, now+skew)
		if err != nil {
			return false
		}
		if hmac.Equal([]byte(want), []byte(code)) {
			return true
		}
	}
	return false
}

// VerifyTOTP checks a code against the secret at the current time.
func VerifyTOTP(secret, code string) bool {
	return verifyTOTPAt(secret, code, time.Now().Unix())
}
