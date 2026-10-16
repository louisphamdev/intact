package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// Claude Code's inline-tools beta sends deferred MCP tools as `tool_addition` blocks inside a
// message. A converted route used to drop them silently, so the model never saw the tool.
const toolAdditionBody = `{"model":"m","max_tokens":10,"messages":[
	{"role":"user","content":"go"},
	{"role":"system","content":[{"type":"text","text":"note"},
		{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"mcp__x__do","description":"d","input_schema":{"type":"object","properties":{"a":{"type":"string"}}}}}},
		{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"Read","input_schema":{"type":"object"}}}},
		{"type":"tool_addition","tool":{"type":"tool_reference","name":"mcp__y__ref"}}]},
	{"role":"user","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"mcp__x__do","input_schema":{"type":"object"}}}}]}],
	"tools":[{"name":"Read","input_schema":{"type":"object"}}]}`

func TestToolAdditionBecomesTool(t *testing.T) {
	body := []byte(toolAdditionBody)
	names := NewToolNames(AnthropicToolNames(body), "p")
	out, err := AnthropicToOpenAI(body, names)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages []map[string]any `json:"messages"`
		Tools    []struct {
			Function struct{ Name string } `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	var have []string
	for _, tl := range got.Tools {
		have = append(have, tl.Function.Name)
	}
	if len(have) != 2 || have[0] != "Read" || have[1] != "mcp__x__do" {
		t.Fatalf("tools = %v, want [Read mcp__x__do] (deduped, tool_reference skipped)", have)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("a message left empty by tool_addition must go; got %d messages: %s", len(got.Messages), out)
	}
}

func TestResponsesIncompleteToolOutputIsError(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]},
		{"type":"function_call","call_id":"c1","name":"Read","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","status":"incomplete","output":"cut off"}]}`)
	out, err := ResponsesToOpenAI(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"content":"Error: cut off"`) {
		t.Fatalf("incomplete tool output not marked as error: %s", out)
	}
}
