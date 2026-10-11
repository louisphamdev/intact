package translate

import (
	"regexp"
	"strings"
	"testing"
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
