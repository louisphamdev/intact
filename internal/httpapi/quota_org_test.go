package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// Tests for claudeOrgID resolution and caching.
func TestClaudeOrgIDResolution(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	var profileCalls atomic.Int32
	validUUID := "11111111-2222-3333-4444-555555555555"

	fakeClaude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/oauth/profile" {
			profileCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"organization": map[string]any{
					"uuid": validUUID,
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer fakeClaude.Close()

	a, _ := newServer(s, map[string]string{"claude": fakeClaude.URL}, nil)

	// 1. Connection with missing claudeOrgId triggers one profile read and stores it.
	c1, _ := s.CreateConnection("claude", "acc1", "token-1")
	orgID, err := a.claudeOrgID(context.Background(), c1, "token-1")
	if err != nil {
		t.Fatalf("claudeOrgID c1: %v", err)
	}
	if orgID != validUUID {
		t.Errorf("orgID = %q, want %q", orgID, validUUID)
	}
	if profileCalls.Load() != 1 {
		t.Errorf("profileCalls = %d, want 1", profileCalls.Load())
	}

	// Verify it was stored in meta
	conns, _ := s.ListConnections()
	var found store.Connection
	for _, c := range conns {
		if c.ID == c1.ID {
			found = c
			break
		}
	}
	if found.Meta["claudeOrgId"] != validUUID {
		t.Errorf("c1 meta claudeOrgId = %q, want %q", found.Meta["claudeOrgId"], validUUID)
	}

	// Second call for c1 should read from meta and make NO profile call
	orgID2, err := a.claudeOrgID(context.Background(), found, "token-1")
	if err != nil {
		t.Fatalf("claudeOrgID c1 second call: %v", err)
	}
	if orgID2 != validUUID {
		t.Errorf("orgID2 = %q, want %q", orgID2, validUUID)
	}
	if profileCalls.Load() != 1 {
		t.Errorf("profileCalls = %d, want still 1", profileCalls.Load())
	}

	// 2. Empty string "" in meta counts as missing and triggers profile read.
	s.SetMeta(c1.ID, map[string]string{"claudeOrgId": ""})
	orgID3, err := a.claudeOrgID(context.Background(), c1, "token-1")
	if err != nil {
		t.Fatalf("claudeOrgID after empty string: %v", err)
	}
	if orgID3 != validUUID {
		t.Errorf("orgID3 = %q, want %q", orgID3, validUUID)
	}
	if profileCalls.Load() != 2 {
		t.Errorf("profileCalls = %d, want 2", profileCalls.Load())
	}
}

// Profile returning a non-UUID is refused.
func TestClaudeOrgIDRefusesNonUUID(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	fakeClaude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"organization": map[string]any{
				"uuid": "not-a-valid-uuid",
			},
		})
	}))
	defer fakeClaude.Close()

	a, _ := newServer(s, map[string]string{"claude": fakeClaude.URL}, nil)
	c, _ := s.CreateConnection("claude", "acc", "token-x")

	orgID, err := a.claudeOrgID(context.Background(), c, "token-x")
	if err == nil {
		t.Errorf("expected error for non-UUID, got orgID = %q", orgID)
	}
}

// Token answer in Claude login with organization.uuid stores claudeOrgId.
func TestClaudeLoginStoresOrgID(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	validUUID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	fakeOAuth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-123",
			"refresh_token": "refresh-123",
			"account": map[string]any{
				"email_address": "test@example.com",
			},
			"organization": map[string]any{
				"uuid": validUUID,
			},
		})
	}))
	defer fakeOAuth.Close()

	origSpec := loginSpecs["claude"]
	t.Cleanup(func() { loginSpecs["claude"] = origSpec })
	claudeSpec := origSpec
	claudeSpec.tokenURL = fakeOAuth.URL
	loginSpecs["claude"] = claudeSpec

	a, _ := newServer(s, nil, nil)
	c, err := a.exchangeLogin(context.Background(), store.PendingLogin{
		Provider: "claude",
	}, "test-code")
	if err != nil {
		t.Fatalf("exchangeLogin: %v", err)
	}

	conns, _ := s.ListConnections()
	var found store.Connection
	for _, cn := range conns {
		if cn.ID == c.ID {
			found = cn
			break
		}
	}
	if found.Meta["claudeOrgId"] != validUUID {
		t.Errorf("stored claudeOrgId = %q, want %q", found.Meta["claudeOrgId"], validUUID)
	}
}
