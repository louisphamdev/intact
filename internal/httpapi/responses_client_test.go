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

// A Responses caller (Codex) reaching a chat provider gets its request
// translated to chat and its answer back as Responses, streamed or whole.
func TestResponsesCallerReachesAChatProvider(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Write([]byte(`{"data":[{"id":"llama"}]}`))
			return
		}
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		if gotBody["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, `data: {"id":"chatcmpl-1","model":"llama","choices":[{"index":0,"delta":{"content":"hi"}}]}`+"\n\n")
			io.WriteString(w, `data: {"id":"chatcmpl-1","model":"llama","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"mcp__db__query","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`+"\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-2","object":"chat.completion","model":"llama","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "g", "kg")
	h := New(s, map[string]string{"groq": up.URL})

	req := `{"model":"groq/llama","stream":true,"instructions":"be brief","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
	 "tools":[{"type":"namespace","name":"mcp__db","tools":[{"type":"function","name":"query","parameters":{"type":"object"}}]},{"type":"web_search"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/v1/responses", strings.NewReader(req)))
	if rec.Code != http.StatusOK {
		t.Fatalf("stream: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/chat/completions" || gotBody["messages"] == nil || gotBody["input"] != nil {
		t.Errorf("upstream got path=%s body=%v, want a chat request", gotPath, gotBody)
	}
	out := rec.Body.String()
	for _, want := range []string{"event: response.created", `"delta":"hi"`, `"name":"query","namespace":"mcp__db"`, "event: response.completed", `"total_tokens":4`} {
		if !strings.Contains(out, want) {
			t.Errorf("stream answer lacks %s:\n%s", want, out)
		}
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/v1/responses", strings.NewReader(`{"model":"groq/llama","input":"hi"}`)))
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusOK || resp["object"] != "response" || resp["output_text"] != "hello" {
		t.Errorf("whole: code=%d body=%s", rec.Code, rec.Body.String())
	}
}
