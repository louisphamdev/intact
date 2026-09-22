// Package httpapi serves the local API and the dashboard.
package httpapi

import (
	"net/http"
	"sync"

	"github.com/louisphamdev/intact/internal/auth"
	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/web"
)

const sessionCookie = "intact_session"

type api struct {
	store *store.Store
	// baseOverride redirects a provider to a test server. Nil in production.
	baseOverride map[string]string
	// auth gates the server. Nil means no gate (loopback use).
	auth *auth.Config
	// rrMu guards rrNext, the per-provider round-robin cursor.
	rrMu   sync.Mutex
	rrNext map[string]int
}

// New builds the route table with no authentication (loopback use and tests).
func New(s *store.Store, baseOverride map[string]string) http.Handler {
	return NewWithAuth(s, baseOverride, nil)
}

// NewWithAuth builds the route table. When authCfg is not nil, a person must log
// in with a password and a TOTP code to reach the dashboard, and a machine must
// present the bearer token to use a provider.
func NewWithAuth(s *store.Store, baseOverride map[string]string, authCfg *auth.Config) http.Handler {
	a := &api{store: s, baseOverride: baseOverride, auth: authCfg, rrNext: map[string]int{}}
	mux := http.NewServeMux()
	// No method prefix: a provider exposes GET endpoints too, and a passthrough
	// that only accepts POST is not a passthrough.
	mux.HandleFunc("/p/{id}/{path...}", a.requireToken(a.proxy))
	// Round-robin: choose an active account of the provider and fail over.
	mux.HandleFunc("/r/{provider}/{path...}", a.requireToken(a.roundRobin))
	mux.HandleFunc("GET /accounts", a.requireSession(a.accounts))
	mux.HandleFunc("POST /accounts", a.requireSession(a.createAccount))
	mux.HandleFunc("POST /accounts/{id}/delete", a.requireSession(a.deleteAccount))
	mux.HandleFunc("GET /accounts/{id}/models", a.requireSession(a.modelsForAccount))
	mux.HandleFunc("POST /accounts/{id}/active", a.requireSession(a.setActive))
	mux.HandleFunc("GET /providers", a.requireSession(a.providers))
	mux.HandleFunc("GET /usage", a.requireSession(a.usage))
	mux.HandleFunc("GET /login", a.loginForm)
	mux.HandleFunc("POST /login", a.loginSubmit)
	mux.HandleFunc("POST /logout", a.logout)
	// "{$}" matches the root and nothing else. A bare "/" would be a catch-all
	// and would answer every mistyped path with the dashboard.
	mux.HandleFunc("GET /{$}", a.requireSession(a.dashboard))
	return mux
}

// requireSession lets a request through when auth is off or the session cookie
// is valid; otherwise it redirects a browser to the login page.
func (a *api) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.auth == nil {
			next(w, r)
			return
		}
		if ck, err := r.Cookie(sessionCookie); err == nil && a.auth.ValidSession(ck.Value) {
			next(w, r)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

// requireToken lets a request through when auth is off or the bearer token
// matches; otherwise it answers 401 for the machine caller.
func (a *api) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.auth == nil || a.auth.CheckAPIToken(r.Header.Get("Authorization")) {
			next(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
	}
}

// dashboard serves the page embedded in the binary.
func (a *api) dashboard(w http.ResponseWriter, r *http.Request) {
	b, err := web.Files.ReadFile("index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "dashboard missing from this build")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}
