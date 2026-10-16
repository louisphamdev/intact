package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// A thinking block intact wrote on a converted route carries signature "" (there is nothing to sign).
// A client that replays that history to Anthropic itself gets 400 "Invalid `signature` in `thinking`
// block" (bifrost#8549, reproduced against anthropic/claude-sonnet-5-5 through intact).
func TestWithoutUnsignedThinking(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"a","signature":""},{"type":"text","text":"hello"}]},
		{"role":"user","content":"again"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"b"}]},
		{"role":"user","content":"x"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"c","signature":"EqQB"},{"type":"text","text":"signed"}]},
		{"role":"user","content":"last"}]}`)
	out := WithoutUnsignedThinking(in)
	var got struct {
		Messages []struct {
			Role    string
			Content json.RawMessage
		}
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 7 {
		t.Fatalf("messages = %d, want 7 (role alternation kept): %s", len(got.Messages), out)
	}
	blocks := func(i int) []map[string]any {
		var c []map[string]any
		_ = json.Unmarshal(got.Messages[i].Content, &c)
		return c
	}
	if c := blocks(1); len(c) != 1 || c[0]["type"] != "text" {
		t.Fatalf("unsigned thinking kept: %v", c)
	}
	if c := blocks(3); len(c) != 1 || c[0]["type"] != "text" || c[0]["text"] == "" {
		t.Fatalf("a turn that was only unsigned thinking needs a stand-in: %v", c)
	}
	if c := blocks(5); len(c) != 2 || c[0]["type"] != "thinking" {
		t.Fatalf("signed thinking must stay byte-for-byte: %v", c)
	}
}

func TestWithoutUnsignedThinkingKeepsCleanBodyBytes(t *testing.T) {
	in := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"c","signature":"EqQB"}]}]}`)
	if string(WithoutUnsignedThinking(in)) != string(in) {
		t.Fatal("a body with only signed thinking changed")
	}
	if !strings.Contains(string(WithoutUnsignedThinking([]byte(`not json`))), "not json") {
		t.Fatal("invalid body must pass through")
	}
}
