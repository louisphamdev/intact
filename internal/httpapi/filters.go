package httpapi

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"

	"github.com/louisphamdev/intact/internal/filter"
	"github.com/louisphamdev/intact/internal/store"
)

// filterCache holds the compiled rules per provider ("*" for all). It is
// rebuilt after every change made through the dashboard.
type filterCache struct {
	mu    sync.RWMutex
	ready bool
	by    map[string][]filter.Rule
}

// rulesFor returns the enabled rules that apply to a provider.
func (a *api) rulesFor(prov string) []filter.Rule {
	a.filters.mu.RLock()
	if a.filters.ready {
		out := append(append([]filter.Rule{}, a.filters.by["*"]...), a.filters.by[prov]...)
		a.filters.mu.RUnlock()
		return out
	}
	a.filters.mu.RUnlock()
	a.reloadFilters()
	return a.rulesFor(prov)
}

// reloadFilters compiles the stored filters. A rule that no longer compiles is
// skipped and logged rather than blocking every request.
func (a *api) reloadFilters() {
	list, err := a.store.ListFilters()
	by := map[string][]filter.Rule{}
	if err != nil {
		log.Printf("load filters: %v", err)
	}
	for _, f := range list {
		if !f.Enabled {
			continue
		}
		// A body-wiping rule stored before the guard existed must not apply now.
		if why := unsafeFilter(f.Kind, f.Pattern); why != "" {
			log.Printf("skip unsafe filter %s (%s %q): %s", f.ID, f.Kind, f.Pattern, why)
			continue
		}
		r, err := filter.Compile(f.Kind, f.Pattern)
		if err != nil {
			log.Printf("skip filter %s: %v", f.ID, err)
			continue
		}
		by[f.Provider] = append(by[f.Provider], r)
	}
	a.filters.mu.Lock()
	a.filters.by, a.filters.ready = by, true
	a.filters.mu.Unlock()
}

// listFilters serves the filters as JSON.
func (a *api) listFilters(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListFilters()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read filters")
		return
	}
	writeJSON(w, map[string]any{"filters": list})
}

// saveFilter creates (no id) or replaces a filter from a JSON body.
func (a *api) saveFilter(w http.ResponseWriter, r *http.Request) {
	var f store.Filter
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&f); err != nil {
		writeError(w, http.StatusBadRequest, "bad json")
		return
	}
	if _, err := filter.Compile(f.Kind, f.Pattern); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if why := unsafeFilter(f.Kind, f.Pattern); why != "" {
		writeError(w, http.StatusBadRequest, why)
		return
	}
	saved, err := a.putFilter(f)
	if errors.Is(err, store.ErrFilterNotFound) {
		writeError(w, http.StatusNotFound, "unknown filter")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot save filter")
		return
	}
	writeJSON(w, saved)
}

// deleteFilter removes one filter.
func (a *api) deleteFilter(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteFilter(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, "unknown filter")
		return
	}
	a.reloadFilters()
	writeJSON(w, map[string]any{"ok": true})
}

// filterInPlace reports an enabled rule already stored for that provider. A
// review asks before it installs: a second row doubles the alert and grows the
// table on every pass.
func (a *api) filterInPlace(provider, kind, pattern string) bool {
	list, err := a.store.ListFilters()
	if err != nil {
		return false
	}
	for _, f := range list {
		if f.Enabled && f.Provider == provider && f.Kind == kind && f.Pattern == pattern {
			return true
		}
	}
	return false
}

// apiProviders lists the providers with their account counts for machines.
func (a *api) apiProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"providers": a.providerSummary()})
}
