package httpapi

import (
	"encoding/json"
	"github.com/louisphamdev/intact/internal/drift"
	"github.com/louisphamdev/intact/internal/provider"
	"net/http"
	"sort"
	"strconv"

	"github.com/louisphamdev/intact/internal/store"
)

// driftChanges lists recorded structure changes, newest first.
// Query: provider, direction (request|response), unacked=1, since=<id>, limit.
func (a *api) driftChanges(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := a.store.ListShapeChanges(store.ShapeChangeFilter{Provider: q.Get("provider"), Direction: q.Get("direction"),
		Unacked: q.Get("unacked") == "1" || q.Get("unacked") == "true", SinceID: since, Limit: limit})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read changes")
		return
	}
	writeJSON(w, map[string]any{"changes": list, "unacked": a.store.CountUnackedShapeChanges()})
}

// driftAck marks changes as seen: {"ids":[…]}, or every change with no ids.
func (a *api) driftAck(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []int64 `json:"ids"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body)
	n, err := a.store.AckShapeChanges(body.IDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot acknowledge")
		return
	}
	writeJSON(w, map[string]any{"acked": n})
}

// driftFields lists the learned fields. Query: provider, direction, endpoint.
func (a *api) driftFields(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	list := a.drift.Fields(q.Get("direction"), q.Get("provider"), q.Get("endpoint"))
	sort.Slice(list, func(i, j int) bool {
		if list[i].Key != list[j].Key {
			return list[i].Key < list[j].Key
		}
		return list[i].Path < list[j].Path
	})
	writeJSON(w, map[string]any{"fields": list})
}

// watched reports whether a provider's traffic is watched for drift.
func watched(prov string) bool {
	p, ok := provider.Lookup(prov)
	return ok && p.Watch
}

// driftSeed learns reference documents without recording changes, e.g. the
// capture of a real tool: {"direction","provider","endpoint","sse",
// "documents":["…"]}. A stream document holds its whole event stream.
func (a *api) driftSeed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Direction string   `json:"direction"`
		Provider  string   `json:"provider"`
		Endpoint  string   `json:"endpoint"`
		SSE       bool     `json:"sse"`
		Documents []string `json:"documents"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad json")
		return
	}
	if (body.Direction != drift.Request && body.Direction != drift.Response) || body.Provider == "" || body.Endpoint == "" {
		writeError(w, http.StatusBadRequest, "direction (request|response), provider and endpoint are required")
		return
	}
	n := 0
	for _, d := range body.Documents {
		n += a.drift.Seed(body.Direction, body.Provider, body.Endpoint, []byte(d), body.SSE)
	}
	a.drift.Flush()
	writeJSON(w, map[string]any{"learned": n})
}
