package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// signSession returns a cookie value "<expiryUnix>.<hmac>" that a client cannot
// forge without the key and cannot extend without breaking the signature.
func signSession(key []byte, expiryUnix int64) string {
	payload := strconv.FormatInt(expiryUnix, 10)
	return payload + "." + sign(key, payload)
}

// validSession reports whether tok carries a valid signature and has not expired.
func validSession(key []byte, tok string, now int64) bool {
	payload, sig, ok := strings.Cut(tok, ".")
	if !ok {
		return false
	}
	if !hmac.Equal([]byte(sig), []byte(sign(key, payload))) {
		return false
	}
	exp, err := strconv.ParseInt(payload, 10, 64)
	if err != nil {
		return false
	}
	return now < exp
}

func sign(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}
