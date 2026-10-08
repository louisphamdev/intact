package httpapi

import (
	"net/http"
)

// requireProxyToken gates the proxy routes: a verified credential intact knows.
// Gate decision is credential-only: every unauthenticated caller is refused with 401.
func (a *api) requireProxyToken(next http.HandlerFunc) http.HandlerFunc {
	return a.requireToken(next)
}
