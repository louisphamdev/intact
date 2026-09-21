package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"os"
	"strconv"
	"time"
)

// Config holds the single operator's credentials. It is built from the
// environment so no secret is compiled in or committed. The person logs in with
// a TOTP code alone; a machine uses the bearer token.
type Config struct {
	TOTPSecret string // base32
	APIToken   string
	sessionKey []byte
	ttlSeconds int64
}

func nowUnix() int64 { return time.Now().Unix() }

// CheckCode verifies a TOTP code. This is the only browser login factor.
func (c *Config) CheckCode(code string) bool {
	return VerifyTOTP(c.TOTPSecret, code)
}

// CheckAPIToken verifies an "Authorization: Bearer <token>" header for machines.
func (c *Config) CheckAPIToken(header string) bool {
	const p = "Bearer "
	if len(header) <= len(p) || header[:len(p)] != p {
		return false
	}
	got := header[len(p):]
	return subtle.ConstantTimeCompare([]byte(got), []byte(c.APIToken)) == 1
}

// IssueSession returns a signed cookie value valid for the configured TTL.
func (c *Config) IssueSession() string {
	return signSession(c.sessionKey, nowUnix()+c.ttlSeconds)
}

// ValidSession reports whether a cookie value is authentic and unexpired.
func (c *Config) ValidSession(tok string) bool {
	return validSession(c.sessionKey, tok, nowUnix())
}

// TTLSeconds is the session lifetime, for the cookie Max-Age.
func (c *Config) TTLSeconds() int { return int(c.ttlSeconds) }

// FromEnv builds a Config from environment variables. It returns nil when
// INTACT_TOTP_SECRET is unset, which leaves the server open (loopback use). A
// missing session key is generated so a restart invalidates old cookies.
func FromEnv() *Config {
	secret := os.Getenv("INTACT_TOTP_SECRET")
	if secret == "" {
		return nil
	}
	ttl := int64(12 * 3600)
	if v, err := strconv.ParseInt(os.Getenv("INTACT_SESSION_TTL"), 10, 64); err == nil && v > 0 {
		ttl = v
	}
	key := []byte(os.Getenv("INTACT_SESSION_KEY"))
	if len(key) == 0 {
		key = randomBytes(32)
	}
	return &Config{
		TOTPSecret: secret,
		APIToken:   os.Getenv("INTACT_API_TOKEN"),
		sessionKey: key,
		ttlSeconds: ttl,
	}
}

// NewConfig builds a Config from explicit values, for callers that hold them.
func NewConfig(totpSecret, apiToken string, sessionKey []byte, ttlSeconds int64) *Config {
	if len(sessionKey) == 0 {
		sessionKey = randomBytes(32)
	}
	return &Config{TOTPSecret: totpSecret, APIToken: apiToken, sessionKey: sessionKey, ttlSeconds: ttlSeconds}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// GenerateTOTPSecret returns a new base32 TOTP secret for enrollment.
func GenerateTOTPSecret() string {
	return base32.StdEncoding.EncodeToString(randomBytes(20))
}

// TOTPNow returns the current code for a secret. Handy for enrollment checks.
func TOTPNow(secret string) string {
	code, _ := totpAt(secret, nowUnix())
	return code
}
