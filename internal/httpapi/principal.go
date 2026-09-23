package httpapi

import (
	"context"
	"net/http"
)

// principal is who sent a request. admin is set only by the master token, a
// signed-in session, and no-auth mode; no key name can set it.
type principal struct {
	admin bool
	keyID string // dashboard key id; empty for admin callers and internal jobs
	name  string // display label for drift and logs; never used for access
}

// principalKey carries the principal in a request's context.
type principalKey struct{}

// intactJob marks a request intact makes for itself: a review, a replay, a
// model test. It owns no key and it is not admin.
var intactJob = principal{name: "intact-review"}

// principalOf reads who sent a request. A request that passed no gate (an
// internal call) gets the zero principal, which owns nothing.
func principalOf(r *http.Request) principal {
	p, _ := r.Context().Value(principalKey{}).(principal)
	return p
}

// withPrincipal returns r with p as its sender.
func withPrincipal(r *http.Request, p principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
}

// clientOf names who made a request, for drift and the error log: its API
// key's name, "env", or "internal" for intact's own calls and an open server.
// It is a label, never an access decision.
func clientOf(r *http.Request) string {
	if n := principalOf(r).name; n != "" {
		return n
	}
	return "internal"
}
