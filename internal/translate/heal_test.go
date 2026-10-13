package translate

import "testing"

// A notice the client puts between two outputs of one parallel batch must not
// cost the batch its real output: it moves after the batch instead.
func TestHealToolPairsDefersAMessageInsideABatch(t *testing.T) {
	msgs := []obj{
		{"role": "user", "content": "hi"},
		{"role": "assistant", "tool_calls": []any{obj{"id": "A"}, obj{"id": "B"}}},
		{"role": "tool", "tool_call_id": "A", "content": "outA"},
		{"role": "user", "content": "notice"},
		{"role": "tool", "tool_call_id": "B", "content": "outB"},
		{"role": "user", "content": "next"},
	}
	got := healToolPairs(msgs)
	want := []string{"user:hi", "assistant:", "tool:outA", "tool:outB", "user:notice", "user:next"}
	if len(got) != len(want) {
		t.Fatalf("got %d messages %v, want %v", len(got), got, want)
	}
	for i, m := range got {
		if s := str(m["role"]) + ":" + contentText(m["content"]); s != want[i] {
			t.Fatalf("message %d = %q, want %q (all: %v)", i, s, want[i], got)
		}
	}
}

// A call that never gets an output still gets the stand-in, and the held
// message still follows the batch.
func TestHealToolPairsStandInStillCoversAMissingOutput(t *testing.T) {
	msgs := []obj{
		{"role": "assistant", "tool_calls": []any{obj{"id": "A"}, obj{"id": "B"}}},
		{"role": "tool", "tool_call_id": "A", "content": "outA"},
		{"role": "user", "content": "notice"},
		{"role": "assistant", "content": "done"},
	}
	got := healToolPairs(msgs)
	want := []string{"assistant:", "tool:outA", "tool:" + missingToolResult, "user:notice", "assistant:done"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, m := range got {
		if s := str(m["role"]) + ":" + contentText(m["content"]); s != want[i] {
			t.Fatalf("message %d = %q, want %q", i, s, want[i])
		}
	}
}
