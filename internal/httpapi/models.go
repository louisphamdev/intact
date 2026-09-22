package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

// modelTestKey marks a test call made from the dashboard, which may reach a
// model the operator switched off (to decide whether to switch it on).
type modelTestKey struct{}

// providerModelTable serves a provider's models with their switch, staleness
// and last test. It fetches the list first when the cache is stale.
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
	writeJSON(w, map[string]any{"models": list, "ok": e.ok, "fetchedAt": e.at.UTC().Format(time.RFC3339)})
}

type modelsBody struct {
	Models []string `json:"models"`
	Active bool     `json:"active"`
	Model  string   `json:"model"`
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
	res := a.runModelTest(r.Context(), prov, b.Model)
	a.store.RecordModelTest(prov, b.Model, res.OK, res.Ms, res.Message)
	writeJSON(w, res)
}

type modelTest struct {
	OK      bool   `json:"ok"`
	Status  int    `json:"status"`
	Ms      int64  `json:"ms"`
	Message string `json:"message"`
}

func (a *api) runModelTest(ctx context.Context, prov, model string) modelTest {
	body, _ := json.Marshal(map[string]any{"model": prov + "/" + model, "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": "Reply with the single word OK."}}})
	ctx, cancel := context.WithTimeout(context.WithValue(ctx, modelTestKey{}, true), 90*time.Second)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.SetPathValue("path", "chat/completions")
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
