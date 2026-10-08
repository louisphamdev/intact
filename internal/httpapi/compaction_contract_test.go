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
	"github.com/louisphamdev/intact/internal/translate"
)

func TestCompactionCapsuleSurvivesTwoCompactionsAndNativeReplay(t *testing.T) {
	item := compactionItem("cmp_one", "KEEP_DECISION")
	body, _ := json.Marshal(map[string]any{"model": "m", "input": []any{item, map[string]any{"type": "compaction_trigger"}}})
	summaryBody := summaryRequest(body)
	if !strings.Contains(string(summaryBody), "KEEP_DECISION") {
		t.Fatalf("second compaction lost summary: %s", summaryBody)
	}
	chat, ok := responsesToChatInput(summaryBody)
	if !ok || !strings.Contains(string(chat), "KEEP_DECISION") {
		t.Fatalf("chat summary lost history: %s", chat)
	}
	replay, _ := json.Marshal(map[string]any{"input": []any{item, map[string]any{"role": "user", "content": "continue"}}})
	native, err := translate.ExpandGatewayCompactions(replay, true)
	if err != nil || !strings.Contains(string(native), "KEEP_DECISION") || strings.Contains(string(native), "encrypted_content") {
		t.Fatalf("native replay: %s (%v)", native, err)
	}
	opaque := []byte(`{"input":[{"type":"compaction","encrypted_content":"native-ciphertext"}]}`)
	unchanged, err := translate.ExpandGatewayCompactions(opaque, true)
	if err != nil || string(unchanged) != string(opaque) {
		t.Fatal("native ciphertext changed")
	}
}

func TestCompactionRequiresSuccessfulTerminalOutput(t *testing.T) {
	for _, body := range []string{
		`{"status":"incomplete","output":[{"type":"message","text":"partial"}]}`,
		`{"choices":[{"message":{"content":"partial"},"finish_reason":"length"}]}`,
		`{"status":"completed","output":[{"type":"message","text":"summary"},{"type":"function_call","call_id":"c"}]}`,
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\ndata: [DONE]\n\n",
	} {
		if completeSummaryResponse([]byte(body)) {
			t.Errorf("accepted partial/mixed summary: %s", body)
		}
	}
	stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"the summary\"}\r\n\r\n" +
		"data: {\"type\":\"response.completed\",\r\ndata: \"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"text\":\"the summary\"}]}}\r\n\r\n"
	if !completeSummaryResponse([]byte(stream)) {
		t.Fatal("complete multiline summary rejected")
	}
	if got := compactionSummaryFromStream([]byte(stream)); got != "the summary" {
		t.Fatalf("summary duplicated: %q", got)
	}
}

func TestSummaryChatConversionRetainsToolResultsAsResults(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"KEEP_RESULT"},{"type":"compaction_trigger"}]}`)
	chat, ok := responsesToChatInput(summaryRequest(body))
	if !ok || !strings.Contains(string(chat), `"role":"tool"`) || !strings.Contains(string(chat), "KEEP_RESULT") {
		t.Fatalf("tool result lost or became a call: %s", chat)
	}
}

func TestCompactionHTTPHandlerReplaysAndRejectsPartialSummary(t *testing.T) {
	var received []string
	reason := "end_turn"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received = append(received, string(body))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"type": "message", "content": []any{map[string]string{"type": "text", "text": "KEEP_HANDLER_DECISION"}}, "stop_reason": reason})
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
	key, err := s.CreateAPIKey("contract fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := NewWithAuth(s, map[string]string{"claude": up.URL}, authConfig())
	first := call(h, "POST", "/v1/responses", "codex_exec/0.160", key.Key, `{"model":"claude/mock","input":[{"role":"user","content":"original"},{"type":"compaction_trigger"}]}`)
	if first.Code != 200 {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}
	var response struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil || len(response.Output) != 1 {
		t.Fatal("no compaction item")
	}
	body, _ := json.Marshal(map[string]any{"model": "claude/mock", "input": []any{response.Output[0], map[string]any{"type": "compaction_trigger"}}})
	second := call(h, "POST", "/v1/responses", "codex_exec/0.160", key.Key, string(body))
	if second.Code != 200 || !strings.Contains(received[len(received)-1], "KEEP_HANDLER_DECISION") {
		t.Fatalf("second: %d %s upstream=%v", second.Code, second.Body, received)
	}
	reason = "max_tokens"
	partial := call(h, "POST", "/v1/responses", "codex_exec/0.160", key.Key, string(body))
	if partial.Code != 502 || strings.Contains(partial.Body.String(), "encrypted_content") {
		t.Fatalf("partial accepted: %d %s", partial.Code, partial.Body)
	}
}
