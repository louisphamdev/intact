package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
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
	// mu guards lastStep, the newest TOTP step that was accepted.
	mu       sync.Mutex
	lastStep int64
}

func nowUnix() int64 { return time.Now().Unix() }

// CheckCode verifies a TOTP code. This is the only browser login factor. A code
// is single use: its step must be newer than the last accepted one, so a code
// read over a shoulder cannot open a second session inside its window.
func (c *Config) CheckCode(code string) bool {
	return c.checkCodeAt(code, nowUnix())
}

func (c *Config) checkCodeAt(code string, now int64) bool {
	step, ok := matchStepAt(c.TOTPSecret, code, now)
	if !ok {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if step <= c.lastStep {
		return false
	}
	c.lastStep = step
	return true
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

// IssueDevice returns a value for the device cookie: a random id and an HMAC
// over it. It grants no access. The login guard reads it to charge a wrong code
// to this browser's own budget instead of the public one.
func (c *Config) IssueDevice() string {
	id := hex.EncodeToString(randomBytes(16))
	return id + "." + sign(c.sessionKey, id)
}

// DeviceID returns the id inside a device cookie. ok is false when the value
// carries no authentic signature, so a forged cookie names no device.
func (c *Config) DeviceID(value string) (id string, ok bool) {
	id, sig, cut := strings.Cut(value, ".")
	if !cut || id == "" {
		return "", false
	}
	if !hmac.Equal([]byte(sig), []byte(sign(c.sessionKey, id))) {
		return "", false
	}
	return id, true
}

// FromEnv builds a Config from environment variables. It returns nil and no
// error when INTACT_TOTP_SECRET is unset, which leaves the server open
// (loopback use). A secret that does not decode is an error: it would refuse
// every code, so the caller must stop instead of starting a dead gate. A
// missing session key is generated so a restart invalidates old cookies.
func FromEnv() (*Config, error) {
	secret := os.Getenv("INTACT_TOTP_SECRET")
	if secret == "" {
		return nil, nil
	}
	if err := ValidateSecret(secret); err != nil {
		return nil, fmt.Errorf("INTACT_TOTP_SECRET: %w", err)
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
	}, nil
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
