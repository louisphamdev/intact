package translate

import (
	"encoding/json"
	"io"
	"strings"
	"time"
)

// Responses is the OpenAI Responses API shape (Codex, some Copilot models).
const Responses = "responses"

// DefaultInstructions fills the Responses "instructions" when the caller sent
// no system prompt; the backend wants the field set.
const DefaultInstructions = "You are a helpful assistant."

// OpenAIToResponses converts a Chat Completions request to a Responses
// request. The answer is always asked for as a stream.
func OpenAIToResponses(body []byte) ([]byte, error) {
	in, err := decode(body)
	if err != nil {
		return nil, err
	}
	out := obj{"model": in["model"], "stream": true, "store": false}
	var instructions []string
	var input []any
	for _, raw := range list(in["messages"]) {
		m := asObj(raw)
		switch role := str(m["role"]); role {
		case "system", "developer":
			if t := contentText(m["content"]); t != "" {
				instructions = append(instructions, t)
			}
		case "user":
			if parts := respParts(m["content"], "input_text"); len(parts) > 0 {
				input = append(input, obj{"type": "message", "role": "user", "content": parts})
			}
		case "assistant":
			if parts := respParts(m["content"], "output_text"); len(parts) > 0 {
				input = append(input, obj{"type": "message", "role": "assistant", "content": parts})
			}
			for _, tc := range list(m["tool_calls"]) {
				t := asObj(tc)
				fn := asObj(t["function"])
				input = append(input, obj{"type": "function_call", "call_id": t["id"], "name": fn["name"], "arguments": str(fn["arguments"])})
			}
		case "tool":
			input = append(input, obj{"type": "function_call_output", "call_id": m["tool_call_id"], "output": contentText(m["content"])})
		}
	}
	if len(input) == 0 {
		input = []any{obj{"type": "message", "role": "user", "content": []any{obj{"type": "input_text", "text": "..."}}}}
	}
	out["input"] = input
	if len(instructions) > 0 {
		out["instructions"] = strings.Join(instructions, "\n\n")
	} else {
		out["instructions"] = DefaultInstructions
	}
	if tools := list(in["tools"]); len(tools) > 0 {
		var ts []any
		for _, t := range tools {
			fn := asObj(asObj(t)["function"])
			if fn == nil {
				continue
			}
			params := fn["parameters"]
			if params == nil {
				params = obj{"type": "object", "properties": obj{}}
			}
			td := obj{"type": "function", "name": fn["name"], "parameters": params}
			if d := str(fn["description"]); d != "" {
				td["description"] = d
			}
			ts = append(ts, td)
		}
		out["tools"] = ts
	}
	switch tc := in["tool_choice"].(type) {
	case string:
		out["tool_choice"] = tc
	case obj:
		if name := str(asObj(tc["function"])["name"]); name != "" {
			out["tool_choice"] = obj{"type": "function", "name": name}
		}
	}
	if e := str(in["reasoning_effort"]); e != "" {
		out["reasoning"] = obj{"effort": e, "summary": "auto"}
		// With store off, the reasoning of a turn comes back encrypted so the
		// next turn can hand it back; the Codex CLI always asks for it.
		out["include"] = []any{"reasoning.encrypted_content"}
	}
	// Fields both APIs share keep their value; the Codex CLI sends each of them.
	for _, k := range []string{"parallel_tool_calls", "service_tier", "prompt_cache_key", "safety_identifier"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	if v := str(in["verbosity"]); v != "" {
		out["text"] = obj{"verbosity": v}
	}
	return json.Marshal(out)
}

// contentText returns the text of OpenAI message content.
func contentText(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var b []string
		for _, p := range v {
			if t := str(asObj(p)["text"]); t != "" {
				b = append(b, t)
			}
		}
		return strings.Join(b, "\n")
	}
	return ""
}

func respParts(c any, textType string) []any {
	switch v := c.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []any{obj{"type": textType, "text": v}}
	case []any:
		var out []any
		for _, p := range v {
			part := asObj(p)
			switch str(part["type"]) {
			case "text", "input_text", "output_text":
				if t := str(part["text"]); t != "" {
					out = append(out, obj{"type": textType, "text": t})
				}
			case "image_url":
				u := str(asObj(part["image_url"])["url"])
				if u == "" {
					u = str(part["image_url"])
				}
				if u != "" && textType == "input_text" {
					detail := str(asObj(part["image_url"])["detail"])
					if detail == "" {
						detail = "auto"
					}
					out = append(out, obj{"type": "input_image", "image_url": u, "detail": detail})
				}
			}
		}
		return out
	}
	return nil
}

// ResponsesStreamToOpenAI reads a Responses event stream and writes the
// equivalent Chat Completions chunks, ending with usage and [DONE].
func ResponsesStreamToOpenAI(dst Flusher, src io.Reader) {
	id, model := newID("chatcmpl-"), ""
	created := time.Now().Unix()
	toolIndex := map[string]int{} // output item id -> tool call index
	sawArgs := map[string]bool{}
	started, done := false, false
	emit := func(delta obj, fin any, extra obj) {
		ch := obj{"index": 0, "delta": delta, "finish_reason": fin}
		c := obj{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{ch}}
		for k, v := range extra {
			c[k] = v
		}
		b, _ := json.Marshal(c)
		io.WriteString(dst, "data: "+string(b)+"\n\n")
		dst.Flush()
	}
	start := func() {
		if !started {
			started = true
			emit(obj{"role": "assistant", "content": ""}, nil, nil)
		}
	}
	finish := func(resp obj) {
		start()
		fin := "stop"
		if len(toolIndex) > 0 {
			fin = "tool_calls"
		}
		if s := str(asObj(resp["incomplete_details"])["reason"]); s == "max_output_tokens" {
			fin = "length"
		}
		var extra obj
		if u := asObj(resp["usage"]); u != nil {
			in, out := num(u["input_tokens"]), num(u["output_tokens"])
			us := obj{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out}
			if c := num(asObj(u["input_tokens_details"])["cached_tokens"]); c > 0 {
				us["prompt_tokens_details"] = obj{"cached_tokens": c}
			}
			if r := num(asObj(u["output_tokens_details"])["reasoning_tokens"]); r > 0 {
				us["completion_tokens_details"] = obj{"reasoning_tokens": r}
			}
			extra = obj{"usage": us}
		}
		emit(obj{}, fin, extra)
		io.WriteString(dst, "data: [DONE]\n\n")
		dst.Flush()
		done = true
	}
	sseEvents(src, func(event, data string) bool {
		ev, err := decode([]byte(data))
		if err != nil {
			return true
		}
		typ := str(ev["type"])
		if typ == "" {
			typ = event
		}
		switch typ {
		case "response.created", "response.in_progress":
			r := asObj(ev["response"])
			if s := str(r["id"]); s != "" {
				id = "chatcmpl-" + strings.TrimPrefix(s, "resp_")
			}
			if s := str(r["model"]); s != "" {
				model = s
			}
			start()
		case "response.output_text.delta":
			start()
			emit(obj{"content": ev["delta"]}, nil, nil)
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			start()
			emit(obj{"reasoning_content": ev["delta"]}, nil, nil)
		case "response.output_item.added":
			item := asObj(ev["item"])
			if t := str(item["type"]); t == "function_call" || t == "custom_tool_call" {
				start()
				i := len(toolIndex)
				toolIndex[str(item["id"])] = i
				emit(obj{"tool_calls": []any{obj{"index": i, "id": item["call_id"], "type": "function",
					"function": obj{"name": item["name"], "arguments": ""}}}}, nil, nil)
			}
		case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
			key := str(ev["item_id"])
			if i, ok := toolIndex[key]; ok {
				sawArgs[key] = true
				emit(obj{"tool_calls": []any{obj{"index": i, "function": obj{"arguments": ev["delta"]}}}}, nil, nil)
			}
		case "response.output_item.done":
			item := asObj(ev["item"])
			key := str(item["id"])
			if i, ok := toolIndex[key]; ok && !sawArgs[key] {
				args := str(item["arguments"])
				if args == "" {
					args = str(item["input"])
				}
				emit(obj{"tool_calls": []any{obj{"index": i, "function": obj{"arguments": args}}}}, nil, nil)
			}
		case "response.completed", "response.done", "response.incomplete":
			finish(asObj(ev["response"]))
			return false
		case "response.failed", "error":
			msg := str(asObj(ev["error"])["message"])
			if msg == "" {
				msg = str(asObj(asObj(ev["response"])["error"])["message"])
			}
			b, _ := json.Marshal(obj{"error": obj{"message": msg, "type": "api_error"}})
			io.WriteString(dst, "data: "+string(b)+"\n\n")
			dst.Flush()
			done = true
			return false
		}
		return true
	})
	if !done {
		finish(obj{})
	}
}
