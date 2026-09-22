package translate

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("bad json %s: %v", b, err)
	}
	return m
}

func j(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestOpenAIToAnthropicRequest(t *testing.T) {
	in := `{"model":"m","max_completion_tokens":100,"temperature":0.2,"stop":"END","stream":true,
	 "messages":[
	  {"role":"system","content":"be brief"},
	  {"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"x\":1}"}}]},
	  {"role":"tool","tool_call_id":"c1","content":"42"},
	  {"role":"user","content":"thanks"}],
	 "tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"}}}],
	 "tool_choice":"required","parallel_tool_calls":false}`
	out, err := OpenAIToAnthropic([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	checks := map[string]string{
		"system":         `[{"text":"be brief","type":"text"}]`,
		"max_tokens":     `100`,
		"stop_sequences": `["END"]`,
		"stream":         `true`,
		"tools":          `[{"description":"d","input_schema":{"type":"object"},"name":"f"}]`,
		"tool_choice":    `{"disable_parallel_tool_use":true,"type":"any"}`,
	}
	for k, want := range checks {
		if got := j(m[k]); got != want {
			t.Errorf("%s = %s, want %s", k, got, want)
		}
	}
	msgs := m["messages"].([]any)
	// user, assistant(tool_use), user(tool_result + "thanks" merged)
	if len(msgs) != 3 {
		t.Fatalf("messages = %s", j(msgs))
	}
	if got := j(msgs[0].(map[string]any)["content"]); !strings.Contains(got, `"media_type":"image/png"`) {
		t.Errorf("image not converted: %s", got)
	}
	if got := j(msgs[1]); !strings.Contains(got, `"input":{"x":1}`) || !strings.Contains(got, `"tool_use"`) {
		t.Errorf("tool call not converted: %s", got)
	}
	if got := j(msgs[2]); !strings.Contains(got, `"tool_result"`) || !strings.Contains(got, `"thanks"`) {
		t.Errorf("tool result and follow-up not merged: %s", got)
	}
}

func TestAnthropicToOpenAIRequest(t *testing.T) {
	in := `{"model":"m","max_tokens":50,"system":[{"type":"text","text":"sys"}],"stream":true,"stop_sequences":["X"],
	 "messages":[
	  {"role":"user","content":"hi"},
	  {"role":"assistant","content":[{"type":"text","text":"calling"},{"type":"tool_use","id":"t1","name":"f","input":{"a":"b"}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"ok"}]},{"type":"text","text":"next"}]}],
	 "tools":[{"name":"f","input_schema":{"type":"object"}},{"type":"web_search_20250305","name":"web_search"}],
	 "tool_choice":{"type":"tool","name":"f"}}`
	out, err := AnthropicToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	msgs := m["messages"].([]any)
	roles := []string{}
	for _, x := range msgs {
		roles = append(roles, x.(map[string]any)["role"].(string))
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool,user" {
		t.Fatalf("roles = %v; messages = %s", roles, j(msgs))
	}
	if got := j(msgs[2]); !strings.Contains(got, `"arguments":"{\"a\":\"b\"}"`) {
		t.Errorf("tool_use not converted: %s", got)
	}
	if got := j(msgs[3]); !strings.Contains(got, `"content":"ok"`) || !strings.Contains(got, `"tool_call_id":"t1"`) {
		t.Errorf("tool_result not converted: %s", got)
	}
	if j(m["stream_options"]) != `{"include_usage":true}` || j(m["stop"]) != `["X"]` {
		t.Errorf("stream_options=%s stop=%s", j(m["stream_options"]), j(m["stop"]))
	}
	if got := j(m["tools"]); strings.Contains(got, "web_search") {
		t.Errorf("server tool leaked into OpenAI tools: %s", got)
	}
	if j(m["tool_choice"]) != `{"function":{"name":"f"},"type":"function"}` {
		t.Errorf("tool_choice = %s", j(m["tool_choice"]))
	}
}

func TestResponsesBothWays(t *testing.T) {
	ant := `{"id":"msg_1","type":"message","model":"claude","content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"t","name":"f","input":{"q":1}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"cache_read_input_tokens":5,"output_tokens":3}}`
	oa, err := AnthropicResponseToOpenAI([]byte(ant))
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, oa)
	ch := m["choices"].([]any)[0].(map[string]any)
	if ch["finish_reason"] != "tool_calls" || j(m["usage"]) != `{"completion_tokens":3,"prompt_tokens":15,"prompt_tokens_details":{"cached_tokens":5},"total_tokens":18}` {
		t.Errorf("openai = %s", oa)
	}
	back, err := OpenAIResponseToAnthropic(oa)
	if err != nil {
		t.Fatal(err)
	}
	b := mustJSON(t, back)
	if b["stop_reason"] != "tool_use" || !strings.Contains(j(b["content"]), `"input":{"q":1}`) ||
		j(b["usage"]) != `{"cache_read_input_tokens":5,"input_tokens":10,"output_tokens":3}` {
		t.Errorf("anthropic = %s", back)
	}
}

type buf struct{ bytes.Buffer }

func (b *buf) Flush() error { return nil }

func TestAnthropicStreamToOpenAI(t *testing.T) {
	src := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude\",\"usage\":{\"input_tokens\":7}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t\",\"name\":\"f\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":4}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var out buf
	AnthropicStreamToOpenAI(&out, strings.NewReader(src))
	s := out.String()
	for _, want := range []string{`"role":"assistant"`, `"content":"Hel"`, `"name":"f"`, `"arguments":"{\"a\":"`,
		`"finish_reason":"tool_calls"`, `"prompt_tokens":7`, `"completion_tokens":4`, "data: [DONE]"} {
		if !strings.Contains(s, want) {
			t.Errorf("stream missing %s:\n%s", want, s)
		}
	}
}

func TestOpenAIStreamToAnthropic(t *testing.T) {
	src := `data: {"id":"chatcmpl-1","model":"llama","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"llama","choices":[{"index":0,"delta":{"content":"Hi"}}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"llama","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"f","arguments":"{}"}}]}}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"llama","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":2}}` + "\n\n" +
		"data: [DONE]\n\n"
	var out buf
	OpenAIStreamToAnthropic(&out, strings.NewReader(src))
	s := out.String()
	order := []string{"event: message_start", `"text":"Hi","type":"text_delta"`, "event: content_block_stop",
		`"type":"tool_use"`, `"partial_json":"{}"`, `"stop_reason":"tool_use"`, `"input_tokens":9`, "event: message_stop"}
	pos := 0
	for _, want := range order {
		i := strings.Index(s[pos:], want)
		if i < 0 {
			t.Fatalf("stream missing %s after byte %d:\n%s", want, pos, s)
		}
		pos += i
	}
}

func TestErrorShapes(t *testing.T) {
	if got := string(Error([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`), OpenAI)); got != `{"error":{"message":"slow","type":"rate_limit_error"}}` {
		t.Errorf("to openai = %s", got)
	}
	if got := string(Error([]byte(`{"error":{"message":"bad","type":"invalid_request_error"}}`), Anthropic)); got != `{"error":{"message":"bad","type":"invalid_request_error"},"type":"error"}` {
		t.Errorf("to anthropic = %s", got)
	}
}

func TestCollectOpenAIStream(t *testing.T) {
	src := `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"Hel"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"lo","tool_calls":[{"index":0,"id":"t","function":{"name":"f","arguments":"{\"a\""}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}` + "\n\n" +
		"data: [DONE]\n\n"
	m := mustJSON(t, CollectOpenAIStream(strings.NewReader(src)))
	msg := m["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Hello" || !strings.Contains(j(msg["tool_calls"]), `"arguments":"{\"a\":1}"`) || j(m["usage"]) != `{"completion_tokens":3,"prompt_tokens":2}` {
		t.Errorf("collected = %s", j(m))
	}
}

func TestOpenAIToResponsesAndBack(t *testing.T) {
	in := `{"model":"gpt-5","reasoning_effort":"low","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"},
	 {"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
	 {"role":"tool","tool_call_id":"c1","content":"ok"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`
	out, err := OpenAIToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	if m["instructions"] != "sys" || m["stream"] != true || m["store"] != false || j(m["reasoning"]) != `{"effort":"low","summary":"auto"}` {
		t.Errorf("responses req = %s", out)
	}
	if s := j(m["input"]); !strings.Contains(s, `"function_call"`) || !strings.Contains(s, `"function_call_output"`) || !strings.Contains(s, `"input_text"`) {
		t.Errorf("input = %s", s)
	}
	src := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hi\"}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"c2\",\"name\":\"f\"}}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"{}\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"input_tokens_details\":{\"cached_tokens\":1}}}}\n\n"
	var o buf
	ResponsesStreamToOpenAI(&o, strings.NewReader(src))
	s := o.String()
	for _, want := range []string{`"content":"Hi"`, `"id":"c2"`, `"arguments":"{}"`, `"finish_reason":"tool_calls"`, `"prompt_tokens":5`, `"cached_tokens":1`, "[DONE]"} {
		if !strings.Contains(s, want) {
			t.Errorf("chunks missing %s:\n%s", want, s)
		}
	}
}

type memSigs map[string]string

func (m memSigs) Get(id string) string { return m[id] }
func (m memSigs) Put(id, s string)     { m[id] = s }

func TestOpenAIToGemini(t *testing.T) {
	sigs := memSigs{"c1": "SIG1"}
	in := `{"model":"gemini-3-flash","max_tokens":100000,"reasoning_effort":"low","messages":[{"role":"system","content":"sys"},
	 {"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]},
	 {"role":"assistant","content":"calling","tool_calls":[{"id":"c1","type":"function","function":{"name":"get weather","arguments":"{\"city\":\"HN\"}"}},{"id":"c9","type":"function","function":{"name":"f2","arguments":"{}"}}]},
	 {"role":"tool","tool_call_id":"c1","content":"{\"t\":30}"},{"role":"tool","tool_call_id":"c9","content":"done"}],
	 "tools":[{"type":"function","function":{"name":"get weather","parameters":{"type":"object","$schema":"x","additionalProperties":false,
	   "properties":{"city":{"type":["string","null"],"minLength":1},"unit":{"anyOf":[{"type":"null"},{"type":"string","enum":["c","f"]}]},"n":{"const":3}},"required":["city","gone"]}}}]}`
	out, err := OpenAIToGemini([]byte(in), sigs)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`"systemInstruction":{"parts":[{"text":"sys"}],"role":"user"}`, `"inlineData":{"data":"QUJD","mimeType":"image/png"}`,
		`"thoughtSignature":"SIG1"`, `"functionResponse":{"id":"c1","name":"get weather","response":{"result":{"t":30}}}`,
		`"maxOutputTokens":64000`, `"thinkingLevel":"low"`, `"name":"get_weather"`, `"city":{"type":"string"}`,
		`"unit":{"enum":["c","f"],"type":"string"}`, `"n":{"enum":["3"],"type":"string"}`, `"required":["city"]`, `"mode":"VALIDATED"`} {
		if !strings.Contains(s, want) {
			t.Errorf("gemini request missing %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, "$schema") || strings.Contains(s, "additionalProperties") || strings.Contains(s, "minLength") {
		t.Errorf("unsupported schema keys left: %s", s)
	}
	// The second call of the turn has no cached signature and gets none.
	if strings.Count(s, "thoughtSignature") != 1 {
		t.Errorf("signatures = %d, want 1", strings.Count(s, "thoughtSignature"))
	}
}

func TestGeminiStreamToOpenAI(t *testing.T) {
	sigs := memSigs{}
	src := `data: {"response":{"responseId":"r1","modelVersion":"gemini-3-flash","candidates":[{"content":{"role":"model","parts":[{"text":"think","thought":true,"thoughtSignature":"S"}]}}]}}` + "\n\n" +
		`data: {"response":{"candidates":[{"content":{"parts":[{"text":"Hi"},{"functionCall":{"id":"fc1","name":"f","args":{"a":1}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"thoughtsTokenCount":2}}}` + "\n\n"
	var o buf
	GeminiStreamToOpenAI(&o, strings.NewReader(src), sigs)
	s := o.String()
	for _, want := range []string{`"reasoning_content":"think"`, `"content":"Hi"`, `"arguments":"{\"a\":1}"`, `"id":"fc1"`,
		`"finish_reason":"tool_calls"`, `"prompt_tokens":7`, `"completion_tokens":5`, "[DONE]"} {
		if !strings.Contains(s, want) {
			t.Errorf("chunks missing %s:\n%s", want, s)
		}
	}
	if sigs["fc1"] != "S" {
		t.Errorf("signature not recorded for the call: %v", sigs)
	}
}
