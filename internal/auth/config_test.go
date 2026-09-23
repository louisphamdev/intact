package auth

import (
	"strings"
	"testing"
)

func newTestConfig() *Config {
	return &Config{
		TOTPSecret: "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ",
		APIToken:   "machine-token-abc",
		sessionKey: []byte("k"),
		ttlSeconds: 3600,
	}
}

func TestCheckCodeUsesTOTP(t *testing.T) {
	c := newTestConfig()
	code, _ := totpAt(c.TOTPSecret, nowUnix())
	if !c.CheckCode(code) {
		t.Error("correct TOTP was rejected")
	}
	if c.CheckCode("000000") {
		t.Error("wrong TOTP accepted")
	}
}

func TestCheckAPIToken(t *testing.T) {
	c := newTestConfig()
	if !c.CheckAPIToken("Bearer machine-token-abc") {
		t.Error("correct bearer token rejected")
	}
	if c.CheckAPIToken("Bearer nope") || c.CheckAPIToken("machine-token-abc") || c.CheckAPIToken("") {
		t.Error("bad authorization accepted")
	}
}

func TestSessionRoundTripThroughConfig(t *testing.T) {
	c := newTestConfig()
	tok := c.IssueSession()
	if !c.ValidSession(tok) {
		t.Error("issued session did not validate")
	}
	if c.ValidSession("garbage") {
		t.Error("garbage session validated")
	}
}

// A code opens one session. The step it belongs to is recorded, so the same
// code inside its window is refused afterwards.
func TestCheckCodeIsSingleUse(t *testing.T) {
	c := newTestConfig()
	code, _ := totpAt(c.TOTPSecret, nowUnix())
	if !c.CheckCode(code) {
		t.Fatal("first use of a correct code was rejected")
	}
	if c.CheckCode(code) {
		t.Error("the same code was accepted twice")
	}
}

func TestCheckCodeRefusesAnOlderStep(t *testing.T) {
	c := newTestConfig()
	now := nowUnix()
	next, _ := totpAt(c.TOTPSecret, now+totpStep)
	if !c.checkCodeAt(next, now) {
		t.Fatal("the code of the next window was rejected")
	}
	current, _ := totpAt(c.TOTPSecret, now)
	if c.checkCodeAt(current, now) {
		t.Error("a code from an older step was accepted after a newer one")
	}
}

func TestDeviceCookieRoundTrip(t *testing.T) {
	c := newTestConfig()
	value := c.IssueDevice()
	id, ok := c.DeviceID(value)
	if !ok || id == "" {
		t.Fatalf("DeviceID(%q) = %q, %v, want an id and true", value, id, ok)
	}
	if again, _ := c.DeviceID(c.IssueDevice()); again == id {
		t.Error("two devices got the same id")
	}
	other := NewConfig(c.TOTPSecret, c.APIToken, []byte("another-key"), 3600)
	for _, bad := range []string{"", "no-dot", id, id + ".", id + ".00", value + "x", "x" + value} {
		if _, ok := c.DeviceID(bad); ok {
			t.Errorf("DeviceID accepted a forged value %q", bad)
		}
	}
	if _, ok := other.DeviceID(value); ok {
		t.Error("a device cookie validated under another session key")
	}
}

func TestFromEnvRefusesASecretThatDoesNotDecode(t *testing.T) {
	t.Setenv("INTACT_TOTP_SECRET", "this is not base32!")
	cfg, err := FromEnv()
	if err == nil {
		t.Fatal("a secret that does not decode was accepted")
	}
	if cfg != nil {
		t.Error("FromEnv returned a config with a bad secret")
	}
	if !strings.Contains(err.Error(), "INTACT_TOTP_SECRET") {
		t.Errorf("message %q does not name the variable", err)
	}
}

func TestFromEnvAcceptsAnUnpaddedSecret(t *testing.T) {
	// 16 bytes of base32 need padding; an authenticator app shows none.
	const unpadded = "AEBAGBAFAYDQQCIKBMGA2DQPCA"
	t.Setenv("INTACT_TOTP_SECRET", unpadded)
	cfg, err := FromEnv()
	if err != nil || cfg == nil {
		t.Fatalf("FromEnv with an unpadded secret: cfg=%v err=%v", cfg, err)
	}
	if !cfg.CheckCode(TOTPNow(unpadded)) {
		t.Error("a code from an unpadded secret was rejected")
	}
}

func TestFromEnvWithNoSecretLeavesTheServerOpen(t *testing.T) {
	t.Setenv("INTACT_TOTP_SECRET", "")
	cfg, err := FromEnv()
	if cfg != nil || err != nil {
		t.Fatalf("FromEnv with no secret: cfg=%v err=%v, want nil nil", cfg, err)
	}
}
