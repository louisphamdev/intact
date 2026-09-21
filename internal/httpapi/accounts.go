package httpapi

import (
	"encoding/json"
	"net/http"
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
