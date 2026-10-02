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

// Cached tokens are the prompt tokens read from the provider's cache. They are
// part of the input, and are counted again on their own so a hit rate can be read.
func TestParseCachedTokens(t *testing.T) {
	cases := map[string]struct {
		body            string
		in, out, cached int64
	}{
		"openai":    {`{"model":"m","usage":{"prompt_tokens":1000,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":896}}}`, 1000, 5, 896},
		"anthropic": {`{"model":"m","usage":{"input_tokens":4,"output_tokens":5,"cache_read_input_tokens":900,"cache_creation_input_tokens":96}}`, 1000, 5, 900},
		"anthropic stream": {"event: message_start\n" +
			`data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":4,"cache_read_input_tokens":900,"cache_creation_input_tokens":96,"output_tokens":1}}}` + "\n\n" +
			`data: {"type":"message_delta","usage":{"output_tokens":5}}` + "\n\n", 1000, 5, 900},
		"responses stream": {"event: response.created\n" +
			`data: {"type":"response.created","response":{"model":"m","usage":null}}` + "\n\n" +
			"event: response.completed\n" +
			`data: {"type":"response.completed","response":{"model":"m","usage":{"input_tokens":1000,"output_tokens":5,"input_tokens_details":{"cached_tokens":768}}}}` + "\n\n", 1000, 5, 768},
		"no cache": {`{"model":"m","usage":{"prompt_tokens":10,"completion_tokens":2}}`, 10, 2, 0},
	}
	for name, c := range cases {
		got := Parse([]byte(c.body))
		if !got.Found || got.InputTokens != c.in || got.OutputTokens != c.out || got.CachedTokens != c.cached {
			t.Errorf("%s: got %+v, want input=%d output=%d cached=%d", name, got, c.in, c.out, c.cached)
		}
	}
}

func TestParseCodeAssistStream(t *testing.T) {
	// Code Assist wraps each Gemini chunk in "response"; the last one holds the
	// totals. Thought tokens are billed as output.
	body := []byte(`data: {"response": {"candidates": [{"content": {"parts": [{"text": "O"}]}}],"modelVersion": "gemini-3.8-flash"},"traceId": "t"}` + "\n\n" +
		`data: {"response": {"candidates": [{"content": {"parts": [{"text": "K"}]},"finishReason": "STOP"}],"usageMetadata": {"promptTokenCount": 4446,"candidatesTokenCount": 29,"totalTokenCount": 4500,"thoughtsTokenCount": 25,"cachedContentTokenCount": 4425},"modelVersion": "gemini-3.8-flash"},"traceId": "t"}` + "\n\n")
	c := Parse(body)
	if !c.Found || c.Model != "gemini-3.8-flash" || c.InputTokens != 4446 || c.OutputTokens != 54 || c.CachedTokens != 4425 {
		t.Errorf("got %+v, want model=gemini-3.8-flash input=4446 output=54 cached=4425", c)
	}
}

func TestParseGeminiPlainJSON(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"parts":[{"text":"OK"}]}}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":3},"modelVersion":"gemini-3-flash"}`)
	c := Parse(body)
	if !c.Found || c.Model != "gemini-3-flash" || c.InputTokens != 12 || c.OutputTokens != 3 {
		t.Errorf("got %+v, want model=gemini-3-flash input=12 output=3", c)
	}
}
