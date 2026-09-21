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
	p, ok := provider.Lookup(prov)
	if !ok {
		writeError(w, http.StatusNotFound, "provider not supported in this build")
		return
	}
	conns := a.activeConnections(prov)
	if len(conns) == 0 {
		writeError(w, http.StatusNotFound, "no active account for this provider")
		return
	}
	// The body is replayed to each account it fails over to, so buffer it once.
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body")
		return
	}

	start := a.nextIndex(prov, len(conns))
	tried := 0
	for i := 0; i < len(conns); i++ {
		conn := conns[(start+i)%len(conns)]
		secret, err := a.secretFor(r.Context(), conn.ID)
		if err != nil {
			continue
		}
		out, err := a.newOutbound(r, p, prov, secret, bytes.NewReader(body))
		if err != nil {
			continue
		}
		tried++
		resp, err := upstream.Do(r.Context(), out, 1)
		if err != nil {
			continue
		}
		// Fail over on a busy status only while another account remains.
		if retryableStatus(resp.StatusCode) && i < len(conns)-1 {
			resp.Body.Close()
			continue
		}
		defer resp.Body.Close()
		a.relay(w, resp, conn.ID)
		return
	}
	if tried == 0 {
		writeError(w, http.StatusInternalServerError, "no usable account for this provider")
		return
	}
	writeError(w, http.StatusBadGateway, "all accounts for this provider failed")
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

// nextIndex returns the round-robin start index for a provider and advances it.
func (a *api) nextIndex(prov string, n int) int {
	a.rrMu.Lock()
	defer a.rrMu.Unlock()
	i := a.rrNext[prov] % n
	a.rrNext[prov] = (i + 1) % n
	return i
}

// retryableStatus reports a status that means "this account is busy, try another".
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests ||
		code == http.StatusInternalServerError ||
		code == http.StatusServiceUnavailable ||
		code == http.StatusConflict
}
