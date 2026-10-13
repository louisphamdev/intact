package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// A tool output that carries an image or a file must not reach the model as
// base64 text (The-NeXT-AI/ai-gateway#28): a 5.9 MB image became 4.1M tokens.
func TestResponsesToolOutputMediaIsNotText(t *testing.T) {
	blob := strings.Repeat("QUFB", 5000)
	body := `{"model":"m","input":[` +
		`{"type":"function_call","call_id":"c1","name":"shot","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":[` +
		`{"type":"input_image","image_url":"data:image/png;base64,` + blob + `"},` +
		`{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,` + blob + `"}]}]}`
	out, err := ResponsesToOpenAIFor([]byte(body), nil, HubOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Messages []obj }
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	var tool, image obj
	for _, m := range got.Messages {
		switch str(m["role"]) {
		case "tool":
			tool = m
		case "user":
			image = m
		}
	}
	if tool == nil || strings.Contains(str(tool["content"]), blob) {
		t.Fatalf("tool message carries the base64 as text: %.300v", tool)
	}
	if !strings.Contains(str(tool["content"]), "a.pdf") {
		t.Errorf("tool text should name the file it left out: %q", tool["content"])
	}
	parts := list(image["content"])
	if len(parts) != 1 || str(asObj(asObj(parts[0])["image_url"])["url"]) != "data:image/png;base64,"+blob {
		t.Fatalf("the image should follow as a user image part, got %.300v", image)
	}
}
