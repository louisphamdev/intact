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
	// rrMu guards rrNext, the per-model rotation cursor.
	rrMu   sync.Mutex
	rrNext map[string]int
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
}

// New builds the route table with no authentication (loopback use and tests).
func New(s *store.Store, baseOverride map[string]string) http.Handler {
	return NewWithAuth(s, baseOverride, nil)
}

// NewWithAuth builds the route table. When authCfg is not nil, a person must log
// in with a password and a TOTP code to reach the dashboard, and a machine must
// present the bearer token to use a provider.
func NewWithAuth(s *store.Store, baseOverride map[string]string, authCfg *auth.Config) http.Handler {
	a := &api{store: s, baseOverride: baseOverride, auth: authCfg, rrNext: map[string]int{},
		cat: catalog{m: map[string]catalogEntry{}}, copilot: copilotCache{m: map[string]copilotToken{}},
		sigs: sigStore{m: map[string]sigEntry{}}, drift: drift.New(s),
		rate: rateHeaders{m: map[string]rateSnapshot{}}, quota: quotaCache{m: map[string]AccountQuota{}},
		auto: autoState{running: map[string]*autoRun{}}}
	go a.autoTestLoop()
	mux := http.NewServeMux()
	// One base URL: the model in the body picks the provider and its accounts.
	mux.HandleFunc("GET /v1/models", a.requireToken(a.models))
	mux.HandleFunc("/v1/{path...}", a.requireToken(a.v1))
	// Management API for machines: the same token as /v1.
	mux.HandleFunc("GET /api/providers", a.requireToken(a.apiProviders))
	mux.HandleFunc("GET /api/accounts", a.requireToken(a.accounts))
	mux.HandleFunc("POST /api/accounts/{id}/active", a.requireToken(a.setActive))
	mux.HandleFunc("GET /api/usage", a.requireToken(a.usage))
	mux.HandleFunc("GET /api/quota", a.requireToken(a.quotaList))
	mux.HandleFunc("GET /quota", a.requireSession(a.quotaList))
	mux.HandleFunc("GET /ui-settings/{key}", a.requireSession(a.getUISetting))
	mux.HandleFunc("POST /ui-settings/{key}", a.requireSession(a.setUISetting))
	mux.HandleFunc("GET /api/drift/changes", a.requireToken(a.driftChanges))
	mux.HandleFunc("POST /api/drift/ack", a.requireToken(a.driftAck))
	mux.HandleFunc("GET /api/drift/fields", a.requireToken(a.driftFields))
	mux.HandleFunc("POST /api/drift/seed", a.requireToken(a.driftSeed))
	mux.HandleFunc("GET /drift/changes", a.requireSession(a.driftChanges))
	mux.HandleFunc("POST /drift/ack", a.requireSession(a.driftAck))
	mux.HandleFunc("GET /drift/fields", a.requireSession(a.driftFields))
	mux.HandleFunc("GET /api/filters", a.requireToken(a.listFilters))
	mux.HandleFunc("POST /api/filters", a.requireToken(a.saveFilter))
	mux.HandleFunc("DELETE /api/filters/{id}", a.requireToken(a.deleteFilter))
	mux.HandleFunc("POST /api/filters/{id}/delete", a.requireToken(a.deleteFilter))
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
	mux.HandleFunc("POST /api/accounts/{id}/test", a.requireToken(a.testAccount))
	mux.HandleFunc("GET /providers", a.requireSession(a.providers))
	mux.HandleFunc("GET /providers/{id}/models", a.requireSession(a.providerModelList))
	mux.HandleFunc("GET /providers/{id}/model-table", a.requireSession(a.providerModelTable))
	mux.HandleFunc("POST /providers/{id}/models/active", a.requireSession(a.setModelsActive))
	mux.HandleFunc("POST /providers/{id}/models/delete", a.requireSession(a.deleteModels))
	mux.HandleFunc("POST /providers/{id}/models/test", a.requireSession(a.testModel))
	mux.HandleFunc("GET /providers/{id}/model-policy", a.requireSession(a.getModelPolicy))
	mux.HandleFunc("POST /providers/{id}/model-policy", a.requireSession(a.setModelPolicy))
	mux.HandleFunc("GET /api/providers/{id}/model-policy", a.requireToken(a.getModelPolicy))
	mux.HandleFunc("POST /api/providers/{id}/model-policy", a.requireToken(a.setModelPolicy))
	mux.HandleFunc("GET /api/providers/{id}/models", a.requireToken(a.providerModelTable))
	mux.HandleFunc("GET /api/providers/{id}/models/raw", a.requireToken(a.providerModelsRaw))
	mux.HandleFunc("POST /api/providers/{id}/models/active", a.requireToken(a.setModelsActive))
	mux.HandleFunc("POST /api/providers/{id}/models/delete", a.requireToken(a.deleteModels))
	mux.HandleFunc("POST /api/providers/{id}/models/test", a.requireToken(a.testModel))
	mux.HandleFunc("GET /icons/{name}", a.icon)
	mux.HandleFunc("GET /favicon.svg", a.siteIcon)
	mux.HandleFunc("GET /favicon.ico", a.siteIcon)
	mux.HandleFunc("GET /apple-touch-icon.png", a.siteIcon)
	mux.HandleFunc("POST /oauth/{provider}/start", a.requireSession(a.loginStart))
	mux.HandleFunc("POST /oauth/{provider}/finish", a.requireSession(a.loginFinish))
	mux.HandleFunc("POST /oauth/github/poll", a.requireSession(a.githubDevicePoll))
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
	return mux
}

// requireSession lets a request through when auth is off or the session cookie
// is valid; otherwise it redirects a browser to the login page.
func (a *api) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.auth == nil {
			next(w, r)
			return
		}
		if ck, err := r.Cookie(sessionCookie); err == nil && a.auth.ValidSession(ck.Value) {
			next(w, r)
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
		tok := r.Header.Get("X-Api-Key")
		if b, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			tok = b
		}
		if a.auth == nil || (tok != "" && (a.auth.CheckAPIToken("Bearer "+tok) || a.store.ValidAPIKey(tok))) {
			next(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
	}
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
