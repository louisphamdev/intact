package translate

import (
	"strings"
	"testing"
)

func toOpenAI(t *testing.T, body string) map[string]any {
	t.Helper()
	out, err := AnthropicToOpenAI([]byte(body), nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	return mustJSON(t, out)
}

func TestAdaptiveThinkingKeepsTheEffort(t *testing.T) {
	cases := map[string]string{
		`{"model":"m","max_tokens":9,"thinking":{"type":"adaptive"},"messages":[]}`:                                     "high",
		`{"model":"m","max_tokens":9,"thinking":{"type":"adaptive"},"output_config":{"effort":"low"},"messages":[]}`:    "low",
		`{"model":"m","max_tokens":9,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"messages":[]}`:    "high",
		`{"model":"m","max_tokens":9,"thinking":{"type":"adaptive"},"output_config":{"effort":"medium"},"messages":[]}`: "medium",
	}
	for in, want := range cases {
		if got := toOpenAI(t, in)["reasoning_effort"]; got != want {
			t.Errorf("%s -> %v, want %s", in, got, want)
		}
	}
}

func TestUnsupportedContentIsRefusedNotDropped(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","max_tokens":9,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"QQ=="}},{"type":"text","text":"sum"}]}]}`,
		`{"model":"m","max_tokens":9,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"file","file_id":"f1"}}]}]}`,
	} {
		if _, err := AnthropicToOpenAI([]byte(body), nil); err == nil || !strings.Contains(err.Error(), "not supported") {
			t.Errorf("%s: err = %v", body, err)
		}
	}
}

func TestToolResultImagesReachTheModel(t *testing.T) {
	m := toOpenAI(t, `{"model":"m","max_tokens":9,"messages":[
	 {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"shot","input":{}}]},
	 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"r"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}]}]}`)
	got := j(m["messages"])
	if !strings.Contains(got, `"role":"tool"`) || !strings.Contains(got, `"url":"data:image/png;base64,QUJD"`) {
		t.Errorf("messages = %s", got)
	}
}

func TestToolChoiceWithoutToolsIsDropped(t *testing.T) {
	m := toOpenAI(t, `{"model":"m","max_tokens":9,"tool_choice":{"type":"any"},"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[]}`)
	if _, ok := m["tool_choice"]; ok {
		t.Errorf("tool_choice sent with no tools: %s", j(m))
	}
}
