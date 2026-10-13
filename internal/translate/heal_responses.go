package translate

import "encoding/json"

// callOutputKinds are the call items whose answer the input itself must carry;
// a kind not listed here is left alone, since a stand-in of the wrong kind is
// refused as well.
var callOutputKinds = map[string]string{
	"function_call":    "function_call_output",
	"custom_tool_call": "custom_tool_call_output",
	"local_shell_call": "local_shell_call_output",
}

// HealResponsesCallOutputs gives every call in a Responses input that has no
// output a stand-in output, placed right after its batch of calls (Codex can
// sample before the last output of a batch is appended, openai/codex#44604).
// It adds no item ids, so a store:false lookup never meets an invented one. A
// body with nothing missing comes back unchanged and false.
func HealResponsesCallOutputs(body []byte) ([]byte, bool) {
	var req map[string]json.RawMessage
	if json.Unmarshal(body, &req) != nil {
		return body, false
	}
	var items []json.RawMessage
	if json.Unmarshal(req["input"], &items) != nil {
		return body, false
	}
	type head struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
	}
	heads := make([]head, len(items))
	answered := map[string]bool{}
	for i, raw := range items {
		json.Unmarshal(raw, &heads[i])
		for _, out := range callOutputKinds {
			if heads[i].Type == out && heads[i].CallID != "" {
				answered[heads[i].CallID] = true
			}
		}
	}
	inBatch := func(t string) bool { _, call := callOutputKinds[t]; return call || t == "reasoning" }
	var out []json.RawMessage
	var missing []json.RawMessage
	for i, raw := range items {
		out = append(out, raw)
		h := heads[i]
		if kind, call := callOutputKinds[h.Type]; call && h.CallID != "" && !answered[h.CallID] {
			stand, _ := json.Marshal(obj{"type": kind, "call_id": h.CallID, "output": missingToolResult})
			missing = append(missing, stand)
		}
		if len(missing) > 0 && (i == len(items)-1 || !inBatch(heads[i+1].Type)) {
			out = append(out, missing...)
			missing = nil
		}
	}
	if len(out) == len(items) {
		return body, false
	}
	b, err := json.Marshal(out)
	if err != nil {
		return body, false
	}
	req["input"] = b
	healed, err := json.Marshal(req)
	if err != nil {
		return body, false
	}
	return healed, true
}
