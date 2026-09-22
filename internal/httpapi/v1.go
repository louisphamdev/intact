package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/louisphamdev/intact/internal/drift"
	"github.com/louisphamdev/intact/internal/filter"
	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/translate"
	"github.com/louisphamdev/intact/internal/upstream"
)

// intact has one base URL, /v1. The caller names a model and intact finds the
// accounts that serve it:
//
//   - "groq/llama-3.3-70b-versatile" names the provider. intact strips the
//     prefix from the body's model and uses that provider's active accounts.
//   - "llama-3.3-70b-versatile" names only the model. intact looks it up in the
//     model list of every provider and pools the accounts of all that list it.
//
// The pool is rotated per model so load spreads, and a busy account fails over
// to the next one. The rest of the path goes to the provider as sent, so
// /v1/chat/completions reaches <base>/chat/completions and /v1/messages reaches
// Anthropic's /messages.

// Model lists are cached so resolving a bare model does not cost an upstream
// call per request. A failed fetch is retried sooner than a good one expires.
const (
	catalogTTL     = 10 * time.Minute
	catalogFailTTL = time.Minute
)

type catalogEntry struct {
	ids []string
	ok  bool
	at  time.Time
	// groups holds the models folded from level variants (Antigravity).
	groups map[string]variantSet
	// raw is the provider's last list answer, as it came.
	raw []byte
	// info is what that answer says of each model's thinking.
	info map[string]ModelInfo
}

type catalog struct {
	mu sync.Mutex
	m  map[string]catalogEntry
}

// v1 forwards a request to the accounts that serve the model named in its body.
func (a *api) v1(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body")
		return
	}
	original := body
	model, ok := bodyModel(body)
	if !ok || model == "" {
		writeError(w, http.StatusBadRequest, "the request names no model; send \"model\": \"<provider>/<model>\" or a model id")
		return
	}
	prov, upstreamModel := a.splitModel(model)
	var targets []store.Connection
	if prov != "" {
		if a.store.InactiveModels(prov)[a.variantBase(prov, upstreamModel)] && r.Context().Value(modelTestKey{}) == nil {
			writeError(w, http.StatusForbidden, "this model is switched off in intact")
			return
		}
		targets = a.activeConnections(prov)
		if upstreamModel != model {
			if body, ok = setModel(body, upstreamModel); !ok {
				writeError(w, http.StatusBadRequest, "cannot rewrite the model")
				return
			}
		}
	} else {
		for _, p := range a.providersServing(r.Context(), model) {
			targets = append(targets, a.activeConnections(p)...)
		}
	}
	// A test pinned to one account reaches that account alone, switched on or not.
	if run, _ := r.Context().Value(modelTestKey{}).(*testRun); run != nil && run.pin != "" {
		targets = nil
		if list, err := a.store.ListConnections(); err == nil {
			for _, c := range list {
				if c.ID == run.pin && (prov == "" || c.Provider == prov) {
					targets = []store.Connection{c}
				}
			}
		}
	}
	if len(targets) == 0 {
		writeError(w, http.StatusNotFound, "no active account serves this model")
		return
	}
	// The client's own request, before intact changes anything, is what shows
	// a tool adding or dropping a field.
	if watched(targets[0].Provider) {
		a.drift.Observe(drift.Request, targets[0].Provider, r.PathValue("path"), original, false)
	}
	if r.PathValue("path") == "messages/count_tokens" && !a.anyAnthropic(targets) {
		countTokensEstimate(w, body)
		return
	}
	a.failover(w, r, body, targets, a.nextIndex("m:"+model, len(targets)))
}

// splitModel reads a "<provider>/<model>" prefix. It reports the provider only
// when the prefix is a registered provider, because model ids carry slashes of
// their own (meta-llama/llama-3.3-70b-instruct at openrouter).
func (a *api) splitModel(model string) (prov, rest string) {
	i := strings.IndexByte(model, '/')
	if i <= 0 {
		return "", model
	}
	if !a.knownProvider(model[:i]) {
		return "", model
	}
	return model[:i], model[i+1:]
}

// knownProvider reports whether id is a registered provider or the id of a
// custom provider that a stored connection defines.
func (a *api) knownProvider(id string) bool {
	if _, ok := provider.Lookup(id); ok {
		return true
	}
	list, _ := a.store.ListConnections()
	for _, c := range list {
		if c.Provider == id && c.BaseURL != "" {
			return true
		}
	}
	return false
}

// providerFor returns how to reach one connection: its registered provider,
// with the connection's base URL when it sets one, or a generic
// OpenAI-compatible upstream for a custom provider. It reports false when the
// connection cannot be reached (unknown provider, or a URL still missing its
// account id).
func (a *api) providerFor(c store.Connection) (provider.Provider, bool) {
	p, ok := provider.Lookup(c.Provider)
	switch {
	case ok && c.BaseURL != "":
		p.BaseURL = c.BaseURL
	case !ok && c.BaseURL != "":
		p, ok = provider.GenericAPI(c.Provider, c.BaseURL, c.Meta["api"]), true
	}
	if !ok || p.BaseURL == "" || strings.Contains(p.BaseURL, "{accountId}") {
		return provider.Provider{}, false
	}
	return p, true
}

// providersServing returns, in id order, the providers with an active account
// whose model list contains model. Lists are fetched in parallel when stale.
func (a *api) providersServing(ctx context.Context, model string) []string {
	provs := a.activeProviders()
	lists := make([][]string, len(provs))
	var wg sync.WaitGroup
	for i, p := range provs {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			lists[i] = a.providerModels(ctx, p)
		}(i, p)
	}
	wg.Wait()
	out := []string{}
	for i, p := range provs {
		want := a.variantBase(p, model)
		for _, id := range lists[i] {
			if id == want {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// models answers GET /v1/models with every provider's models, each named
// "<provider>/<model>" so the id a client picks routes back to one provider.
func (a *api) models(w http.ResponseWriter, r *http.Request) {
	// One entry serves both vocabularies: OpenAI reads object and owned_by,
	// Anthropic reads type and display_name.
	type entry struct {
		ID          string `json:"id"`
		Object      string `json:"object"`
		OwnedBy     string `json:"owned_by"`
		Type        string `json:"type"`
		DisplayName string `json:"display_name"`
	}
	provs := a.activeProviders()
	lists := make([][]string, len(provs))
	var wg sync.WaitGroup
	for i, p := range provs {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			lists[i] = a.providerModels(r.Context(), p)
		}(i, p)
	}
	wg.Wait()
	data := []entry{}
	for i, p := range provs {
		for _, id := range lists[i] {
			data = append(data, entry{ID: p + "/" + id, Object: "model", OwnedBy: p, Type: "model", DisplayName: p + "/" + id})
		}
	}
	first, last := "", ""
	if len(data) > 0 {
		first, last = data[0].ID, data[len(data)-1].ID
	}
	writeJSON(w, map[string]any{"object": "list", "data": data, "has_more": false, "first_id": first, "last_id": last})
}

// activeProviders returns the registered providers that have an active account.
func (a *api) activeProviders() []string {
	list, err := a.store.ListConnections()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, c := range list {
		if _, ok := a.providerFor(c); ok && c.IsActive {
			seen[c.Provider] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// providerModels returns the models a provider serves: what it lists, from
// the cache when fresh, minus the ones the operator switched off.
func (a *api) providerModels(ctx context.Context, prov string) []string {
	ids := a.catalogIDs(ctx, prov)
	off := a.store.InactiveModels(prov)
	if len(off) == 0 {
		return ids
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !off[id] {
			out = append(out, id)
		}
	}
	return out
}

// catalogIDs returns everything a provider lists, fetching when the cache is
// stale and recording the list in the database: a live list drops the models
// that are gone, a fallback list does not. New models start by the provider's
// policy; under AutoTest they are tested in the background.
func (a *api) catalogIDs(ctx context.Context, prov string) []string {
	a.cat.mu.Lock()
	e, hit := a.cat.m[prov]
	a.cat.mu.Unlock()
	ttl := catalogTTL
	if !e.ok {
		ttl = catalogFailTTL
	}
	if hit && time.Since(e.at) < ttl {
		return e.ids
	}
	ids, ok := []string(nil), false
	var groups map[string]variantSet
	var raw []byte
	if conns := a.activeConnections(prov); len(conns) > 0 {
		if p, _ := a.providerFor(conns[0]); p.API == translate.Antigravity {
			if ids, raw, ok = a.antigravityModels(ctx, conns[0]); ok {
				ids, groups = groupVariants(ids)
			}
		} else {
			ids, raw, ok = a.fetchModelIDs(ctx, conns[0])
		}
	}
	live := ok && len(ids) > 0
	if p, known := provider.Lookup(prov); known && len(p.Models) > 0 && len(ids) == 0 {
		ids, ok = p.Models, true
	}
	if len(ids) > 0 {
		pol := a.modelPolicy(prov)
		added, err := a.store.SyncModels(prov, ids, live, pol.startsOn)
		if err != nil {
			log.Printf("models %s: %v", prov, err)
		} else if pol.AutoTest && len(added) > 0 && a.auto.start(prov) {
			go a.autoTestHeld(prov, added, false)
		}
	}
	a.cat.mu.Lock()
	a.cat.m[prov] = catalogEntry{ids: ids, ok: ok, at: time.Now(), groups: groups, raw: raw,
		info: foldInfos(modelInfos(raw), groups)}
	a.cat.mu.Unlock()
	return ids
}

// refreshCatalog drops a provider's cached list and fetches it again.
func (a *api) refreshCatalog(ctx context.Context, prov string) []string {
	a.cat.mu.Lock()
	delete(a.cat.m, prov)
	a.cat.mu.Unlock()
	return a.catalogIDs(ctx, prov)
}

// fetchModelIDs reads one account's model list and returns its ids.
func (a *api) fetchModelIDs(ctx context.Context, conn store.Connection) ([]string, []byte, bool) {
	resp, err := a.getModels(ctx, conn)
	if err != nil {
		return nil, nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nil, false
	}
	var d struct {
		Data []struct {
			ID, Name     string
			Capabilities struct {
				Type string `json:"type"`
			} `json:"capabilities"`
			Policy *struct {
				State string `json:"state"`
			} `json:"policy"`
		} `json:"data"`
		Models []json.RawMessage `json:"models"`
		// Cloudflare's model search.
		Result []struct {
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, nil, false
	}
	ids := []string{}
	for _, m := range d.Data {
		// Copilot lists embedding models and models the plan has not enabled.
		if (m.Capabilities.Type != "" && m.Capabilities.Type != "chat") || (m.Policy != nil && m.Policy.State != "enabled") {
			continue
		}
		if m.ID != "" {
			ids = append(ids, m.ID)
		} else if m.Name != "" {
			ids = append(ids, m.Name)
		}
	}
	for _, m := range d.Result {
		if m.Name != "" {
			ids = append(ids, m.Name)
		}
	}
	for _, raw := range d.Models {
		var s string
		var o struct{ ID, Slug, Name string }
		if json.Unmarshal(raw, &s) == nil && s != "" {
			ids = append(ids, s)
		} else if json.Unmarshal(raw, &o) == nil && (o.ID != "" || o.Slug != "" || o.Name != "") {
			ids = append(ids, firstNonEmpty(o.Slug, o.ID, o.Name))
		}
	}
	sort.Strings(ids)
	return ids, raw, true
}

// getModels sends GET <base>/models for one account.
func (a *api) getModels(ctx context.Context, conn store.Connection) (*http.Response, error) {
	p, ok := a.providerFor(conn)
	if !ok {
		return nil, errors.New("provider not reachable")
	}
	secret, err := a.secretFor(ctx, conn.ID)
	if err != nil {
		return nil, err
	}
	if secret, err = a.exchanged(ctx, p, conn.ID, secret); err != nil {
		return nil, err
	}
	base := p.BaseURL
	if over, ok := a.baseOverride[conn.Provider]; ok {
		base = over
	}
	url := base + "/models"
	if p.ModelsPath != "" {
		url = strings.TrimSuffix(base, "/v1") + p.ModelsPath
	}
	if p.ModelsQuery != "" {
		url += "?" + p.ModelsQuery
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range p.Defaults {
		req.Header.Set(k, v)
	}
	for k, v := range p.Identity {
		req.Header.Set(k, v)
	}
	if p.AuthHeader != "" {
		req.Header.Set(p.AuthHeader, p.AuthPrefix+secret)
	}
	req.Header.Set("Accept-Encoding", "identity")
	return upstream.Do(ctx, req, 2)
}

// bodyModel returns the top-level "model" string of a JSON body.
func bodyModel(body []byte) (string, bool) {
	start, end, err := topLevelValue(body, "model")
	if err != nil {
		return "", false
	}
	var s string
	if json.Unmarshal(body[start:end], &s) != nil {
		return "", false
	}
	return s, true
}

// failover tries the accounts in order from start, wrapping around, and relays
// the first answer that is not a busy status. The last account's answer is
// relayed whatever it is, so the caller sees the real upstream error. An OAuth
// account that answers 401 is refreshed and retried once, which recovers a token
// the provider revoked before its recorded expiry.
func (a *api) failover(w http.ResponseWriter, r *http.Request, body []byte, targets []store.Connection, start int) {
	client := clientShape(r.PathValue("path"))
	stream := translate.Stream(body)
	translated := map[string][]byte{}
	tried := 0
	attempts := a.attemptsFor(r.Context(), targets, start, body)
	for i, at := range attempts {
		conn := at.conn
		p, ok := a.providerFor(conn)
		if !ok {
			continue
		}
		secret, err := a.secretFor(r.Context(), conn.ID)
		if err != nil {
			continue
		}
		if run, _ := r.Context().Value(modelTestKey{}).(*testRun); run != nil {
			run.used = conn.ID
		}
		if secret, err = a.exchanged(r.Context(), p, conn.ID, secret); err != nil {
			log.Printf("connection %s: %v", conn.ID, err)
			continue
		}
		// A caller of one shape reaching a provider of the other gets its
		// request translated, and the answer translated back. A caller that
		// already speaks the provider's shape is passed through untouched.
		model, _ := bodyModel(body)
		if at.model != "" {
			model = at.model
		}
		path, send, to, via := r.PathValue("path"), body, "", ""
		prepare := func() error {
			want, wantPath := shapeFor(p, model)
			path, send, to, via = r.PathValue("path"), body, "", ""
			if client == "" || !translatable(want) {
				return nil
			}
			if want == client {
				// Same shape: bytes pass through, at the provider's own path.
				if wantPath != "" {
					path = wantPath
				}
				return nil
			}
			if want == translate.Antigravity {
				// The envelope names the account's project, so it is built
				// per account from the chat form of the request.
				hub, err := toProvider(body, client, translate.OpenAI)
				if err != nil {
					return err
				}
				inner, err := translate.OpenAIToGemini(hub, &a.sigs)
				if err != nil {
					return err
				}
				env, err := a.antigravityEnvelope(r.Context(), conn, secret, model, inner)
				if err != nil {
					return errSkipAccount{err}
				}
				path, send, to, via = wantPath, env, client, want
				return nil
			}
			tb, ok := translated[want]
			if !ok {
				var err error
				if tb, err = toProvider(body, client, want); err != nil {
					return err
				}
				translated[want] = tb
			}
			path, send, to, via = wantPath, tb, client, want
			return nil
		}
		if err := prepare(); err != nil {
			var skip errSkipAccount
			if errors.As(err, &skip) {
				log.Printf("connection %s: %v", conn.ID, skip.err)
				continue
			}
			writeError(w, http.StatusBadRequest, "cannot translate the request: "+err.Error())
			return
		}
		send = filterFor(a, p, conn.Provider, send)
		tried++
		resp, err := a.send(r, p, conn.Provider, path, secret, send)
		if err != nil {
			log.Printf("connection %s: %v", conn.ID, err)
			continue
		}
		// Copilot answers some models only on /responses and says so with a
		// 400; remember the model and send it there.
		if p.Exchange == "copilot" && resp.StatusCode == http.StatusBadRequest && client != "" && path != "responses" {
			if b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10)); isResponsesOnly(b) {
				resp.Body.Close()
				copilotResponsesModels.Store(model, true)
				if err := prepare(); err == nil {
					send = filterFor(a, p, conn.Provider, send)
					if resp, err = a.send(r, p, conn.Provider, path, secret, send); err != nil {
						continue
					}
				}
			} else {
				resp.Body = io.NopCloser(bytes.NewReader(b))
			}
		}
		if resp.StatusCode == http.StatusUnauthorized && p.Exchange != "" {
			a.dropExchanged(conn.ID)
			if fresh, err := a.exchanged(r.Context(), p, conn.ID, secret0(a, r, conn.ID)); err == nil {
				resp.Body.Close()
				if resp, err = a.send(r, p, conn.Provider, path, fresh, send); err != nil {
					continue
				}
			}
		}
		if resp.StatusCode == http.StatusUnauthorized && a.isOAuth(conn.ID) {
			if fresh, ok := a.forceRefresh(r.Context(), conn.ID); ok {
				resp.Body.Close()
				if resp, err = a.send(r, p, conn.Provider, path, fresh, send); err != nil {
					continue
				}
			}
		}
		// Fail over on a busy status only while another account remains.
		if retryableStatus(resp.StatusCode) {
			log.Printf("connection %s: %s answered %d%s", conn.ID, conn.Provider, resp.StatusCode, modelNote(at.model))
		}
		if retryableStatus(resp.StatusCode) && i < len(attempts)-1 {
			resp.Body.Close()
			continue
		}
		defer resp.Body.Close()
		if at.model != "" {
			// The model that answered, when intact chose it (a level variant).
			w.Header().Set("X-Intact-Model", at.model)
		}
		if to != "" {
			a.relayVia(w, resp, conn.ID, to, via, stream, conn.Provider, path)
		} else {
			a.relayObserved(w, resp, conn.ID, conn.Provider, path)
		}
		return
	}
	if tried == 0 {
		writeError(w, http.StatusInternalServerError, "no usable account")
		return
	}
	writeError(w, http.StatusBadGateway, "all accounts failed")
}

// filterFor applies a provider's own request rules, then the blacklist, which
// runs last on the exact bytes the provider will receive.
func filterFor(a *api, p provider.Provider, prov string, body []byte) []byte {
	body = adjustForProvider(p, body)
	body, _ = filter.Apply(body, a.rulesFor(prov))
	return body
}

// isResponsesOnly reports Copilot's refusal of a model on /chat/completions.
func isResponsesOnly(b []byte) bool {
	s := string(b)
	return strings.Contains(s, "not accessible via the /chat/completions endpoint") ||
		strings.Contains(s, "The requested model is not supported")
}

// send builds and sends one upstream request for a connection.
func (a *api) send(r *http.Request, p provider.Provider, providerID, path, secret string, body []byte) (*http.Response, error) {
	out, err := a.newOutbound(r, p, providerID, path, secret, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	return upstream.Do(r.Context(), out, 1)
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

// nextIndex returns the rotation start for a key and advances it.
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

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// errSkipAccount is a failure of one account (not of the request), after
// which the next account is tried.
type errSkipAccount struct{ err error }

func (e errSkipAccount) Error() string { return e.err.Error() }

func modelNote(m string) string {
	if m == "" {
		return ""
	}
	return " on " + m
}
