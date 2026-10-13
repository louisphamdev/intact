package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// A summary request moves to the next account when one account is out of
// credit (402), as a normal request does.
func TestCompactionSummaryFailsOverOnA402(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Write([]byte(`{"data":[]}`))
			return
		}
		calls++
		if r.Header.Get("Authorization") == "Bearer ka" {
			w.WriteHeader(http.StatusPaymentRequired)
			w.Write([]byte(`{"error":{"message":"out of credit"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"the summary"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "a", "ka")
	s.CreateConnection("groq", "b", "kb")
	h := New(s, map[string]string{"groq": up.URL})
	w := call(h, "POST", "/v1/responses", "review-client", "", `{"model":"groq/llama","store":false,"stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"compaction_trigger"}]}`)
	if w.Code != http.StatusOK || calls != 2 || !strings.Contains(w.Body.String(), `"type":"compaction"`) {
		t.Fatalf("code=%d calls=%d body=%s, want the second account's summary", w.Code, calls, w.Body.String())
	}
}

// A chat upstream cut mid-stream reaches a Responses caller as response.failed,
// never as response.completed.
func TestCutChatStreamFailsTheResponsesCaller(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Write([]byte(`{"data":[]}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "a", "ka")
	h := New(s, map[string]string{"groq": up.URL})
	w := call(h, "POST", "/v1/responses", "review-client", "", `{"model":"groq/llama","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	b := w.Body.String()
	if strings.Contains(b, "response.completed") || !strings.Contains(b, "response.failed") {
		t.Fatalf("a cut chat stream reached the Responses caller as:\n%s", b)
	}
}
