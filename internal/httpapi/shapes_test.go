package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

func TestOpenAICallerReachesAnthropicProvider(t *testing.T) {
	var gotPath, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.URL.Path, string(b)
		if strings.Contains(gotBody, `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-x\",\"usage\":{\"input_tokens\":5}}}\n\n"+
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n"+
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"+
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-x","content":[{"type":"text","text":"Hi"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("claude", "c", "tok")
	h := New(s, map[string]string{"claude": up.URL})

	rec := postV1(h, `{"model":"claude/claude-x","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/messages" || !strings.Contains(gotBody, `"system":[{"text":"sys","type":"text"}]`) || !strings.Contains(gotBody, `"max_tokens":8192`) {
		t.Errorf("upstream got %s %s", gotPath, gotBody)
	}
	if b := rec.Body.String(); !strings.Contains(b, `"object":"chat.completion"`) || !strings.Contains(b, `"content":"Hi"`) {
		t.Errorf("caller got %s", b)
	}

	rec = postV1(h, `{"model":"claude/claude-x","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	if b := rec.Body.String(); !strings.Contains(b, `"content":"Hi"`) || !strings.Contains(b, "data: [DONE]") {
		t.Errorf("stream caller got %s", b)
	}
	// Usage is counted from the provider's own answer, for both calls.
	rows, _ := s.Usage()
	if len(rows) != 1 || rows[0].ConnectionID != c.ID || rows[0].Requests != 2 || rows[0].InputTokens != 10 {
		t.Errorf("usage rows = %+v", rows)
	}
}

func TestAnthropicCallerReachesOpenAIProvider(t *testing.T) {
	var gotPath, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.URL.Path, string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"id":"chatcmpl-1","model":"llama","choices":[{"index":0,"delta":{"content":"Yo"}}]}`+"\n\n"+
			`data: {"id":"chatcmpl-1","model":"llama","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"+
			`data: {"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1}}`+"\n\n"+"data: [DONE]\n\n")
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "g", "k")
	h := New(s, map[string]string{"groq": up.URL})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"groq/llama","max_tokens":10,"stream":true,"system":"sys","messages":[{"role":"user","content":"hi"}]}`)))
	if gotPath != "/chat/completions" || !strings.Contains(gotBody, `"include_usage":true`) || !strings.Contains(gotBody, `"role":"system"`) {
		t.Errorf("upstream got %s %s", gotPath, gotBody)
	}
	b := rec.Body.String()
	for _, want := range []string{"event: message_start", `"text":"Yo"`, `"stop_reason":"end_turn"`, "event: message_stop"} {
		if !strings.Contains(b, want) {
			t.Errorf("caller stream missing %s:\n%s", want, b)
		}
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}
}

func TestTranslatedErrorKeepsStatusAndShape(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"bad model","type":"invalid_request_error"}}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "g", "k")
	h := New(s, map[string]string{"groq": up.URL})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"groq/x","max_tokens":5,"messages":[]}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"type":"error"`) || !strings.Contains(rec.Body.String(), "bad model") {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}
