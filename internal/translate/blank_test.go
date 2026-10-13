package translate

import (
	"encoding/json"
	"testing"
)

func TestWithoutBlankTextDropsBlankSystemAndMessageBlocks(t *testing.T) {
	in := []byte(`{"model":"m","system":[{"type":"text","text":"keep"},{"type":"text","text":""},{"type":"text","text":"  \n"}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":" "},{"type":"text","text":"hi"}]},` +
		`{"role":"assistant","content":""},{"role":"user","content":[{"type":"text","text":""}]}]}`)
	var got map[string]any
	if err := json.Unmarshal(WithoutBlankText(in), &got); err != nil {
		t.Fatal(err)
	}
	if sys := got["system"].([]any); len(sys) != 1 || sys[0].(map[string]any)["text"] != "keep" {
		t.Fatalf("system = %v", got["system"])
	}
	msgs := got["messages"].([]any)
	if c := msgs[0].(map[string]any)["content"].([]any); len(c) != 1 || c[0].(map[string]any)["text"] != "hi" {
		t.Fatalf("message 0 = %v", msgs[0])
	}
	for _, i := range []int{1, 2} {
		if s := contentText(msgs[i].(map[string]any)["content"]); s != blankStandIn {
			t.Fatalf("message %d content = %q, want the stand-in", i, s)
		}
	}
}

func TestWithoutBlankTextDropsABlankSystemString(t *testing.T) {
	var got map[string]any
	_ = json.Unmarshal(WithoutBlankText([]byte(`{"system":"  ","messages":[{"role":"user","content":"hi"}]}`)), &got)
	if _, ok := got["system"]; ok {
		t.Fatalf("system kept: %v", got)
	}
}

// A body with nothing blank goes out byte for byte: Claude Code's prompt cache
// keys on those bytes.
func TestWithoutBlankTextKeepsTheBytesOfACleanBody(t *testing.T) {
	in := []byte(`{"system":[{"type":"text","text":"a"}],"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`)
	if out := WithoutBlankText(in); string(out) != string(in) {
		t.Fatalf("clean body rewritten:\n%s", out)
	}
}

// A blank last assistant turn is dropped, not filled: filled, it becomes a
// prefill, which current models refuse.
func TestWithoutBlankTextDropsABlankLastAssistantTurn(t *testing.T) {
	var got map[string]any
	_ = json.Unmarshal(WithoutBlankText([]byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"text","text":" "}]}]}`)), &got)
	if msgs := got["messages"].([]any); len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
}

// Hermes replays both names of the reasoning field; vLLM-style upstreams refuse the pair as a
// duplicate field (NousResearch/hermes-agent#136486), so only reasoning_content goes on.
func TestWithoutDuplicateReasoningKeepsOnlyReasoningContent(t *testing.T) {
	in := []byte(`{"messages":[{"role":"assistant","content":"a","reasoning":"r","reasoning_content":"r"},{"role":"assistant","content":"b","reasoning":"only"}]}`)
	var got map[string]any
	_ = json.Unmarshal(WithoutDuplicateReasoning(in), &got)
	msgs := got["messages"].([]any)
	first, second := msgs[0].(map[string]any), msgs[1].(map[string]any)
	if _, ok := first["reasoning"]; ok || first["reasoning_content"] != "r" {
		t.Fatalf("first = %v", first)
	}
	if second["reasoning"] != "only" {
		t.Fatalf("a lone reasoning field must stay: %v", second)
	}
	clean := []byte(`{"messages":[{"role":"assistant","content":"a","reasoning_content":"r"}]}`)
	if string(WithoutDuplicateReasoning(clean)) != string(clean) {
		t.Fatal("clean body rewritten")
	}
}
