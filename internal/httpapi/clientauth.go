package httpapi

import (
	"net/http"
	"strings"

	"github.com/louisphamdev/intact/internal/provider"
)

// A provider's own client, reaching that provider through intact.
//
// Codex talks to ChatGPT directly, with its own credential. Pointed at intact instead, it keeps
// sending that credential, which intact cannot check: intact only knows the keys it issued. So
// the call arrives with no key intact recognises and is refused, even though the request is
// exactly the one intact exists to serve -- the provider's own client asking that provider for a
// model, with intact swapping the credential (Bifrost).
//
// The exemption is narrow on purpose, and narrow in two ways at once.
//
// It is only on the proxy routes. Not on /api/*, where a forged User-Agent would otherwise open
// usage, quota, the error log and the drift tables. The proxy routes answer "the model said
// this", which is the only thing a client without a credential is allowed to ask for.
//
// It is only for a provider's own client, and it grants nothing. The principal carries no key id
// and is not admin, so it owns no models, reaches no dashboard, and changes nothing. What it may
// do is reach a model, and only through the account intact already holds for that provider: the
// credential is swapped in provider.go, never taken from the caller. A caller that forges the
// User-Agent gets a proxy that will only talk to the provider it named, with intact's stored
// account, at intact's stored rate limits -- which is the same thing the key holder already had.

// providerClientFor names the provider whose own client sent this request, by the User-Agent each
// provider declares in BifrostUA. A prefix match, because a client sends its version after it.
func providerClientFor(ua string) (string, bool) {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return "", false
	}
	for _, id := range provider.BifrostProviderIDs() {
		p, ok := provider.Lookup(id)
		if ok && p.Bifrost(ua) {
			return id, true
		}
	}
	return "", false
}

// requireProxyToken gates the proxy routes: a token intact knows, or the provider's own client.
// Everything else is refused, exactly as before.
func (a *api) requireProxyToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p, ok := a.tokenPrincipal(w, r); ok {
			next(w, withPrincipal(r, p))
			return
		}
		if id, ok := providerClientFor(r.Header.Get("User-Agent")); ok {
			next(w, withPrincipal(r, principal{name: "client:" + id, providerClient: id}))
			return
		}
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
	}
}
