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

// A token revoked before its recorded expiry: the provider returns 401, intact
// force-refreshes and retries, and the caller sees the successful response.
func TestProxySelfHealsOn401(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"good-token","expires_in":3600}`))
	}))
	defer tokenSrv.Close()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer good-token" {
			w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"revoked"}`))
	}))
	defer up.Close()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("claude", "P", "revoked-token")
	s.SetOAuth(c.ID, store.OAuthCreds{
		RefreshToken: "rt", TokenURL: tokenSrv.URL, ClientID: "cid",
		ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), // "valid" per record, but revoked upstream
	})

	h := NewWithAuth(s, map[string]string{"claude": up.URL}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude/m"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200 after self-heal; body=%s", rec.Code, rec.Body.String())
	}
	if sec, _ := s.Secret(c.ID); sec != "good-token" {
		t.Errorf("stored secret=%q, want the refreshed token", sec)
	}
}
