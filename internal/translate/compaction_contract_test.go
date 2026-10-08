package translate

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesGatewayCompactionReplayAndStrict(t *testing.T) {
	capsule := "llm-gateway-compact-v1:" + base64.RawURLEncoding.EncodeToString([]byte(`{"summary":"KEEP_DECISION"}`))
	body, _ := json.Marshal(map[string]any{"input": []any{map[string]any{"type": "compaction", "encrypted_content": capsule}, map[string]any{"role": "user", "content": "continue"}}, "tools": []any{map[string]any{"type": "function", "name": "f", "strict": true}}})
	out, err := ResponsesToOpenAI(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "KEEP_DECISION") {
		t.Errorf("summary lost: %s", out)
	}
	if !strings.Contains(string(out), `"strict":true`) {
		t.Errorf("strict lost: %s", out)
	}
	if _, err := ResponsesToOpenAI([]byte(`{"input":[{"type":"compaction","encrypted_content":"opaque-native-ciphertext"}]}`), nil); err == nil {
		t.Error("unknown native ciphertext silently dropped")
	}
}
