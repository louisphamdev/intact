package translate

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// truncReader hands over its data once, then reports the stream was cut short
// (io.ErrUnexpectedEOF), the way a reset upstream connection does.
type truncReader struct {
	s    string
	done bool
}

func (r *truncReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.ErrUnexpectedEOF
	}
	r.done = true
	return copy(p, r.s), nil
}

type capFlush struct{ b *bytes.Buffer }

func (c capFlush) Write(p []byte) (int, error) { return c.b.Write(p) }
func (c capFlush) Flush() error                { return nil }

func TestOpenAIStreamTruncationDoesNotFakeCompletion(t *testing.T) {
	var buf bytes.Buffer
	src := &truncReader{s: `data: {"id":"chatcmpl-x","model":"m","choices":[{"delta":{"content":"hel"}}]}` + "\n\n"}
	OpenAIStreamToAnthropic(capFlush{&buf}, src)
	out := buf.String()
	if strings.Contains(out, "message_stop") || strings.Contains(out, "end_turn") {
		t.Errorf("truncated stream emitted a clean finish:\n%s", out)
	}
	if !strings.Contains(out, "error") {
		t.Errorf("truncated stream did not signal an error:\n%s", out)
	}
}

func TestGeminiStreamTruncationDoesNotFakeCompletion(t *testing.T) {
	var buf bytes.Buffer
	src := &truncReader{s: `data: {"candidates":[{"content":{"parts":[{"text":"hel"}]}}]}` + "\n\n"}
	GeminiStreamToOpenAI(capFlush{&buf}, src, nil)
	out := buf.String()
	if strings.Contains(out, "[DONE]") || strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("truncated Gemini stream emitted a clean finish:\n%s", out)
	}
	if !strings.Contains(out, "error") {
		t.Errorf("truncated Gemini stream did not signal an error:\n%s", out)
	}
}

func TestOpenAIStreamParallelToolCallsKeepAllArgs(t *testing.T) {
	lines := []string{
		`{"id":"c","model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"id":"t0","type":"function","function":{"name":"f0","arguments":""}},{"index":1,"id":"t1","type":"function","function":{"name":"f1","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"b\":2}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	}
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString("data: " + l + "\n\n")
	}
	var buf bytes.Buffer
	OpenAIStreamToAnthropic(capFlush{&buf}, strings.NewReader(sb.String()))
	out := buf.String()
	if n := strings.Count(out, "input_json_delta"); n < 2 {
		t.Errorf("want 2 tool-arg deltas, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, `"name":"f0"`) || !strings.Contains(out, `"name":"f1"`) {
		t.Errorf("a tool_use block was dropped:\n%s", out)
	}
	for _, want := range []string{`\"a\":1`, `\"b\":2`} {
		if !strings.Contains(out, want) {
			t.Errorf("lost tool args %q:\n%s", want, out)
		}
	}
}

func TestCollectOpenAIStreamTruncationReturnsError(t *testing.T) {
	// A stream with content but cut short before [DONE] / finish_reason.
	src := &truncReader{s: `data: {"id":"c","model":"m","choices":[{"delta":{"content":"partial"}}]}` + "\n\n"}
	out := CollectOpenAIStream(src)
	if !strings.Contains(string(out), `"error"`) {
		t.Errorf("truncated collect returned a non-error body: %s", out)
	}
	// A clean stream still collects normally.
	clean := strings.NewReader("data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	out2 := CollectOpenAIStream(clean)
	if strings.Contains(string(out2), `"error"`) {
		t.Errorf("clean collect wrongly returned an error: %s", out2)
	}
}
