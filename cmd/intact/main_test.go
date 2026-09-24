package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/auth"
	"github.com/louisphamdev/intact/internal/store"
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

func TestC17ContractResetCLI(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cli_reset.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.UpsertContractLearned(store.ContractLearned{
		Kind: "model", Subject: "m1", Direction: "request", Half: "intact", Format: "openai", Path: "p1", Type: "string", KeyID: "k", ReducerVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// 1. Both flags -> returns error, deletes nothing
	err = runContractReset([]string{"--db", dbPath, "--key", "k", "--reducer", "1"})
	if err == nil {
		t.Fatal("expected error with both --key and --reducer, got nil")
	}
	learned, _ := s.ListContractLearned("model", "m1")
	if len(learned) != 1 {
		t.Fatalf("expected learned row to be preserved, got %d", len(learned))
	}

	// 2. Neither flag -> returns error
	err = runContractReset([]string{"--db", dbPath})
	if err == nil {
		t.Fatal("expected error with neither flag, got nil")
	}

	// 3. --key alone -> succeeds and deletes
	err = runContractReset([]string{"--db", dbPath, "--key", "k"})
	if err != nil {
		t.Fatalf("expected success with --key alone, got %v", err)
	}
	learned, _ = s.ListContractLearned("model", "m1")
	if len(learned) != 0 {
		t.Fatalf("expected 0 learned rows after key reset, got %d", len(learned))
	}
}

// Each new install gets its own TOTP secret on its first start, kept in its database and
// shown once with the steps to add it to an authenticator app. An environment secret wins.
func TestTOTPSecretIsCreatedOncePerInstall(t *testing.T) {
	open := func(name string) *store.Store {
		s, err := store.Open(filepath.Join(t.TempDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	a, b := open("a.db"), open("b.db")
	secretA, fresh, err := totpSecret(a, "")
	if err != nil || !fresh || auth.ValidateSecret(secretA) != nil {
		t.Fatalf("first start: %q fresh=%v %v", secretA, fresh, err)
	}
	again, fresh, _ := totpSecret(a, "")
	if again != secretA || fresh {
		t.Errorf("restart: %q fresh=%v, want the same secret, not fresh", again, fresh)
	}
	if secretB, _, _ := totpSecret(b, ""); secretB == secretA {
		t.Error("two installs share one secret")
	}
	if got, fresh, _ := totpSecret(a, "JBSWY3DPEHPK3PXP"); got != "JBSWY3DPEHPK3PXP" || fresh {
		t.Errorf("the environment secret: %q fresh=%v", got, fresh)
	}
	guide := enrollmentGuide(secretA)
	for _, want := range []string{secretA, "otpauth://totp/intact?secret=" + secretA, "authenticator", "-show-totp"} {
		if !strings.Contains(guide, want) {
			t.Errorf("the guide misses %q:\n%s", want, guide)
		}
	}
}
