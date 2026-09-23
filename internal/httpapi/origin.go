package httpapi

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// guardRequest wraps the whole mux. It answers the two attacks that a bind
// address cannot stop: DNS rebinding against an ungated server, and a
// cross-site write from a page the operator has open. It also refuses framing.
func (a *api) guardRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		// With no gate, the Host is the only thing that tells the operator's own
		// browser apart from a name that resolves to 127.0.0.1.
		if a.auth == nil && !loopbackHost(r.Host) {
			writeError(w, http.StatusForbidden, "this server has no authentication, so it answers a loopback Host only")
			return
		}
		if !crossSiteWrite(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusForbidden, "cross-site request refused")
	})
}

// crossSiteWrite reports whether a request changes state and comes from another
// site. A request with no Origin header is not cross-site: an API client sends
// none, and every browser sends one on a cross-origin write.
func crossSiteWrite(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	// A sandboxed frame or a redirected form sends "null", which names no site.
	if origin == "null" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return true
	}
	// Host only, never scheme: a TLS-terminating tunnel gives the browser https
	// and the server plain http, and both carry the same host and port.
	return u.Host != r.Host
}

// loopbackHost reports whether the request's Host names this machine.
func loopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}
