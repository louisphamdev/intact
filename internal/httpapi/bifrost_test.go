package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// Claude Code reaching a Claude Code account must arrive as Claude Code: only
// the credential changes. No identity override, no blacklist on body or headers.
func TestBifrostChangesOnlyTheCredential(t *testing.T) {
	var gotUA, gotApp, gotBeta, gotAuth, gotKey, gotStainless, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotApp = r.Header.Get("X-App")
		gotBeta = r.Header.Get("Anthropic-Beta")
		gotAuth = r.Header.Get("Authorization")
		gotKey = r.Header.Get("X-Api-Key")
		gotStainless = r.Header.Get("X-Stainless-Lang")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte(`{}`))
	}))
	defer up.Close()

	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.CreateConnection("claude", "test", "oauth-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveFilter(store.Filter{Provider: "claude", Kind: "header", Pattern: "x-stainless-lang", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveFilter(store.Filter{Provider: "claude", Kind: "field", Pattern: "thinking", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	h := New(s, map[string]string{"claude": up.URL})
	body := `{"model":"claude/claude-opus-5","max_tokens":1,"thinking":{"type":"adaptive"},"tools":[{"name":"t","input_schema":{"type":"object","cache_control":{"type":"ephemeral"}}}]}`
	req := loopbackRequest("POST", "/v1/messages?beta=true", strings.NewReader(body))
	req.Header.Set("User-Agent", "claude-cli/2.1.300 (external, cli)")
	req.Header.Set("X-App", "cli")
	req.Header.Set("X-Stainless-Lang", "js")
	req.Header.Set("X-Api-Key", "sk-intact-caller")
	req.Header.Set("Anthropic-Beta", "interleaved-thinking-2025-05-14,some-new-beta")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotUA != "claude-cli/2.1.300 (external, cli)" {
		t.Errorf("User-Agent = %q: the client's own identity must pass", gotUA)
	}
	if gotApp != "cli" || gotStainless != "js" {
		t.Errorf("X-App = %q, X-Stainless-Lang = %q: client headers must pass, header filters must not run", gotApp, gotStainless)
	}
	if gotAuth != "Bearer oauth-token" || gotKey != "" {
		t.Errorf("Authorization = %q, X-Api-Key = %q: only the account credential may reach Anthropic", gotAuth, gotKey)
	}
	for _, want := range []string{"interleaved-thinking-2025-05-14", "some-new-beta", "oauth-2025-04-20", "claude-code-20250219"} {
		if !strings.Contains(gotBeta, want) {
			t.Errorf("Anthropic-Beta = %q, missing %s", gotBeta, want)
		}
	}
	want := strings.Replace(body, "claude/claude-opus-5", "claude-opus-5", 1)
	if gotBody != want {
		t.Errorf("body was filtered:\n got %s\nwant %s", gotBody, want)
	}
}

// Another client keeps the old behaviour: intact's identity, and the blacklist.
func TestForeignClientStillGetsIdentityAndFilters(t *testing.T) {
	var gotUA, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("claude", "test", "oauth-token")
	s.SaveFilter(store.Filter{Provider: "claude", Kind: "field", Pattern: "thinking", Enabled: true})

	h := New(s, map[string]string{"claude": up.URL})
	req := loopbackRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude/m","max_tokens":1,"thinking":{"type":"adaptive"}}`))
	req.Header.Set("User-Agent", "opencode/1.0")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !strings.HasPrefix(gotUA, "claude-cli/") || strings.Contains(gotUA, "opencode") {
		t.Errorf("User-Agent = %q, want intact's identity", gotUA)
	}
	if strings.Contains(gotBody, "thinking") {
		t.Errorf("body = %s: the blacklist must still run for another client", gotBody)
	}
}

// The model list tells a caller which client a model is native to.
func TestModelsNameTheBifrostClient(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("claude", "a", "k")
	h := New(s, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("GET", "/v1/models/claude/claude-opus-5", nil))
	var e map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if e["bifrost_ua"] != "claude-cli/" {
		t.Errorf("bifrost_ua = %v, want claude-cli/ (body %s)", e["bifrost_ua"], rec.Body.String())
	}
}

// The query of the caller belongs to the caller's API (Anthropic's ?beta=true).
// A translated request goes to an API of another shape and must not carry it:
// Antigravity's path already holds ?alt=sse, and "?alt=sse?beta=true" is a 400.
func TestTranslatedRequestDropsTheCallerQuery(t *testing.T) {
	var gotQuery, gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery, gotPath = r.URL.RawQuery, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "a", "k")
	h := New(s, map[string]string{"groq": up.URL})
	req := loopbackRequest("POST", "/v1/messages?beta=true", strings.NewReader(`{"model":"groq/m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("User-Agent", "claude-cli/2.1.283 (external, cli)")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/chat/completions" || gotQuery != "" {
		t.Errorf("upstream got %s?%s, want /chat/completions with no query", gotPath, gotQuery)
	}
}
