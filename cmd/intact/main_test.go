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
