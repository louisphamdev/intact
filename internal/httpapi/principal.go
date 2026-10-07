package httpapi

import (
	"context"
	"net/http"
	"slices"
	"strings"
)

// principal is who sent a request. admin is set only by the master token, a
// signed-in session, and no-auth mode; no key name can set it.
type principal struct {
	admin bool
	keyID string // dashboard key id; empty for admin callers and internal jobs
	name  string // display label for drift and logs; never used for access
	// models lists the "<provider>/<model>" ids a dashboard key may call;
	// empty allows every model.
	models []string
	// providerClient names the provider whose own client sent the request, when
	// it was let in without a token. It grants nothing: no key id, not admin.
	// It is here so the request can be checked against the provider it claims
	// and the log can say where it came from.
	providerClient string
}

// allowsAllModels reports whether p may call any model: admin callers, intact's
// own jobs, and keys with no model list.
func (p principal) allowsAllModels() bool {
	return p.keyID == "" || len(p.models) == 0 || slices.Contains(p.models, "*")
}

// allowsModel reports whether p may call model, as the caller named it.
func (p principal) allowsModel(prov, upstreamModel, model string) bool {
	if prov != "" {
		return p.allowsProviderModel(prov, upstreamModel)
	}
	return p.allowsBareModel(model)
}

// allowsProviderModel matches the id exactly, case included, as the upstream
// sees it: "-high" on an allowed id is another model.
func (p principal) allowsProviderModel(prov, model string) bool {
	return p.allowsAllModels() || slices.Contains(p.models, prov+"/"+model)
}

// allowsBareModel reports whether some allowed "<provider>/<model>" has this id.
func (p principal) allowsBareModel(model string) bool {
	if p.allowsAllModels() {
		return true
	}
	for _, m := range p.models {
		if _, rest, ok := strings.Cut(m, "/"); ok && rest == model {
			return true
		}
	}
	return false
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
