package main

import (
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/auth"
)

func TestAuthDecisionRefusesAnUngatedServer(t *testing.T) {
	err := authDecision(nil, false)
	if err == nil {
		t.Fatal("no TOTP secret and no opt-in: got nil, want an error")
	}
	if !strings.Contains(err.Error(), "-insecure-no-auth") {
		t.Errorf("message %q does not name the opt-in flag", err.Error())
	}
}

func TestAuthDecisionAcceptsTheExplicitOptIn(t *testing.T) {
	if err := authDecision(nil, true); err != nil {
		t.Errorf("with the opt-in: %v, want nil", err)
	}
}

func TestAuthDecisionAcceptsAConfiguredGate(t *testing.T) {
	if err := authDecision(&auth.Config{}, false); err != nil {
		t.Errorf("with a TOTP secret: %v, want nil", err)
	}
}
