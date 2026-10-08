package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/translate"
)

// Codex accepts a compaction answer only when it carries exactly one output item of type
// "compaction". These tests hold that line: anything else is the fatal error that leaves a
// thread unable to make progress again.

func triggerBody(stream bool) []byte {
	return []byte(`{"model":"gpt-5.6-sol","stream":` + map[bool]string{true: "true", false: "false"}[stream] +
		`,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"start"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]},` +
		`{"type":"compaction_trigger"}]}`)
}

func TestCompactionTriggerIsTheLastInputItem(t *testing.T) {
	if !compactionTrigger(triggerBody(false)) {
		t.Fatal("a trigger as the last item was not recognised")
	}
	if compactionTrigger([]byte(`{"input":[{"type":"compaction_trigger"},{"type":"message","role":"user","content":[]}]}`)) {
		t.Fatal("a trigger that is not last was treated as a request to compact")
	}
	if compactionTrigger([]byte(`{"messages":[{"role":"user","content":"start"}]}`)) {
		t.Fatal("a chat conversation was read as a compaction request")
	}
	if compactionTrigger([]byte(`{`)) {
		t.Fatal("broken json was read as a compaction request")
	}
}

// The provider must never see the trigger: it is an item type no provider knows, and the whole
// point is that the summary is asked as an ordinary question.
func TestSummaryRequestDropsTheTriggerAndAsksForASummary(t *testing.T) {
	out := summaryRequest(triggerBody(false))
	if out == nil {
		t.Fatal("no summary request")
	}
	if strings.Contains(string(out), "compaction_trigger") {
		t.Fatalf("the trigger went to the provider: %s", out)
	}
	items, ok := responsesInput(out)
	if !ok {
		t.Fatal("the summary request carries no input array")
	}
	var last struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	json.Unmarshal(items[len(items)-1], &last)
	if last.Role != "user" {
		t.Fatalf("the last item is not the instruction: %s", items[len(items)-1])
	}
	if !strings.Contains(string(items[len(items)-1]), compactionInstruction[:40]) {
		t.Fatal("the instruction is not in the last item")
	}
	// The history itself is still there, because the summary is of it.
	if len(items) != 3 {
		t.Fatalf("the summary request carries %d items, want the two history items and the instruction", len(items))
	}
}

// A summary request rebuilt from the few fields this needs fails on the accounts that work for
// everything else: OpenAI's Responses refuses a request that does not set store to false, and
// the reasoning settings decide what the summary comes back with. So the client's own body is
// carried over and only the input changes.
func TestSummaryRequestCarriesTheClientsOwnSettings(t *testing.T) {
	body := []byte(`{"model":"m","store":false,"include":["reasoning.encrypted_content"],` +
		`"reasoning":{"effort":"medium","summary":"auto"},"max_output_tokens":8192,"stream":true,` +
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"start"}]},` +
		`{"type":"compaction_trigger"}]}`)
	out := summaryRequest(body)
	if out == nil {
		t.Fatal("no summary request")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["store"] != false {
		t.Fatalf("store was lost, and the provider refuses a request without it: %s", out)
	}
	if _, ok := got["reasoning"]; !ok {
		t.Fatalf("the reasoning settings were lost: %s", out)
	}
	if _, ok := got["include"]; !ok {
		t.Fatalf("include was lost: %s", out)
	}
	if got["max_output_tokens"] == nil {
		t.Fatalf("max_output_tokens was lost: %s", out)
	}
	// stream passes through rather than being turned off. Codex's account refuses a
	// Responses request without it, and the answer is read either way, so overriding it
	// here would break exactly the provider this exists for.
	if got["stream"] != true {
		t.Fatalf("stream is %v, want the client's true: the account refuses a request without it", got["stream"])
	}
	if strings.Contains(string(out), "compaction_trigger") {
		t.Fatalf("the trigger survived: %s", out)
	}
}

// The check Codex makes is on the count and the type, so these count the items rather than
// looking for one.
func TestCompactionAnswerCarriesExactlyOneCompactionItem(t *testing.T) {
	for _, stream := range []bool{false, true} {
		var raw []byte
		itemID, respID := compactionIDs()
		if stream {
			raw = compactionStream(itemID, respID, "the summary", "gpt-5.6-sol")
		} else {
			raw = compactionBody(itemID, respID, "the summary", "gpt-5.6-sol")
		}
		items := countCompactionItems(t, raw, stream)
		if items != 1 {
			t.Fatalf("stream=%v: %d compaction items in the answer, codex wants exactly 1:\n%s", stream, items, raw)
		}
		if !strings.Contains(string(raw), "encrypted_content") {
			t.Fatalf("stream=%v: the item has no encrypted_content:\n%s", stream, raw)
		}
		capsule := translate.EncodeCompaction("the summary")
		if !strings.Contains(string(raw), capsule) {
			t.Fatalf("stream=%v: summary capsule missing: %s", stream, raw)
		}
		if decoded, err := translate.DecodeCompaction(capsule); err != nil || decoded != "the summary" {
			t.Fatalf("summary cannot replay: %q %v", decoded, err)
		}
	}
}

// countCompactionItems reads an answer the way codex does: every output item named anywhere in
// it, counted, because the fatal error is exactly "got N from M output items".
func countCompactionItems(t *testing.T, raw []byte, stream bool) int {
	t.Helper()
	count := 0
	if stream {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(line[len("data:"):])
			if data == "[DONE]" {
				continue
			}
			count += countItemsIn(t, data)
		}
		return count
	}
	return countItemsIn(t, string(raw))
}

func countItemsIn(t *testing.T, jsonText string) int {
	t.Helper()
	var ev struct {
		Output []map[string]any `json:"output"`
		Item   map[string]any   `json:"item"`
	}
	if json.Unmarshal([]byte(jsonText), &ev) != nil {
		return 0
	}
	n := 0
	for _, set := range [][]map[string]any{ev.Output, {ev.Item}} {
		for _, item := range set {
			if item != nil && item["type"] == "compaction" {
				n++
			}
		}
	}
	return n
}

func TestCompactionSummaryIsReadFromEveryProviderShape(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"responses", `{"output":[{"type":"message","content":[{"type":"output_text","text":"the summary"}]}]}`, "the summary"},
		{"responses flat text", `{"output":[{"type":"message","text":"the summary"}]}`, "the summary"},
		{"chat", `{"choices":[{"message":{"content":"the summary"}}]}`, "the summary"},
		{"gemini", `{"candidates":[{"content":{"parts":[{"text":"the summary"}]}}]}`, "the summary"},
	}
	for _, c := range cases {
		if got := compactionSummary([]byte(c.body)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// A provider that answers the summary request with a tool call has no summary. Storing an empty
// one would replace the whole history with nothing.
func TestCompactionSummaryRefusesAToolCall(t *testing.T) {
	if got := compactionSummary([]byte(`{"output":[{"type":"function_call","call_id":"c1","name":"shell"}]}`)); got != "" {
		t.Fatalf("a tool call was read as a summary: %q", got)
	}
	if got := compactionSummary([]byte(`not json`)); got != "" {
		t.Fatalf("broken json produced a summary: %q", got)
	}
}

func TestCompactionSummaryIsReadFromAStream(t *testing.T) {
	s := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"the \"}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"summary\"}\n\n" +
		"data: [DONE]\n\n"
	if got := compactionSummaryFromStream([]byte(s)); got != "the summary" {
		t.Fatalf("got %q", got)
	}
	chat := "data: {\"choices\":[{\"delta\":{\"content\":\"the \"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"summary\"}}]}\n\n"
	if got := compactionSummaryFromStream([]byte(chat)); got != "the summary" {
		t.Fatalf("chat stream: got %q", got)
	}
}

func TestSummaryCapCutsAtABoundary(t *testing.T) {
	long := strings.Repeat("a sentence about the work. ", 4000)
	got := capSummary(long)
	if len(got) > compactionSummaryCap {
		t.Fatalf("the summary was not capped: %d bytes", len(got))
	}
	if !strings.HasSuffix(got, ".") {
		t.Fatalf("the summary was cut mid-sentence: ...%s", got[len(got)-40:])
	}
	if capSummary("short") != "short" {
		t.Fatal("a short summary was changed")
	}
}

// The end to end shape: a provider that answers as an ordinary completion, and a caller that
// gets back the one item codex accepts.
func TestCompactionThroughTheProvider(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			w.Write([]byte(`{"choices":[{"message":{"content":"a summary of the work"}}]}`))
			return
		}
		w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"a summary of the work"}]}]}`))
	}))
	t.Cleanup(up.Close)

	body := summaryRequest(triggerBody(false))
	if body == nil {
		t.Fatal("no summary request")
	}
	resp, err := http.Post(up.URL+"/v1/responses", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var whole struct {
		Output []map[string]any `json:"output"`
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = whole
	summary := compactionSummary(raw)
	if summary != "a summary of the work" {
		t.Fatalf("summary %q", summary)
	}
	itemID, respID := compactionIDs()
	answer := compactionBody(itemID, respID, summary, "gpt-5.6-sol")
	if n := countItemsIn(t, string(answer)); n != 1 {
		t.Fatalf("%d compaction items", n)
	}
}

func TestResponsesToChatInputAsksAChatProviderInChat(t *testing.T) {
	out, ok := responsesToChatInput(summaryRequest(triggerBody(false)))
	if !ok {
		t.Fatal("a responses body was not turned into chat messages")
	}
	if strings.Contains(string(out), "compaction_trigger") {
		t.Fatalf("the trigger survived into the chat request: %s", out)
	}
	var chat struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 3 {
		t.Fatalf("%d messages, want the two history items and the instruction", len(chat.Messages))
	}
	if chat.Messages[2].Role != "user" {
		t.Fatalf("the instruction is not a user message: %+v", chat.Messages[2])
	}
}
