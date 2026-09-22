package httpapi

import (
	"encoding/json"
	"net/http"
	"path"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/web"
)

// setActive turns a connection on or off from a JSON body {"active":bool}.
func (a *api) setActive(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Active bool `json:"active"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad json")
		return
	}
	if err := a.store.SetActive(r.PathValue("id"), body.Active); err != nil {
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}
	writeJSON(w, map[string]any{"ok": true, "active": body.Active})
}

// providers lists the providers this build can proxy and what a new
// connection of each needs, so the dashboard can offer them and mark a stored
// connection it cannot reach.
func (a *api) providers(w http.ResponseWriter, r *http.Request) {
	type info struct {
		ID    string `json:"id"`
		Setup string `json:"setup"`
		// Auth is "oauth" for an account that signs in as a real tool, or
		// "apikey" for a documented API reached with a key.
		Auth string `json:"auth"`
		Icon bool   `json:"icon"`
	}
	out := []info{}
	for _, id := range provider.IDs() {
		p, _ := provider.Lookup(id)
		setup := p.Setup
		if setup == "" {
			setup = "key"
		}
		auth := "apikey"
		if p.Setup == "oauth" || p.Exchange != "" {
			auth = "oauth"
		}
		_, err := web.Files.Open("icons/" + id + ".png")
		out = append(out, info{ID: id, Setup: setup, Auth: auth, Icon: err == nil})
	}
	writeJSON(w, map[string]any{"providers": out})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// providerModelList serves a provider's models from the same catalog as
// /v1/models (cached, filtered, with the provider's fallback list).
func (a *api) providerModelList(w http.ResponseWriter, r *http.Request) {
	prov := r.PathValue("id")
	ids := a.providerModels(r.Context(), prov)
	a.cat.mu.Lock()
	ok := a.cat.m[prov].ok
	a.cat.mu.Unlock()
	if ids == nil {
		ids = []string{}
	}
	writeJSON(w, map[string]any{"models": ids, "ok": ok})
}

// icon serves a provider logo embedded in the binary.
func (a *api) icon(w http.ResponseWriter, r *http.Request) {
	b, err := web.Files.ReadFile("icons/" + path.Base(r.PathValue("name")))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(b)
}
