package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
)

// modelTestKey marks a test call made from the dashboard, which may reach a
// model the operator switched off (to decide whether to switch it on). Its
// value is a *testRun.
type modelTestKey struct{}

// testRun pins a test to one account (pin, when set) and learns which account
// answered it (used).
type testRun struct {
	pin  string
	used string
}

// providerModelTable serves a provider's models with their switch and last
// test, and the provider's policy. It fetches the list first when the cache is stale.
func (a *api) providerModelTable(w http.ResponseWriter, r *http.Request) {
	prov := r.PathValue("id")
	if r.URL.Query().Get("refresh") == "1" {
		a.refreshCatalog(r.Context(), prov)
	} else {
		a.catalogIDs(r.Context(), prov)
	}
	list, err := a.store.ListModels(prov)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read models")
		return
	}
	a.cat.mu.Lock()
	e := a.cat.m[prov]
	a.cat.mu.Unlock()
	ids := make([]string, len(list))
	for i, m := range list {
		ids[i] = m.Model
	}
	variants := map[string][]string{}
	for base, g := range e.groups {
		variants[base] = g.Variants()
	}
	writeJSON(w, map[string]any{"models": list, "ok": e.ok, "fetchedAt": e.at.UTC().Format(time.RFC3339), "variants": variants, "info": e.info,
		"arena": a.arenaFor(prov, ids), "arenaMeta": a.arenaMeta(),
		"policy": a.modelPolicy(prov), "running": a.auto.isRunning(prov)})
}

type modelsBody struct {
	Models []string `json:"models"`
	Active bool     `json:"active"`
	Model  string   `json:"model"`
	// Account pins a test to one connection id.
	Account string `json:"account"`
}

func readModels(w http.ResponseWriter, r *http.Request) (modelsBody, bool) {
	var b modelsBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "bad json")
		return b, false
	}
	return b, true
}

// setModelsActive switches models on or off: {"models":[…],"active":bool}.
func (a *api) setModelsActive(w http.ResponseWriter, r *http.Request) {
	b, ok := readModels(w, r)
	if !ok {
		return
	}
	n, err := a.store.SetModelsActive(r.PathValue("id"), b.Models, b.Active)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot update models")
		return
	}
	writeJSON(w, map[string]any{"updated": n})
}

// deleteModels forgets models: {"models":[…]}.
func (a *api) deleteModels(w http.ResponseWriter, r *http.Request) {
	b, ok := readModels(w, r)
	if !ok {
		return
	}
	n, err := a.store.DeleteModels(r.PathValue("id"), b.Models)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot delete models")
		return
	}
	writeJSON(w, map[string]any{"deleted": n})
}

// testModel sends a short chat request to one model through the same /v1
// path a client uses (translation and failover included) and records the
// outcome: {"model":"…"}.
func (a *api) testModel(w http.ResponseWriter, r *http.Request) {
	b, ok := readModels(w, r)
	if !ok || b.Model == "" {
		if ok {
			writeError(w, http.StatusBadRequest, "model is required")
		}
		return
	}
	prov := r.PathValue("id")
	res := a.runModelTest(r.Context(), prov, b.Model, b.Account)
	a.store.RecordModelTest(prov, b.Model, res.OK, res.Ms, res.Message, res.Account)
	// Under auto test, a test decides the switch whoever runs it.
	if pol := a.modelPolicy(prov); pol.AutoTest {
		on := res.OK && pol.wanted(b.Model)
		a.store.SetModelsActive(prov, []string{b.Model}, on)
		res.Active = &on
	}
	writeJSON(w, res)
}

type modelTest struct {
	OK      bool   `json:"ok"`
	Status  int    `json:"status"`
	Ms      int64  `json:"ms"`
	Message string `json:"message"`
	// Account is the connection id that answered.
	Account string `json:"account,omitempty"`
	Model   string `json:"model,omitempty"`
	// Active is the model's switch after the test, when the test set it.
	Active *bool `json:"active,omitempty"`
}

// runModelTest tests one model, on the account pin when set, else on any
// account of the provider as a client call would.
func (a *api) runModelTest(ctx context.Context, prov, model, pin string) modelTest {
	run := &testRun{pin: pin}
	ctx = context.WithValue(ctx, modelTestKey{}, run)
	var res modelTest
	if p, ok := provider.Lookup(prov); ok && p.API == "typesafe" {
		res = a.runTypeSafeTest(ctx, prov, model)
	} else {
		res = a.runChatTest(ctx, prov, model)
	}
	res.Account, res.Model = run.used, model
	return res
}

func (a *api) runChatTest(ctx context.Context, prov, model string) modelTest {
	body, _ := json.Marshal(map[string]any{"model": prov + "/" + model, "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": "Reply with the single word OK."}}})
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.SetPathValue("path", "chat/completions")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	start := time.Now()
	a.v1(rec, req)
	res := modelTest{Status: rec.Code, Ms: time.Since(start).Milliseconds()}
	raw, _ := io.ReadAll(rec.Body)
	var d struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Error json.RawMessage `json:"error"`
	}
	json.Unmarshal(raw, &d)
	switch {
	case rec.Code == http.StatusOK && len(d.Choices) > 0:
		res.OK = true
		res.Message = strings.TrimSpace(d.Choices[0].Message.Content)
		if res.Message == "" && d.Choices[0].Message.Reasoning != "" {
			res.Message = "(reasoning only)"
		}
	case len(d.Error) > 0:
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(d.Error, &e) == nil && e.Message != "" {
			res.Message = e.Message
		} else {
			res.Message = strings.Trim(string(d.Error), `"`)
		}
	default:
		res.Message = strings.TrimSpace(string(raw))
	}
	if len(res.Message) > 300 {
		res.Message = res.Message[:300]
	}
	return res
}

// modelRows is the model table for MCP and the API.
func (a *api) modelRows(ctx context.Context, prov string) ([]store.ProviderModel, error) {
	a.catalogIDs(ctx, prov)
	return a.store.ListModels(prov)
}

func accountTestKey(id string) string { return "account-test:" + id }

// testAccount tests one account: {"model":"…"} or, without a model, the
// provider's model that last passed a test, else its first model switched on.
// The result is kept and served with the account.
func (a *api) testAccount(w http.ResponseWriter, r *http.Request) {
	var b modelsBody
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&b)
	res, code, err := a.runAccountTest(r.Context(), r.PathValue("id"), b.Model)
	if err != nil {
		writeError(w, code, err.Error())
		return
	}
	writeJSON(w, res)
}

func (a *api) runAccountTest(ctx context.Context, id, model string) (map[string]any, int, error) {
	var conn *store.Connection
	if list, err := a.store.ListConnections(); err == nil {
		for i := range list {
			if list[i].ID == id {
				conn = &list[i]
			}
		}
	}
	if conn == nil {
		return nil, http.StatusNotFound, errors.New("no such account")
	}
	if model == "" {
		model = a.accountTestModel(ctx, conn.Provider)
	}
	if model == "" {
		return nil, http.StatusBadRequest, errors.New("the provider lists no model to test")
	}
	res := a.runModelTest(ctx, conn.Provider, model, conn.ID)
	saved := map[string]any{"ok": res.OK, "status": res.Status, "ms": res.Ms, "message": res.Message,
		"model": model, "at": time.Now().UTC().Format(time.RFC3339)}
	raw, _ := json.Marshal(saved)
	a.store.SetSetting(accountTestKey(id), string(raw))
	return saved, http.StatusOK, nil
}

// accountTestModel picks the model an account test uses.
func (a *api) accountTestModel(ctx context.Context, prov string) string {
	rows, _ := a.modelRows(ctx, prov)
	for _, m := range rows {
		if m.Active && m.TestOK {
			return m.Model
		}
	}
	for _, m := range rows {
		if m.Active {
			return m.Model
		}
	}
	if len(rows) > 0 {
		return rows[0].Model
	}
	return ""
}

// accountTests serves the last test of every account, by connection id.
func (a *api) accountTests(w http.ResponseWriter, r *http.Request) {
	out := map[string]json.RawMessage{}
	if list, err := a.store.ListConnections(); err == nil {
		for _, c := range list {
			if v, _ := a.store.GetSetting(accountTestKey(c.ID)); v != "" {
				out[c.ID] = json.RawMessage(v)
			}
		}
	}
	writeJSON(w, out)
}

// providerModelsRaw serves the provider's last model list answer as it came,
// fetching it first when there is none.
func (a *api) providerModelsRaw(w http.ResponseWriter, r *http.Request) {
	prov := r.PathValue("id")
	a.catalogIDs(r.Context(), prov)
	a.cat.mu.Lock()
	raw := a.cat.m[prov].raw
	a.cat.mu.Unlock()
	if raw == nil {
		writeError(w, http.StatusNotFound, "no list answer from this provider")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(raw)
}
