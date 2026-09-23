// Package httpapi serves the local API and the dashboard.
package httpapi

import (
	"net/http"
	"strings"
	"sync"

	"github.com/louisphamdev/intact/internal/auth"
	"github.com/louisphamdev/intact/internal/drift"
	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/web"
)

const sessionCookie = "intact_session"

type api struct {
	store *store.Store
	// baseOverride redirects a provider to a test server. Nil in production.
	baseOverride map[string]string
	// auth gates the server. Nil means no gate (loopback use).
	auth *auth.Config
	// rrMu guards rrNext, the rotation cursors: per provider ("p:"), or per
	// model for a pool across providers ("m:").
	rrMu   sync.Mutex
	rrNext map[string]rrCursor
	// cat caches each provider's model list for resolving a bare model id.
	cat catalog
	// filters caches the compiled request filters.
	filters filterCache
	// copilot caches exchanged Copilot tokens per connection.
	copilot copilotCache
	// sigs remembers Gemini thought signatures by function call id.
	sigs sigStore
	// drift learns the structure of what passes through and records changes.
	drift *drift.Observer
	// rate keeps the last rate-limit headers per account.
	rate rateHeaders
	// quota caches the quota read from each account.
	quota quotaCache
	// auto tracks the providers whose models are being auto-tested.
	auto autoState
	// arena holds the model leaderboard and its name index.
	arena arenaState
	// review is the drift review.
	review reviewState
	// notes throttles alerts; errReview is the error review's state.
	notes     notifier
	errReview reviewState
	// login caps failed sign-in attempts so the TOTP code cannot be brute forced.
	login *loginGuard
	// refresh serializes OAuth token refreshes per connection.
	refresh refreshLocks
}

// New builds the route table with no authentication (loopback use and tests).
func New(s *store.Store, baseOverride map[string]string) http.Handler {
	return NewWithAuth(s, baseOverride, nil)
}

// NewWithAuth builds the route table. When authCfg is not nil, a person must log
// in with a TOTP code to reach the dashboard, and a machine must present the
// master token or a dashboard key. Only the session and the master token are admin.
func NewWithAuth(s *store.Store, baseOverride map[string]string, authCfg *auth.Config) http.Handler {
	_, h := newServer(s, baseOverride, authCfg)
	return h
}

// newServer builds the api and its routes; tests reach the api through it.
func newServer(s *store.Store, baseOverride map[string]string, authCfg *auth.Config) (*api, http.Handler) {
	a := &api{store: s, baseOverride: baseOverride, auth: authCfg, rrNext: map[string]rrCursor{},
		cat:     catalog{m: map[string]catalogEntry{}, inflight: map[string]*catalogFetch{}},
		copilot: copilotCache{m: map[string]copilotToken{}},
		sigs:    sigStore{m: map[string]sigEntry{}}, drift: drift.New(s),
		rate: rateHeaders{m: map[string]rateSnapshot{}}, quota: quotaCache{m: map[string]AccountQuota{}},
		auto: autoState{running: map[string]*autoRun{}}, login: newLoginGuard()}
	go a.autoTestLoop()
	a.loadDefs()
	a.migrateCustomEndpoints()
	a.loadArena()
	go a.arenaLoop()
	go a.reviewLoop()
	go a.errorReviewLoop()
	mux := http.NewServeMux()
	// One base URL: the model in the body picks the provider and its accounts.
	mux.HandleFunc("GET /v1/models", a.requireToken(a.models))
	mux.HandleFunc("/v1/{path...}", a.requireToken(a.v1))
	// Management API for machines: the same token as /v1.
	mux.HandleFunc("GET /api/providers", a.requireToken(a.apiProviders))
	mux.HandleFunc("GET /api/accounts", a.requireToken(a.accounts))
	mux.HandleFunc("POST /api/accounts/{id}/active", a.requireAdmin(a.setActive))
	mux.HandleFunc("GET /api/usage", a.requireToken(a.usage))
	mux.HandleFunc("GET /api/quota", a.requireToken(a.quotaList))
	mux.HandleFunc("GET /quota", a.requireSession(a.quotaList))
	mux.HandleFunc("GET /ui-settings/{key}", a.requireSession(a.getUISetting))
	mux.HandleFunc("POST /ui-settings/{key}", a.requireSession(a.setUISetting))
	mux.HandleFunc("GET /api/drift/changes", a.requireToken(a.driftChanges))
	mux.HandleFunc("POST /api/drift/ack", a.requireAdmin(a.driftAck))
	mux.HandleFunc("GET /api/drift/fields", a.requireToken(a.driftFields))
	mux.HandleFunc("POST /api/drift/seed", a.requireAdmin(a.driftSeed))
	for _, pre := range []string{"", "/api"} {
		// read: the dashboard session, or any machine token. write: the session
		// or the master token only, so a shared inference key cannot redirect an
		// alert channel or trigger a review. The channel list is a write-level
		// read: a webhook url IS the credential for Slack, Discord and ntfy.
		read, write := a.requireSession, a.requireSession
		if pre != "" {
			read, write = a.requireToken, a.requireAdmin
		}
		mux.HandleFunc("GET "+pre+"/notify", write(a.notifyInfo))
		mux.HandleFunc("POST "+pre+"/notify/channels", write(a.putChannel))
		mux.HandleFunc("PUT "+pre+"/notify/channels/{id}", write(a.putChannel))
		mux.HandleFunc("DELETE "+pre+"/notify/channels/{id}", write(a.deleteChannel))
		mux.HandleFunc("POST "+pre+"/notify/channels/{id}/test", write(a.testChannel))
		mux.HandleFunc("GET "+pre+"/errors/review", read(a.errorReview))
		mux.HandleFunc("POST "+pre+"/errors/review", write(a.errorReview))
		mux.HandleFunc("GET "+pre+"/errors/verdicts", read(a.errorVerdicts))
	}
	mux.HandleFunc("GET /errors", a.requireSession(a.errorsList))
	mux.HandleFunc("GET /errors/stats", a.requireSession(a.errorStats))
	mux.HandleFunc("GET /errors/{id}", a.requireSession(a.errorGet))
	mux.HandleFunc("GET /api/errors", a.requireToken(a.errorsList))
	mux.HandleFunc("GET /api/errors/stats", a.requireToken(a.errorStats))
	mux.HandleFunc("GET /api/errors/{id}", a.requireToken(a.errorGet))
	mux.HandleFunc("GET /drift/changes", a.requireSession(a.driftChanges))
	mux.HandleFunc("GET /drift/review", a.requireSession(a.driftReview))
	mux.HandleFunc("POST /drift/review", a.requireSession(a.driftReview))
	mux.HandleFunc("GET /api/drift/review", a.requireToken(a.driftReview))
	mux.HandleFunc("POST /api/drift/review", a.requireAdmin(a.driftReview))
	mux.HandleFunc("POST /drift/ack", a.requireSession(a.driftAck))
	mux.HandleFunc("GET /drift/fields", a.requireSession(a.driftFields))
	mux.HandleFunc("GET /api/filters", a.requireToken(a.listFilters))
	mux.HandleFunc("POST /api/filters", a.requireAdmin(a.saveFilter))
	mux.HandleFunc("DELETE /api/filters/{id}", a.requireAdmin(a.deleteFilter))
	mux.HandleFunc("POST /api/filters/{id}/delete", a.requireAdmin(a.deleteFilter))
	// MCP (Streamable HTTP): the management API as tools for an agent.
	mux.HandleFunc("POST /mcp", a.requireToken(a.mcp))
	mux.HandleFunc("GET /mcp", a.requireToken(a.mcpGet))
	mux.HandleFunc("GET /accounts", a.requireSession(a.accounts))
	mux.HandleFunc("POST /accounts", a.requireSession(a.createAccount))
	mux.HandleFunc("POST /accounts/{id}/delete", a.requireSession(a.deleteAccount))
	mux.HandleFunc("GET /accounts/{id}/models", a.requireSession(a.modelsForAccount))
	mux.HandleFunc("POST /accounts/{id}/active", a.requireSession(a.setActive))
	mux.HandleFunc("POST /accounts/{id}/label", a.requireSession(a.setLabel))
	mux.HandleFunc("POST /accounts/{id}/test", a.requireSession(a.testAccount))
	mux.HandleFunc("GET /account-tests", a.requireSession(a.accountTests))
	mux.HandleFunc("POST /api/accounts/{id}/test", a.requireAdmin(a.testAccount))
	mux.HandleFunc("GET /providers", a.requireSession(a.providers))
	mux.HandleFunc("GET /providers/{id}/models", a.requireSession(a.providerModelList))
	mux.HandleFunc("GET /providers/{id}/model-table", a.requireSession(a.providerModelTable))
	mux.HandleFunc("POST /providers/{id}/models/active", a.requireSession(a.setModelsActive))
	mux.HandleFunc("POST /providers/{id}/models/delete", a.requireSession(a.deleteModels))
	mux.HandleFunc("POST /providers/{id}/models/test", a.requireSession(a.testModel))
	mux.HandleFunc("GET /providers/{id}/rotation", a.requireSession(a.getRotation))
	mux.HandleFunc("POST /providers/{id}/rotation", a.requireSession(a.setRotation))
	mux.HandleFunc("GET /api/providers/{id}/rotation", a.requireToken(a.getRotation))
	mux.HandleFunc("POST /api/providers/{id}/rotation", a.requireAdmin(a.setRotation))
	mux.HandleFunc("GET /providers/{id}/model-policy", a.requireSession(a.getModelPolicy))
	mux.HandleFunc("POST /providers/{id}/model-policy", a.requireSession(a.setModelPolicy))
	mux.HandleFunc("GET /api/providers/{id}/model-policy", a.requireToken(a.getModelPolicy))
	mux.HandleFunc("POST /api/providers/{id}/model-policy", a.requireAdmin(a.setModelPolicy))
	mux.HandleFunc("GET /api/providers/{id}/models", a.requireToken(a.providerModelTable))
	mux.HandleFunc("GET /api/providers/{id}/models/raw", a.requireToken(a.providerModelsRaw))
	mux.HandleFunc("GET /api/provider-defs", a.requireToken(a.listDefs))
	mux.HandleFunc("GET /api/provider-defs/{id}", a.requireToken(a.getDef))
	mux.HandleFunc("POST /api/provider-defs", a.requireAdmin(a.putDef))
	mux.HandleFunc("DELETE /api/provider-defs/{id}", a.requireAdmin(a.deleteDef))
	mux.HandleFunc("GET /provider-defs", a.requireSession(a.listDefs))
	mux.HandleFunc("GET /provider-defs/{id}", a.requireSession(a.getDef))
	mux.HandleFunc("POST /provider-defs", a.requireSession(a.putDef))
	mux.HandleFunc("POST /provider-defs/{id}/delete", a.requireSession(a.deleteDef))
	mux.HandleFunc("GET /api/rankings", a.requireToken(a.rankings))
	mux.HandleFunc("POST /api/rankings/refresh", a.requireAdmin(a.refreshRankings))
	mux.HandleFunc("POST /api/rankings/alias", a.requireAdmin(a.setArenaAlias))
	mux.HandleFunc("GET /rankings", a.requireSession(a.rankings))
	mux.HandleFunc("POST /rankings/refresh", a.requireSession(a.refreshRankings))
	mux.HandleFunc("POST /rankings/alias", a.requireSession(a.setArenaAlias))
	mux.HandleFunc("POST /api/providers/{id}/models/active", a.requireAdmin(a.setModelsActive))
	mux.HandleFunc("POST /api/providers/{id}/models/delete", a.requireAdmin(a.deleteModels))
	mux.HandleFunc("POST /api/providers/{id}/models/test", a.requireAdmin(a.testModel))
	mux.HandleFunc("GET /icons/{name}", a.icon)
	mux.HandleFunc("GET /favicon.svg", a.siteIcon)
	mux.HandleFunc("GET /favicon.ico", a.siteIcon)
	mux.HandleFunc("GET /apple-touch-icon.png", a.siteIcon)
	mux.HandleFunc("POST /oauth/{provider}/start", a.requireSession(a.loginStart))
	mux.HandleFunc("POST /oauth/{provider}/finish", a.requireSession(a.loginFinish))
	mux.HandleFunc("POST /oauth/github/poll", a.requireSession(a.githubDevicePoll))
	mux.HandleFunc("POST /oauth/{provider}/poll", a.requireSession(a.devicePoll))
	mux.HandleFunc("GET /keys", a.requireSession(a.listKeys))
	mux.HandleFunc("POST /keys", a.requireSession(a.createKey))
	mux.HandleFunc("POST /keys/{id}/reveal", a.requireSession(a.revealKey))
	mux.HandleFunc("POST /keys/{id}/active", a.requireSession(a.setKeyActive))
	mux.HandleFunc("POST /keys/{id}/delete", a.requireSession(a.deleteKey))
	mux.HandleFunc("GET /filters", a.requireSession(a.listFilters))
	mux.HandleFunc("POST /filters", a.requireSession(a.saveFilter))
	mux.HandleFunc("POST /filters/{id}/delete", a.requireSession(a.deleteFilter))
	mux.HandleFunc("GET /usage", a.requireSession(a.usage))
	mux.HandleFunc("GET /login", a.loginForm)
	mux.HandleFunc("POST /login", a.loginSubmit)
	mux.HandleFunc("POST /logout", a.logout)
	// "{$}" matches the root and nothing else. A bare "/" would be a catch-all
	// and would answer every mistyped path with the dashboard.
	mux.HandleFunc("GET /{$}", a.requireSession(a.dashboard))
	return a, a.guardRequest(mux)
}

// requireSession lets a request through when auth is off or the session cookie
// is valid; otherwise it redirects a browser to the login page.
func (a *api) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.auth == nil {
			next(w, withPrincipal(r, principal{admin: true}))
			return
		}
		if ck, err := r.Cookie(sessionCookie); err == nil && a.auth.ValidSession(ck.Value) {
			next(w, withPrincipal(r, principal{admin: true}))
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

// requireToken lets a request through when auth is off or it carries a valid
// API token: the one in the environment, or an enabled key made in the
// dashboard. The token is read from Authorization: Bearer, or from x-api-key,
// which Anthropic clients send. Otherwise it answers 401 for the machine caller.
func (a *api) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearerToken(r)
		// Who the caller is travels with the request: the master token is
		// admin, and a dashboard key owns only what its key id caused. The key
		// name is a label for drift and the error log, never a right.
		if tok != "" && a.auth != nil && a.auth.CheckAPIToken("Bearer "+tok) {
			next(w, withPrincipal(r, principal{admin: true, name: "env"}))
			return
		}
		if id, name, ok := a.store.APIKeyByToken(tok); ok {
			next(w, withPrincipal(r, principal{keyID: id, name: name}))
			return
		}
		if a.auth == nil {
			next(w, withPrincipal(r, principal{admin: true}))
			return
		}
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
	}
}

// requireAdmin gates management that can redirect a stored credential or wipe a
// request: a valid session, or the environment's master token. A key made in
// the dashboard (handed to a machine for /v1) is refused here, so such a key
// cannot change a provider's URL, install a body filter, or read another
// client's stored data.
func (a *api) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.auth == nil {
			next(w, withPrincipal(r, principal{admin: true}))
			return
		}
		if ck, err := r.Cookie(sessionCookie); err == nil && a.auth.ValidSession(ck.Value) {
			next(w, withPrincipal(r, principal{admin: true}))
			return
		}
		if tok := bearerToken(r); tok != "" && a.auth.CheckAPIToken("Bearer "+tok) {
			next(w, withPrincipal(r, principal{admin: true, name: "env"}))
			return
		}
		writeError(w, http.StatusForbidden, "this action needs the dashboard session or the master token")
	}
}

// bearerToken reads the caller's token from Authorization: Bearer, or from
// x-api-key, which Anthropic clients send.
func bearerToken(r *http.Request) string {
	tok := r.Header.Get("X-Api-Key")
	if b, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		tok = b
	}
	return tok
}

// dashboard serves the page embedded in the binary.
func (a *api) dashboard(w http.ResponseWriter, r *http.Request) {
	b, err := web.Files.ReadFile("index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "dashboard missing from this build")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}
