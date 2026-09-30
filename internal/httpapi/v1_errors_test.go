package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// v1Env is a server with auth on and one groq account behind url.
func v1Env(t *testing.T, url string) (http.Handler, string) {
	t.Helper()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	t.Cleanup(func() { s.Close() })
	s.CreateConnection("groq", "g", "kg")
	return NewWithAuth(s, map[string]string{"groq": url}, authConfig()), "machine-tok"
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body is not JSON: %s", rec.Body.String())
	}
	return m
}

func wantAnthropicError(t *testing.T, rec *httptest.ResponseRecorder, status int, typ string) {
	t.Helper()
	m := decodeBody(t, rec)
	e, _ := m["error"].(map[string]any)
	if rec.Code != status || m["type"] != "error" || e["type"] != typ || e["message"] == "" {
		t.Errorf("got %d %s, want %d Anthropic %s", rec.Code, rec.Body.String(), status, typ)
	}
}

func wantOpenAIError(t *testing.T, rec *httptest.ResponseRecorder, status int) map[string]any {
	t.Helper()
	m := decodeBody(t, rec)
	e, _ := m["error"].(map[string]any)
	_, hasParam := e["param"]
	_, hasCode := e["code"]
	if rec.Code != status || e == nil || e["message"] == "" || e["type"] == "" || !hasParam || !hasCode {
		t.Errorf("got %d %s, want %d OpenAI error", rec.Code, rec.Body.String(), status)
	}
	return e
}

func TestV1ErrorsSpeakTheCallersShape(t *testing.T) {
	f := &fakeProvider{models: `{"data":[{"id":"llama"}]}`}
	h, key := v1Env(t, f.start(t))

	wantAnthropicError(t, callV1(h, "POST", "/v1/messages", "bogus", `{}`), 401, "authentication_error")
	e := wantOpenAIError(t, callV1(h, "POST", "/v1/chat/completions", "bogus", `{}`), 401)
	if e["code"] != "invalid_api_key" {
		t.Errorf("code = %v", e["code"])
	}
	wantAnthropicError(t, callV1(h, "POST", "/v1/messages", key, `{"max_tokens":5,"messages":[]}`), 400, "invalid_request_error")
	wantAnthropicError(t, callV1(h, "POST", "/v1/messages", key, `{"model":"nope/x","max_tokens":5,"messages":[]}`), 404, "not_found_error")
	e = wantOpenAIError(t, callV1(h, "POST", "/v1/chat/completions", key, `{"model":"nope/x","messages":[]}`), 404)
	if e["code"] != "model_not_found" || e["param"] != "model" {
		t.Errorf("unknown model error = %v", e)
	}
	// Dashboard routes keep their own shape.
	if rec := callV1(h, "GET", "/api/usage", "bogus", ""); !strings.HasPrefix(rec.Body.String(), `{"error":"`) {
		t.Errorf("/api error changed shape: %s", rec.Body.String())
	}
}

func TestV1UnknownRoutesAndMethods(t *testing.T) {
	f := &fakeProvider{models: `{"data":[{"id":"llama"}]}`}
	h, key := v1Env(t, f.start(t))
	wantOpenAIError(t, callV1(h, "GET", "/v1/nope", key, ""), 404)
	rec := callV1(h, "GET", "/v1/messages", key, "")
	wantAnthropicError(t, rec, 405, "invalid_request_error")
	if rec.Header().Get("Allow") != "POST" {
		t.Errorf("Allow = %q", rec.Header().Get("Allow"))
	}
	wantOpenAIError(t, callV1(h, "GET", "/v1/chat/completions", key, ""), 405)

	req := loopbackRequest("OPTIONS", "/v1/messages", nil)
	req.Header.Set("Origin", "https://example.com")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	wantAnthropicError(t, rec, 405, "invalid_request_error")
}

func TestV1AnswersCarryARequestID(t *testing.T) {
	f := &fakeProvider{models: `{"data":[{"id":"llama"}]}`}
	h, key := v1Env(t, f.start(t))
	for _, rec := range []*httptest.ResponseRecorder{
		callV1(h, "POST", "/v1/chat/completions", key, `{"model":"groq/llama"}`),
		callV1(h, "POST", "/v1/messages", "bogus", `{}`),
	} {
		if id := rec.Header().Get("Request-Id"); !strings.HasPrefix(id, "req_") || rec.Header().Get("X-Request-Id") != id {
			t.Errorf("request ids = %q / %q", id, rec.Header().Get("X-Request-Id"))
		}
	}
}

func TestMessagesRequiresAPositiveMaxTokens(t *testing.T) {
	f := &fakeProvider{models: `{"data":[{"id":"llama"}]}`}
	h, key := v1Env(t, f.start(t))
	for _, body := range []string{
		`{"model":"groq/llama","messages":[{"role":"user","content":"x"}]}`,
		`{"model":"groq/llama","max_tokens":-5,"messages":[{"role":"user","content":"x"}]}`,
		`{"model":"groq/llama","max_tokens":"9","messages":[{"role":"user","content":"x"}]}`,
	} {
		rec := callV1(h, "POST", "/v1/messages", key, body)
		wantAnthropicError(t, rec, 400, "invalid_request_error")
		if !strings.Contains(rec.Body.String(), "max_tokens") {
			t.Errorf("message does not name max_tokens: %s", rec.Body.String())
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("an invalid request reached the upstream: %q", f.calls)
	}
	// count_tokens takes no max_tokens.
	if rec := callV1(h, "POST", "/v1/messages/count_tokens", key, `{"model":"groq/llama","messages":[{"role":"user","content":"x"}]}`); rec.Code != 200 {
		t.Errorf("count_tokens = %d %s", rec.Code, rec.Body.String())
	}
}

func TestAWholeErrorBodyUnder200IsNotASuccess(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Write([]byte(`{"data":[{"id":"llama"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error":{"message":"quota gone","type":"insufficient_quota"}}`))
	}))
	defer up.Close()
	h, key := v1Env(t, up.URL)
	rec := callV1(h, "POST", "/v1/messages", key, `{"model":"groq/llama","max_tokens":5,"messages":[{"role":"user","content":"x"}]}`)
	wantAnthropicError(t, rec, 502, "billing_error")
}

func TestModelsCarryCreatedAndOneModelIsServed(t *testing.T) {
	f := &fakeProvider{models: `{"data":[{"id":"llama"}]}`}
	h, key := v1Env(t, f.start(t))
	list := decodeBody(t, callV1(h, "GET", "/v1/models", key, ""))
	entry := list["data"].([]any)[0].(map[string]any)
	if c, ok := entry["created"].(float64); !ok || c <= 0 {
		t.Errorf("created = %v", entry["created"])
	}
	if _, ok := entry["created_at"].(string); !ok {
		t.Errorf("created_at = %v", entry["created_at"])
	}
	for _, p := range []string{"/v1/models/groq/llama", "/v1/models/groq%2Fllama", "/v1/models/llama"} {
		rec := callV1(h, "GET", p, key, "")
		if m := decodeBody(t, rec); rec.Code != 200 || m["id"] != "groq/llama" || m["object"] != "model" || m["type"] != "model" {
			t.Errorf("%s = %d %s", p, rec.Code, rec.Body.String())
		}
	}
	e := wantOpenAIError(t, callV1(h, "GET", "/v1/models/groq/none", key, ""), 404)
	if e["code"] != "model_not_found" {
		t.Errorf("code = %v", e["code"])
	}
}
