package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/louisphamdev/intact/internal/provider"
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
	}
	out := []info{}
	for _, id := range provider.IDs() {
		p, _ := provider.Lookup(id)
		setup := p.Setup
		if setup == "" {
			setup = "key"
		}
		out = append(out, info{ID: id, Setup: setup})
	}
	writeJSON(w, map[string]any{"providers": out})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
