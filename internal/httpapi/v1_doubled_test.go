package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// A client that puts /v1 in its base URL and then adds /v1/messages itself
// (Claude Code does) must reach the same route as a correct client.
func TestDoubledV1PrefixReachesTheSameRoute(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Write([]byte(`{"data":[{"id":"llama"}]}`))
			return
		}
		io.ReadAll(r.Body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"llama","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "g", "kg")
	cfg := authConfig()
	h := NewWithAuth(s, map[string]string{"groq": up.URL}, cfg)
	ck := loginCookie(t, h, cfg)
	key := keyThroughDashboard(t, h, ck, "cc").Key

	rec := callV1(h, "POST", "/v1/v1/messages?beta=true", key, `{"model":"groq/llama","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"type":"message"`) {
		t.Errorf("/v1/v1/messages: code=%d body=%s, want an Anthropic answer", rec.Code, rec.Body.String())
	}
	rec = callV1(h, "POST", "/v1/v1/chat/completions", key, `{"model":"groq/llama"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("/v1/v1/chat/completions: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Join(paths, ",") != "/chat/completions,/chat/completions" {
		t.Errorf("upstream paths = %q, want the doubled /v1 gone", paths)
	}
	if rec := callV1(h, "GET", "/v1/v1/models", key, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "groq/llama") {
		t.Errorf("/v1/v1/models: code=%d body=%s", rec.Code, rec.Body.String())
	}
}
