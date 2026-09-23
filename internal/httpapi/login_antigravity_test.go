package httpapi

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// A first Antigravity sign-in must work on a fresh install: no imported account,
// no environment variable, no Antigravity CLI on the host.
func TestAntigravitySecretOnAFreshInstall(t *testing.T) {
	t.Setenv("INTACT_ANTIGRAVITY_CLIENT_SECRET", "")
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	a, _ := newServer(s, nil, nil)
	if got := a.antigravityClientSecret(); !strings.HasPrefix(got, "GOCSPX-") {
		t.Fatalf("fresh install got %q, want the built-in installed-app secret", got)
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
