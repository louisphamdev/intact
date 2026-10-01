package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// stubFinder replaces the secret lookup of a fresh install, so no test downloads
// the Antigravity app.
func stubFinder(t *testing.T, secret string, err error) *int {
	t.Helper()
	calls := new(int)
	orig := findAntigravitySecret
	t.Cleanup(func() { findAntigravitySecret = orig })
	findAntigravitySecret = func(context.Context) (string, string, error) {
		*calls++
		return secret, "test", err
	}
	return calls
}

// A fresh install finds the secret by itself on the first sign-in, keeps it, and
// does not look again.
func TestAntigravitySignInOnAFreshInstallFindsTheSecret(t *testing.T) {
	t.Setenv("INTACT_ANTIGRAVITY_CLIENT_SECRET", "")
	calls := stubFinder(t, "GOCSPX-found", nil)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	a, h := newServer(s, nil, nil)
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, loopbackRequest("POST", "/oauth/antigravity/start", strings.NewReader(`{}`)))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "accounts.google.com") {
			t.Fatalf("start %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	if *calls != 1 {
		t.Fatalf("the lookup ran %d times, want 1", *calls)
	}
	if got := a.antigravityClientSecret(); got != "GOCSPX-found" {
		t.Fatalf("install secret %q, want the found one", got)
	}
	// The found secret survives a restart: a new server on the same store has it.
	a2, _ := newServer(s, nil, nil)
	if got := a2.antigravityClientSecret(); got != "GOCSPX-found" {
		t.Fatalf("after restart %q, want the found one", got)
	}
}

// When the lookup fails, the sign-in stops before Google and names the variable
// that a person can set instead.
func TestAntigravitySignInWithNoSecretFoundNamesTheVariable(t *testing.T) {
	t.Setenv("INTACT_ANTIGRAVITY_CLIENT_SECRET", "")
	stubFinder(t, "", errors.New("repository unreachable"))
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	_, h := newServer(s, nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/oauth/antigravity/start", strings.NewReader(`{}`)))
	body := rec.Body.String()
	if rec.Code != http.StatusBadRequest || !strings.Contains(body, "INTACT_ANTIGRAVITY_CLIENT_SECRET") ||
		!strings.Contains(body, "repository unreachable") {
		t.Fatalf("start: %d %s", rec.Code, body)
	}
}

func TestAntigravitySecretFromTheEnvironmentWins(t *testing.T) {
	t.Setenv("INTACT_ANTIGRAVITY_CLIENT_SECRET", "GOCSPX-from-env")
	calls := stubFinder(t, "GOCSPX-found", nil)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	a, h := newServer(s, nil, nil)
	if got := a.antigravityClientSecret(); got != "GOCSPX-from-env" {
		t.Fatalf("got %q, want the environment value", got)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/oauth/antigravity/start", strings.NewReader(`{}`)))
	if rec.Code != http.StatusOK || *calls != 0 {
		t.Fatalf("start: %d, lookups %d", rec.Code, *calls)
	}
}
