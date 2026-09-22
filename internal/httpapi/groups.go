package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
)

// groups lists every group with its members in order.
func (a *api) groups(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListGroups()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read groups")
		return
	}
	writeJSON(w, map[string]any{"groups": list})
}

// saveGroup creates or replaces a group from a JSON body:
// {"name":"fast","strategy":"round-robin","members":[{"connectionId":"…","model":"…"}]}.
func (a *api) saveGroup(w http.ResponseWriter, r *http.Request) {
	var g store.Group
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&g); err != nil {
		writeError(w, http.StatusBadRequest, "bad json")
		return
	}
	if g.Strategy == "" {
		g.Strategy = store.StrategyRoundRobin
	}
	if !store.ValidGroupName(g.Name) {
		writeError(w, http.StatusBadRequest, "name must be lowercase letters, digits, - or _")
		return
	}
	if !store.ValidStrategy(g.Strategy) {
		writeError(w, http.StatusBadRequest, "unknown strategy")
		return
	}
	if err := a.store.SaveGroup(g); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "a member is not a known connection")
			return
		}
		writeError(w, http.StatusInternalServerError, "cannot save group")
		return
	}
	saved, err := a.store.GetGroup(g.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read group")
		return
	}
	writeJSON(w, saved)
}

// deleteGroup removes a group; its connections stay.
func (a *api) deleteGroup(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteGroup(r.PathValue("name")); err != nil {
		writeError(w, http.StatusNotFound, "unknown group")
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

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
