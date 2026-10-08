package translate

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// This capsule is gateway-owned context, not native OpenAI ciphertext.
const CompactionPrefix = "llm-gateway-compact-v1:"

func EncodeCompaction(summary string) string {
	raw, _ := json.Marshal(map[string]string{"summary": summary})
	return CompactionPrefix + base64.RawURLEncoding.EncodeToString(raw)
}

func DecodeCompaction(value string) (string, error) {
	if !strings.HasPrefix(value, CompactionPrefix) {
		return "", fmt.Errorf("cannot convert native or legacy compaction state; resume through its original Responses provider")
	}
	encoded := strings.TrimPrefix(value, CompactionPrefix)
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return "", fmt.Errorf("invalid gateway compaction capsule")
	}
	var capsule struct {
		Summary string `json:"summary"`
	}
	if json.Unmarshal(raw, &capsule) != nil || strings.TrimSpace(capsule.Summary) == "" {
		return "", fmt.Errorf("invalid gateway compaction summary")
	}
	return capsule.Summary, nil
}

// ExpandGatewayCompactions preserves native items on Responses forwarding. Converted
// routes reject unknown state rather than silently dropping the conversation summary.
func ExpandGatewayCompactions(body []byte, native bool) ([]byte, error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	var items []json.RawMessage
	if json.Unmarshal(req["input"], &items) != nil {
		return body, nil
	}
	changed := false
	for i, raw := range items {
		var item struct {
			Type    string `json:"type"`
			Content string `json:"encrypted_content"`
		}
		if json.Unmarshal(raw, &item) != nil || item.Type != "compaction" {
			continue
		}
		if native && !strings.HasPrefix(item.Content, CompactionPrefix) {
			continue
		}
		summary, err := DecodeCompaction(item.Content)
		if err != nil {
			return nil, err
		}
		items[i], _ = json.Marshal(map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Conversation summary from an earlier turn:\n" + summary}}})
		changed = true
	}
	if !changed {
		return body, nil
	}
	req["input"], _ = json.Marshal(items)
	return json.Marshal(req)
}
