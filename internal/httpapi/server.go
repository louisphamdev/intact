// Package httpapi serves the local API and the dashboard.
package httpapi

import (
	"net/http"

	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/web"
)

type api struct {
	store *store.Store
	// baseOverride redirects a provider to a test server. Nil in production.
	baseOverride map[string]string
}

// New builds the route table.
func New(s *store.Store, baseOverride map[string]string) http.Handler {
	a := &api{store: s, baseOverride: baseOverride}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /p/{id}/{path...}", a.proxy)
	mux.HandleFunc("GET /accounts", a.accounts)
	// "{$}" matches the root and nothing else. A bare "/" would be a catch-all
	// and would answer every mistyped path with the dashboard.
	mux.HandleFunc("GET /{$}", a.dashboard)
	return mux
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
