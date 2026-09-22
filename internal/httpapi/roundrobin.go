package httpapi

import (
	"bytes"
	"io"
	"net/http"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/upstream"
)

// roundRobin forwards a request to the provider named in the path. It picks one
// active account, rotating the start across calls so load spreads, and fails
// over to the next account when an account returns a busy status.
func (a *api) roundRobin(w http.ResponseWriter, r *http.Request) {
	prov := r.PathValue("provider")
	if _, ok := provider.Lookup(prov); !ok {
		writeError(w, http.StatusNotFound, "provider not supported in this build")
		return
	}
	targets := a.activeConnections(prov)
	if len(targets) == 0 {
		writeError(w, http.StatusNotFound, "no active account for this provider")
		return
	}
	a.failover(w, r, targets, a.nextIndex("p:"+prov, len(targets)))
}

// failover tries the targets in order from start, wrapping around, and relays
// the first answer that is not a busy status. The last target's answer is
// relayed whatever it is, so the caller sees the real upstream error.
func (a *api) failover(w http.ResponseWriter, r *http.Request, targets []store.Connection, start int) {
	// The body is replayed to each account it fails over to, so buffer it once.
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body")
		return
	}

	tried := 0
	for i := 0; i < len(targets); i++ {
		conn := targets[(start+i)%len(targets)]
		p, ok := provider.Lookup(conn.Provider)
		if !ok {
			continue
		}
		secret, err := a.secretFor(r.Context(), conn.ID)
		if err != nil {
			continue
		}
		out, err := a.newOutbound(r, p, conn.Provider, secret, bytes.NewReader(body))
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
		a.relay(w, resp, conn.ID)
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

// nextIndex returns the round-robin start index for a key and advances it.
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
