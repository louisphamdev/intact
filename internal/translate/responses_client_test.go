package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// codexRequest is shaped like what the Codex CLI sends: a developer message,
// a namespaced tool call from an earlier turn, a hosted tool, a freeform tool.
const codexRequest = `{"model":"m","instructions":"be a coding agent","stream":true,"store":false,
 "max_output_tokens":500,"parallel_tool_calls":true,"tool_choice":"auto",
 "reasoning":{"effort":"medium","summary":"auto"},"include":["reasoning.encrypted_content"],
 "text":{"format":{"type":"json_schema","name":"out","strict":true,"schema":{"type":"object"}}},
 "input":[
  {"type":"message","role":"developer","content":[{"type":"input_text","text":"sandbox rules"}]},
  {"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]},
  {"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAA"},
  {"type":"function_call","call_id":"c1","name":"connect_to_db","namespace":"mcp__oracle","arguments":"{\"db\":\"x\"}"},
  {"type":"function_call","call_id":"c2","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"},
  {"type":"function_call_output","call_id":"c1","output":"connected"},
  {"type":"function_call_output","call_id":"c2","output":[{"type":"input_text","text":"a.go"}]},
  {"type":"custom_tool_call","call_id":"c3","name":"apply_patch","input":"*** Begin Patch"},
  {"role":"user","content":"go on"}],
 "tools":[
  {"type":"function","name":"exec_command","description":"run","strict":false,"parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}},
  {"type":"namespace","name":"mcp__oracle","description":"db","tools":[{"type":"function","name":"connect_to_db","parameters":{"type":"object"}}]},
  {"type":"custom","name":"apply_patch","description":"patch","format":{"type":"grammar","syntax":"lark","definition":"start: x"}},
  {"type":"web_search","external_web_access":false}]}`

func TestResponsesToOpenAIRequest(t *testing.T) {
	out, err := ResponsesToOpenAI([]byte(codexRequest))
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	msgs := m["messages"].([]any)
	roles := []string{}
	for _, x := range msgs {
		roles = append(roles, x.(map[string]any)["role"].(string))
	}
	// c3 has no output, so a placeholder answers it before the next user turn.
	if got := strings.Join(roles, ","); got != "system,user,assistant,tool,tool,assistant,tool,user" {
		t.Fatalf("roles = %s\n%s", got, out)
	}
	if s := msgs[0].(map[string]any)["content"]; s != "be a coding agent\n\nsandbox rules" {
		t.Errorf("system = %q", s)
	}
	calls := msgs[2].(map[string]any)["tool_calls"].([]any)
	if len(calls) != 2 || j(calls[0].(map[string]any)["function"]) != `{"arguments":"{\"db\":\"x\"}","name":"mcp__oracle__connect_to_db"}` {
		t.Errorf("tool calls = %s", j(calls))
	}
	if c := msgs[4].(map[string]any)["content"]; c != "a.go" {
		t.Errorf("array output = %q", c)
	}
	if a := j(msgs[5].(map[string]any)["tool_calls"]); !strings.Contains(a, `"arguments":"{\"input\":\"*** Begin Patch\"}"`) {
		t.Errorf("custom call = %s", a)
	}
	names := []string{}
	for _, x := range m["tools"].([]any) {
		names = append(names, x.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	if got := strings.Join(names, ","); got != "exec_command,mcp__oracle__connect_to_db,apply_patch" {
		t.Errorf("tools = %s", got)
	}
	checks := map[string]string{
		"max_tokens":       `500`,
		"stream":           `true`,
		"stream_options":   `{"include_usage":true}`,
		"reasoning_effort": `"medium"`,
		"tool_choice":      `"auto"`,
		"response_format":  `{"json_schema":{"name":"out","schema":{"type":"object"},"strict":true},"type":"json_schema"}`,
	}
	for k, want := range checks {
		if got := j(m[k]); got != want {
			t.Errorf("%s = %s, want %s", k, got, want)
		}
	}
	for _, k := range []string{"input", "instructions", "include", "store", "text", "reasoning"} {
		if _, ok := m[k]; ok {
			t.Errorf("Responses field %q reached the chat body", k)
		}
	}
}

// events splits a Responses SSE stream into its data objects.
func events(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, block := range strings.Split(s, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if d, ok := strings.CutPrefix(line, "data: "); ok {
				out = append(out, mustJSON(t, []byte(d)))
			}
		}
	}
	return out
}

func TestOpenAIStreamToResponses(t *testing.T) {
	tools := ResponsesTools([]byte(codexRequest))
	chat := strings.Join([]string{
		`data: {"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think"}}]}`,
		`data: {"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
		`data: {"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		`data: {"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"mcp__oracle__connect_to_db","arguments":"{\"db\""}}]}}]}`,
		`data: {"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"y\"}"}}]}}]}`,
		`data: {"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_10","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\\n\"}"}}]}}]}`,
		`data: {"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"id":"chatcmpl-1","model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":2}}}`,
		`data: [DONE]`, ``}, "\n\n")
	var out buf
	OpenAIStreamToResponses(&out, strings.NewReader(chat), tools)
	evs := events(t, out.String())
	if len(evs) == 0 || evs[0]["type"] != "response.created" {
		t.Fatalf("first event = %v", evs)
	}
	last := evs[len(evs)-1]
	if last["type"] != "response.completed" {
		t.Fatalf("last event = %s", j(last))
	}
	resp := last["response"].(map[string]any)
	if u := j(resp["usage"]); u != `{"input_tokens":10,"input_tokens_details":{"cached_tokens":4},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":15}` {
		t.Errorf("usage = %s", u)
	}
	var done []string
	var text strings.Builder
	for i, e := range evs {
		if n := int(e["sequence_number"].(float64)); n != i {
			t.Errorf("event %d has sequence_number %d", i, n)
		}
		switch e["type"] {
		case "response.output_item.done":
			done = append(done, j(e["item"]))
		case "response.output_text.delta":
			text.WriteString(e["delta"].(string))
		}
	}
	if text.String() != "Hello" {
		t.Errorf("text = %q", text.String())
	}
	if len(done) != 4 {
		t.Fatalf("done items = %v", done)
	}
	want := []string{`"type":"reasoning"`, `"text":"Hello"`,
		`"arguments":"{\"db\":\"y\"}","call_id":"call_9"`, `"input":"*** Begin Patch\n"`}
	for i, w := range want {
		if !strings.Contains(done[i], w) {
			t.Errorf("item %d = %s, want %s", i, done[i], w)
		}
	}
	if !strings.Contains(done[2], `"name":"connect_to_db","namespace":"mcp__oracle"`) || !strings.Contains(done[3], `"type":"custom_tool_call"`) {
		t.Errorf("tool items lost their Responses names: %s / %s", done[2], done[3])
	}
}

func TestOpenAIStreamToResponsesErrors(t *testing.T) {
	var out buf
	OpenAIStreamToResponses(&out, strings.NewReader(`data: {"error":{"message":"quota"}}`+"\n\n"), nil)
	evs := events(t, out.String())
	if last := evs[len(evs)-1]; last["type"] != "response.failed" || !strings.Contains(j(last), `"message":"quota"`) {
		t.Errorf("error chunk: %s", out.String())
	}
	// A length stop drops a tool call whose arguments were cut off.
	out.Reset()
	OpenAIStreamToResponses(&out, strings.NewReader(
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"exec_command","arguments":"{\"cmd"}}]}}]}`+"\n\n"+
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`+"\n\ndata: [DONE]\n\n"), nil)
	s := out.String()
	if strings.Contains(s, `"type":"response.output_item.done"`) || !strings.Contains(s, `"reason":"max_output_tokens"`) {
		t.Errorf("length stop: %s", s)
	}
}

func TestOpenAIResponseToResponses(t *testing.T) {
	tools := ResponsesTools([]byte(codexRequest))
	chat := `{"id":"chatcmpl-1","object":"chat.completion","model":"m","choices":[{"index":0,
	 "message":{"role":"assistant","content":"done","reasoning_content":"hmm",
	 "tool_calls":[{"id":"call_1","type":"function","function":{"name":"mcp__oracle__connect_to_db","arguments":"{}"}}]},
	 "finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`
	out, err := OpenAIResponseToResponses([]byte(chat), tools)
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	if m["object"] != "response" || m["status"] != "completed" || m["output_text"] != "done" {
		t.Errorf("response = %s", out)
	}
	var types []string
	for _, it := range m["output"].([]any) {
		types = append(types, it.(map[string]any)["type"].(string))
	}
	if strings.Join(types, ",") != "reasoning,message,function_call" {
		t.Errorf("output types = %v", types)
	}
	fc := m["output"].([]any)[2].(map[string]any)
	if fc["name"] != "connect_to_db" || fc["namespace"] != "mcp__oracle" || fc["call_id"] != "call_1" {
		t.Errorf("function_call = %s", j(fc))
	}
	var u map[string]any
	json.Unmarshal([]byte(j(m["usage"])), &u)
	if u["total_tokens"].(float64) != 5 {
		t.Errorf("usage = %s", j(u))
	}
}
