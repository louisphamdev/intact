package httpapi

import (
	"errors"
	"net/http"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/upstream"
)

// hopByHop headers belong to a single transport hop and must not be forwarded.
var hopByHop = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// proxy forwards one request to the account named in the path.
//
// It never reads the body and never re-serializes the response. The caller
// already speaks the provider's own protocol; this handler only adds identity.
func (a *api) proxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	conn, err := a.connection(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}
	p, ok := provider.Lookup(conn.Provider)
	if !ok {
		writeError(w, http.StatusNotFound, "provider not supported in this build")
		return
	}
	secret, err := a.store.Secret(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}

	base := p.BaseURL
	if over, ok := a.baseOverride[conn.Provider]; ok {
		base = over
	}
	target := base + "/" + r.PathValue("path")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	out, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad upstream target")
		return
	}

	// Copy the caller's headers, minus the ones that belong to this hop and minus
	// Authorization: the stored credential must win, so a caller can never send a
	// request on an account using a token of their own choosing.
	for k, vs := range r.Header {
		if hopByHop[k] || k == "Authorization" || k == "Host" {
			continue
		}
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}
	out.Header.Set(p.AuthHeader, p.AuthPrefix+secret)

	resp, err := upstream.Do(r.Context(), out, 3)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream unreachable")
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		if hopByHop[k] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	streamBody(w, resp)
}

// streamBody copies the upstream body to the caller and flushes each chunk as it
// arrives. A plain io.Copy would let the server buffer, which holds a streamed
// response until the upstream finishes and breaks every SSE client.
func streamBody(w http.ResponseWriter, resp *http.Response) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			// A flush failure only means this writer cannot flush; keep copying.
			_ = rc.Flush()
		}
		if readErr != nil {
			return
		}
	}
}

// writeError replies with a JSON body that never names a credential.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write([]byte(`{"error":"` + msg + `"}`))
}

func (a *api) connection(id string) (store.Connection, error) {
	list, err := a.store.ListConnections()
	if err != nil {
		return store.Connection{}, err
	}
	for _, c := range list {
		if c.ID == id {
			return c, nil
		}
	}
	return store.Connection{}, errors.New("not found")
}
