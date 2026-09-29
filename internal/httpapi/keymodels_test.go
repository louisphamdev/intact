package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// callV1 sends one /v1 request with a dashboard key.
func callV1(h http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	req := loopbackRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestKeyModelsLimitWhatAKeyCanCall(t *testing.T) {
	groq := &fakeProvider{models: `{"data":[{"id":"llama"},{"id":"other"}]}`}
	nv := &fakeProvider{models: `{"data":[{"id":"llama"}]}`}
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "g", "kg")
	s.CreateConnection("nvidia", "n", "kn")
	cfg := authConfig()
	h := NewWithAuth(s, map[string]string{"groq": groq.start(t), "nvidia": nv.start(t)}, cfg)
	ck := loginCookie(t, h, cfg)

	rec := postKey(h, "/keys", `{"name":"shared","models":["nvidia/llama"," ","nvidia/llama"]}`, ck, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("create key: %d %s", rec.Code, rec.Body.String())
	}
	list, _ := s.ListAPIKeys()
	if len(list) != 1 || strings.Join(list[0].Models, ",") != "nvidia/llama" {
		t.Fatalf("stored models = %+v, want [nvidia/llama]", list)
	}
	key, _ := s.RevealAPIKey(list[0].ID)

	cases := []struct {
		model string
		code  int
	}{
		{"nvidia/llama", http.StatusOK},
		{"groq/llama", http.StatusForbidden},
		{"groq/other", http.StatusForbidden},
		{"other", http.StatusForbidden},
		{"NVIDIA/llama", http.StatusForbidden},
		{"llama", http.StatusOK},
	}
	for _, c := range cases {
		rec := callV1(h, "POST", "/v1/chat/completions", key, `{"model":"`+c.model+`"}`)
		if rec.Code != c.code {
			t.Errorf("%s: code=%d body=%s, want %d", c.model, rec.Code, rec.Body.String(), c.code)
		}
	}
	// The bare id is served by groq too, but the key allows it only at nvidia.
	if len(groq.calls) != 0 {
		t.Errorf("groq was called for a key limited to nvidia: %q", groq.calls)
	}
	if rec := callV1(h, "POST", "/v1/messages/count_tokens", key, `{"model":"groq/other"}`); rec.Code != http.StatusForbidden {
		t.Errorf("count_tokens for a refused model: code=%d", rec.Code)
	}

	// Emptying the list gives the key every model again.
	if rec := postKey(h, "/keys/"+list[0].ID+"/models", `{"models":[]}`, ck, ""); rec.Code != http.StatusOK {
		t.Fatalf("set models: %d %s", rec.Code, rec.Body.String())
	}
	if rec := callV1(h, "POST", "/v1/chat/completions", key, `{"model":"groq/other"}`); rec.Code != http.StatusOK {
		t.Errorf("all models: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := postKey(h, "/keys/"+list[0].ID+"/models", `{"models":["groq/other"]}`, nil, key); rec.Code == http.StatusOK {
		t.Error("a dashboard key changed its own model list")
	}
}
