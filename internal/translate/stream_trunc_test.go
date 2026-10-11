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
	OpenAIStreamToAnthropic(capFlush{&buf}, src, Reply{})
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
	GeminiStreamToOpenAI(capFlush{&buf}, src, nil, nil)
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
	OpenAIStreamToAnthropic(capFlush{&buf}, strings.NewReader(sb.String()), Reply{})
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

func geminiChunks(src string) string {
	var buf bytes.Buffer
	GeminiStreamToOpenAI(capFlush{&buf}, strings.NewReader(src), nil, nil)
	return buf.String()
}

// rendered passes Gemini-made chunks on to a Messages and a Responses caller, which must both
// still see the stream end.
func rendered(chunks string) (anthropic, responses string) {
	var a, r bytes.Buffer
	OpenAIStreamToAnthropic(capFlush{&a}, strings.NewReader(chunks), Reply{})
	OpenAIStreamToResponses(capFlush{&r}, strings.NewReader(chunks), nil)
	return a.String(), r.String()
}

// Gemini always closes an answer with a finishReason. A clean EOF without one is a cut stream,
// and reporting it as a stop hands the caller a truncated answer as complete.
func TestGeminiStreamWithoutFinishReasonIsAnError(t *testing.T) {
	out := geminiChunks(`data: {"response":{"candidates":[{"content":{"parts":[{"text":"hel"}]}}]}}` + "\n\n")
	if strings.Contains(out, "[DONE]") || strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("a stream with no finishReason closed cleanly:\n%s", out)
	}
	if !strings.Contains(out, `"error"`) {
		t.Fatalf("no error chunk:\n%s", out)
	}
	a, r := rendered(out)
	if !strings.Contains(a, "event: error") || strings.Contains(a, "end_turn") {
		t.Errorf("messages caller did not get an error end:\n%s", a)
	}
	if !strings.Contains(r, "response.failed") || strings.Contains(r, "response.completed") {
		t.Errorf("responses caller did not get a failed end:\n%s", r)
	}
}

// MALFORMED_FUNCTION_CALL means the model tried a call Gemini could not parse. Mapped to a stop it
// gave an empty, successful turn, and an agent loop took that as the end of the work.
func TestGeminiMalformedFunctionCallSurfaces(t *testing.T) {
	out := geminiChunks(`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"MALFORMED_FUNCTION_CALL","finishMessage":"Malformed function call: print(x"}]}}` + "\n\n")
	if strings.Contains(out, "[DONE]") || strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("a malformed call closed as a clean answer:\n%s", out)
	}
	if !strings.Contains(out, `"error"`) || !strings.Contains(out, "MALFORMED_FUNCTION_CALL") {
		t.Fatalf("the error does not name the cause:\n%s", out)
	}
	if a, r := rendered(out); !strings.Contains(a, "event: error") || !strings.Contains(r, "response.failed") {
		t.Errorf("callers did not get an error end:\nmessages:\n%s\nresponses:\n%s", a, r)
	}

	// An upstream error event is also the last chunk: a clean stop after it reads as success.
	out = geminiChunks(`data: {"error":{"code":503,"message":"overloaded","status":"UNAVAILABLE"}}` + "\n\n")
	if !strings.Contains(out, "overloaded") || strings.Contains(out, "[DONE]") || strings.Contains(out, "finish_reason\":\"stop") {
		t.Errorf("an upstream error was followed by a clean finish:\n%s", out)
	}
}

// A blocked prompt comes back with promptFeedback.blockReason and no candidates. That is a
// refusal, not an empty answer.
func TestGeminiBlockReasonIsContentFilter(t *testing.T) {
	out := geminiChunks(`data: {"response":{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":5}}}` + "\n\n")
	if !strings.Contains(out, `"finish_reason":"content_filter"`) || !strings.Contains(out, "[DONE]") {
		t.Fatalf("blocked prompt is not a content_filter finish:\n%s", out)
	}
	a, r := rendered(out)
	if !strings.Contains(a, `"stop_reason":"refusal"`) || !strings.Contains(a, "message_stop") {
		t.Errorf("messages caller did not get a refusal:\n%s", a)
	}
	if !strings.Contains(r, "response.completed") {
		t.Errorf("responses caller stream did not end:\n%s", r)
	}
}
