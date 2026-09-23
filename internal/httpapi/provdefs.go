package httpapi

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/louisphamdev/intact/internal/provider"
)

// loadDefs publishes the declared providers to the registry.
func (a *api) loadDefs() {
	defs, err := a.store.ProviderDefs()
	if err != nil {
		log.Printf("provider defs: %v", err)
		return
	}
	provider.SetDeclared(defs)
}

// migrateCustomEndpoints turns the custom endpoints of older versions (a base
// URL kept on each connection) into declared providers, once.
func (a *api) migrateCustomEndpoints() {
	conns, err := a.store.ListConnections()
	if err != nil {
		return
	}
	for _, c := range conns {
		if c.BaseURL == "" || provider.Builtin(c.Provider) {
			continue
		}
		if _, ok := provider.Declared(c.Provider); ok {
			continue
		}
		d := provider.Def{ID: c.Provider, Kind: provider.KindAPIKey, API: c.Meta["api"], BaseURL: c.BaseURL}
		if err := d.Normalize(); err != nil {
			log.Printf("provider defs: cannot migrate %s: %v", c.Provider, err)
			continue
		}
		if err := a.store.PutProviderDef(d); err != nil {
			log.Printf("provider defs: %v", err)
			continue
		}
		a.loadDefs()
		log.Printf("provider defs: %s is now a declared provider", c.Provider)
	}
}

// maskDef hides the credentials a declared provider carries: the OAuth client
// secret, and the static header values, which hold an organisation key. It
// copies what it changes, so the stored def and the registry keep their value.
func maskDef(d provider.Def) provider.Def {
	if d.OAuth != nil && d.OAuth.ClientSecret != "" {
		o := *d.OAuth
		o.ClientSecret = maskedValue
		d.OAuth = &o
	}
	if len(d.Headers) > 0 {
		h := make(map[string]string, len(d.Headers))
		for k := range d.Headers {
			h[k] = maskedValue
		}
		d.Headers = h
	}
	return d
}

// listDefs serves the declared providers.
func (a *api) listDefs(w http.ResponseWriter, r *http.Request) {
	defs, err := a.store.ProviderDefs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read providers")
		return
	}
	if !principalOf(r).admin {
		for i := range defs {
			defs[i] = maskDef(defs[i])
		}
	}
	writeJSON(w, map[string]any{"providers": defs})
}

// getDef serves one declared provider.
func (a *api) getDef(w http.ResponseWriter, r *http.Request) {
	d, ok := provider.Declared(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no declared provider of that id")
		return
	}
	if !principalOf(r).admin {
		d = maskDef(d)
	}
	writeJSON(w, d)
}

// putDef declares a provider or replaces its declaration: a Def as JSON.
func (a *api) putDef(w http.ResponseWriter, r *http.Request) {
	var d provider.Def
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&d); err != nil {
		writeError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	saved, err := a.saveDef(d)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, saved)
}

func (a *api) saveDef(d provider.Def) (provider.Def, error) {
	d.Builtin = false
	if err := d.Normalize(); err != nil {
		return d, err
	}
	if err := a.store.PutProviderDef(d); err != nil {
		return d, err
	}
	a.loadDefs()
	// Its model list may live elsewhere now.
	a.cat.mu.Lock()
	delete(a.cat.m, d.ID)
	a.cat.mu.Unlock()
	return d, nil
}

// deleteDef removes a declaration; a provider with accounts is kept.
func (a *api) deleteDef(w http.ResponseWriter, r *http.Request) {
	if err := a.removeDef(r.PathValue("id")); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"deleted": true})
}

func (a *api) removeDef(id string) error {
	conns, _ := a.store.ListConnections()
	n := 0
	for _, c := range conns {
		if c.Provider == id {
			n++
		}
	}
	if n > 0 {
		return fmt.Errorf("delete its %d account(s) first", n)
	}
	if err := a.store.DeleteProviderDef(id); err != nil {
		return fmt.Errorf("no declared provider of that id")
	}
	a.loadDefs()
	return nil
}
