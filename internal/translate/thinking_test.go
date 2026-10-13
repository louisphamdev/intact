package translate

import (
	"encoding/json"
	"testing"
)

func chatToAnthropic(t *testing.T, body string, opt AnthropicOptions) obj {
	t.Helper()
	out, err := OpenAIToAnthropicFor([]byte(body), opt)
	if err != nil {
		t.Fatal(err)
	}
	var m obj
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

const loopTail = `{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"12:00"}`

// A model that refuses budget_tokens (Opus 4.7 and later) is asked with adaptive
// thinking and an effort level, and gets no sampling values, which it also refuses.
func TestAdaptiveModelGetsAdaptiveThinkingAndEffort(t *testing.T) {
	m := chatToAnthropic(t, `{"model":"m","reasoning_effort":"high","temperature":0.2,"top_p":0.9,"max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`, AnthropicOptions{Adaptive: true})
	if th := asObj(m["thinking"]); str(th["type"]) != "adaptive" || th["budget_tokens"] != nil {
		t.Fatalf("thinking = %v", m["thinking"])
	}
	if e := str(asObj(m["output_config"])["effort"]); e != "high" {
		t.Fatalf("effort = %q", e)
	}
	if m["temperature"] != nil || m["top_p"] != nil {
		t.Fatalf("sampling kept: %v %v", m["temperature"], m["top_p"])
	}
}

func TestAdaptiveEffortKeepsTheResponseFormat(t *testing.T) {
	m := chatToAnthropic(t, `{"model":"m","reasoning_effort":"minimal","response_format":{"type":"json_schema","json_schema":{"schema":{"type":"object"}}},"messages":[{"role":"user","content":"hi"}]}`, AnthropicOptions{Adaptive: true})
	oc := asObj(m["output_config"])
	if str(oc["effort"]) != "low" || asObj(oc["format"]) == nil {
		t.Fatalf("output_config = %v", oc)
	}
}

// A budget model in the middle of a tool loop gets no thinking: the translated
// history carries no signed thinking block, and Anthropic refuses the turn
// ("a final assistant message must start with a thinking block").
func TestBudgetModelSkipsThinkingInAnUnsignedToolLoop(t *testing.T) {
	m := chatToAnthropic(t, `{"model":"m","reasoning_effort":"high","max_tokens":8192,"messages":[{"role":"user","content":"go"},`+loopTail+`]}`, AnthropicOptions{})
	if m["thinking"] != nil {
		t.Fatalf("thinking = %v", m["thinking"])
	}
	m = chatToAnthropic(t, `{"model":"m","reasoning_effort":"high","max_tokens":8192,"messages":[{"role":"user","content":"go"}]}`, AnthropicOptions{})
	if str(asObj(m["thinking"])["type"]) != "enabled" {
		t.Fatalf("a fresh turn keeps budget thinking: %v", m["thinking"])
	}
}
