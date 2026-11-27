package translate

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// These cases pin the Responses converter, not the Chat protocol: a Chat stream
// may keep partial tool arguments with its original finish_reason, but the
// converter must never present a cut call as a completed Responses item.
func auditChat(finish, args string, withCall bool) string {
	msg := `{"role":"assistant","content":""}`
	if withCall {
		msg = fmt.Sprintf(`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":%s}}]}`, strconv.Quote(args))
	}
	return fmt.Sprintf(`{"id":"chatcmpl-1","model":"m","choices":[{"index":0,"message":%s,"finish_reason":%s}]}`, msg, strconv.Quote(finish))
}

func auditSSE(finish, args string, withCall bool) string {
	var parts []string
	if withCall {
		parts = append(parts, fmt.Sprintf(`data: {"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":%s}}]}}]}`, strconv.Quote(args)))
	}
	parts = append(parts,
		fmt.Sprintf(`data: {"choices":[{"index":0,"delta":{},"finish_reason":%s}]}`, strconv.Quote(finish)),
		`data: [DONE]`, ``)
	return strings.Join(parts, "\n\n")
}

func auditCheckResponse(t *testing.T, name string, out []byte, reason string, wantCall bool) {
	t.Helper()
	m := mustJSON(t, out)
	if m["status"] != "incomplete" {
		t.Errorf("%s: status = %v, want incomplete: %s", name, m["status"], out)
	}
	d := asObj(m["incomplete_details"])
	if d == nil || str(d["reason"]) != reason {
		t.Errorf("%s: incomplete_details = %s, want reason %q", name, j(m["incomplete_details"]), reason)
	}
	calls := 0
	for _, raw := range list(m["output"]) {
		if it := asObj(raw); it != nil && str(it["type"]) == "function_call" {
			calls++
		}
	}
	if wantCall && calls != 1 {
		t.Errorf("%s: function_call items = %d, want 1: %s", name, calls, out)
	}
	if !wantCall && calls != 0 {
		t.Errorf("%s: cut arguments became %d completed function_call items: %s", name, calls, out)
	}
}

func auditCheckLive(t *testing.T, name, sse, reason string, wantCall bool) {
	t.Helper()
	var out buf
	OpenAIStreamToResponses(&out, strings.NewReader(sse), nil)
	evs := events(t, out.String())
	if len(evs) == 0 {
		t.Fatalf("%s: no events", name)
	}
	last := asObj(evs[len(evs)-1]["response"])
	if last == nil || last["status"] != "incomplete" {
		t.Fatalf("%s: last = %s", name, out.String())
	}
	d := asObj(last["incomplete_details"])
	if d == nil || str(d["reason"]) != reason {
		t.Errorf("%s: incomplete_details = %s, want reason %q", name, j(last["incomplete_details"]), reason)
	}
	calls, doneArgs := 0, 0
	for _, e := range evs {
		if str(e["type"]) == "response.function_call_arguments.done" {
			doneArgs++
		}
		if str(e["type"]) == "response.output_item.done" {
			if it := asObj(e["item"]); it != nil && str(it["type"]) == "function_call" {
				calls++
			}
		}
	}
	if wantCall && (calls != 1 || doneArgs != 1) {
		t.Errorf("%s: done function_call = %d, arguments.done = %d, want 1/1", name, calls, doneArgs)
	}
	if !wantCall && (calls != 0 || doneArgs != 0) {
		t.Errorf("%s: cut arguments were finalized: done function_call = %d, arguments.done = %d", name, calls, doneArgs)
	}
}

func TestContentFilterToolArgsNonstream(t *testing.T) {
	cases := []struct {
		name     string
		finish   string
		args     string
		call     bool
		reason   string
		wantCall bool
	}{
		{"content_filter/cut", "content_filter", `{"a":`, true, "content_filter", false},
		{"content_filter/complete", "content_filter", `{"a":1}`, true, "content_filter", true},
		{"length/cut", "length", `{"a":`, true, "max_output_tokens", false},
		{"length/complete", "length", `{"a":1}`, true, "max_output_tokens", true},
		{"content_filter/no_tool", "content_filter", "", false, "content_filter", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := OpenAIResponseToResponses([]byte(auditChat(c.finish, c.args, c.call)), nil)
			if err != nil {
				t.Fatal(err)
			}
			auditCheckResponse(t, c.name, out, c.reason, c.wantCall)
		})
	}
}

func TestContentFilterToolArgsLive(t *testing.T) {
	cases := []struct {
		name     string
		finish   string
		args     string
		call     bool
		reason   string
		wantCall bool
	}{
		{"content_filter/cut", "content_filter", `{"a":`, true, "content_filter", false},
		{"content_filter/complete", "content_filter", `{"a":1}`, true, "content_filter", true},
		{"length/cut", "length", `{"a":`, true, "max_output_tokens", false},
		{"length/complete", "length", `{"a":1}`, true, "max_output_tokens", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			auditCheckLive(t, c.name, auditSSE(c.finish, c.args, c.call), c.reason, c.wantCall)
		})
	}
}

func TestContentFilterToolArgsBuffered(t *testing.T) {
	cases := []struct {
		name     string
		finish   string
		args     string
		reason   string
		wantCall bool
	}{
		{"content_filter/cut", "content_filter", `{"a":`, "content_filter", false},
		{"content_filter/complete", "content_filter", `{"a":1}`, "content_filter", true},
		{"length/cut", "length", `{"a":`, "max_output_tokens", false},
		{"length/complete", "length", `{"a":1}`, "max_output_tokens", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			chat := CollectOpenAIStream(strings.NewReader(auditSSE(c.finish, c.args, true)))
			m := mustJSON(t, chat)
			choice := asObj(list(m["choices"])[0])
			if str(choice["finish_reason"]) != c.finish {
				t.Fatalf("%s: Chat finish_reason = %v, want %v", c.name, choice["finish_reason"], c.finish)
			}
			out, err := OpenAIResponseToResponses(chat, nil)
			if err != nil {
				t.Fatal(err)
			}
			auditCheckResponse(t, c.name, out, c.reason, c.wantCall)
		})
	}
}
