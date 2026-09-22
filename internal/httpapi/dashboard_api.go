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

// providers lists the provider ids this build can proxy, so the dashboard can
// offer them and mark a stored connection whose provider is not wired.
func (a *api) providers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"providers": provider.IDs()})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
