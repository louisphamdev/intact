package translate

import (
	"encoding/json"
	"io"
	"strings"
	"time"
)

// CollectOpenAIStream reads Chat Completions chunks and returns the single
// chat.completion they add up to. An upstream that only streams can still
// answer a caller that asked for a whole response.
func CollectOpenAIStream(src io.Reader) []byte {
	id, model := "", ""
	var text, reasoning strings.Builder
	type call struct{ id, name, args string }
	var calls []*call
	finish := ""
	var usage obj
	var errObj obj
	sawDone := false
	streamErr := sseEvents(src, func(_, data string) bool {
		if strings.TrimSpace(data) == "[DONE]" {
			sawDone = true
			return false
		}
		ch, err := decode([]byte(data))
		if err != nil {
			return true
		}
		if e := asObj(ch["error"]); e != nil {
			errObj = e
			return false
		}
		if id == "" {
			id = str(ch["id"])
		}
		if model == "" {
			model = str(ch["model"])
		}
		if u := asObj(ch["usage"]); u != nil {
			usage = u
		}
		c := asObj(firstOf(ch["choices"]))
		if c == nil {
			return true
		}
		d := asObj(c["delta"])
		text.WriteString(str(d["content"]))
		reasoning.WriteString(str(d["reasoning_content"]))
		for _, raw := range list(d["tool_calls"]) {
			tc := asObj(raw)
			i := int(num(tc["index"]))
			for len(calls) <= i {
				calls = append(calls, &call{})
			}
			fn := asObj(tc["function"])
			if s := str(tc["id"]); s != "" {
				calls[i].id = s
			}
			if s := str(fn["name"]); s != "" {
				calls[i].name = s
			}
			calls[i].args += str(fn["arguments"])
		}
		if f := str(c["finish_reason"]); f != "" {
			finish = f
		}
		return true
	})
	if errObj != nil {
		b, _ := json.Marshal(obj{"error": errObj})
		return b
	}
	// A cut-short stream (a read error, and neither [DONE] nor a finish_reason
	// arrived) is not a finished answer: return an error envelope so relayVia
	// answers non-200 instead of presenting partial text as complete.
	if streamErr != nil && !sawDone && finish == "" {
		b, _ := json.Marshal(obj{"error": obj{"message": "upstream stream ended early: " + streamErr.Error(), "type": "api_error"}})
		return b
	}
	msg := obj{"role": "assistant", "content": text.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		var tcs []any
		for _, c := range calls {
			tcs = append(tcs, obj{"id": c.id, "type": "function", "function": obj{"name": c.name, "arguments": c.args}})
		}
		msg["tool_calls"] = tcs
	}
	if finish == "" {
		finish = "stop"
	}
	if id == "" {
		id = newID("chatcmpl-")
	}
	out := obj{"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []any{obj{"index": 0, "message": msg, "finish_reason": finish}}}
	if usage != nil {
		out["usage"] = usage
	}
	b, _ := json.Marshal(out)
	return b
}
