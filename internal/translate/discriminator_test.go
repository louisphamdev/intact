package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// Gemini's Schema has no `discriminator` field (OpenAPI keyword); one of them in a tool schema
// fails the whole request with 400 `Unknown name "discriminator"`. A property that is merely
// named "discriminator" is data and must survive.
func TestCleanGeminiSchemaDropsDiscriminatorKeyword(t *testing.T) {
	var in map[string]any
	raw := `{"type":"object","discriminator":{"propertyName":"k"},"properties":{
		"discriminator":{"type":"string"},
		"list":{"type":"array","items":{"type":"object","discriminator":{"propertyName":"k"},"properties":{"k":{"type":"string"}}}}}}`
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(CleanGeminiSchema(in))
	out := string(b)
	if strings.Contains(out, "propertyName") {
		t.Fatalf("discriminator keyword survived: %s", out)
	}
	if !strings.Contains(out, `"discriminator":{"type":"string"}`) {
		t.Fatalf("property named discriminator was lost: %s", out)
	}
}
