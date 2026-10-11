package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

// An account that answers 403 for itself (Antigravity "Verify your account to
// continue.") must not reach the caller while another account is healthy.
func TestGapForbiddenAccountFailsOver(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Write([]byte(`{"data":[]}`))
			return
		}
		if r.Header.Get("Authorization") == "Bearer ka" {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":{"message":"Verify your account to continue."}}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "a", "ka")
	s.CreateConnection("groq", "b", "kb")
	h := New(s, map[string]string{"groq": up.URL})
	for i := 0; i < 2; i++ {
		if rec := postV1(h, `{"model":"groq/llama","stream":false}`); rec.Code != http.StatusOK {
			t.Fatalf("call %d code=%d body=%s, want the healthy account's 200", i, rec.Code, rec.Body.String())
		}
	}
}

// The last account's error is still what the caller sees when every account
// refuses.
func TestGapForbiddenEveryAccountRelaysLastError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"message":"denied ` + r.Header.Get("Authorization") + `"}}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "a", "ka")
	s.CreateConnection("groq", "b", "kb")
	h := New(s, map[string]string{"groq": up.URL})
	rec := postV1(h, `{"model":"groq/llama","stream":false}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "denied Bearer") {
		t.Fatalf("code=%d body=%s, want the last account's 403", rec.Code, rec.Body.String())
	}
}

// A revoked OAuth account whose refresh fails (invalid_grant) must give way to
// the next account instead of relaying its 401.
func TestGapDeadOAuthAccountFailsOver(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer tokenSrv.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.Header.Get("Authorization") == "Bearer revoked" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"revoked"}}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	dead, _ := s.CreateConnection("claude", "dead", "revoked")
	s.SetOAuth(dead.ID, store.OAuthCreds{
		RefreshToken: "rt", TokenURL: tokenSrv.URL, ClientID: "cid",
		ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	s.CreateConnection("claude", "live", "good")
	h := NewWithAuth(s, map[string]string{"claude": up.URL}, nil)
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, loopbackRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude/m","max_tokens":1}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d code=%d body=%s, want the live account's 200", i, rec.Code, rec.Body.String())
		}
	}
}

// A chat caller reaching a chat upstream is passed through, so a tool name over 64 characters went
// out as it came and the upstream refused the request. It goes out shortened and comes back whole.
func TestToolNameOver64ShortenedAndRestoredOnChatPassthrough(t *testing.T) {
	long := "mcp__plugin_chrome-devtools-mcp_chrome-devtools__list_console_messages"
	var sent string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sent = string(b)
		short := regexp.MustCompile(`"name":"(mcp__[^"]*)"`).FindStringSubmatch(sent)
		if strings.Contains(sent, long) || short == nil || len(short[1]) > 64 {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"function tool names must be 1-64 letters, digits, underscores or hyphens"}}`))
			return
		}
		call := `"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"` + short[1] + `","arguments":"{}"}}]`
		if strings.Contains(sent, `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte(`data: {"id":"x","model":"llama","choices":[{"index":0,"delta":{` + call + `}}]}` + "\n\n" + "data: [DONE]\n\n"))
			return
		}
		w.Write([]byte(`{"id":"x","model":"llama","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant",` + call + `}}]}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "a", "ka")
	h := New(s, map[string]string{"groq": up.URL})
	for _, stream := range []string{"false", "true"} {
		rec := postV1(h, `{"model":"groq/llama","stream":`+stream+`,"messages":[{"role":"user","content":"go"},
			{"role":"assistant","tool_calls":[{"id":"c0","type":"function","function":{"name":"`+long+`","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"c0","content":"ok"}],
			"tools":[{"type":"function","function":{"name":"`+long+`","parameters":{"type":"object"}}}]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("stream=%s code=%d body=%s upstream got %s", stream, rec.Code, rec.Body.String(), sent)
		}
		if !strings.Contains(rec.Body.String(), `"name":"`+long+`"`) {
			t.Errorf("stream=%s: the call did not come back under the caller's name: %s", stream, rec.Body.String())
		}
	}
}

// An account that answers 200 and closes before the first byte gave the caller an empty 200, because
// failover had already committed to it. The next account must take the request.
func TestGapEmpty200FailsOver(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if r.Header.Get("Authorization") == "Bearer ka" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Write([]byte(`data: {"id":"x","model":"llama","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" + "data: [DONE]\n\n"))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "a", "ka")
	s.CreateConnection("groq", "b", "kb")
	h := New(s, map[string]string{"groq": up.URL})
	for i := 0; i < 2; i++ {
		rec := postV1(h, `{"model":"groq/llama","stream":true,"messages":[{"role":"user","content":"go"}]}`)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content":"hi"`) {
			t.Fatalf("call %d code=%d body=%q, want the healthy account's stream", i, rec.Code, rec.Body.String())
		}
	}
}

// Reasoning that one ChatGPT account encrypted cannot be read by another, so a Codex thread that
// moved account is refused with invalid_encrypted_content on every turn. The turn is sent again to
// the same account without that foreign reasoning, once.
func TestCodexFailoverStripsForeignEncryptedReasoning(t *testing.T) {
	var bodies []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if strings.Contains(string(b), "encrypted_content") {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"The encrypted content for item rs_1 could not be verified.","type":"invalid_request_error","param":null,"code":"invalid_encrypted_content"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"output\":[]}}\n\n")
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("codex", "cx", fakeJWT("acct-9"))
	h := New(s, map[string]string{"codex": up.URL})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/v1/responses", strings.NewReader(`{"model":"codex/gpt-5.5","stream":true,"store":false,"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAA-foreign"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}]}`)))
	if rec.Code != http.StatusOK || len(bodies) != 2 {
		t.Fatalf("code=%d upstream calls=%d body=%s, want a 200 after one retry", rec.Code, len(bodies), rec.Body.String())
	}
	if retry := bodies[1]; strings.Contains(retry, "gAAA-foreign") || !strings.Contains(retry, `"text":"hello"`) || !strings.Contains(retry, `"text":"again"`) {
		t.Errorf("the retry must drop only the foreign reasoning: %s", retry)
	}
}

// A Messages client that is not Claude Code sends its own Anthropic-Beta, which kept the default
// from being applied, so the OAuth credential reached Anthropic without the betas it cannot work
// without. They are added to the caller's list, on every path.
func TestGapNonBifrostBetaKeepsCredentialBetas(t *testing.T) {
	var gotBeta string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBeta = r.Header.Get("Anthropic-Beta")
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("claude", "test", "oauth-token")
	h := New(s, map[string]string{"claude": up.URL})
	req := loopbackRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude/m","max_tokens":1}`))
	req.Header.Set("User-Agent", "Anthropic/Python 0.70.0")
	req.Header.Set("Anthropic-Beta", "prompt-caching-2024-07-31")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(gotBeta, "prompt-caching-2024-07-31") || !strings.Contains(gotBeta, "oauth-2025-04-20") || !strings.Contains(gotBeta, "claude-code-20250219") {
		t.Errorf("Anthropic-Beta = %q, want the caller's list with the credential's betas added", gotBeta)
	}
}
