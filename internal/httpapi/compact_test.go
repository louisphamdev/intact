package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/translate"
)

func compactAPI(t *testing.T) *api {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	a, _ := newServer(s, nil, nil)
	return a
}

// longMessages builds a conversation with the shape of an agent session: an opening
// request, some turns of tool calls with bulky results, and a recent window whose
// tool calls must survive untouched.
func longMessages(turns int, resultLen int) []map[string]any {
	msgs := []map[string]any{
		{"role": "user", "content": "fix the failing test in update.test.mjs"},
		{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "reading the file"},
			map[string]any{"type": "tool_use", "id": "t0", "name": "Read", "input": map[string]any{"path": "update.test.mjs"}},
		}},
		{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t0", "content": strings.Repeat("x", resultLen)},
		}},
	}
	for i := 1; i < turns; i++ {
		id := fmt.Sprintf("t%d", i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": fmt.Sprintf("step %d", i)},
				map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": "ls"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": id,
					"content": fmt.Sprintf("turn%d:", i) + strings.Repeat("y", resultLen)},
			}},
		)
	}
	return msgs
}

func bodyWith(t *testing.T, msgs []map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"model": "m", "max_tokens": 4096, "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decodeMessages(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var out struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("compacted body is not json: %v", err)
	}
	return out.Messages
}

func TestCompactKeepsTheOpeningRequestAndTheRecentTurnsVerbatim(t *testing.T) {
	p := compactPolicy{}.withDefaults()
	in := bodyWith(t, longMessages(8, 4000))
	out, st, ok := compactConversation(in, p, "openai answered 429 for it")
	if !ok {
		t.Fatal("a long agent conversation was not compacted")
	}
	if st.saved() <= 0 || len(out) >= len(in) {
		t.Fatalf("saved %d bytes, %d -> %d", st.saved(), st.before, st.after)
	}
	msgs := decodeMessages(t, out)
	if msgs[0]["content"] != "fix the failing test in update.test.mjs" {
		t.Fatalf("the opening request changed: %v", msgs[0]["content"])
	}
	// The tail is the client's own bytes, so the newest tool call and its result are
	// both still there and still paired.
	last := msgs[len(msgs)-2]
	blocks, _ := last["content"].([]any)
	found := false
	for _, b := range blocks {
		if m, _ := b.(map[string]any); m != nil && m["type"] == "tool_use" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the newest tool call is gone: %s", out)
	}
}

func TestCompactLeavesNoToolResultWithoutItsCall(t *testing.T) {
	p := compactPolicy{}.withDefaults()
	out, _, ok := compactConversation(bodyWith(t, longMessages(9, 3000)), p, "openai answered 429 for it")
	if !ok {
		t.Fatal("not compacted")
	}
	calls := map[string]bool{}
	for _, m := range decodeMessages(t, out) {
		blocks, _ := m["content"].([]any)
		for _, b := range blocks {
			bm, _ := b.(map[string]any)
			if bm == nil {
				continue
			}
			switch bm["type"] {
			case "tool_use":
				calls[bm["id"].(string)] = true
			case "tool_result":
				// A provider refuses a result whose call is not in the request.
				if !calls[bm["tool_use_id"].(string)] {
					t.Fatalf("tool_result %v has no tool_use before it", bm["tool_use_id"])
				}
			}
		}
	}
}

func TestCompactDropsTheBulkyToolOutputFromTheMiddle(t *testing.T) {
	p := compactPolicy{}.withDefaults()
	out, st, ok := compactConversation(bodyWith(t, longMessages(9, 3000)), p, "openai answered 429 for it")
	if !ok {
		t.Fatal("not compacted")
	}
	if st.droppedToolOnly == 0 {
		t.Fatal("no tool result was dropped, so nothing was shed")
	}
	text := string(out)
	// The words of a dropped middle message survive; only the output goes.
	if !strings.Contains(text, "step 3") {
		t.Fatal("a middle assistant message lost its text")
	}
	// The tool result of an early turn is gone, and the one of a recent turn is not:
	// the tail is the client's own bytes, which is what the next turn reasons over.
	if strings.Contains(text, "turn1:") {
		t.Fatal("an early middle tool result came through whole")
	}
	if !strings.Contains(text, "turn7:") {
		t.Fatal("a recent tool result was cut, so the next turn lost what it reasons over")
	}
}

func TestCompactSaysWhatItCut(t *testing.T) {
	p := compactPolicy{}.withDefaults()
	out, _, ok := compactConversation(bodyWith(t, longMessages(8, 3000)), p, "openai answered 429 for it")
	if !ok {
		t.Fatal("not compacted")
	}
	text := string(out)
	if !strings.Contains(text, "[intact]") {
		t.Fatal("nothing tells the model the history was shortened")
	}
	// The reason names the provider, so a model reading it knows why.
	if !strings.Contains(text, "answered 429") {
		t.Fatalf("the marker does not say why: %s", text[:400])
	}
}

func TestCompactLeavesOtherFieldsAlone(t *testing.T) {
	p := compactPolicy{}.withDefaults()
	in := bodyWith(t, longMessages(8, 3000))
	var before map[string]json.RawMessage
	json.Unmarshal(in, &before)
	out, _, ok := compactConversation(in, p, "openai answered 429 for it")
	if !ok {
		t.Fatal("not compacted")
	}
	var after map[string]json.RawMessage
	json.Unmarshal(out, &after)
	for _, k := range []string{"model", "max_tokens"} {
		if string(before[k]) != string(after[k]) {
			t.Fatalf("%s changed: %s -> %s", k, before[k], after[k])
		}
	}
}

func TestCompactDoesNotEscapeTheConversationItKeeps(t *testing.T) {
	p := compactPolicy{}.withDefaults()
	msgs := longMessages(8, 3000)
	msgs[0]["content"] = "compare a < b && c > d in update.mjs"
	// Encoded the way a client sends it, with no HTML escaping.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]any{"model": "m", "max_tokens": 4096, "messages": msgs}); err != nil {
		t.Fatal(err)
	}
	out, _, ok := compactConversation(buf.Bytes(), p, "openai answered 429 for it")
	if !ok {
		t.Fatal("not compacted")
	}
	if !strings.Contains(string(out), "a < b && c > d") {
		t.Fatalf("html escaping rewrote the client's text: %s", out[:300])
	}
}

// ---- the two gates the feature is built on ----

func setRotation(t *testing.T, a *api, prov, mode string) {
	t.Helper()
	if err := a.store.SetSetting(rotationKey(prov), `{"mode":"`+mode+`"}`); err != nil {
		t.Fatal(err)
	}
}

func conns(ids ...string) []store.Connection {
	var out []store.Connection
	for _, id := range ids {
		out = append(out, store.Connection{ID: id, Provider: "openai"})
	}
	return out
}

func TestCompactForRateLimitNeedsRoundRobin(t *testing.T) {
	a := compactAPI(t)
	body := bodyWith(t, longMessages(9, 4000))
	setRotation(t, a, "openai", RotateFallback)
	if _, _, ok := a.compactForRateLimit("openai", conns("b"), body); ok {
		t.Fatal("a provider that pins one account was compacted, with nowhere to send it")
	}
	setRotation(t, a, "openai", RotateRoundRobin)
	if _, st, ok := a.compactForRateLimit("openai", conns("b"), body); !ok || st.saved() <= 0 {
		t.Fatal("round-robin with a ready account did not compact")
	}
}

func TestCompactForRateLimitNeedsAnAccountThatIsNotStandby(t *testing.T) {
	a := compactAPI(t)
	body := bodyWith(t, longMessages(9, 4000))
	setRotation(t, a, "openai", RotateRoundRobin)
	rest := conns("b")
	rest[0].Standby = true
	if _, _, ok := a.compactForRateLimit("openai", rest, body); ok {
		t.Fatal("compacted for a standby, which is the last resort not a ready account")
	}
	if _, _, ok := a.compactForRateLimit("openai", nil, body); ok {
		t.Fatal("compacted with no account left to receive the session")
	}
}

func TestCompactForRateLimitLeavesShortConversationsAlone(t *testing.T) {
	a := compactAPI(t)
	setRotation(t, a, "openai", RotateRoundRobin)
	short := bodyWith(t, longMessages(2, 10))
	if _, _, ok := a.compactForRateLimit("openai", conns("b"), short); ok {
		t.Fatal("a short conversation was compacted")
	}
}

func TestCompactForRateLimitCanBeSwitchedOff(t *testing.T) {
	a := compactAPI(t)
	setRotation(t, a, "openai", RotateRoundRobin)
	if err := a.store.SetSetting(rateLimitCompactKey, `{"enabled":false}`); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := a.compactForRateLimit("openai", conns("b"), bodyWith(t, longMessages(9, 4000))); ok {
		t.Fatal("the setting did not switch compaction off")
	}
}

func TestCompactForRateLimitKeepsDefaultsOnABrokenSetting(t *testing.T) {
	a := compactAPI(t)
	setRotation(t, a, "openai", RotateRoundRobin)
	if err := a.store.SetSetting(rateLimitCompactKey, `{not json`); err != nil {
		t.Fatal(err)
	}
	if _, st, ok := a.compactForRateLimit("openai", conns("b"), bodyWith(t, longMessages(9, 4000))); !ok || st.saved() <= 0 {
		t.Fatal("a broken setting stopped compaction instead of falling back to the defaults")
	}
}

// ---- the other three request shapes ----

func TestCompactReadsAResponsesBody(t *testing.T) {
	p := compactPolicy{}.withDefaults()
	var input []map[string]any
	input = append(input, map[string]any{"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": "fix the failing test"}}})
	for i := 0; i < 8; i++ {
		input = append(input,
			map[string]any{"type": "function_call", "call_id": fmt.Sprintf("c%d", i), "name": "shell", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": fmt.Sprintf("c%d", i),
				"output": fmt.Sprintf("call%d:", i) + strings.Repeat("z", 3000)},
		)
	}
	in, _ := json.Marshal(map[string]any{"model": "m", "input": input})
	out, st, ok := compactConversation(in, p, "openai answered 429 for it")
	if !ok || st.saved() <= 0 {
		t.Fatal("a Responses conversation was not compacted")
	}
	text := string(out)
	if strings.Contains(text, "call1:") {
		t.Fatal("an early middle function output came through whole")
	}
	if !strings.Contains(text, "call7:") {
		t.Fatal("a recent function output was cut, so the next turn lost what it reasons over")
	}
	var back struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if back.Input[0]["role"] != "user" {
		t.Fatalf("the opening request is gone: %v", back.Input[0])
	}
}

func TestCompactReadsAChatCompletionsBody(t *testing.T) {
	p := compactPolicy{}.withDefaults()
	msgs := []map[string]any{
		{"role": "system", "content": "be brief"},
		{"role": "user", "content": "fix the failing test"},
	}
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": "working",
				"tool_calls": []any{map[string]any{"id": id, "type": "function",
					"function": map[string]any{"name": "shell", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": id, "content": fmt.Sprintf("call%d:", i) + strings.Repeat("q", 3000)},
		)
	}
	in := bodyWith(t, msgs)
	out, st, ok := compactConversation(in, p, "openai answered 429 for it")
	if !ok || st.saved() <= 0 {
		t.Fatal("a Chat Completions conversation was not compacted")
	}
	got := decodeMessages(t, out)
	if got[0]["content"] != "be brief" {
		t.Fatalf("the system message changed: %v", got[0])
	}
	text := string(out)
	if strings.Contains(text, "call1:") {
		t.Fatal("an early middle tool output came through whole")
	}
	if !strings.Contains(text, "call7:") {
		t.Fatal("a recent tool output was cut, so the next turn lost what it reasons over")
	}
}

func TestCompactReadsACodeAssistBody(t *testing.T) {
	p := compactPolicy{}.withDefaults()
	contents := []map[string]any{
		{"role": "user", "parts": []any{map[string]any{"text": "fix the failing test"}}},
	}
	for i := 0; i < 8; i++ {
		contents = append(contents,
			map[string]any{"role": "model", "parts": []any{
				map[string]any{"text": fmt.Sprintf("step %d", i)},
				map[string]any{"functionCall": map[string]any{"name": "shell", "args": map[string]any{}}},
			}},
			map[string]any{"role": "user", "parts": []any{
				map[string]any{"functionResponse": map[string]any{"name": "shell", "response": map[string]any{"output": fmt.Sprintf("call%d:", i) + strings.Repeat("w", 3000)}}},
			}},
		)
	}
	in, _ := json.Marshal(map[string]any{
		"model": "m",
		"request": map[string]any{
			"contents": contents,
		},
	})
	out, st, ok := compactConversation(in, p, "antigravity answered 429 for it")
	if !ok || st.saved() <= 0 {
		t.Fatal("a Code Assist conversation was not compacted")
	}
	text := string(out)
	if strings.Contains(text, "call1:") {
		t.Fatal("an early middle function response came through whole")
	}
	if !strings.Contains(text, "call7:") {
		t.Fatal("a recent function response was cut, so the next turn lost what it reasons over")
	}
	var back struct {
		Request struct {
			Contents []map[string]any `json:"contents"`
		} `json:"request"`
	}
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Request.Contents) == 0 {
		t.Fatal("the compacted envelope has no contents")
	}
}

// The request that reaches a provider is not the request the client wrote when the
// two speak different shapes, so the retry has to carry the shortened body through
// translation rather than the original.
func TestCompactedBodyStillTranslates(t *testing.T) {
	p := compactPolicy{}.withDefaults()
	msgs := []map[string]any{{"role": "user", "content": "fix the failing test"}}
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": "working",
				"tool_calls": []any{map[string]any{"id": id, "type": "function",
					"function": map[string]any{"name": "shell", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": id, "content": strings.Repeat("q", 3000)},
		)
	}
	in := bodyWith(t, msgs)
	out, _, ok := compactConversation(in, p, "openai answered 429 for it")
	if !ok {
		t.Fatal("not compacted")
	}
	hub, _, err := toProvider(out, translate.OpenAI, translate.Anthropic, "openai")
	if err != nil {
		t.Fatalf("the shortened body does not translate: %v", err)
	}
	if len(hub) == 0 {
		t.Fatal("translation produced nothing")
	}
}

func TestRateLimitIsTheStatusThatCompacts(t *testing.T) {
	// The feature hangs off 429 alone. 529, 503 and 500 are busy or broken too, and a
	// shortened conversation would not help any of them, so the branch is written on
	// this one status rather than on retryableStatus.
	if !retryableStatus(http.StatusTooManyRequests) {
		t.Fatal("429 is not retryable, so the branch would never run")
	}
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusInternalServerError, http.StatusNotFound} {
		if !retryableStatus(code) {
			t.Fatalf("%d is not retryable, so this test no longer shows that 429 is one case among several", code)
		}
	}
}
