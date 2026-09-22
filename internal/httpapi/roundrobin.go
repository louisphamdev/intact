package httpapi

import (
	"bytes"
	"io"
	"net/http"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/upstream"
)

// target is one account a failover run may try, with the model to put in the
// body for it (empty keeps the caller's body as sent).
type target struct {
	conn  store.Connection
	model string
}

// roundRobin forwards a request to the provider named in the path. It picks one
// active account, rotating the start across calls so load spreads, and fails
// over to the next account when an account returns a busy status.
func (a *api) roundRobin(w http.ResponseWriter, r *http.Request) {
	prov := r.PathValue("provider")
	if _, ok := provider.Lookup(prov); !ok {
		writeError(w, http.StatusNotFound, "provider not supported in this build")
		return
	}
	targets := []target{}
	for _, c := range a.activeConnections(prov) {
		targets = append(targets, target{conn: c})
	}
	if len(targets) == 0 {
		writeError(w, http.StatusNotFound, "no active account for this provider")
		return
	}
	a.failover(w, r, targets, a.nextIndex("p:"+prov, len(targets)))
}

// groupProxy forwards a request to a group. The strategy picks the member to
// try first (round-robin rotates it, fallback keeps the saved order); a member
// that stands for a whole provider expands to that provider's active accounts,
// rotated as /r rotates them. The run fails over through every account of every
// member on a busy status.
func (a *api) groupProxy(w http.ResponseWriter, r *http.Request) {
	g, err := a.store.GetGroup(r.PathValue("group"))
	if err != nil {
		writeError(w, http.StatusNotFound, "unknown group")
		return
	}
	conns, err := a.store.ListConnections()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read connections")
		return
	}
	// Each member becomes the list of accounts it may use, in the order to try.
	per := [][]target{}
	for _, m := range g.Members {
		if _, ok := provider.Lookup(m.Provider); !ok {
			continue
		}
		ts := []target{}
		for _, c := range conns {
			if !c.IsActive || c.Provider != m.Provider {
				continue
			}
			if m.ConnectionID != "" && c.ID != m.ConnectionID {
				continue
			}
			ts = append(ts, target{conn: c, model: m.Model})
		}
		if len(ts) == 0 {
			continue
		}
		if m.ConnectionID == "" && len(ts) > 1 {
			ts = rotate(ts, a.nextIndex("p:"+m.Provider, len(ts)))
		}
		per = append(per, ts)
	}
	if len(per) == 0 {
		writeError(w, http.StatusNotFound, "no active account in this group")
		return
	}
	if g.Strategy == store.StrategyRoundRobin {
		per = rotate(per, a.nextIndex("g:"+g.Name, len(per)))
	}
	targets := []target{}
	for _, ts := range per {
		targets = append(targets, ts...)
	}
	a.failover(w, r, targets, 0)
}

// rotate returns s starting at index i and wrapping around.
func rotate[T any](s []T, i int) []T {
	out := make([]T, 0, len(s))
	out = append(out, s[i:]...)
	return append(out, s[:i]...)
}

// failover tries the targets in order from start, wrapping around, and relays
// the first answer that is not a busy status. The last target's answer is
// relayed whatever it is, so the caller sees the real upstream error.
func (a *api) failover(w http.ResponseWriter, r *http.Request, targets []target, start int) {
	// The body is replayed to each account it fails over to, so buffer it once.
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body")
		return
	}

	tried := 0
	for i := 0; i < len(targets); i++ {
		t := targets[(start+i)%len(targets)]
		p, ok := provider.Lookup(t.conn.Provider)
		if !ok {
			continue
		}
		secret, err := a.secretFor(r.Context(), t.conn.ID)
		if err != nil {
			continue
		}
		send := body
		if t.model != "" {
			send, _ = setModel(body, t.model)
		}
		out, err := a.newOutbound(r, p, t.conn.Provider, secret, bytes.NewReader(send))
		if err != nil {
			continue
		}
		tried++
		resp, err := upstream.Do(r.Context(), out, 1)
		if err != nil {
			continue
		}
		// Fail over on a busy status only while another account remains.
		if retryableStatus(resp.StatusCode) && i < len(targets)-1 {
			resp.Body.Close()
			continue
		}
		defer resp.Body.Close()
		a.relay(w, resp, t.conn.ID)
		return
	}
	if tried == 0 {
		writeError(w, http.StatusInternalServerError, "no usable account")
		return
	}
	writeError(w, http.StatusBadGateway, "all accounts failed")
}

// activeConnections returns the active connections of one provider.
func (a *api) activeConnections(prov string) []store.Connection {
	list, err := a.store.ListConnections()
	if err != nil {
		return nil
	}
	out := []store.Connection{}
	for _, c := range list {
		if c.Provider == prov && c.IsActive {
			out = append(out, c)
		}
	}
	return out
}

// nextIndex returns the round-robin start index for a key and advances it. A
// provider and a group keep separate cursors ("p:groq", "g:fast").
func (a *api) nextIndex(key string, n int) int {
	a.rrMu.Lock()
	defer a.rrMu.Unlock()
	i := a.rrNext[key] % n
	a.rrNext[key] = (i + 1) % n
	return i
}

// retryableStatus reports a status that means "this account is busy, try another".
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests ||
		code == http.StatusInternalServerError ||
		code == http.StatusServiceUnavailable ||
		code == http.StatusConflict
}
