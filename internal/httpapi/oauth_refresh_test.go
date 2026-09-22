package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

func TestProxyRefreshesExpiredOAuthToken(t *testing.T) {
	// The token endpoint hands back a new access token.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"fresh-token","expires_in":3600}`))
	}))
	defer tokenSrv.Close()

	// The provider echoes the Authorization it received.
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("claude", "Personal", "stale-token")
	s.SetOAuth(c.ID, store.OAuthCreds{
		RefreshToken: "rt",
		TokenURL:     tokenSrv.URL,
		ClientID:     "cid",
		ExpiresAt:    time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), // expired
	})

	h := NewWithAuth(s, map[string]string{"claude": up.URL}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude/m"}`)))

	if gotAuth != "Bearer fresh-token" {
		t.Fatalf("upstream saw auth %q, want the refreshed Bearer fresh-token", gotAuth)
	}
	if sec, _ := s.Secret(c.ID); sec != "fresh-token" {
		t.Errorf("stored secret = %q, want the refreshed token", sec)
	}
}

func TestProxyKeepsValidOAuthToken(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("claude", "P", "good-token")
	s.SetOAuth(c.ID, store.OAuthCreds{
		RefreshToken: "rt", TokenURL: "http://127.0.0.1:0/never",
		ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), // still valid
	})
	h := NewWithAuth(s, map[string]string{"claude": up.URL}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude/m"}`)))
	if gotAuth != "Bearer good-token" {
		t.Errorf("auth=%q, want the existing token (no refresh)", gotAuth)
	}
}
