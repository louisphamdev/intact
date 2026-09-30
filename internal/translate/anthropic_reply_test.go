package translate

import (
	"strings"
	"testing"
)

// sse joins chat chunks into an upstream stream; "[DONE]" is sent as given.
func sse(lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("data: " + l + "\n\n")
	}
	return b.String()
}

func toAnthropic(t *testing.T, src string, r Reply) string {
	t.Helper()
	var out buf
	OpenAIStreamToAnthropic(&out, strings.NewReader(src), r)
	return out.String()
}

// inOrder fails unless each want appears in s after the one before it.
func inOrder(t *testing.T, s string, want ...string) {
	t.Helper()
	pos := 0
	for _, w := range want {
		i := strings.Index(s[pos:], w)
		if i < 0 {
			t.Fatalf("missing %s after byte %d:\n%s", w, pos, s)
		}
		pos += i + len(w)
	}
}

func TestStreamThinkingBecomesAThinkingBlock(t *testing.T) {
	src := sse(`{"id":"c","model":"up","choices":[{"index":0,"delta":{"role":"assistant","reasoning":"hm"}}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{"reasoning_content":"m"}}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{"content":"ok"}}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, "[DONE]")
	s := toAnthropic(t, src, Reply{Thinking: true})
	inOrder(t, s, `"content_block":{"signature":"","thinking":"","type":"thinking"},"index":0`,
		`"thinking":"hm","type":"thinking_delta"`, `"thinking":"m","type":"thinking_delta"`,
		`"signature":"","type":"signature_delta"`, `"index":0,"type":"content_block_stop"`,
		`"content_block":{"text":"","type":"text"},"index":1`, `"text":"ok"`, "message_stop")

	if s := toAnthropic(t, src, Reply{}); strings.Contains(s, "thinking") {
		t.Errorf("thinking not asked for, but sent:\n%s", s)
	}
}

func TestWholeThinkingBecomesAThinkingBlock(t *testing.T) {
	body := `{"id":"c","model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"ok","reasoning_content":"hm"},"finish_reason":"stop"}]}`
	out, err := OpenAIResponseToAnthropic([]byte(body), Reply{Thinking: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := j(mustJSON(t, out)["content"]); got != `[{"signature":"","thinking":"hm","type":"thinking"},{"text":"ok","type":"text"}]` {
		t.Errorf("content = %s", got)
	}
}

func TestCollectKeepsReasoningUnderEitherName(t *testing.T) {
	src := sse(`{"id":"c","choices":[{"index":0,"delta":{"reasoning":"a"}}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{"reasoning_content":"b","content":"x"},"finish_reason":"stop"}]}`, "[DONE]")
	msg := mustJSON(t, CollectOpenAIStream(strings.NewReader(src)))["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["reasoning_content"] != "ab" {
		t.Errorf("message = %s", j(msg))
	}
}

func TestReplyEchoesTheCallersModelAndCountsInput(t *testing.T) {
	src := sse(`{"id":"","model":"space-bunny-free","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`, "[DONE]")
	s := toAnthropic(t, src, Reply{Model: "zencore/space-bunny-free", InputTokens: 42})
	inOrder(t, s, `"id":"msg_`, `"model":"zencore/space-bunny-free"`, `"input_tokens":42`, "event: ping")
	if strings.Contains(s, `"id":"msg_"`) {
		t.Errorf("empty upstream id gave an empty message id:\n%s", s)
	}
	out, _ := OpenAIResponseToAnthropic([]byte(`{"id":"c","model":"space-bunny-free","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`),
		Reply{Model: "zencore/space-bunny-free"})
	if m := mustJSON(t, out); m["model"] != "zencore/space-bunny-free" {
		t.Errorf("model = %v", m["model"])
	}
}

func TestContentFilterIsARefusal(t *testing.T) {
	src := sse(`{"id":"c","choices":[{"index":0,"delta":{"refusal":"I cannot help"}}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`, "[DONE]")
	inOrder(t, toAnthropic(t, src, Reply{}), `"text":"I cannot help"`, `"stop_reason":"refusal"`)

	out, _ := OpenAIResponseToAnthropic([]byte(`{"id":"c","choices":[{"message":{"content":null,"refusal":"no"},"finish_reason":"content_filter"}]}`), Reply{})
	m := mustJSON(t, out)
	if m["stop_reason"] != "refusal" || j(m["content"]) != `[{"text":"no","type":"text"}]` {
		t.Errorf("whole = %s", out)
	}
}

func TestCleanEOFWithoutAFinishIsAnError(t *testing.T) {
	src := sse(`{"id":"c","choices":[{"index":0,"delta":{"content":"partial"}}]}`)
	s := toAnthropic(t, src, Reply{})
	if strings.Contains(s, "message_stop") || !strings.Contains(s, "event: error") {
		t.Errorf("an unfinished stream passed as complete:\n%s", s)
	}
	if b := CollectOpenAIStream(strings.NewReader(src)); !isError(b) {
		t.Errorf("collected an unfinished stream as an answer: %s", b)
	}
}

func isError(b []byte) bool { return strings.Contains(string(b), `"error"`) }

func TestToolCallsWithoutIndexStaySeparate(t *testing.T) {
	src := sse(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"id":"c1","function":{"name":"f","arguments":"{\"a\":1}"}}]}}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"id":"c2","function":{"name":"g","arguments":"{\"b\":2}"}}]}}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, "[DONE]")
	s := toAnthropic(t, src, Reply{})
	inOrder(t, s, `"id":"c1"`, `"partial_json":"{\"a\":1}"`, `"id":"c2"`, `"partial_json":"{\"b\":2}"`)
}

func TestFunctionCallDeltaIsAToolUse(t *testing.T) {
	src := sse(`{"id":"c","choices":[{"index":0,"delta":{"function_call":{"name":"f","arguments":"{\"a\":1}"}}}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"function_call"}]}`, "[DONE]")
	inOrder(t, toAnthropic(t, src, Reply{}), `"id":"toolu_`, `"name":"f"`, `"partial_json":"{\"a\":1}"`, `"stop_reason":"tool_use"`)
}

func TestToolCallIDAndArgumentsAreAlwaysUsable(t *testing.T) {
	src := sse(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"f","arguments":"{bad"}}]}}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, "[DONE]")
	s := toAnthropic(t, src, Reply{})
	inOrder(t, s, `"id":"toolu_`, `"partial_json":"{}"`)
	out, _ := OpenAIResponseToAnthropic([]byte(`{"id":"c","choices":[{"message":{"tool_calls":[{"type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`), Reply{})
	if !strings.Contains(string(out), `"id":"toolu_`) {
		t.Errorf("whole tool id = %s", out)
	}
}

func TestUsageNeverGoesNegativeAndCountsCacheWrites(t *testing.T) {
	got := j(anthropicUsage(obj{"prompt_tokens": 100.0, "completion_tokens": 5.0,
		"prompt_tokens_details": obj{"cached_tokens": 30.0, "cache_write_tokens": 20.0}}))
	if got != `{"cache_creation_input_tokens":20,"cache_read_input_tokens":30,"input_tokens":50,"output_tokens":5}` {
		t.Errorf("usage = %s", got)
	}
	if got := j(anthropicUsage(obj{"prompt_tokens": 5.0, "prompt_tokens_details": obj{"cached_tokens": 15.0}})); got != `{"cache_read_input_tokens":15,"input_tokens":0,"output_tokens":0}` {
		t.Errorf("usage = %s", got)
	}
}

func TestWholeContentAsPartsIsText(t *testing.T) {
	out, _ := OpenAIResponseToAnthropic([]byte(`{"id":"c","choices":[{"message":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]},"finish_reason":"stop"}]}`), Reply{})
	if got := j(mustJSON(t, out)["content"]); got != `[{"text":"ab","type":"text"}]` {
		t.Errorf("content = %s", got)
	}
}

func TestErrorTypesAreAnthropicTypes(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{429, `{"error":{"type":"requests","message":"slow"}}`, "rate_limit_error"},
		{400, `{"error":{"type":"server_error","message":"Model is unavailable."}}`, "api_error"},
		{400, `{"error":{"type":"invalid_request_error","message":"x"}}`, "invalid_request_error"},
		{404, `{"error":"gone"}`, "not_found_error"},
		{529, `oops`, "overloaded_error"},
		{401, `{"error":{"message":"k"}}`, "authentication_error"},
	}
	for _, c := range cases {
		m := mustJSON(t, Error(c.status, []byte(c.body), Anthropic))
		if m["type"] != "error" || m["error"].(map[string]any)["type"] != c.want {
			t.Errorf("%d %s -> %s, want %s", c.status, c.body, j(m), c.want)
		}
	}
	src := sse(`{"error":{"type":"rate_limit_exceeded","message":"slow"}}`)
	inOrder(t, toAnthropic(t, src, Reply{}), "event: error", `"type":"rate_limit_error"`)
}

func TestErrorToOpenAIHasTheFullEnvelope(t *testing.T) {
	if got := string(Error(429, []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`), OpenAI)); got != `{"error":{"code":null,"message":"slow","param":null,"type":"rate_limit_error"}}` {
		t.Errorf("to openai = %s", got)
	}
}
