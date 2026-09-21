package store

import (
	"path/filepath"
	"testing"
)

func TestOAuthCredsRoundTripAndRefreshUpdate(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, _ := s.CreateConnection("claude", "Personal", "old-access-token")

	// a fresh connection has no OAuth config.
	creds, err := s.OAuth(c.ID)
	if err != nil {
		t.Fatalf("OAuth: %v", err)
	}
	if creds.RefreshToken != "" || creds.TokenURL != "" {
		t.Fatalf("new connection has OAuth set: %+v", creds)
	}

	want := OAuthCreds{
		RefreshToken: "rt-123",
		TokenURL:     "https://api.anthropic.com/v1/oauth/token",
		ClientID:     "cid",
		ClientSecret: "",
		ExpiresAt:    "2026-09-21T00:00:00Z",
	}
	if err := s.SetOAuth(c.ID, want); err != nil {
		t.Fatalf("SetOAuth: %v", err)
	}
	got, _ := s.OAuth(c.ID)
	if got != want {
		t.Fatalf("OAuth = %+v, want %+v", got, want)
	}

	// a refresh replaces the access token (secret) and the expiry.
	if err := s.UpdateAccessToken(c.ID, "new-access-token", "2026-09-22T00:00:00Z"); err != nil {
		t.Fatalf("UpdateAccessToken: %v", err)
	}
	sec, _ := s.Secret(c.ID)
	if sec != "new-access-token" {
		t.Errorf("Secret = %q, want the refreshed token", sec)
	}
	got2, _ := s.OAuth(c.ID)
	if got2.ExpiresAt != "2026-09-22T00:00:00Z" || got2.RefreshToken != "rt-123" {
		t.Errorf("after refresh OAuth = %+v", got2)
	}
}
