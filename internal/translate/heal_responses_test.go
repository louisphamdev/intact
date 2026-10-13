package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func inputTypes(t *testing.T, body []byte) []string {
	t.Helper()
	var req struct{ Input []obj }
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range req.Input {
		s := str(it["type"])
		if id := str(it["call_id"]); id != "" {
			s += ":" + id
		}
		out = append(out, s)
	}
	return out
}

// Codex can sample before the last output of a batch is in its history
// (openai/codex#44604); the upstream answers 400 and the thread is wedged. The
// missing output gets a stand-in right after the call batch.
func TestHealResponsesCallOutputsAddsAStandInAfterTheBatch(t *testing.T) {
	body := []byte(`{"model":"m","store":false,"input":[` +
		`{"type":"message","role":"user","content":"go"},` +
		`{"type":"reasoning","encrypted_content":"x"},` +
		`{"type":"function_call","call_id":"a","name":"f","arguments":"{}"},` +
		`{"type":"function_call","call_id":"b","name":"f","arguments":"{}"},` +
		`{"type":"message","role":"developer","content":"notice"},` +
		`{"type":"function_call_output","call_id":"a","output":"A"}]}`)
	out, ok := HealResponsesCallOutputs(body)
	if !ok {
		t.Fatal("nothing healed")
	}
	got := strings.Join(inputTypes(t, out), " ")
	want := "message reasoning function_call:a function_call:b function_call_output:b message function_call_output:a"
	if got != want {
		t.Fatalf("input = %s\nwant    %s", got, want)
	}
	if !strings.Contains(string(out), `"store":false`) {
		t.Fatalf("other fields lost: %s", out)
	}
}

func TestHealResponsesCallOutputsKeepsACompleteBodyByteForByte(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"function_call","call_id":"a","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"A"},{"type":"tool_search_call","call_id":"z"}]}`)
	out, ok := HealResponsesCallOutputs(body)
	if ok || string(out) != string(body) {
		t.Fatalf("complete body rewritten (ok=%v): %s", ok, out)
	}
}

func TestHealResponsesCallOutputsAnswersEachKnownCallKind(t *testing.T) {
	body := []byte(`{"input":[{"type":"custom_tool_call","call_id":"c","name":"apply_patch","input":"x"},{"type":"local_shell_call","call_id":"l","action":{}}]}`)
	out, _ := HealResponsesCallOutputs(body)
	got := strings.Join(inputTypes(t, out), " ")
	if got != "custom_tool_call:c local_shell_call:l custom_tool_call_output:c local_shell_call_output:l" {
		t.Fatalf("input = %s", got)
	}
}
