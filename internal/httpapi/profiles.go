package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
)

// A profile is a named group of one provider's accounts. Its model prefix is
// "<provider>-<name>": "groq-lite/llama" names provider groq, profile lite,
// and only the members of that profile serve the call. The provider's own
// prefix ("groq/llama") keeps serving every account, so a profile splits
// accounts off for some callers without moving them away from the others.

// listProfiles answers GET /api/profiles with every profile.
func (a *api) listProfiles(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListProfiles()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read profiles")
		return
	}
	writeJSON(w, map[string]any{"profiles": list})
}

// createProfile stores a profile: {"provider","name","members"}. Members are
// connection ids of that provider, in the order they should serve.
func (a *api) createProfile(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Provider string   `json:"provider"`
		Name     string   `json:"name"`
		Members  []string `json:"members"`
	}
	if !readBody(w, r, &b) {
		return
	}
	if b.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if !a.checkProfile(w, b.Provider, b.Name, b.Members) {
		return
	}
	p, err := a.store.CreateProfile(b.Provider, b.Name, b.Members)
	if errors.Is(err, store.ErrProfileExists) {
		writeError(w, http.StatusConflict, b.Provider+" already has a profile named "+b.Name)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot store the profile")
		return
	}
	writeJSON(w, p)
}

// updateProfile changes a profile's name, members or both: fields left out
// keep their value.
func (a *api) updateProfile(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name    *string   `json:"name"`
		Members *[]string `json:"members"`
	}
	if !readBody(w, r, &b) {
		return
	}
	id := r.PathValue("id")
	p, err := a.store.Profile(id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no profile has that id")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read the profile")
		return
	}
	name := ""
	if b.Name != nil {
		name = *b.Name
	}
	var members []string
	if b.Members != nil {
		members = *b.Members
	}
	if !a.checkProfile(w, p.Provider, name, members) {
		return
	}
	p, err = a.store.UpdateProfile(id, name, members)
	if errors.Is(err, store.ErrProfileExists) {
		writeError(w, http.StatusConflict, p.Provider+" already has a profile named "+name)
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no profile has that id")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot store the profile")
		return
	}
	writeJSON(w, p)
}

// deleteProfile removes a profile. The accounts stay: a profile is a routing
// group, never the accounts themselves.
func (a *api) deleteProfile(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteProfile(r.PathValue("id")); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no profile has that id")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot delete the profile")
		return
	}
	writeJSON(w, map[string]any{"deleted": r.PathValue("id")})
}

// checkProfile reports whether a profile's fields fit the routing. It writes
// the refusal itself and returns false when one does not. An empty name means
// "keep the current one" (an update that only moves members); createProfile
// checks for itself that its name is there.
func (a *api) checkProfile(w http.ResponseWriter, prov, name string, members []string) bool {
	if prov == "" {
		writeError(w, http.StatusBadRequest, "provider is required")
		return false
	}
	if !a.knownProvider(prov) {
		writeError(w, http.StatusBadRequest, "provider "+prov+" is not registered")
		return false
	}
	if name != "" {
		if !customID.MatchString(name) {
			writeError(w, http.StatusBadRequest, "profile name must be lowercase letters, digits or -")
			return false
		}
		// The prefix the profile builds must not collide with a provider id: a
		// real provider's prefix wins the routing, so such a profile would never
		// be reached.
		if _, ok := provider.Lookup(prov + "-" + name); ok {
			writeError(w, http.StatusBadRequest, "a provider named "+prov+"-"+name+" already exists; pick another profile name")
			return false
		}
	}
	if len(members) > 0 {
		byID := map[string]store.Connection{}
		if list, err := a.store.ListConnections(); err == nil {
			for _, c := range list {
				byID[c.ID] = c
			}
		}
		seen := map[string]bool{}
		for _, id := range members {
			if seen[id] {
				continue
			}
			seen[id] = true
			c, ok := byID[id]
			if !ok {
				writeError(w, http.StatusBadRequest, "no account has the id "+id)
				return false
			}
			if c.Provider != prov {
				writeError(w, http.StatusBadRequest, "account "+c.Label+" serves "+c.Provider+", not "+prov)
				return false
			}
		}
	}
	return true
}

// readBody decodes a JSON request body of a set size into v.
func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad json")
		return false
	}
	return true
}

// routeProfile reports the profile a model prefix names, when it names one:
// "<provider>-<profile>/<model>". It runs after splitModel, so a real
// provider's prefix wins and a profile only fills what it leaves.
func (a *api) routeProfile(model string) (store.Profile, bool) {
	i := strings.IndexByte(model, '/')
	if i <= 0 {
		return store.Profile{}, false
	}
	prefix := model[:i]
	list, err := a.store.ListProfiles()
	if err != nil {
		return store.Profile{}, false
	}
	for _, p := range list {
		if p.Provider+"-"+p.Name == prefix {
			return p, true
		}
	}
	return store.Profile{}, false
}

// profileMembers returns the profile's active members: its provider's
// connections it names, in the order its list has. A member that is gone or
// switched off just does not serve.
func (a *api) profileMembers(p store.Profile) []store.Connection {
	list, err := a.store.ListConnections()
	if err != nil {
		return nil
	}
	byID := map[string]store.Connection{}
	for _, c := range list {
		if c.Provider == p.Provider {
			byID[c.ID] = c
		}
	}
	out := []store.Connection{}
	for _, id := range p.Members {
		if c, ok := byID[id]; ok && c.IsActive {
			out = append(out, c)
		}
	}
	return out
}
