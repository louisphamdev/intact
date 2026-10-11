package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/translate"
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

// A caller may send its betas on several Anthropic-Beta lines. Reading only the first line and then
// setting the header dropped the rest before the credential's betas were added.
func TestGapBetaLinesMergedBeforeCredentialBetas(t *testing.T) {
	for _, ua := range []string{"Anthropic/Python 0.70.0", "claude-cli/2.1.0 (external, cli)"} {
		t.Run(ua, func(t *testing.T) {
			var lines []string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				lines = r.Header.Values("Anthropic-Beta")
				w.Write([]byte(`{}`))
			}))
			defer up.Close()
			s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
			defer s.Close()
			s.CreateConnection("claude", "test", "oauth-token")
			h := New(s, map[string]string{"claude": up.URL})
			req := loopbackRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude/m","max_tokens":1}`))
			req.Header.Set("User-Agent", ua)
			req.Header.Add("Anthropic-Beta", "prompt-caching-2024-07-31")
			req.Header.Add("Anthropic-Beta", "files-api-2025-04-14")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if len(lines) != 1 {
				t.Fatalf("upstream got %d Anthropic-Beta lines %q, want one", len(lines), lines)
			}
			var betas []string
			count := map[string]int{}
			for _, b := range strings.Split(lines[0], ",") {
				b = strings.TrimSpace(b)
				betas = append(betas, b)
				count[b]++
			}
			if len(betas) < 2 || betas[0] != "prompt-caching-2024-07-31" || betas[1] != "files-api-2025-04-14" {
				t.Errorf("Anthropic-Beta = %q, want both caller betas first, in order", lines[0])
			}
			for _, b := range []string{"claude-code-20250219", "oauth-2025-04-20"} {
				if count[b] != 1 {
					t.Errorf("Anthropic-Beta = %q carries %s %d times, want once", lines[0], b, count[b])
				}
			}
		})
	}
}

// Responses and Antigravity always stream upstream, so a chat caller that did not ask for a stream
// still gets nothing from an empty 200; the next account must take the request.
func TestGapEmpty200FailsOverOnAlwaysStreamingShape(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.URL.Path != "/responses" {
			w.Write([]byte(`{"data":[]}`))
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.WriteHeader(http.StatusOK)
			return
		}
		io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.5\"}}\n\n"+
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n")
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("codex", "a", fakeJWT("acct-1"))
	s.CreateConnection("codex", "b", fakeJWT("acct-2"))
	h := New(s, map[string]string{"codex": up.URL})
	rec := postV1(h, `{"model":"codex/gpt-5.5","messages":[{"role":"user","content":"hi"}]}`)
	if b := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(b, `"object":"chat.completion"`) || !strings.Contains(b, `"content":"OK"`) || calls != 2 {
		t.Fatalf("code=%d upstream calls=%d body=%s, want the second account's answer after one empty 200", rec.Code, calls, b)
	}
}

// Only invalid_encrypted_content earns the retry without encrypted reasoning. Any other 400 is the
// caller's to read, as the upstream sent it, after one call.
func TestCodexResponses400WithoutMarkerIsRelayedOnce(t *testing.T) {
	const refusal = `{"error":{"message":"Invalid value for 'input[1].summary'.","type":"invalid_request_error","param":"input","code":"invalid_value"}}`
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.URL.Path != "/responses" {
			w.Write([]byte(`{"data":[]}`))
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, refusal)
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
	if rec.Code != http.StatusBadRequest || rec.Body.String() != refusal || calls != 1 {
		t.Fatalf("code=%d upstream calls=%d body=%s, want the upstream 400 unchanged after one call", rec.Code, calls, rec.Body.String())
	}
}

// The last account's empty 200 has no account after it, so it is relayed rather than dropped.
func TestGapEmpty200OnLastAccountIsRelayed(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.URL.Path != "/responses" {
			w.Write([]byte(`{"data":[]}`))
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("codex", "a", fakeJWT("acct-1"))
	s.CreateConnection("codex", "b", fakeJWT("acct-2"))
	h := New(s, map[string]string{"codex": up.URL})
	rec := postV1(h, `{"model":"codex/gpt-5.5","messages":[{"role":"user","content":"hi"}]}`)
	if b := rec.Body.String(); calls != 2 || !strings.Contains(b, "upstream stream ended without response.completed") {
		t.Fatalf("code=%d upstream calls=%d body=%s, want the last account's empty answer relayed as a cut stream", rec.Code, calls, b)
	}
}

// The marker alone does not earn the retry: with no encrypted reasoning to drop (none sent, or
// encrypted_content null) the same body would go out again, so the 400 is relayed after one call.
func TestCodexResponses400MarkerWithNothingToStripIsRelayedOnce(t *testing.T) {
	const refusal = `{"error":{"message":"The encrypted content for item rs_1 could not be verified.","type":"invalid_request_error","param":null,"code":"invalid_encrypted_content"}}`
	inputs := map[string]string{
		"no reasoning":        `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`,
		"null encrypted text": `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":null}`,
	}
	for name, item := range inputs {
		t.Run(name, func(t *testing.T) {
			calls := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if r.URL.Path != "/responses" {
					w.Write([]byte(`{"data":[]}`))
					return
				}
				calls++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, refusal)
			}))
			defer up.Close()
			s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
			defer s.Close()
			s.CreateConnection("codex", "cx", fakeJWT("acct-9"))
			h := New(s, map[string]string{"codex": up.URL})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, loopbackRequest("POST", "/v1/responses", strings.NewReader(`{"model":"codex/gpt-5.5","stream":true,"store":false,"input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},`+item+`]}`)))
			if rec.Code != http.StatusBadRequest || rec.Body.String() != refusal || calls != 1 {
				t.Fatalf("code=%d upstream calls=%d body=%s, want the upstream 400 unchanged after one call", rec.Code, calls, rec.Body.String())
			}
		})
	}
}

// An input made only of foreign encrypted reasoning would go again as input:[], a different turn the
// caller never asked for. The refusal is relayed after one call, on the 400 and on the stream.
func TestCodexOnlyForeignReasoningIsNotRetriedEmpty(t *testing.T) {
	const refusal = `{"error":{"message":"The encrypted content for item rs_1 could not be verified.","type":"invalid_request_error","param":null,"code":"invalid_encrypted_content"}}`
	const request = `{"model":"codex/gpt-5.5","stream":true,"store":false,"input":[
		{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAA-foreign"}]}`
	if _, ok := withoutEncryptedReasoning([]byte(request)); ok {
		t.Errorf("withoutEncryptedReasoning offered an empty input as a retry")
	}
	rec, bodies := codexSSEFor(t, request, func(w http.ResponseWriter, n int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, refusal)
	})
	if rec.Code != http.StatusBadRequest || rec.Body.String() != refusal || len(bodies) != 1 {
		t.Errorf("400: code=%d upstream calls=%d body=%s, want the upstream 400 unchanged after one call", rec.Code, len(bodies), rec.Body.String())
	}
	rec, bodies = codexSSEFor(t, request, func(w http.ResponseWriter, n int, body string) {
		io.WriteString(w, codexRefusalEvent)
	})
	if rec.Code != http.StatusOK || rec.Body.String() != codexRefusalEvent || len(bodies) != 1 {
		t.Errorf("stream: code=%d upstream calls=%d body=%q, want the upstream stream unchanged after one call", rec.Code, len(bodies), rec.Body.String())
	}
}

// A retry that cannot reach the upstream leaves the first answer standing: the caller reads the
// upstream's own 400, not an error intact made up.
func TestCodexResponsesRetryFailureRelaysFirst400(t *testing.T) {
	const refusal = `{"error":{"message":"The encrypted content for item rs_1 could not be verified.","type":"invalid_request_error","param":null,"code":"invalid_encrypted_content"}}`
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.URL.Path != "/responses" {
			w.Write([]byte(`{"data":[]}`))
			return
		}
		calls++
		if calls > 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, refusal)
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("codex", "cx", fakeJWT("acct-9"))
	h := New(s, map[string]string{"codex": up.URL})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/v1/responses", strings.NewReader(`{"model":"codex/gpt-5.5","stream":true,"store":false,"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAA-foreign"}]}`)))
	if rec.Code != http.StatusBadRequest || rec.Body.String() != refusal || calls != 2 {
		t.Fatalf("code=%d upstream calls=%d body=%s, want the first 400 unchanged after a failed retry", rec.Code, calls, rec.Body.String())
	}
}

// codexSSE runs one streamed Codex Responses turn whose input carries foreign encrypted reasoning.
// answer writes the upstream's 200 stream for the n-th call; the caller's recorder and the bodies the
// upstream got come back.
func codexSSE(t *testing.T, answer func(w http.ResponseWriter, n int, body string)) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	return codexSSEFor(t, `{"model":"codex/gpt-5.5","stream":true,"store":false,"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAA-foreign"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}]}`, answer)
}

// codexSSEFor is codexSSE with the caller's own request.
func codexSSEFor(t *testing.T, request string, answer func(w http.ResponseWriter, n int, body string)) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	var bodies []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/responses" {
			w.Write([]byte(`{"data":[]}`))
			return
		}
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "text/event-stream")
		answer(w, len(bodies), string(b))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("codex", "cx", fakeJWT("acct-9"))
	h := New(s, map[string]string{"codex": up.URL})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/v1/responses", strings.NewReader(request)))
	return rec, bodies
}

const codexOKStream = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_2\"}}\n\n" +
	"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n" +
	"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\",\"output\":[]}}\n\n"

// An upstream can refuse foreign encrypted reasoning inside a 200 stream, as its first event. That is
// the same refusal as the 400, so the turn goes again to the same account without it, once.
func TestCodexSSEEncryptedContentErrorRetries(t *testing.T) {
	firsts := map[string]string{
		"error":           "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_encrypted_content\",\"message\":\"The encrypted content for item rs_1 could not be verified.\"}}\n\n",
		"response.failed": "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_1\",\"status\":\"failed\",\"error\":{\"code\":\"invalid_encrypted_content\",\"message\":\"The encrypted content for item rs_1 could not be verified.\"}}}\n\n",
	}
	for name, first := range firsts {
		t.Run(name, func(t *testing.T) {
			rec, bodies := codexSSE(t, func(w http.ResponseWriter, n int, body string) {
				if strings.Contains(body, "encrypted_content") {
					io.WriteString(w, first)
					return
				}
				io.WriteString(w, codexOKStream)
			})
			if rec.Code != http.StatusOK || len(bodies) != 2 || rec.Body.String() != codexOKStream {
				t.Fatalf("code=%d upstream calls=%d body=%q, want the retry's stream after one retry", rec.Code, len(bodies), rec.Body.String())
			}
			if retry := bodies[1]; strings.Contains(retry, "gAAA-foreign") || !strings.Contains(retry, `"text":"again"`) {
				t.Errorf("the retry must drop only the foreign reasoning: %s", retry)
			}
		})
	}
}

// Any other error as the first event is the caller's to read, byte for byte, after one call.
func TestCodexSSEOtherErrorRelayedOnce(t *testing.T) {
	const stream = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"context_length_exceeded\",\"message\":\"Your input exceeds the context window of this model.\"}}\n\n"
	rec, bodies := codexSSE(t, func(w http.ResponseWriter, n int, body string) {
		io.WriteString(w, stream)
	})
	if rec.Code != http.StatusOK || len(bodies) != 1 || rec.Body.String() != stream {
		t.Fatalf("code=%d upstream calls=%d body=%q, want the upstream stream unchanged after one call", rec.Code, len(bodies), rec.Body.String())
	}
}

// The look at the first event must not change the stream: an answer that starts normally reaches the
// caller byte for byte, also when its first event arrives in pieces.
func TestCodexSSEFirstEventRelayedByteIdentical(t *testing.T) {
	rec, bodies := codexSSE(t, func(w http.ResponseWriter, n int, body string) {
		f := w.(http.Flusher)
		half := len(codexOKStream) / 4
		io.WriteString(w, codexOKStream[:half])
		f.Flush()
		io.WriteString(w, codexOKStream[half:])
	})
	if rec.Code != http.StatusOK || len(bodies) != 1 || rec.Body.String() != codexOKStream {
		t.Fatalf("code=%d upstream calls=%d body=%q, want the stream unchanged after one call", rec.Code, len(bodies), rec.Body.String())
	}
}

// Only an error event earns the retry. A first event that merely quotes the marker, here in the
// model's own text, is the answer and reaches the caller unchanged after one call.
func TestCodexSSEQuotedMarkerInDeltaIsNotRetried(t *testing.T) {
	const stream = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"The code invalid_encrypted_content means the reasoning came from another account.\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[]}}\n\n"
	rec, bodies := codexSSE(t, func(w http.ResponseWriter, n int, body string) {
		io.WriteString(w, stream)
	})
	if rec.Code != http.StatusOK || len(bodies) != 1 || rec.Body.String() != stream {
		t.Fatalf("code=%d upstream calls=%d body=%q, want the upstream stream unchanged after one call", rec.Code, len(bodies), rec.Body.String())
	}
}

// A retry that cannot reach the upstream leaves the first stream standing, every byte of it,
// including the event that was read before the retry.
func TestCodexSSERetryFailureRelaysFirstStream(t *testing.T) {
	const first = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_encrypted_content\",\"message\":\"The encrypted content for item rs_1 could not be verified.\"}}\n\n"
	rec, bodies := codexSSE(t, func(w http.ResponseWriter, n int, body string) {
		if n >= 2 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		io.WriteString(w, first)
	})
	if len(bodies) != 2 || rec.Body.String() != first {
		t.Fatalf("code=%d upstream calls=%d body=%q, want the first stream unchanged after a failed retry", rec.Code, len(bodies), rec.Body.String())
	}
}

// An upstream may open a stream with a keep-alive comment frame. The refusal behind it is still the
// first event, so the turn goes again without the foreign reasoning, once.
func TestCodexSSERefusalAfterCommentFrameRetries(t *testing.T) {
	const refusal = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_encrypted_content\",\"message\":\"The encrypted content for item rs_1 could not be verified.\"}}\n\n"
	for _, ping := range []string{": ping\n\n", ": keep-alive\r\n\r\n", ": ping\n\n: ping\n\n"} {
		t.Run(strings.TrimSpace(ping), func(t *testing.T) {
			rec, bodies := codexSSE(t, func(w http.ResponseWriter, n int, body string) {
				if strings.Contains(body, "encrypted_content") {
					io.WriteString(w, ping)
					w.(http.Flusher).Flush()
					io.WriteString(w, refusal)
					return
				}
				io.WriteString(w, codexOKStream)
			})
			if rec.Code != http.StatusOK || len(bodies) != 2 || rec.Body.String() != codexOKStream {
				t.Fatalf("code=%d upstream calls=%d body=%q, want the retry's stream after one retry", rec.Code, len(bodies), rec.Body.String())
			}
		})
	}
}

// Comment frames read ahead are relayed with the rest, byte for byte, also when a frame delimiter
// arrives split across reads.
func TestCodexSSECommentFrameRelayedByteIdentical(t *testing.T) {
	pieces := []string{": ping\n", "\n: keep-alive\r\n\r", "\n" + codexOKStream[:20], codexOKStream[20:]}
	rec, bodies := codexSSE(t, func(w http.ResponseWriter, n int, body string) {
		for _, p := range pieces {
			io.WriteString(w, p)
			w.(http.Flusher).Flush()
		}
	})
	if want := strings.Join(pieces, ""); rec.Code != http.StatusOK || len(bodies) != 1 || rec.Body.String() != want {
		t.Fatalf("code=%d upstream calls=%d body=%q, want %q after one call", rec.Code, len(bodies), rec.Body.String(), want)
	}
}

// firstSSEEvent stops at the end of the first real event, past any comment frames before it.
func TestFirstSSEEventSkipsCommentFrames(t *testing.T) {
	ev := "event: error\ndata: {}\n\n"
	src := ": ping\n\n: keep-alive\r\n\r\n" + ev
	got := firstSSEEvent(iotest.OneByteReader(strings.NewReader(src+"tail")), 64<<10)
	if string(got) != src {
		t.Errorf("firstSSEEvent read %q, want %q", got, src)
	}
	if got := firstSSEEvent(strings.NewReader(": ping\n\n"), 64<<10); string(got) != ": ping\n\n" {
		t.Errorf("a stream of comments only read %q", got)
	}
}

// Comment frames are held back only for a while: past sseCommentWait what was read goes to the caller.
func TestFirstSSEEventCommentSkipIsTimeBounded(t *testing.T) {
	defer sseCommentWait.Set(20 * time.Millisecond)()
	r := &pacedReader{pieces: []string{": ping\n\n", ": ping\n\n", "event: error\ndata: {}\n\n"}, pause: 60 * time.Millisecond}
	if got := string(firstSSEEvent(r, 64<<10)); got != ": ping\n\n: ping\n\n" {
		t.Errorf("firstSSEEvent read %q, want the two pings and no more", got)
	}
}

// The bound stops only the skipping of comments: an event already begun is read to its end, so a
// refusal split across reads after the bound still earns the retry.
func TestFirstSSEEventBoundDoesNotCutAnEvent(t *testing.T) {
	defer sseCommentWait.Set(20 * time.Millisecond)()
	half := len(codexRefusalEvent) / 2
	r := &pacedReader{pieces: []string{": ping\n\n", codexRefusalEvent[:half], codexRefusalEvent[half:]}, pause: 60 * time.Millisecond}
	head := firstSSEEvent(r, 64<<10)
	if string(head) != ": ping\n\n"+codexRefusalEvent || !isEncryptedContentRefusal(head) {
		t.Errorf("firstSSEEvent read %q, want the ping and the whole refusal", head)
	}
}

// A frame is a comment only once its blank line is read: past the bound, a frame cut after its comment
// line (or inside it) can still be an event, so it is read on and the refusal behind it is seen.
func TestFirstSSEEventBoundDoesNotCutAMixedFrame(t *testing.T) {
	defer sseCommentWait.Set(20 * time.Millisecond)()
	for _, cut := range []string{": note\n", ": no"} {
		rest := strings.TrimPrefix(": note\n"+codexRefusalEvent, cut)
		r := &pacedReader{pieces: []string{": ping\n\n", cut, rest}, pause: 60 * time.Millisecond}
		head := firstSSEEvent(r, 64<<10)
		if string(head) != ": ping\n\n: note\n"+codexRefusalEvent || !isEncryptedContentRefusal(head) {
			t.Errorf("cut after %q: firstSSEEvent read %q, want the ping and the whole mixed frame", cut, head)
		}
	}
}

// The same through the gateway: a mixed frame that arrives after the bound still earns the retry.
func TestCodexSSEMixedFrameAfterBoundRetries(t *testing.T) {
	defer sseCommentWait.Set(20 * time.Millisecond)()
	rec, bodies := codexSSE(t, func(w http.ResponseWriter, n int, body string) {
		if !strings.Contains(body, "encrypted_content") {
			io.WriteString(w, codexOKStream)
			return
		}
		for i, p := range []string{": ping\n\n", ": note\n", codexRefusalEvent} {
			if i > 0 {
				time.Sleep(60 * time.Millisecond)
			}
			io.WriteString(w, p)
			w.(http.Flusher).Flush()
		}
	})
	if rec.Code != http.StatusOK || len(bodies) != 2 || rec.Body.String() != codexOKStream {
		t.Fatalf("code=%d upstream calls=%d body=%q, want the retry's stream after one retry", rec.Code, len(bodies), rec.Body.String())
	}
}

// pacedReader returns one piece per Read and waits before each piece after the first.
type pacedReader struct {
	pieces []string
	pause  time.Duration
	n      int
}

func (p *pacedReader) Read(b []byte) (int, error) {
	if p.n >= len(p.pieces) {
		return 0, io.EOF
	}
	if p.n > 0 {
		time.Sleep(p.pause)
	}
	p.n++
	return copy(b, p.pieces[p.n-1]), nil
}

// A frame with a comment line and an event line is an event, so the refusal behind a note is seen.
func TestFirstSSEEventMixedCommentFrameIsAnEvent(t *testing.T) {
	frame := ": note\n" + codexRefusalEvent
	if isCommentFrame([]byte(strings.TrimSuffix(frame, "\n\n"))) {
		t.Errorf("isCommentFrame took a frame with event lines for a comment")
	}
	head := firstSSEEvent(iotest.OneByteReader(strings.NewReader(frame+"tail")), 64<<10)
	if string(head) != frame || !isEncryptedContentRefusal(head) {
		t.Errorf("firstSSEEvent read %q; the refusal behind the note was not seen", head)
	}
}

// Comments alone past the byte cap stop the read at the cap, and are not a refusal.
func TestFirstSSEEventCommentsPastCap(t *testing.T) {
	src := strings.Repeat(": ping\n\n", 100)
	head := firstSSEEvent(iotest.OneByteReader(strings.NewReader(src)), 64)
	if string(head) != src[:64] || isEncryptedContentRefusal(head) {
		t.Errorf("firstSSEEvent read %d bytes %q, want the first 64 and no refusal", len(head), head)
	}
}

const codexRefusalEvent = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_encrypted_content\",\"message\":\"The encrypted content for item rs_1 could not be verified.\"}}\n\n"

// The marker alone does not earn the retry: with no encrypted reasoning to drop the same body would
// go out again, so the stream is relayed unchanged after one call.
func TestCodexSSERefusalWithNothingToStripRelayedOnce(t *testing.T) {
	rec, bodies := codexSSEFor(t, `{"model":"codex/gpt-5.5","stream":true,"store":false,"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`, func(w http.ResponseWriter, n int, body string) {
		io.WriteString(w, codexRefusalEvent)
	})
	if rec.Code != http.StatusOK || len(bodies) != 1 || rec.Body.String() != codexRefusalEvent {
		t.Fatalf("code=%d upstream calls=%d body=%q, want the upstream stream unchanged after one call", rec.Code, len(bodies), rec.Body.String())
	}
}

// Only a streamed turn is read ahead. A caller that asked for no stream gets what the upstream sent,
// after one call.
func TestCodexSSENonStreamCallerNotProbed(t *testing.T) {
	rec, bodies := codexSSEFor(t, `{"model":"codex/gpt-5.5","stream":false,"store":false,"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAA-foreign"}]}`, func(w http.ResponseWriter, n int, body string) {
		io.WriteString(w, codexRefusalEvent)
	})
	if len(bodies) != 1 || rec.Body.String() != codexRefusalEvent {
		t.Fatalf("code=%d upstream calls=%d body=%q, want the upstream answer unchanged after one call", rec.Code, len(bodies), rec.Body.String())
	}
}

// The read ahead stops at 64 KiB. A first event longer than that, with the marker past the cap, is
// not a refusal intact can see, and every byte, those read ahead included, reaches the caller.
func TestCodexSSEFirstEventOverCapNotRetried(t *testing.T) {
	stream := "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"" + strings.Repeat("x", 70<<10) +
		"\",\"code\":\"invalid_encrypted_content\"}}\n\n"
	rec, bodies := codexSSE(t, func(w http.ResponseWriter, n int, body string) {
		io.WriteString(w, stream)
	})
	if rec.Code != http.StatusOK || len(bodies) != 1 || rec.Body.String() != stream {
		t.Fatalf("code=%d upstream calls=%d body length=%d (want %d), want the upstream stream unchanged after one call", rec.Code, len(bodies), rec.Body.Len(), len(stream))
	}
}

// An empty Anthropic-Beta line joins as an empty entry; it must not reach Anthropic as a leading,
// trailing or doubled comma.
func TestWithBetasSkipsEmptyEntries(t *testing.T) {
	need := []string{"oauth-2025-04-20"}
	cases := map[string]string{
		"":                   "oauth-2025-04-20",
		",":                  "oauth-2025-04-20",
		",a":                 "a,oauth-2025-04-20",
		"a,":                 "a,oauth-2025-04-20",
		"a,,b":               "a,b,oauth-2025-04-20",
		" , a , b":           "a,b,oauth-2025-04-20",
		"a,oauth-2025-04-20": "a,oauth-2025-04-20",
	}
	for in, want := range cases {
		if got := withBetas(in, need); got != want {
			t.Errorf("withBetas(%q) = %q, want %q", in, got, want)
		}
	}
}

// A chat upstream's context overflow reaches a Messages caller as "prompt is too long" and a
// Responses caller as context_length_exceeded, so each client compacts; a chat caller gets the body.
func TestChatUpstreamContextOverflowPerClient(t *testing.T) {
	const overflow = `{"error":{"message":"This model's maximum context length is 131,072 tokens. However, you requested 140,000 tokens.","type":"invalid_request_error","code":null}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, overflow)
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "a", "ka")
	h := New(s, map[string]string{"groq": up.URL})
	send := func(path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, loopbackRequest("POST", path, strings.NewReader(body)))
		return rec
	}
	rec := send("/v1/messages", `{"model":"groq/llama","max_tokens":10,"messages":[{"role":"user","content":"go"}]}`)
	var m struct {
		Error struct{ Type, Message string }
	}
	json.Unmarshal(rec.Body.Bytes(), &m)
	if rec.Code != http.StatusBadRequest || m.Error.Type != "invalid_request_error" ||
		!strings.HasPrefix(m.Error.Message, "prompt is too long: 140000 tokens > 131072 maximum") {
		t.Errorf("messages caller: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = send("/v1/responses", `{"model":"groq/llama","input":"go"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"context_length_exceeded"`) {
		t.Errorf("responses caller: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = send("/v1/chat/completions", `{"model":"groq/llama","messages":[{"role":"user","content":"go"}]}`)
	if rec.Code != http.StatusBadRequest || rec.Body.String() != overflow {
		t.Errorf("chat caller: code=%d body=%s, want the upstream body", rec.Code, rec.Body.String())
	}
}

// Only a provider whose def opts in (zencore, which serves DeepSeek and Kimi in thinking mode) gets the
// caller's thinking back as reasoning_content on the tool-call turn; Copilot and plain OpenAI-compatible
// upstreams get the request without the field.
func TestReasoningReplayFollowsProviderDef(t *testing.T) {
	const turn = `{"model":"%s","max_tokens":100,"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"I should read it.","signature":"s"},{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a"}]}],
		"tools":[{"name":"Read","input_schema":{"type":"object"}}]}`
	cases := []struct {
		provider, model string
		def             *provider.Def
		want            bool
	}{
		{"zencore", "zencore/deepseek-v4", &provider.Def{ID: "zencore", Kind: provider.KindAPIKey, API: "openai", ReplayReasoning: true}, true},
		{"infron", "infron/gpt-5", &provider.Def{ID: "infron", Kind: provider.KindAPIKey, API: "openai"}, false},
		{"groq", "groq/llama", nil, false},
	}
	for _, c := range cases {
		t.Run(c.provider, func(t *testing.T) {
			var got string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				if strings.HasSuffix(r.URL.Path, "/chat/completions") {
					got = string(b)
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"1","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
			}))
			defer up.Close()
			s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
			defer s.Close()
			if c.def != nil {
				d := *c.def
				d.BaseURL = up.URL
				if err := s.PutProviderDef(d); err != nil {
					t.Fatal(err)
				}
				defer provider.SetDeclared(nil)
			}
			s.CreateConnection(c.provider, "a", "k")
			h := New(s, map[string]string{c.provider: up.URL})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, loopbackRequest("POST", "/v1/messages", strings.NewReader(fmt.Sprintf(turn, c.model))))
			if rec.Code != http.StatusOK || got == "" {
				t.Fatalf("code=%d body=%s upstream=%s", rec.Code, rec.Body.String(), got)
			}
			if has := strings.Contains(got, `"reasoning_content":"I should read it."`); has != c.want {
				t.Errorf("reasoning_content sent = %v, want %v: %s", has, c.want, got)
			}
			got = ""
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, loopbackRequest("POST", "/v1/responses", strings.NewReader(`{"model":"`+c.model+`","input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]},
				{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"I should read it."}]},
				{"type":"function_call","call_id":"c1","name":"Read","arguments":"{}"},
				{"type":"function_call_output","call_id":"c1","output":"a"}],
				"tools":[{"type":"function","name":"Read","parameters":{"type":"object"}}]}`)))
			if rec.Code != http.StatusOK || got == "" {
				t.Fatalf("responses: code=%d body=%s upstream=%s", rec.Code, rec.Body.String(), got)
			}
			if has := strings.Contains(got, `"reasoning_content":"I should read it."`); has != c.want {
				t.Errorf("responses: reasoning_content sent = %v, want %v: %s", has, c.want, got)
			}
		})
	}
}

// A provider that cannot be looked up cannot say whether it wants reasoning replayed; that is logged
// once, with its id, rather than replay going off in silence.
func TestReasoningReplayLookupMissIsLogged(t *testing.T) {
	var logs strings.Builder
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	replayMissLogged.Delete("gone-provider") // a rerun (-count=2) starts unlogged
	body := []byte(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"go"}]}`)
	for range 2 {
		if _, _, err := toProvider(body, translate.Anthropic, translate.OpenAI, "gone-provider"); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), "gone-provider"); n != 1 {
		t.Errorf("want one log line naming gone-provider, got %d:\n%s", n, logs.String())
	}
	logs.Reset()
	if _, _, err := toProvider(body, translate.Anthropic, translate.OpenAI, "groq"); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Errorf("a known provider logged:\n%s", logs.String())
	}
}

// A /v1 body is read whole before it is routed, so it is capped at 64 MiB: one byte over is a 413 with a
// JSON error and never reaches an upstream; a large body under the cap goes through.
func TestV1BodyCap(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Write([]byte(`{"data":[]}`))
			return
		}
		io.Copy(io.Discard, r.Body)
		calls++
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.CreateConnection("groq", "a", "ka"); err != nil {
		t.Fatal(err)
	}
	h := New(s, map[string]string{"groq": up.URL})
	pad := func(n int) string {
		head, tail := `{"model":"groq/llama","stream":false,"pad":"`, `"}`
		return head + strings.Repeat("x", n-len(head)-len(tail)) + tail
	}
	if rec := postV1(h, pad(5<<20)); rec.Code != http.StatusOK || calls != 1 {
		t.Fatalf("a 5 MiB body: code=%d upstream calls=%d body=%.200s, want 200 after one call", rec.Code, calls, rec.Body.String())
	}
	if rec := postV1(h, pad(64<<20)); rec.Code != http.StatusOK || calls != 2 {
		t.Fatalf("a body of exactly 64 MiB: code=%d upstream calls=%d body=%.200s, want 200", rec.Code, calls, rec.Body.String())
	}
	rec := postV1(h, pad(64<<20+1))
	var e struct {
		Error struct{ Message string } `json:"error"`
	}
	if rec.Code != http.StatusRequestEntityTooLarge || json.Unmarshal(rec.Body.Bytes(), &e) != nil ||
		!strings.Contains(e.Error.Message, "64 MiB") || calls != 2 {
		t.Errorf("a body over 64 MiB: code=%d upstream calls=%d body=%.200s, want 413 with a JSON error naming the cap", rec.Code, calls, rec.Body.String())
	}
}
