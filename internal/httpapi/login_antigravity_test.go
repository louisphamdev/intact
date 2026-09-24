package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// The source carries no client secret: a fresh install asks for one, by name, before it
// sends the user to Google.
func TestAntigravitySignInOnAFreshInstallNamesTheSecret(t *testing.T) {
	t.Setenv("INTACT_ANTIGRAVITY_CLIENT_SECRET", "")
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	a, h := newServer(s, nil, nil)
	if got := a.antigravityClientSecret(); got != "" {
		t.Fatalf("fresh install got a secret %q, want none", got)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/oauth/antigravity/start", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "INTACT_ANTIGRAVITY_CLIENT_SECRET") {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAntigravitySecretFromTheEnvironmentWins(t *testing.T) {
	t.Setenv("INTACT_ANTIGRAVITY_CLIENT_SECRET", "GOCSPX-from-env")
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	a, _ := newServer(s, nil, nil)
	if got := a.antigravityClientSecret(); got != "GOCSPX-from-env" {
		t.Fatalf("got %q, want the environment value", got)
	}
}
