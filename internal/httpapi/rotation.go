package httpapi

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/louisphamdev/intact/internal/store"
)

// Rotation is how a provider's accounts take turns:
//   - round-robin: each account serves Sticky requests in a row, then the next
//     one takes over (Sticky 1 turns on every request);
//   - fallback: the first account in Order serves every request, and the next
//     one only when it fails or is busy.
//
// Order is the accounts' priority, first first; accounts it does not name
// follow in the order they were added.
type Rotation struct {
	Mode   string   `json:"mode"`
	Sticky int      `json:"sticky"`
	Order  []string `json:"order"`
}

const (
	RotateRoundRobin = "round-robin"
	RotateFallback   = "fallback"
)

// rrCursor is a rotation's place: the account index and how many requests it
// has served in a row.
type rrCursor struct{ i, used int }

func rotationKey(prov string) string { return "rotation:" + prov }

func (a *api) rotation(prov string) Rotation {
	r := Rotation{Mode: RotateRoundRobin, Sticky: 1}
	if v, _ := a.store.GetSetting(rotationKey(prov)); v != "" {
		json.Unmarshal([]byte(v), &r)
	}
	if r.Mode != RotateFallback {
		r.Mode = RotateRoundRobin
	}
	if r.Sticky < 1 {
		r.Sticky = 1
	}
	return r
}

// ordered sorts a provider's connections by the rotation's priority.
func (r Rotation) ordered(conns []store.Connection) []store.Connection {
	pos := map[string]int{}
	for i, id := range r.Order {
		pos[id] = i
	}
	out := append([]store.Connection(nil), conns...)
	sort.SliceStable(out, func(i, j int) bool {
		pi, iok := pos[out[i].ID]
		pj, jok := pos[out[j].ID]
		switch {
		case iok && jok:
			return pi < pj
		case iok != jok:
			return iok
		}
		return false
	})
	return out
}

// startFor picks the first account a request tries and advances the
// rotation. One provider's accounts follow its rotation; a pool across
// providers (a bare model several providers list) turns on every request,
// per model.
func (a *api) startFor(model string, targets []store.Connection) ([]store.Connection, int) {
	prov := targets[0].Provider
	for _, c := range targets[1:] {
		if c.Provider != prov {
			return targets, a.advance("m:"+model, len(targets), 1)
		}
	}
	rot := a.rotation(prov)
	targets = rot.ordered(targets)
	if rot.Mode == RotateFallback {
		return targets, 0
	}
	return targets, a.advance("p:"+prov, len(targets), rot.Sticky)
}

// advance returns the cursor's account for this request and moves it on
// once that account has served sticky requests in a row.
func (a *api) advance(key string, n, sticky int) int {
	a.rrMu.Lock()
	defer a.rrMu.Unlock()
	c := a.rrNext[key]
	c.i %= n
	i := c.i
	c.used++
	if c.used >= sticky {
		c.i, c.used = (c.i+1)%n, 0
	}
	a.rrNext[key] = c
	return i
}

// getRotation serves a provider's rotation and whose turn it is:
// {"rotation":{…},"next":"<connection id>","used":n}.
func (a *api) getRotation(w http.ResponseWriter, r *http.Request) {
	prov := r.PathValue("id")
	writeJSON(w, a.rotationState(prov))
}

func (a *api) rotationState(prov string) map[string]any {
	rot := a.rotation(prov)
	conns := rot.ordered(a.activeConnections(prov))
	out := map[string]any{"rotation": rot}
	if len(conns) == 0 {
		return out
	}
	if rot.Mode == RotateFallback {
		out["next"] = conns[0].ID
		return out
	}
	a.rrMu.Lock()
	c := a.rrNext["p:"+prov]
	a.rrMu.Unlock()
	out["next"], out["used"] = conns[c.i%len(conns)].ID, c.used
	return out
}

// setRotation stores {"mode","sticky","order"}; fields left out keep their
// value.
func (a *api) setRotation(w http.ResponseWriter, r *http.Request) {
	prov := r.PathValue("id")
	var b struct {
		Mode   *string   `json:"mode"`
		Sticky *int      `json:"sticky"`
		Order  *[]string `json:"order"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "bad json")
		return
	}
	rot := a.rotation(prov)
	if b.Mode != nil {
		if *b.Mode != RotateRoundRobin && *b.Mode != RotateFallback {
			writeError(w, http.StatusBadRequest, "mode is round-robin or fallback")
			return
		}
		rot.Mode = *b.Mode
	}
	if b.Sticky != nil {
		if *b.Sticky < 1 || *b.Sticky > 1000 {
			writeError(w, http.StatusBadRequest, "sticky is 1 to 1000")
			return
		}
		rot.Sticky = *b.Sticky
	}
	if b.Order != nil {
		rot.Order = *b.Order
	}
	raw, _ := json.Marshal(rot)
	if err := a.store.SetSetting(rotationKey(prov), string(raw)); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot save")
		return
	}
	// A new rotation starts from its first account.
	a.rrMu.Lock()
	delete(a.rrNext, "p:"+prov)
	a.rrMu.Unlock()
	writeJSON(w, a.rotationState(prov))
}
