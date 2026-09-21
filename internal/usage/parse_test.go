package usage

import "testing"

func TestParseOpenAIPlainJSON(t *testing.T) {
	body := []byte(`{"model":"llama-3.3-70b-versatile","choices":[{"message":{"content":"OK"}}],
		"usage":{"prompt_tokens":78,"completion_tokens":56,"total_tokens":134}}`)
	c := Parse(body)
	if !c.Found {
		t.Fatal("Found = false, want true")
	}
	if c.Model != "llama-3.3-70b-versatile" || c.InputTokens != 78 || c.OutputTokens != 56 {
		t.Errorf("got %+v, want model=llama-3.3-70b-versatile input=78 output=56", c)
	}
}

func TestParseAnthropicPlainJSON(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","content":[{"type":"text","text":"OK"}],
		"usage":{"input_tokens":120,"output_tokens":45}}`)
	c := Parse(body)
	if !c.Found || c.Model != "claude-opus-4-8" || c.InputTokens != 120 || c.OutputTokens != 45 {
		t.Errorf("got %+v, want model=claude-opus-4-8 input=120 output=45", c)
	}
}

func TestParseAnthropicStream(t *testing.T) {
	// Anthropic splits usage: message_start carries input_tokens and a partial
	// output, message_delta carries the final cumulative output_tokens.
	body := []byte("event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-opus-4-8","usage":{"input_tokens":78,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":56}}` + "\n\n")
	c := Parse(body)
	if !c.Found || c.Model != "claude-opus-4-8" || c.InputTokens != 78 || c.OutputTokens != 56 {
		t.Errorf("got %+v, want model=claude-opus-4-8 input=78 output=56", c)
	}
}

func TestParseOpenAIStream(t *testing.T) {
	// OpenAI carries usage only in the final data chunk before [DONE], and the
	// earlier chunks send usage: null.
	body := []byte(
		`data: {"choices":[{"delta":{"content":"OK"}}],"usage":null}` + "\n\n" +
			`data: {"model":"llama-3.3-70b","choices":[],"usage":{"prompt_tokens":78,"completion_tokens":56}}` + "\n\n" +
			"data: [DONE]\n\n")
	c := Parse(body)
	if !c.Found || c.InputTokens != 78 || c.OutputTokens != 56 {
		t.Errorf("got %+v, want input=78 output=56", c)
	}
}

func TestParseNoUsage(t *testing.T) {
	c := Parse([]byte(`{"error":{"message":"bad request"}}`))
	if c.Found {
		t.Errorf("Found = true on a body with no usage: %+v", c)
	}
}

// Bug A: a plain JSON body whose content contains the substring "data:" must
// not be mistaken for an SSE stream.
func TestParsePlainJSONWithDataSubstring(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","choices":[{"message":{"content":"see data:image/png;base64,iVB"}}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":20}}`)
	c := Parse(body)
	if !c.Found || c.Model != "gpt-4o" || c.InputTokens != 10 || c.OutputTokens != 20 {
		t.Errorf("got %+v, want model=gpt-4o input=10 output=20 found=true", c)
	}
}

// Bug J: Anthropic reports cached prompt tokens in separate fields. They are
// part of the input and must be counted.
func TestParseAnthropicCacheTokens(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","usage":{"input_tokens":10,` +
		`"cache_read_input_tokens":5000,"cache_creation_input_tokens":200,"output_tokens":20}}`)
	c := Parse(body)
	if !c.Found || c.InputTokens != 5210 || c.OutputTokens != 20 {
		t.Errorf("got %+v, want input=5210 output=20", c)
	}
}
