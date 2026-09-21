package auth

import "testing"

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
