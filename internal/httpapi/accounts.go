package httpapi

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/upstream"
)

// accounts lists the connections so a caller can choose an id.
// The listing type carries no credential, which is what keeps this endpoint safe.
func (a *api) accounts(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListConnections()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read connections")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"accounts": list})
}

// createAccount stores a new connection from the dashboard form, then returns
// to the dashboard. The secret is taken by value and never echoed back.
func (a *api) createAccount(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "bad form")
		return
	}
	provider := r.PostFormValue("provider")
	secret := r.PostFormValue("secret")
	if provider == "" || secret == "" {
		writeError(w, http.StatusBadRequest, "provider and secret are required")
		return
	}
	if _, err := a.store.CreateConnection(provider, r.PostFormValue("label"), secret); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create connection")
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// deleteAccount removes one connection, then returns to the dashboard.
func (a *api) deleteAccount(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteConnection(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// usage serves the daily token counters, newest day first.
func (a *api) usage(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.Usage()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read usage")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"usage": rows})
}

// modelsForAccount fetches the provider's model list for one connection, from
// the server side so the dashboard (which holds a session, not the bearer token)
// can show it. It returns the provider's own JSON unchanged.
func (a *api) modelsForAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	conn, err := a.connection(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}
	p, ok := provider.Lookup(conn.Provider)
	if !ok {
		writeError(w, http.StatusNotFound, "provider not supported in this build")
		return
	}
	secret, err := a.store.Secret(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}
	base := p.BaseURL
	if over, ok := a.baseOverride[conn.Provider]; ok {
		base = over
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, base+"/models", nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad upstream target")
		return
	}
	for k, v := range p.Defaults {
		req.Header.Set(k, v)
	}
	for k, v := range p.Identity {
		req.Header.Set(k, v)
	}
	req.Header.Set(p.AuthHeader, p.AuthPrefix+secret)
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := upstream.Do(r.Context(), req, 2)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream unreachable")
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
