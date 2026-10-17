package translate

import (
	"encoding/json"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Chat upstreams refuse a tool name over 64 characters ("function tool names must be 1-64 letters,
// digits, underscores or hyphens"). MCP names reach 70 and more, so the name goes out shortened and
// comes back whole, in the declaration, the transcript, tool_choice and the answer.
func TestToolNameOver64ShortenedAndRestored(t *testing.T) {
	long := "mcp__plugin_chrome-devtools-mcp_chrome-devtools__list_console_messages"
	body := []byte(`{"model":"m","max_tokens":100,"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"` + long + `","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a"}]}],
		"tool_choice":{"type":"tool","name":"` + long + `"},
		"tools":[{"name":"` + long + `","input_schema":{"type":"object"}},{"name":"Read","input_schema":{"type":"object"}}]}`)
	names := NewToolNames(AnthropicToolNames(body), "zencore")
	short := names.ToProvider(long)
	valid := regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	if !valid.MatchString(short) || short == long {
		t.Fatalf("%q went out as %q, want a valid name of at most 64 characters", long, short)
	}
	if again := NewToolNames([]string{long}, "zencore").ToProvider(long); again != short {
		t.Errorf("the short name is %q then %q, want the same name for the same tool", short, again)
	}
	if got := names.ToProvider("Read"); got != "Read" {
		t.Errorf("a short name went out as %q", got)
	}

	out, err := AnthropicToOpenAI(body, names)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); strings.Contains(s, long) || strings.Count(s, short) != 3 {
		t.Errorf("the chat request still carries the long name, or not the short one in tools, transcript and tool_choice:\n%s", s)
	}

	whole, err := OpenAIResponseToAnthropic([]byte(`{"id":"1","model":"m","choices":[{"index":0,"finish_reason":"tool_calls",
		"message":{"content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"`+short+`","arguments":"{}"}}]}}]}`), Reply{Tools: names})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(whole), `"name":"`+long+`"`) {
		t.Errorf("the answer did not come back under the caller's name: %s", whole)
	}

	streamed := toAnthropic(t, sse(
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"`+short+`","arguments":"{}"}}]}}]}`,
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"[DONE]"), Reply{Tools: names})
	if !strings.Contains(streamed, `"name":"`+long+`"`) {
		t.Errorf("the streamed call did not come back under the caller's name:\n%s", streamed)
	}
}

// Gemini refuses a functionCall with no functionResponse after it, and a functionResponse with no
// name. A chat history with an unanswered call and a result whose call was trimmed must be healed
// the way the Responses path already heals it.
func TestGapGeminiUnpairedTools(t *testing.T) {
	chat := []byte(`{"model":"m","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"Read","arguments":"{}"}}]},
		{"role":"user","content":"never mind"},
		{"role":"tool","tool_call_id":"c9","content":"orphan result"}]}`)
	out, err := OpenAIToGemini(chat, memSigs{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	calls, answers := map[string]string{}, map[string]string{}
	for _, raw := range list(m["contents"]) {
		for _, p := range list(asObj(raw)["parts"]) {
			if fc := asObj(asObj(p)["functionCall"]); fc != nil {
				calls[str(fc["id"])] = str(fc["name"])
			}
			if fr := asObj(asObj(p)["functionResponse"]); fr != nil {
				answers[str(fr["id"])] = str(fr["name"])
			}
		}
	}
	for id, name := range calls {
		if answers[id] != name {
			t.Errorf("functionCall %s (%s) has answer %q, want a functionResponse of the same name", id, name, answers[id])
		}
	}
	for id, name := range answers {
		if name == "" || calls[id] == "" {
			t.Errorf("functionResponse %s named %q answers no call", id, name)
		}
	}
	if !strings.Contains(string(out), "orphan result") {
		t.Errorf("the orphan result's text was lost: %s", out)
	}
}

func responsesToChat(src string) string {
	var out buf
	ResponsesStreamToOpenAI(&out, strings.NewReader(src))
	return out.String()
}

// A Responses stream cut before its terminal event is not an answer: a clean "stop" + [DONE] would
// let the caller take a half answer as whole.
func TestGapResponsesUpstreamCutIsAnError(t *testing.T) {
	got := responsesToChat("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"half an ans\"}\n\n")
	if strings.Contains(got, `"finish_reason":"stop"`) || strings.Contains(got, "[DONE]") || !strings.Contains(got, `"error"`) {
		t.Errorf("a cut stream closed as a clean answer:\n%s", got)
	}
	// An error event carries its message at the top level.
	got = responsesToChat("event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"boom\"}\n\n")
	if !strings.Contains(got, `"message":"boom"`) {
		t.Errorf("the error event lost its message:\n%s", got)
	}
}

// A truncated answer keeps its length finish.
func TestResponsesIncompleteMaxTokensIsLength(t *testing.T) {
	got := responsesToChat("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"abc\"}\n\n" +
		"event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_1\",\"output\":[{\"type\":\"message\"}]," +
		"\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n")
	if !strings.Contains(got, `"finish_reason":"length"`) {
		t.Errorf("a truncated answer lost its length finish:\n%s", got)
	}
}

// A refusal arrives as response.incomplete with no output and no output tokens; reporting it as
// length or stop would hide it. Content already streamed keeps the max_output_tokens length finish.
func TestResponsesIncompleteZeroUsageIsError(t *testing.T) {
	ev := func(typ, data string) string { return "event: " + typ + "\ndata: " + data + "\n\n" }
	created := ev("response.created", `{"type":"response.created","response":{"id":"resp_1","model":"m"}}`)
	incomplete := func(output, reason, usage string) string {
		return ev("response.incomplete", `{"type":"response.incomplete","response":{"id":"resp_1","output":`+output+
			`,"incomplete_details":{"reason":"`+reason+`"}`+usage+`}}`)
	}
	zero := `,"usage":{"input_tokens":10,"output_tokens":0}`
	text := ev("response.output_text.delta", `{"type":"response.output_text.delta","delta":"abc"}`)
	cases := []struct {
		name, src string
		wantErr   bool
	}{
		{"refusal after response.created", created + incomplete(`[]`, "max_output_tokens", zero), true},
		{"refusal with no created event", incomplete(`[]`, "max_output_tokens", zero), true},
		{"empty delta is no content", created + ev("response.output_text.delta", `{"type":"response.output_text.delta","delta":""}`) +
			incomplete(`[]`, "max_output_tokens", zero), true},
		{"text then max_output_tokens without usage", text + incomplete(`[]`, "max_output_tokens", ""), false},
		{"text then content_filter", text + incomplete(`[]`, "content_filter", ""), true},
		{"reasoning then max_output_tokens", ev("response.reasoning_summary_text.delta",
			`{"type":"response.reasoning_summary_text.delta","delta":"think"}`) + incomplete(`[]`, "max_output_tokens", ""), false},
		{"tool call then max_output_tokens", ev("response.output_item.added",
			`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"c1","name":"Read"}}`) +
			incomplete(`[]`, "max_output_tokens", ""), false},
		{"encrypted reasoning item then max_output_tokens", ev("response.output_item.added",
			`{"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_1","summary":[]}}`) +
			ev("response.output_item.done", `{"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAA"}}`) +
			incomplete(`[]`, "max_output_tokens", ""), false},
		{"item added only then max_output_tokens", ev("response.output_item.added",
			`{"type":"response.output_item.added","item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}`) +
			incomplete(`[]`, "max_output_tokens", ""), false},
		{"hosted tool item then max_output_tokens", ev("response.output_item.done",
			`{"type":"response.output_item.done","item":{"type":"web_search_call","id":"ws_1","status":"completed"}}`) +
			incomplete(`[]`, "max_output_tokens", ""), false},
		{"reasoning item then content_filter", ev("response.output_item.added",
			`{"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_1","summary":[]}}`) +
			incomplete(`[]`, "content_filter", ""), true},
		{"output tokens counted", incomplete(`[]`, "max_output_tokens", `,"usage":{"output_tokens":5}`), false},
		{"output item present", incomplete(`[{"type":"message"}]`, "max_output_tokens", zero), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := responsesToChat(c.src)
			isErr := strings.Contains(got, `"error"`)
			fin := strings.Contains(got, `"finish_reason":"length"`) || strings.Contains(got, `"finish_reason":"stop"`) ||
				strings.Contains(got, `"finish_reason":"tool_calls"`)
			if c.wantErr && (!isErr || fin) {
				t.Errorf("want an error and no finish:\n%s", got)
			}
			if !c.wantErr && (isErr || !strings.Contains(got, `"finish_reason":"length"`)) {
				t.Errorf("want a length finish and no error:\n%s", got)
			}
		})
	}
}

// Some chat upstreams close a tool call turn with finish_reason "stop". A Messages caller reads
// end_turn as "the model is done" and never runs the tool_use blocks beside it.
func TestGapChatStopWithToolCallsStopReason(t *testing.T) {
	whole, err := OpenAIResponseToAnthropic([]byte(`{"id":"1","model":"m","choices":[{"index":0,"finish_reason":"stop",
		"message":{"content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"Read","arguments":"{}"}}]}}]}`), Reply{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(whole), `"stop_reason":"tool_use"`) {
		t.Errorf("whole answer with a tool call: %s", whole)
	}
	streamed := toAnthropic(t, sse(
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"Read","arguments":"{}"}}]}}]}`,
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"[DONE]"), Reply{})
	if !strings.Contains(streamed, `"stop_reason":"tool_use"`) {
		t.Errorf("streamed answer with a tool call:\n%s", streamed)
	}
}

// An OpenAI Chat request to Antigravity carries no vocabulary, so a long tool name must reach every
// site in one form: Gemini refuses a call or tool_choice that names an undeclared function.
func TestGapGeminiLongToolNameConsistent(t *testing.T) {
	long := "mcp__plugin_chrome-devtools-mcp_chrome-devtools__list_console_messages"
	body := []byte(`{"model":"m","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"` + long + `","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"ok"}],
		"tools":[{"type":"function","function":{"name":"` + long + `","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"` + long + `"}}}`)
	out, err := OpenAIToGemini(body, memSigs{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	got := map[string]string{
		"declaration": str(asObj(list(asObj(list(m["tools"])[0])["functionDeclarations"])[0])["name"]),
		"allowed":     str(list(asObj(asObj(m["toolConfig"])["functionCallingConfig"])["allowedFunctionNames"])[0]),
	}
	for _, raw := range list(m["contents"]) {
		for _, p := range list(asObj(raw)["parts"]) {
			if fc := asObj(asObj(p)["functionCall"]); fc != nil {
				got["functionCall"] = str(fc["name"])
			}
			if fr := asObj(asObj(p)["functionResponse"]); fr != nil {
				got["functionResponse"] = str(fr["name"])
			}
		}
	}
	want := got["declaration"]
	if want == "" || len(want) > 64 {
		t.Fatalf("declaration name %q, want 1-64 bytes", want)
	}
	for _, site := range []string{"functionCall", "functionResponse", "allowed"} {
		if got[site] != want {
			t.Errorf("%s names %q, the declaration %q", site, got[site], want)
		}
	}
	if v := (&ToolNames{}).ToProvider(long); v != long {
		t.Errorf("a zero ToolNames changed %q to %q", long, v)
	}
	if v := (*ToolNames)(nil).ToProvider(long); v != long {
		t.Errorf("a nil ToolNames changed %q to %q", long, v)
	}
}

// A trimmed call's result whose content is an array of text parts must keep its text.
func TestGapGeminiOrphanArrayContent(t *testing.T) {
	chat := []byte(`{"model":"m","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"Read","arguments":"{}"}}]},
		{"role":"user","content":"never mind"},
		{"role":"tool","tool_call_id":"c9","content":[{"type":"text","text":"orphan result"}]}]}`)
	out, err := OpenAIToGemini(chat, memSigs{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "orphan result") {
		t.Errorf("the orphan result's array text was lost: %s", out)
	}
}

// The reply map must name the same tool the declaration kept: the first tool to produce a Gemini name
// owns it, even when that tool's name needed no change.
func TestGeminiCutNamesFirstWins(t *testing.T) {
	a := strings.Repeat("a", 64)
	l1 := strings.Repeat("b", 64) + "_one"
	l2 := strings.Repeat("b", 64) + "_two"
	digit := "1" + strings.Repeat("c", 63)
	cases := []struct {
		name  string
		tools []string
		ask   string
		want  string
	}{
		{"a valid name keeps its own", []string{a, a + "_v2"}, a, a},
		{"two cuts alike go to the first", []string{l1, l2}, geminiName(l1), l1},
		{"one long name", []string{l1}, geminiName(l1), l1},
		{"a prefixed name cut back to 64", []string{digit}, geminiName(digit), digit},
		{"a name Gemini takes as it is", []string{"get_weather"}, "get_weather", "get_weather"},
		{"a sanitized twin of a declared name", []string{"a_b", "a b"}, "a_b", "a_b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var tools []string
			for _, n := range c.tools {
				tools = append(tools, `{"type":"function","function":{"name":"`+n+`","parameters":{"type":"object"}}}`)
			}
			body := []byte(`{"model":"m","messages":[{"role":"user","content":"go"}],"tools":[` + strings.Join(tools, ",") + `]}`)
			if got := GeminiCutNames(body).ToClient(c.ask); got != c.want {
				t.Errorf("ToClient(%q) = %q, want %q", c.ask, got, c.want)
			}
		})
	}
}

// The object form of tool_choice must name the function the declaration carries for that tool. A tool
// whose Gemini name another tool already owns has no declaration, so forcing it must not force that
// other tool.
func TestGeminiForcedToolUsesDeclaredName(t *testing.T) {
	long := "mcp__plugin_chrome-devtools-mcp_chrome-devtools__list_console_messages"
	a := strings.Repeat("a", 64)
	forced := func(t *testing.T, tools []string, choice string, vocab *ToolNames) (decls []string, allowed []any) {
		t.Helper()
		var ts []string
		for _, n := range tools {
			ts = append(ts, `{"type":"function","function":{"name":"`+n+`","parameters":{"type":"object"}}}`)
		}
		body := []byte(`{"model":"m","messages":[{"role":"user","content":"go"}],"tools":[` + strings.Join(ts, ",") +
			`],"tool_choice":{"type":"function","function":{"name":"` + choice + `"}}}`)
		out, err := OpenAIToGemini(body, memSigs{}, vocab)
		if err != nil {
			t.Fatal(err)
		}
		m := mustJSON(t, out)
		for _, d := range list(asObj(list(m["tools"])[0])["functionDeclarations"]) {
			decls = append(decls, str(asObj(d)["name"]))
		}
		return decls, list(asObj(asObj(m["toolConfig"])["functionCallingConfig"])["allowedFunctionNames"])
	}
	t.Run("long name through a vocabulary", func(t *testing.T) {
		decls, allowed := forced(t, []string{long}, long, NewToolNames([]string{long}, "antigravity"))
		if len(allowed) != 1 || str(allowed[0]) != decls[0] {
			t.Errorf("allowedFunctionNames %v, the declaration %q", allowed, decls[0])
		}
	})
	t.Run("the owner is forced by its own name", func(t *testing.T) {
		decls, allowed := forced(t, []string{a, a + "_v2"}, a, nil)
		if len(decls) != 1 || len(allowed) != 1 || str(allowed[0]) != decls[0] {
			t.Errorf("declarations %v, allowedFunctionNames %v", decls, allowed)
		}
	})
	t.Run("a tool with no declaration forces nothing else", func(t *testing.T) {
		var logs strings.Builder
		log.SetOutput(&logs)
		defer log.SetOutput(os.Stderr)
		forcedMissLast.Store(0) // each subtest starts outside the rate limit, also on a rerun
		for range 2 {
			if _, allowed := forced(t, []string{a, a + "_v2"}, a+"_v2", nil); len(allowed) != 0 {
				t.Errorf("forcing %q forces %v, another tool's declaration", a+"_v2", allowed)
			}
		}
		// The caller asked for a tool and gets none forced; that is logged, the name cut to 64 bytes with a
		// marker so it never reads as the declared tool a.
		if got := logs.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, `…" (67 bytes)`) ||
			strings.Contains(got, `"`+a+`" has no declaration`) {
			t.Errorf("want one log line with the cut marker, got:\n%s", got)
		}
	})
	t.Run("a huge name is cut", func(t *testing.T) {
		var logs strings.Builder
		log.SetOutput(&logs)
		defer log.SetOutput(os.Stderr)
		forcedMissLast.Store(0)
		forced(t, []string{a}, strings.Repeat("x", 1<<20), nil)
		if got := logs.String(); strings.Count(got, "\n") != 1 || len(got) >= 1024 || !strings.Contains(got, `…" (1048576 bytes)`) {
			t.Errorf("want one line under 1 KiB with the cut marker, got %d bytes: %.300q", len(got), got)
		}
	})
	t.Run("a name of 64 bytes is shown whole", func(t *testing.T) {
		var logs strings.Builder
		log.SetOutput(&logs)
		defer log.SetOutput(os.Stderr)
		for _, n := range []int{64, 65} {
			forcedMissLast.Store(0)
			forced(t, []string{a}, strings.Repeat("b", n), nil)
		}
		whole, cut := `"`+strings.Repeat("b", 64)+`" has no`, `"`+strings.Repeat("b", 64)+`…" (65 bytes) has no`
		if got := logs.String(); strings.Count(got, "\n") != 2 || !strings.Contains(got, whole) || !strings.Contains(got, cut) {
			t.Errorf("want the 64-byte name whole and the 65-byte name cut, got:\n%s", got)
		}
	})
	t.Run("a miss after the interval logs again", func(t *testing.T) {
		var logs strings.Builder
		log.SetOutput(&logs)
		defer log.SetOutput(os.Stderr)
		forcedMissLast.Store(0)
		forced(t, []string{a}, "missing", nil)
		forced(t, []string{a}, "missing", nil)
		if n := strings.Count(logs.String(), "\n"); n != 1 {
			t.Errorf("two misses inside the interval logged %d lines", n)
		}
		forcedMissLast.Add(-int64(forcedMissEvery))
		forced(t, []string{a}, "missing", nil)
		if n := strings.Count(logs.String(), "\n"); n != 2 {
			t.Errorf("a miss after the interval: %d lines, want 2", n)
		}
		// A clock that went backwards does not silence the log until it catches up.
		forcedMissLast.Store(time.Now().Add(time.Hour).UnixNano())
		forced(t, []string{a}, "missing", nil)
		if n := strings.Count(logs.String(), "\n"); n != 3 {
			t.Errorf("a miss after the clock went back: %d lines, want 3", n)
		}
	})
	t.Run("concurrent misses log once", func(t *testing.T) {
		var logs strings.Builder
		log.SetOutput(&logs)
		defer log.SetOutput(os.Stderr)
		forcedMissLast.Store(0)
		body := []byte(`{"model":"m","messages":[{"role":"user","content":"go"}],"tools":[{"type":"function","function":{"name":"` + a +
			`","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"missing"}}}`)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range 64 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				OpenAIToGemini(body, memSigs{}, nil)
			}()
		}
		close(start)
		wg.Wait()
		if n := strings.Count(logs.String(), "\n"); n != 1 {
			t.Errorf("64 concurrent misses logged %d lines, want 1", n)
		}
	})
	t.Run("an empty name logs nothing", func(t *testing.T) {
		var logs strings.Builder
		log.SetOutput(&logs)
		defer log.SetOutput(os.Stderr)
		forcedMissLast.Store(0)
		if _, allowed := forced(t, []string{a}, "", nil); len(allowed) != 0 || logs.Len() != 0 {
			t.Errorf("an empty forced name: allowedFunctionNames %v, log %q; want neither", allowed, logs.String())
		}
	})
	t.Run("the logged name is escaped", func(t *testing.T) {
		var logs strings.Builder
		log.SetOutput(&logs)
		defer log.SetOutput(os.Stderr)
		forcedMissLast.Store(0)
		forced(t, []string{a}, `x\ny`, nil)
		if !strings.Contains(logs.String(), `"x\ny"`) || strings.Count(logs.String(), "\n") != 1 {
			t.Errorf("want the name quoted on one line, got %q", logs.String())
		}
	})
}

// Claude Code compacts only on an invalid_request_error that reads "prompt is too long", and Codex
// only on code context_length_exceeded. A chat upstream names the overflow its own way, so its 400
// is mapped per client; a chat caller reads the upstream's own body.
func TestContextOverflowErrorPerClient(t *testing.T) {
	cases := []struct {
		name, body, wantAnthropic string
	}{
		{"openai wording", `{"error":{"message":"This model's maximum context length is 131072 tokens. However, you requested 140000 tokens (132000 in the messages, 8000 in the completion).","type":"invalid_request_error","code":"invalid_request_error"}}`,
			"prompt is too long: 132000 tokens > 131072 maximum"},
		// The prompt size leads only when it is over the maximum; here the completion budget is the excess.
		{"deepseek wording", `{"error":{"message":"This model's maximum context length is 65536 tokens. However, you requested 70543 tokens (62351 in the messages, 8192 in the completion). Please reduce the length of the messages or completion.","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`,
			"prompt is too long: 70543 tokens > 65536 maximum"},
		// A proxy (LiteLLM) serializes the upstream's error inside its own message.
		{"litellm nested error", `{"error":{"message":"litellm.BadRequestError: OpenAIException - {'error': {'message': \"This model's maximum context length is 8192 tokens. However, you requested 9000 tokens (8000 in the messages, 1000 in the completion).\", 'type': 'invalid_request_error'}}","type":"invalid_request_error"}}`,
			"prompt is too long: 9000 tokens > 8192 maximum"},
		// The nested message need not be the error object's first key (an Anthropic envelope, Azure).
		{"anthropic envelope nested", `{"error":{"message":"litellm.BadRequestError: AnthropicException - {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"prompt is too long: 210000 tokens > 200000 maximum\"}}","type":"invalid_request_error"}}`,
			"prompt is too long"},
		{"azure nested, message not first", `{"error":{"message":"litellm.BadRequestError: AzureException - {'error': {'code': 'context_length_exceeded', 'message': \"This model's maximum context length is 8192 tokens. However, you requested 9000 tokens (8000 in the messages, 1000 in the completion).\"}}","type":"invalid_request_error"}}`,
			"prompt is too long: 9000 tokens > 8192 maximum"},
		{"nested error single-quoted", `{"error":{"message":"OpenAIException - {'error': {'message': 'Your input exceeds the context window of this model.', 'type': 'invalid_request_error'}}","type":"invalid_request_error"}}`,
			"prompt is too long: OpenAIException"},
		{"messages resulted in", `{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 130512 tokens (including 1024 in the functions). Please reduce the length of the messages or functions.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`,
			"prompt is too long: 130512 tokens > 128000 maximum"},
		{"thousands separators", `{"error":{"message":"This model's maximum context length is 131,072 tokens. However, you requested 140,000 tokens.","type":"invalid_request_error"}}`,
			"prompt is too long: 140000 tokens > 131072 maximum"},
		{"code only", `{"error":{"message":"Input is too large.","type":"invalid_request_error","code":"context_length_exceeded"}}`,
			"prompt is too long: Input is too large."},
		{"responses wording", `{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error"}}`,
			"prompt is too long: Your input exceeds the context window of this model."},
		{"moonshot wording", `{"error":{"message":"Invalid request: Your request exceeded model token limit: 262144 (requested: 300000)","type":"invalid_request_error"}}`,
			"prompt is too long: 300000 tokens > 262144 maximum"},
		{"gemini wording", `{"error":{"code":400,"message":"The input token count (1200000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`,
			"prompt is too long: 1200000 tokens > 1048576 maximum"},
		{"already anthropic wording", `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 205000 tokens > 200000 maximum"}}`,
			"prompt is too long: 205000 tokens > 200000 maximum"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := asObj(mustJSON(t, Error(400, []byte(c.body), Anthropic))["error"])
			if str(e["type"]) != "invalid_request_error" || !strings.HasPrefix(str(e["message"]), c.wantAnthropic) {
				t.Errorf("anthropic client got %v, want invalid_request_error starting %q", e, c.wantAnthropic)
			}
			want := str(asObj(mustJSON(t, []byte(c.body))["error"])["message"])
			if e := asObj(mustJSON(t, Error(400, []byte(c.body), Responses))["error"]); str(e["code"]) != "context_length_exceeded" ||
				str(e["type"]) != "invalid_request_error" || str(e["message"]) != want {
				t.Errorf("responses client got %v, want code context_length_exceeded, type invalid_request_error, message %q", e, want)
			}
			if got, want := string(Error(400, []byte(c.body), OpenAI)), string(errorBefore(t, c.body)); got != want {
				t.Errorf("chat client got %s, want %s", got, want)
			}
		})
	}
	// Not an overflow: a rate limit on tokens per minute, a server error, another 400.
	others := []struct {
		status int
		body   string
	}{
		{429, `{"error":{"message":"Rate limit reached: tokens per min (TPM): limit 30000, requested 140000. This model's maximum context length is 131072 tokens.","type":"tokens"}}`},
		{500, `{"error":{"message":"maximum context length exceeded while batching","type":"server_error"}}`},
		{400, `{"error":{"message":"max_tokens is too large: 9000000. This model supports at most 64000 completion tokens.","type":"invalid_request_error"}}`},
		// The caller's own words, echoed back inside quotes.
		{400, `{"error":{"message":"Invalid value for 'tool_choice': 'what is the maximum context length of this model?' is not a tool.","type":"invalid_request_error","code":"invalid_value"}}`},
		{400, `{"error":{"message":"Unknown tool \"prompt is too long\" in tool_choice.","type":"invalid_request_error"}}`},
		{400, `{"error":{"message":"Invalid value for 'messages[2].tool_calls[0].function.arguments': '{\"message\": \"# check maximum context length\"}'","type":"invalid_request_error"}}`},
		{400, `{"error":{"message":"Invalid value for 'messages[2].tool_calls[0].function.arguments': '{\"code\": \"# check maximum context length\"}'","type":"invalid_request_error"}}`},
		{400, `{"error":{"message":"Invalid value for 'messages[2].tool_calls[0].function.arguments': '{\"q\": \"it's\", \"message\": \"# check maximum context length\"}'","type":"invalid_request_error"}}`},
		{400, `{"error":{"message":"OpenAIException - {'error': {'message': \"Invalid value: 'what is the maximum context length?' is not a tool.\"}}","type":"invalid_request_error"}}`},
	}
	for _, o := range others {
		if e := asObj(mustJSON(t, Error(o.status, []byte(o.body), Anthropic))["error"]); strings.HasPrefix(str(e["message"]), "prompt is too long") {
			t.Errorf("%d %s became an overflow for an anthropic client: %v", o.status, o.body, e)
		}
		if e := asObj(mustJSON(t, Error(o.status, []byte(o.body), Responses))["error"]); str(e["code"]) == "context_length_exceeded" {
			t.Errorf("%d %s became an overflow for a responses client: %v", o.status, o.body, e)
		}
	}
}

// vLLM and Mistral answer with a top-level message and no "error" object; its message is the one read.
func TestContextOverflowTopLevelMessage(t *testing.T) {
	const msg = "This model's maximum context length is 4096 tokens. However, you requested 5000 tokens (4500 in the messages, 500 in the completion)."
	body := `{"object":"error","message":"` + msg + `","type":"BadRequestError","param":null,"code":400}`
	if e := asObj(mustJSON(t, Error(400, []byte(body), Anthropic))["error"]); str(e["type"]) != "invalid_request_error" ||
		!strings.HasPrefix(str(e["message"]), "prompt is too long: 4500 tokens > 4096 maximum") {
		t.Errorf("anthropic client got %v, want prompt is too long: 4500 tokens > 4096 maximum", e)
	}
	if e := asObj(mustJSON(t, Error(400, []byte(body), Responses))["error"]); str(e["code"]) != "context_length_exceeded" ||
		str(e["type"]) != "invalid_request_error" || str(e["message"]) != msg {
		t.Errorf("responses client got %v, want code context_length_exceeded, type invalid_request_error, message %q", e, msg)
	}
	echo := `{"object":"error","message":"Invalid value for 'tool_choice': 'what is the maximum context length of this model?' is not a tool.","type":"BadRequestError","param":null,"code":400}`
	if e := asObj(mustJSON(t, Error(400, []byte(echo), Anthropic))["error"]); strings.HasPrefix(str(e["message"]), "prompt is too long") {
		t.Errorf("a caller echo became an overflow for an anthropic client: %v", e)
	}
	if e := asObj(mustJSON(t, Error(400, []byte(echo), Responses))["error"]); str(e["code"]) == "context_length_exceeded" {
		t.Errorf("a caller echo became an overflow for a responses client: %v", e)
	}
}

// An upstream's own error code is kept for a Responses caller; only a missing one, or one that only
// repeats the type, becomes context_length_exceeded.
func TestContextOverflowKeepsUpstreamCode(t *testing.T) {
	body := `{"error":{"message":"Your input exceeds the context window of this model.","type":"invalid_request_error","code":"string_above_max_length"}}`
	if e := asObj(mustJSON(t, Error(400, []byte(body), Responses))["error"]); str(e["code"]) != "string_above_max_length" {
		t.Errorf("responses client got %v, want the upstream code string_above_max_length", e)
	}
	if e := asObj(mustJSON(t, Error(400, []byte(body), Anthropic))["error"]); !strings.HasPrefix(str(e["message"]), "prompt is too long") {
		t.Errorf("anthropic client got %v, want prompt is too long", e)
	}
}

// errorBefore is the chat-client error envelope as intact wrote it before overflow mapping existed.
func errorBefore(t *testing.T, body string) []byte {
	t.Helper()
	m := mustJSON(t, []byte(body))
	e := asObj(m["error"])
	typ := str(e["type"])
	if typ == "" {
		typ = "invalid_request_error"
	}
	var code any
	if s := str(e["code"]); s != "" {
		code = s
	}
	b, _ := json.Marshal(obj{"error": obj{"message": str(e["message"]), "type": typ, "param": nil, "code": code}})
	return b
}

// A thinking-mode chat model (DeepSeek, Kimi) refuses the second tool step unless the assistant turn
// that called the tool carries its reasoning back as reasoning_content. Only a provider that opts in
// gets the field: a strict OpenAI server can refuse it.
func TestReasoningReplayedOnToolCallTurn(t *testing.T) {
	anthropic := []byte(`{"model":"m","max_tokens":100,"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"I should read it.","signature":"s"},{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a"}]}],
		"tools":[{"name":"Read","input_schema":{"type":"object"}}]}`)
	responses := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]},
		{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"I should read it."}]},
		{"type":"function_call","call_id":"c1","name":"Read","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"a"},
		{"type":"reasoning","id":"rs_2","summary":[{"type":"summary_text","text":"Answer now."}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],
		"tools":[{"type":"function","name":"Read","parameters":{"type":"object"}}]}`)
	convert := map[string]func(HubOptions) ([]byte, error){
		"anthropic": func(o HubOptions) ([]byte, error) { return AnthropicToOpenAIFor(anthropic, nil, o) },
		"responses": func(o HubOptions) ([]byte, error) { return ResponsesToOpenAIFor(responses, nil, o) },
	}
	for name, conv := range convert {
		t.Run(name, func(t *testing.T) {
			out, err := conv(HubOptions{ReplayReasoning: true})
			if err != nil {
				t.Fatal(err)
			}
			var withCalls, others []any
			for _, raw := range list(mustJSON(t, out)["messages"]) {
				if m := asObj(raw); str(m["role"]) == "assistant" {
					if m["tool_calls"] != nil {
						withCalls = append(withCalls, m["reasoning_content"])
					} else {
						others = append(others, m["reasoning_content"])
					}
				}
			}
			if len(withCalls) != 1 || withCalls[0] != "I should read it." {
				t.Errorf("tool-call turn reasoning_content = %v, want the thinking text: %s", withCalls, out)
			}
			for _, r := range others {
				if r != nil {
					t.Errorf("a turn without tool calls carries reasoning_content %v: %s", r, out)
				}
			}
			out, err = conv(HubOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(out), "reasoning_content") {
				t.Errorf("a provider that did not opt in got reasoning_content: %s", out)
			}
		})
	}
}

// A user turn ends the reasoning that came before it, and two reasoning items before parallel calls of
// one turn both reach that turn.
func TestResponsesReasoningReplayTurnBoundaries(t *testing.T) {
	user := func(s string) string {
		return `{"type":"message","role":"user","content":[{"type":"input_text","text":"` + s + `"}]}`
	}
	reasoning := func(s string) string {
		return `{"type":"reasoning","summary":[{"type":"summary_text","text":"` + s + `"}]}`
	}
	call := func(id string) string {
		return `{"type":"function_call","call_id":"` + id + `","name":"Read","arguments":"{}"}`
	}
	output := func(id string) string { return `{"type":"function_call_output","call_id":"` + id + `","output":"ok"}` }
	callTurns := func(t *testing.T, items ...string) []obj {
		t.Helper()
		out, err := ResponsesToOpenAIFor([]byte(`{"model":"m","input":[`+strings.Join(items, ",")+`]}`), nil, HubOptions{ReplayReasoning: true})
		if err != nil {
			t.Fatal(err)
		}
		var turns []obj
		for _, raw := range list(mustJSON(t, out)["messages"]) {
			if m := asObj(raw); str(m["role"]) == "assistant" && m["tool_calls"] != nil {
				turns = append(turns, m)
			}
		}
		return turns
	}
	t.Run("user turn resets", func(t *testing.T) {
		turns := callTurns(t, user("go"), reasoning("Old"),
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}`,
			user("next"), reasoning("New"), call("c2"), output("c2"))
		if len(turns) != 1 || str(asObj(list(turns[0]["tool_calls"])[0])["id"]) != "c2" || turns[0]["reasoning_content"] != "New" {
			t.Errorf("c2's turn = %v, want reasoning_content \"New\"", turns)
		}
	})
	t.Run("each turn its own reasoning", func(t *testing.T) {
		turns := callTurns(t, user("go"), reasoning("A"), call("c1"), output("c1"), reasoning("B"), call("c2"), output("c2"))
		if len(turns) != 2 || turns[0]["reasoning_content"] != "A" || turns[1]["reasoning_content"] != "B" {
			t.Errorf("turns = %v, want \"A\" then \"B\"", turns)
		}
	})
	t.Run("tool output resets", func(t *testing.T) {
		turns := callTurns(t, user("go"), reasoning("A"), call("c1"), reasoning("X"), output("c1"), reasoning("B"), call("c2"), output("c2"))
		if len(turns) != 2 || turns[1]["reasoning_content"] != "B" {
			t.Errorf("turns = %v, want c2's turn to carry only \"B\"", turns)
		}
	})
	t.Run("parallel calls merge", func(t *testing.T) {
		turns := callTurns(t, user("go"), reasoning("A"), call("c1"), reasoning("B"), call("c2"), output("c1"), output("c2"))
		if len(turns) != 1 || len(list(turns[0]["tool_calls"])) != 2 || turns[0]["reasoning_content"] != "A\n\nB" {
			t.Errorf("turns = %v, want one turn with 2 calls and reasoning_content \"A\\n\\nB\"", turns)
		}
	})
}

// Only response.incomplete can be a refusal. A completed answer with no output and no output tokens
// still closes as an answer.
func TestResponsesTerminalEmptyOutputIsNotARefusal(t *testing.T) {
	for _, typ := range []string{"response.completed", "response.done"} {
		for _, usage := range []string{"", `,"usage":{"output_tokens":0}`} {
			t.Run(typ+usage, func(t *testing.T) {
				got := responsesToChat("event: " + typ + "\ndata: {\"type\":\"" + typ + "\",\"response\":{\"id\":\"resp_1\",\"output\":[]" + usage + "}}\n\n")
				if strings.Contains(got, `"error"`) || !strings.Contains(got, `"finish_reason":`) || !strings.Contains(got, "data: [DONE]") {
					t.Errorf("want a finish and [DONE], no error:\n%s", got)
				}
			})
		}
	}
}

// Several thinking blocks of one tool-call turn reach it joined; thinking on a turn without tool calls
// is not replayed.
func TestAnthropicReasoningReplayJoinsBlocks(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":100,"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"plain answer","signature":"s"},{"type":"text","text":"hi"}]},
		{"role":"user","content":"read it"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"first","signature":"s"},{"type":"tool_use","id":"t1","name":"Read","input":{}},
			{"type":"thinking","thinking":"second","signature":"s"},{"type":"tool_use","id":"t2","name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a"},{"type":"tool_result","tool_use_id":"t2","content":"b"}]}],
		"tools":[{"name":"Read","input_schema":{"type":"object"}}]}`)
	out, err := AnthropicToOpenAIFor(body, nil, HubOptions{ReplayReasoning: true})
	if err != nil {
		t.Fatal(err)
	}
	var withCalls, without []any
	for _, raw := range list(mustJSON(t, out)["messages"]) {
		if m := asObj(raw); str(m["role"]) == "assistant" {
			if m["tool_calls"] != nil {
				withCalls = append(withCalls, m["reasoning_content"])
			} else {
				without = append(without, m["reasoning_content"])
			}
		}
	}
	if len(withCalls) != 1 || withCalls[0] != "first\n\nsecond" {
		t.Errorf("tool-call turn reasoning_content = %v, want \"first\\n\\nsecond\": %s", withCalls, out)
	}
	if len(without) != 1 || without[0] != nil {
		t.Errorf("the turn without tool calls carries reasoning_content %v: %s", without, out)
	}
}

// Whitespace-only thinking is no reasoning: a tool-call turn does not carry reasoning_content "   ".
func TestReasoningReplaySkipsBlankThinking(t *testing.T) {
	anth, err := AnthropicToOpenAIFor([]byte(`{"model":"m","max_tokens":100,"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"   ","signature":"s"},{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a"}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":" \n ","signature":"s"},{"type":"thinking","thinking":"real","signature":"s"},
			{"type":"tool_use","id":"t2","name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"b"}]}],
		"tools":[{"name":"Read","input_schema":{"type":"object"}}]}`), nil, HubOptions{ReplayReasoning: true})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ResponsesToOpenAIFor([]byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"  \n"}]},
		{"type":"function_call","call_id":"c1","name":"Read","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"ok"},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"\t"},{"type":"summary_text","text":"real"}]},
		{"type":"function_call","call_id":"c2","name":"Read","arguments":"{}"},
		{"type":"function_call_output","call_id":"c2","output":"ok"}]}`), nil, HubOptions{ReplayReasoning: true})
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string][]byte{"anthropic": anth, "responses": resp} {
		var got []any
		for _, raw := range list(mustJSON(t, out)["messages"]) {
			if m := asObj(raw); str(m["role"]) == "assistant" && m["tool_calls"] != nil {
				got = append(got, m["reasoning_content"])
			}
		}
		if len(got) != 2 || got[0] != nil || got[1] != "real" {
			t.Errorf("%s: tool-call turns carry reasoning_content %q, want none then \"real\": %s", name, got, out)
		}
	}
}

// A terminal event that carries status failed or cancelled is a failure, not an answer: the caller
// must get an error and no clean "stop" (OmniRoute#16909).
func TestResponsesTerminalFailedStatusIsAnError(t *testing.T) {
	for _, typ := range []string{"response.done", "response.completed"} {
		for _, status := range []string{"failed", "cancelled"} {
			t.Run(typ+"/"+status, func(t *testing.T) {
				got := responsesToChat("event: " + typ + "\ndata: {\"type\":\"" + typ + "\",\"response\":{\"id\":\"resp_1\",\"status\":\"" + status + "\",\"output\":[],\"error\":{\"message\":\"upstream boom\"}}}\n\n")
				if strings.Contains(got, `"finish_reason":"stop"`) || strings.Contains(got, "[DONE]") || !strings.Contains(got, `"error"`) {
					t.Errorf("a %s response closed as a clean answer:\n%s", status, got)
				}
			})
		}
	}
}
