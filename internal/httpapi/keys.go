package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
)

// API keys are managed from the dashboard only: a machine holding one key must
// not be able to mint more.

func (a *api) listKeys(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListAPIKeys()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read api keys")
		return
	}
	writeJSON(w, map[string]any{"keys": list})
}

// createKey makes a key from {"name": "…"} and returns it in full once.
func (a *api) createKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body)
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "key"
	}
	k, err := a.store.CreateAPIKey(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create api key")
		return
	}
	writeJSON(w, k)
}

func (a *api) revealKey(w http.ResponseWriter, r *http.Request) {
	k, err := a.store.RevealAPIKey(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "unknown api key")
		return
	}
	writeJSON(w, map[string]any{"key": k})
}

func (a *api) setKeyActive(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Active bool `json:"active"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad json")
		return
	}
	if err := a.store.SetAPIKeyEnabled(r.PathValue("id"), body.Active); err != nil {
		writeError(w, http.StatusNotFound, "unknown api key")
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *api) deleteKey(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteAPIKey(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, "unknown api key")
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
