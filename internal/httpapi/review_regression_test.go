package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/translate"
)

func TestReviewExpiredKeyNeverCallsUpstreamWithForgedUA(t *testing.T) {
	called := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"type":"message","id":"msg_mock","role":"assistant","content":[{"type":"text","text":"mock only"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()

	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.CreateConnection("claude", "mock", "mock-credential"); err != nil {
		t.Fatal(err)
	}
	key, err := s.CreateAPIKey("expired review key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetAPIKeyLimits(key.ID, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), 0); err != nil {
		t.Fatal(err)
	}

	h := NewWithAuth(s, map[string]string{"claude": up.URL}, authConfig())
	w := call(h, "POST", "/v1/messages", "claude-cli/2.1.293", key.Key, `{"model":"claude/mock","max_tokens":1,"messages":[{"role":"user","content":"mock"}]}`)

	if w.Code != http.StatusUnauthorized || called != 0 {
		t.Fatalf("expected 401 with 0 upstream calls, got code=%d calls=%d body=%s", w.Code, called, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "expired") {
		t.Fatalf("expected expired error message, got %s", w.Body.String())
	}
}

func TestReviewExtremeKeepRecentNoPanic(t *testing.T) {
	p := compactPolicy{}.withDefaults()
	p.KeepRecent = int(^uint(0) >> 1) // MaxInt
	body, _ := json.Marshal(map[string]any{
		"messages": []any{
			map[string]string{"role": "user", "content": strings.Repeat("x", 30000)},
		},
	})
	panicked := false
	func() {
		defer func() {
			if v := recover(); v != nil {
				panicked = true
				t.Fatalf("keepRecent MaxInt panicked: %v", v)
			}
		}()
		compactConversation(body, p, "mock429")
	}()
	if panicked {
		t.Fatal("expected no panic with extreme keepRecent")
	}
}

func TestReviewFailedStreamRetryReportsCause(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":{"message":"stream required"}}`))
			return
		}
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		c.Close()
	}))
	defer up.Close()

	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.CreateConnection("openrouter", "mock", "mock-secret"); err != nil {
		t.Fatal(err)
	}
	h := New(s, map[string]string{"openrouter": up.URL})
	w := call(h, "POST", "/v1/responses", "review-client", "", `{"model":"openrouter/mock","store":false,"stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"mock only"}]},{"type":"compaction_trigger"}]}`)

	if calls != 2 || w.Code != http.StatusBadGateway {
		t.Fatalf("expected 2 calls and 502, got calls=%d code=%d body=%s", calls, w.Code, w.Body.String())
	}
	// Error body must report the actual connection error, not generic "cannot read the summary"
	if strings.Contains(w.Body.String(), "cannot read the summary") {
		t.Fatalf("expected real cause reported, but got closed body diagnostic: %s", w.Body.String())
	}
}

func TestReviewResponsesCompactionKeepsProtocol(t *testing.T) {
	input := []map[string]any{
		{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "opening"}}},
	}
	for i := 0; i < 10; i++ {
		input = append(input, map[string]any{
			"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": strings.Repeat("x", 4000)}},
		})
	}
	body, _ := json.Marshal(map[string]any{"input": input})
	out, _, ok := compactConversation(body, compactPolicy{}.withDefaults(), "mock 429")
	if !ok {
		t.Fatal("no compaction")
	}
	var got struct {
		Input []map[string]json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	legacy := 0
	for _, v := range got.Input {
		var s string
		if json.Unmarshal(v["content"], &s) == nil {
			legacy++
		}
	}
	if legacy != 0 {
		t.Fatalf("Responses input message objects rebuilt with bare string content: %d", legacy)
	}
}

func TestReviewGeminiCompactionKeepsProtocol(t *testing.T) {
	contents := []map[string]any{
		{"role": "user", "parts": []any{map[string]any{"text": "opening"}}},
	}
	for i := 0; i < 10; i++ {
		contents = append(contents, map[string]any{
			"role": "model", "parts": []any{map[string]any{"text": strings.Repeat("x", 4000)}},
		})
	}
	body, _ := json.Marshal(map[string]any{"request": map[string]any{"contents": contents}})
	out, _, ok := compactConversation(body, compactPolicy{}.withDefaults(), "mock 429")
	if !ok {
		t.Fatal("no compaction")
	}
	var got struct {
		Request struct {
			Contents []map[string]any `json:"contents"`
		} `json:"request"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	invalid := 0
	for _, v := range got.Request.Contents {
		if _, ok := v["parts"]; !ok {
			invalid++
		}
	}
	if invalid != 0 {
		t.Fatalf("Gemini contents items missing required parts: %d", invalid)
	}
}

func TestReviewDeclaredToolNamePreserved(t *testing.T) {
	names := translate.NewToolNames([]string{"ReadAndWrite", "Read"}, "openrouter")
	got := names.ToClient("ReadAndWrite")
	if got != "ReadAndWrite" {
		t.Fatalf("expected ReadAndWrite, got %q", got)
	}
}
