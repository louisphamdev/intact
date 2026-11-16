package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/translate"
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

// createProfile stores a profile: {"provider","name","members","models"}.
// Members are connection ids of that provider, in the order they should
// serve; models are the model ids the profile serves (empty or left out
// means everything its provider does).
func (a *api) createProfile(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Provider string   `json:"provider"`
		Name     string   `json:"name"`
		Members  []string `json:"members"`
		Models   []string `json:"models"`
	}
	if !readBody(w, r, &b) {
		return
	}
	if b.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if !a.checkProfile(w, b.Provider, b.Name, b.Members, b.Models) {
		return
	}
	p, err := a.store.CreateProfile(b.Provider, b.Name, b.Members, b.Models)
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

// updateProfile changes a profile's name, members, models or any of them;
// fields left out keep their value.
func (a *api) updateProfile(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name    *string   `json:"name"`
		Members *[]string `json:"members"`
		Models  *[]string `json:"models"`
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
	name, members, models := "", []string(nil), []string(nil)
	if b.Name != nil {
		name = *b.Name
	}
	if b.Members != nil {
		members = *b.Members
	}
	if b.Models != nil {
		models = *b.Models
	}
	if !a.checkProfile(w, p.Provider, name, members, models) {
		return
	}
	p, err = a.store.UpdateProfile(id, store.ProfileUpdate{
		Name: b.Name, Members: b.Members, Models: b.Models})
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
// checks for itself that its name is there. A nil members or models list
// keeps its value too; an empty one sets that empty value.
func (a *api) checkProfile(w http.ResponseWriter, prov, name string, members, models []string) bool {
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
	// A model list of its own is checked against the provider's, so a profile
	// cannot be configured with a model no account of it serves.
	if len(models) > 0 {
		known := map[string]bool{}
		if list, err := a.store.ListModels(prov); err == nil {
			for _, m := range list {
				known[m.Model] = true
			}
		}
		for _, id := range models {
			if !known[id] {
				if base, _ := splitVariant(id); base == id || !known[base] {
					writeError(w, http.StatusBadRequest, "model "+id+" is not in "+prov+"'s model list")
					return false
				}
			}
		}
	}
	return true
}

// profileServes reports whether a profile serves model. With no list of its
// own it serves everything its provider does; with one, the model must be
// listed or be a level variant of a listed one ("gemini-3.8-flash" serves
// "gemini-3.8-flash-high").
func profileServes(p store.Profile, model string) bool {
	if len(p.Models) == 0 {
		return true
	}
	if slices.Contains(p.Models, model) {
		return true
	}
	base, _ := splitVariant(model)
	return base != model && slices.Contains(p.Models, base)
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

// profileEntries lists a profile's models under its own prefix
// ("<provider>-<profile>/<model>"), in the provider's own order. With no
// list of its own it lists the provider's whole list; with one it lists the
// ids it names, the folded bases a level variant refers to included.
func (a *api) profileEntries(ctx context.Context, p store.Profile) []modelEntry {
	ids := a.profileModelIDs(ctx, p)
	out := make([]modelEntry, 0, len(ids))
	for _, e := range a.entriesOf(p.Provider, ids) {
		e.ID = p.Provider + "-" + p.Name + "/" + strings.TrimPrefix(e.ID, p.Provider+"/")
		out = append(out, e)
	}
	return out
}

// profileModelIDs returns the models a profile serves: its own list when it
// has one, else everything its provider does. The ids come from the stored
// model table, which is what a profile's list is checked against, so a
// profile never serves an id the provider's own list does not carry.
func (a *api) profileModelIDs(ctx context.Context, p store.Profile) []string {
	if len(p.Models) == 0 {
		return a.providerModels(ctx, p.Provider)
	}
	list, err := a.store.ListModels(p.Provider)
	if err != nil {
		return nil
	}
	a.cat.mu.Lock()
	groups := a.cat.m[p.Provider].groups
	a.cat.mu.Unlock()
	out := []string{}
	for _, m := range list {
		if !m.Active {
			continue
		}
		if profileHasModel(p.Models, m.Model, groups) {
			out = append(out, m.Model)
		}
	}
	return out
}

// profileHasModel reports whether a profile's list covers a provider's model
// id: the id is listed, or its folded base is, so "gemini-3.8-flash" in a
// list keeps the "gemini-3.8-flash-high" the provider offers.
func profileHasModel(want []string, id string, groups map[string]variantSet) bool {
	if slices.Contains(want, id) {
		return true
	}
	// A folded base (a level variant folded into it) is listed by its own
	// base id, so the provider's -high of it keeps serving.
	if g, ok := groups[id]; ok {
		_ = g
		return slices.Contains(want, id)
	}
	base, _ := splitVariant(id)
	return base != id && slices.Contains(want, base)
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

// profileModelsHandler answers GET /api/profiles/{id}/models with the
// models a profile serves, read fresh from one of its own accounts when
// ?refresh=1 is set. The ids the provider's list gives are filtered by the
// profile's own list, so a profile with one sees only its own models.
func (a *api) profileModelsHandler(w http.ResponseWriter, r *http.Request) {
	p, err := a.store.Profile(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no profile has that id")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read the profile")
		return
	}
	if r.URL.Query().Get("refresh") != "" {
		if ids, _, ok := a.fetchProfileModels(r.Context(), p); ok {
			a.catalogIDs(r.Context(), p.Provider) // refresh the shared cache too
			_ = ids
		}
	}
	writeJSON(w, map[string]any{"models": a.profileModelIDs(r.Context(), p)})
}

// fetchProfileModels reads one of the profile's own accounts' model list
// straight from the provider: the profile fetches through its members, not
// through whichever account of the provider the catalog picked.
func (a *api) fetchProfileModels(ctx context.Context, p store.Profile) ([]string, []byte, bool) {
	conns := a.profileMembers(p)
	if len(conns) == 0 {
		return nil, nil, false
	}
	pr, ok := a.providerFor(conns[0])
	if !ok {
		return nil, nil, false
	}
	if pr.API == translate.Antigravity {
		ids, raw, ok := a.antigravityModels(ctx, conns[0])
		if ok {
			ids, _ = groupVariants(ids)
		}
		return ids, raw, ok
	}
	if pr.NoModelList {
		return append([]string(nil), pr.Models...), nil, true
	}
	return a.fetchModelIDs(ctx, conns[0])
}

// profileRotationHandler answers GET /api/profiles/{id}/rotation with whose
// turn it is inside the profile: its own cursor, which moves apart from the
// provider's and from every other profile's.
func (a *api) profileRotationHandler(w http.ResponseWriter, r *http.Request) {
	p, err := a.store.Profile(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no profile has that id")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read the profile")
		return
	}
	writeJSON(w, a.profileRotationState(p))
}

// profileRotationState is a profile's rotation: the provider's rotation
// (mode, sticky, order) applied to the profile's members, plus whose turn it
// is on the profile's own cursor.
func (a *api) profileRotationState(p store.Profile) map[string]any {
	rot := a.rotation(p.Provider)
	out := map[string]any{"rotation": rot}
	conns := rot.ordered(a.profileMembers(p))
	if len(conns) == 0 {
		return out
	}
	if rot.Mode == RotateFallback {
		conns, _ = withStandbyLast(conns, func(int) int { return 0 })
		out["next"] = conns[0].ID
		return out
	}
	a.rrMu.Lock()
	c := a.rrNext["p:"+p.Provider+"@"+p.Name]
	a.rrMu.Unlock()
	conns, i := withStandbyLast(conns, func(n int) int { return c.i % n })
	out["next"], out["used"] = conns[i].ID, c.used
	return out
}
